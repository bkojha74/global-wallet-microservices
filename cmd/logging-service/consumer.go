package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"wallet-system/pkg/observability"
	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	exchangeName    = "wallet.logs.v1"
	queueName       = "wallet.logging.ingest.v1"
	dlxName         = "wallet.logs.dlx.v1"
	dlqName         = "wallet.logging.dead.v1"
	routingKey      = "#" // Match all topics
	prefetchCount   = 50
	maxRetries      = 3
)

var retryBackoff = 2 * time.Second

type Consumer struct {
	conn *amqp.Connection
	ch   *amqp.Channel
	repo LogRepository
}

func NewConsumer(amqpURI string, repo LogRepository) (*Consumer, error) {
	conn, err := amqp.Dial(amqpURI)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to RabbitMQ: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to open a channel: %w", err)
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
		return nil, fmt.Errorf("failed to declare DLX: %w", err)
	}

	_, err = ch.QueueDeclare(
		dlqName,
		true,  // durable
		false, // delete when unused
		false, // exclusive
		false, // no-wait
		nil,   // arguments
	)
	if err != nil {
		return nil, fmt.Errorf("failed to declare DLQ: %w", err)
	}

	err = ch.QueueBind(
		dlqName,
		"#", // bind all DLX routing keys
		dlxName,
		false,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to bind DLQ: %w", err)
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
		return nil, fmt.Errorf("failed to declare main exchange: %w", err)
	}

	args := amqp.Table{
		"x-dead-letter-exchange": dlxName,
	}
	_, err = ch.QueueDeclare(
		queueName,
		true,  // durable
		false, // delete when unused
		false, // exclusive
		false, // no-wait
		args,  // arguments
	)
	if err != nil {
		return nil, fmt.Errorf("failed to declare main queue: %w", err)
	}

	err = ch.QueueBind(
		queueName,
		routingKey,
		exchangeName,
		false,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to bind main queue: %w", err)
	}

	// 3. Set QoS
	err = ch.Qos(
		prefetchCount, // prefetch count
		0,             // prefetch size
		false,         // global
	)
	if err != nil {
		return nil, fmt.Errorf("failed to set QoS: %w", err)
	}

	return &Consumer{
		conn: conn,
		ch:   ch,
		repo: repo,
	}, nil
}

func (c *Consumer) Start(ctx context.Context) error {
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
		return fmt.Errorf("failed to register a consumer: %w", err)
	}

	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				return fmt.Errorf("consumer channel closed")
			}
			c.processMessage(ctx, msg)
		case <-ctx.Done():
			log.Println("Context done, stopping consumer")
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
