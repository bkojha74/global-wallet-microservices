package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"wallet-system/pkg/observability"
)

const (
	exchangeName  = "wallet.logs.v1"
	queueName     = "wallet.logging.ingest.v1"
	dlxName       = "wallet.logs.dlx.v1"
	dlqName       = "wallet.logging.dead.v1"
	routingKey    = "#" // Match all topics
	prefetchCount = 50
	maxRetries    = 3
)

var retryBackoff = 2 * time.Second

// queueTypeArgs returns the amqp.Table needed to declare a queue of the correct
// type. Use LOGGING_QUEUE_TYPE=quorum for production; leave blank for local dev.
func queueTypeArgs() amqp.Table {
	if os.Getenv("LOGGING_QUEUE_TYPE") == "quorum" {
		log.Println("[LOGGING-SERVICE] Queue type: quorum (production mode)")
		return amqp.Table{"x-queue-type": "quorum"}
	}
	return nil // classic queue — default
}

type Consumer struct {
	amqpURI string
	conn    *amqp.Connection
	ch      *amqp.Channel
	repo    LogRepository
}

func NewConsumer(amqpURI string, repo LogRepository) (*Consumer, error) {
	c := &Consumer{
		amqpURI: amqpURI,
		repo:    repo,
	}
	if err := c.connect(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Consumer) connect() error {
	c.cleanup()

	conn, err := amqp.Dial(c.amqpURI)
	if err != nil {
		return fmt.Errorf("failed to connect to RabbitMQ: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return fmt.Errorf("failed to open a channel: %w", err)
	}

	// 1. Declare DLX and DLQ
	err = ch.ExchangeDeclare(
		dlxName,
		"direct", // type
		true,     // durable
		false,    // auto-deleted
		false,    // internal
		false,    // no-wait
		nil,      // arguments
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return fmt.Errorf("failed to declare DLX: %w", err)
	}

	queueArgs := queueTypeArgs()
	_, err = ch.QueueDeclare(
		dlqName,
		true,      // durable
		false,     // delete when unused
		false,     // exclusive
		false,     // no-wait
		queueArgs, // x-queue-type: quorum when LOGGING_QUEUE_TYPE=quorum
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return fmt.Errorf("failed to declare DLQ: %w", err)
	}

	err = ch.QueueBind(
		dlqName,
		"#", // bind all DLX routing keys
		dlxName,
		false,
		nil,
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return fmt.Errorf("failed to bind DLQ: %w", err)
	}

	// 2. Declare Main Exchange and Queue
	err = ch.ExchangeDeclare(
		exchangeName,
		"topic", // type
		true,    // durable
		false,   // auto-deleted
		false,   // internal
		false,   // no-wait
		nil,     // arguments
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return fmt.Errorf("failed to declare main exchange: %w", err)
	}

	// Merge DLX routing with optional quorum type args.
	mainQueueArgs := amqp.Table{
		"x-dead-letter-exchange": dlxName,
	}
	if os.Getenv("LOGGING_QUEUE_TYPE") == "quorum" {
		mainQueueArgs["x-queue-type"] = "quorum"
	}
	_, err = ch.QueueDeclare(
		queueName,
		true,          // durable
		false,         // delete when unused
		false,         // exclusive
		false,         // no-wait
		mainQueueArgs, // DLX + optional quorum type
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return fmt.Errorf("failed to declare main queue: %w", err)
	}

	err = ch.QueueBind(
		queueName,
		routingKey,
		exchangeName,
		false,
		nil,
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return fmt.Errorf("failed to bind main queue: %w", err)
	}

	// 3. Set QoS
	err = ch.Qos(
		prefetchCount, // prefetch count
		0,             // prefetch size
		false,         // global
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return fmt.Errorf("failed to set QoS: %w", err)
	}

	c.conn = conn
	c.ch = ch
	return nil
}

func (c *Consumer) cleanup() {
	if c.ch != nil {
		_ = c.ch.Close()
		c.ch = nil
	}
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

func (c *Consumer) Start(ctx context.Context) error {
	backoff := 1 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			log.Println("[LOGGING-SERVICE] Context cancelled, stopping consumer")
			return nil
		default:
		}

		if c.conn == nil || c.conn.IsClosed() || c.ch == nil || c.ch.IsClosed() {
			log.Println("[LOGGING-SERVICE] Reconnecting to RabbitMQ...")
			if err := c.connect(); err != nil {
				log.Printf("[WARN] RabbitMQ reconnect failed: %v. Retrying in %v...", err, backoff)
				select {
				case <-time.After(backoff):
					backoff = backoff * 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
					continue
				case <-ctx.Done():
					return nil
				}
			}
			backoff = 1 * time.Second
			log.Println("[LOGGING-SERVICE] Successfully reconnected to RabbitMQ.")
		}

		msgs, err := c.ch.Consume(
			queueName,
			"logging-service", // consumer
			false,             // auto-ack
			false,             // exclusive
			false,             // no-local
			false,             // no-wait
			nil,               // args
		)
		if err != nil {
			log.Printf("[WARN] Failed to register consumer: %v. Reconnecting in %v...", err, backoff)
			c.cleanup()
			select {
			case <-time.After(backoff):
				backoff = backoff * 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			case <-ctx.Done():
				return nil
			}
		}

		log.Println("[LOGGING-SERVICE] RabbitMQ consumer listening for incoming events...")

		consumeErr := c.consumeLoop(ctx, msgs)
		if ctx.Err() != nil {
			return nil
		}

		log.Printf("[WARN] RabbitMQ consumer channel closed: %v. Initiating automatic reconnect...", consumeErr)
		c.cleanup()
		time.Sleep(1 * time.Second)
	}
}

func (c *Consumer) consumeLoop(ctx context.Context, msgs <-chan amqp.Delivery) error {
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				return fmt.Errorf("consumer channel closed")
			}
			c.processMessage(ctx, msg)
		case <-ctx.Done():
			return nil
		}
	}
}

func (c *Consumer) processMessage(ctx context.Context, msg amqp.Delivery) {
	var event observability.Event
	if err := json.Unmarshal(msg.Body, &event); err != nil {
		log.Printf("[ERROR] Failed to unmarshal event, dead-lettering: %v", err)
		msg.Nack(false, false) // Requeue=false -> goes to DLQ
		return
	}

	if err := observability.ValidateEvent(event); err != nil {
		log.Printf("[ERROR] Invalid event schema %s, dead-lettering: %v", event.EventID, err)
		msg.Nack(false, false) // Requeue=false -> goes to DLQ
		return
	}

	// Attempt to save to repository with transient error retry
	var saveErr error
	for i := 0; i < maxRetries; i++ {
		saveErr = c.repo.Save(ctx, event)
		if saveErr == nil {
			break
		}
		log.Printf("[WARN] Failed to save event %s (attempt %d/%d): %v", event.EventID, i+1, maxRetries, saveErr)
		time.Sleep(retryBackoff)
	}

	if saveErr != nil {
		log.Printf("[ERROR] Max retries reached for event %s, dead-lettering: %v", event.EventID, saveErr)
		msg.Nack(false, false) // Give up and send to DLQ
		return
	}

	// Success, acknowledge the message
	msg.Ack(false)
}

func (c *Consumer) Close() error {
	if c.ch != nil {
		c.ch.Close()
	}
	if c.conn != nil {
		c.conn.Close()
	}
	return nil
}
