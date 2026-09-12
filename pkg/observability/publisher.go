package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	DefaultLoggingExchange     = "wallet.logs.v1"
	DefaultLoggingExchangeType = "topic"
)

type AsyncLogger struct {
	mu          sync.Mutex
	service     string
	environment string
	region      string
	instanceID  string
	publisher   EventPublisher
	spool       *FileSpool
	queue       chan Event
	stop        chan struct{}
	stopped     chan struct{}
	standard    *log.Logger
}

func NewAsyncLogger(service, environment, region string, publisher EventPublisher, spool *FileSpool, bufferSize int, writer io.Writer) *AsyncLogger {
	return NewAsyncLoggerWithInstance(service, environment, region, ResolveInstanceID(), publisher, spool, bufferSize, writer)
}

func NewAsyncLoggerWithInstance(service, environment, region, instanceID string, publisher EventPublisher, spool *FileSpool, bufferSize int, writer io.Writer) *AsyncLogger {
	if instanceID == "" {
		instanceID = ResolveInstanceID()
	}
	if bufferSize <= 0 {
		bufferSize = 256
	}
	logger := &AsyncLogger{
		service:     service,
		environment: environment,
		region:      region,
		instanceID:  instanceID,
		publisher:   publisher,
		spool:       spool,
		queue:       make(chan Event, bufferSize),
		stop:        make(chan struct{}),
		stopped:     make(chan struct{}),
		standard:    log.New(writer, "", 0),
	}
	go logger.run()
	return logger
}

func (l *AsyncLogger) Emit(ctx context.Context, event Event) {
	event = normalizeEvent(ctx, event, l.service, l.environment, l.region, l.instanceID)
	select {
	case l.queue <- event:
	default:
		if err := l.spool.Append(event); err != nil {
			l.standard.Printf("{\"service\":%q,\"event_type\":\"logging.spool_failed\",\"error\":%q}", l.service, err.Error())
		}
	}
}

func (l *AsyncLogger) run() {
	defer close(l.stopped)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case event := <-l.queue:
			l.publish(event)
		case <-ticker.C:
			_ = l.spool.Replay(context.Background(), l.publisher)
		case <-l.stop:
			for {
				select {
				case event := <-l.queue:
					l.publish(event)
				default:
					return
				}
			}
		}
	}
}

func (l *AsyncLogger) publish(event Event) {
	if err := l.publisher.Publish(context.Background(), event); err != nil {
		if spoolErr := l.spool.Append(event); spoolErr != nil {
			l.standard.Printf("{\"service\":%q,\"event_type\":\"logging.spool_failed\",\"error\":%q}", l.service, spoolErr.Error())
		}
	}
}

func (l *AsyncLogger) Sync(ctx context.Context) error {
	select {
	case <-l.stopped:
		return l.spool.Replay(ctx, l.publisher)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *AsyncLogger) Close(ctx context.Context) error {
	close(l.stop)
	select {
	case <-l.stopped:
		return l.publisher.Close()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func normalizeEvent(ctx context.Context, event Event, service, environment, region, instanceID string) Event {
	correlation := FromContext(ctx)
	if event.SchemaVersion == 0 {
		event.SchemaVersion = 1
	}
	if event.EventID == "" {
		event.EventID = NewAssociationID()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if event.Service == "" {
		event.Service = service
	}
	if event.InstanceID == "" {
		event.InstanceID = instanceID
	}
	if event.Environment == "" {
		event.Environment = environment
	}
	if event.Region == "" {
		event.Region = region
	}
	if event.AssociationID == "" {
		event.AssociationID = correlation.AssociationID
	}
	if event.TransactionID == "" {
		event.TransactionID = correlation.TransactionID
	}
	if event.IdempotencyKey == "" {
		event.IdempotencyKey = correlation.IdempotencyKey
	}
	return event
}

type RabbitPublisher struct {
	mu       sync.Mutex
	url      string
	exchange string
	conn     *amqp.Connection
	channel  *amqp.Channel
}

func NewRabbitPublisher(url, exchange string) *RabbitPublisher {
	if exchange == "" {
		exchange = DefaultLoggingExchange
	}
	return &RabbitPublisher{url: url, exchange: exchange}
}

func (p *RabbitPublisher) Publish(ctx context.Context, event Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureConnection(); err != nil {
		return err
	}
	confirmations := p.channel.NotifyPublish(make(chan amqp.Confirmation, 1))
	key := fmt.Sprintf("%s.%s.%s", event.Environment, event.Service, event.Level)
	if err := p.channel.PublishWithContext(ctx, p.exchange, key, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    event.EventID,
		Body:         payload,
	}); err != nil {
		p.resetConnection()
		return err
	}
	select {
	case confirmation := <-confirmations:
		if !confirmation.Ack {
			p.resetConnection()
			return fmt.Errorf("rabbitmq publish was negatively acknowledged")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *RabbitPublisher) ensureConnection() error {
	if p.channel != nil && !p.channel.IsClosed() {
		return nil
	}
	p.resetConnection()
	conn, err := amqp.Dial(p.url)
	if err != nil {
		return err
	}
	channel, err := conn.Channel()
	if err != nil {
		conn.Close()
		return err
	}
	if err := channel.Confirm(false); err != nil {
		channel.Close()
		conn.Close()
		return err
	}
	if err := channel.ExchangeDeclare(p.exchange, DefaultLoggingExchangeType, true, false, false, false, nil); err != nil {
		channel.Close()
		conn.Close()
		return err
	}
	p.conn = conn
	p.channel = channel
	return nil
}

func (p *RabbitPublisher) resetConnection() {
	if p.channel != nil {
		_ = p.channel.Close()
	}
	if p.conn != nil {
		_ = p.conn.Close()
	}
	p.channel = nil
	p.conn = nil
}

func (p *RabbitPublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resetConnection()
	return nil
}

func LoggerFromEnvironment(service, environment, region string, writer io.Writer) Logger {
	instanceID := ResolveInstanceID()
	url := os.Getenv("LOGGING_RABBITMQ_URL")
	if url == "" {
		return NewStructuredLoggerWithInstance(service, environment, region, instanceID, writer)
	}
	spoolPath := os.Getenv("LOGGING_SPOOL_PATH")
	if spoolPath == "" {
		spoolPath = filepath.Join("data", "logging", fmt.Sprintf("%s-%s.jsonl", service, instanceID))
	}
	exchange := os.Getenv("LOGGING_RABBITMQ_EXCHANGE")
	return NewAsyncLoggerWithInstance(service, environment, region, instanceID, NewRabbitPublisher(url, exchange), NewFileSpool(spoolPath), 256, writer)
}
