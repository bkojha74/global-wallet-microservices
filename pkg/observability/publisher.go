package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
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
	mu           sync.RWMutex
	url          string
	exchange     string
	conn         *amqp.Connection
	channel      *amqp.Channel
	stop         chan struct{}
	stopped      chan struct{}
	connected    bool
	reconnectSig chan struct{}
}

func NewRabbitPublisher(url, exchange string) *RabbitPublisher {
	if exchange == "" {
		exchange = DefaultLoggingExchange
	}
	p := &RabbitPublisher{
		url:          url,
		exchange:     exchange,
		stop:         make(chan struct{}),
		stopped:      make(chan struct{}),
		reconnectSig: make(chan struct{}, 1),
	}
	go p.reconnectLoop()
	return p
}

func (p *RabbitPublisher) IsConnected() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.connected && p.conn != nil && !p.conn.IsClosed() && p.channel != nil && !p.channel.IsClosed()
}

func (p *RabbitPublisher) reconnectLoop() {
	defer close(p.stopped)

	baseInterval := 500 * time.Millisecond
	maxInterval := 30 * time.Second
	multiplier := 2.0
	currentInterval := baseInterval
	consecutiveFailures := 0

	for {
		err := p.connect()
		if err == nil {
			consecutiveFailures = 0
			currentInterval = baseInterval

			closeChan := make(chan *amqp.Error, 1)
			p.conn.NotifyClose(closeChan)

			select {
			case closeErr, ok := <-closeChan:
				p.mu.Lock()
				p.connected = false
				p.resetConnectionLocked()
				p.mu.Unlock()
				if ok && closeErr != nil {
					log.Printf("[LOGGING-PUBLISHER] RabbitMQ connection closed: %v. Reconnecting...", closeErr)
				}
			case <-p.reconnectSig:
				p.mu.Lock()
				p.connected = false
				p.resetConnectionLocked()
				p.mu.Unlock()
			case <-p.stop:
				return
			}
		} else {
			consecutiveFailures++
			// Jitter ±20%: factor in range [0.8, 1.2]
			jitterFactor := 0.8 + (rand.Float64() * 0.4)
			sleepDuration := time.Duration(float64(currentInterval) * jitterFactor)

			if consecutiveFailures%10 == 0 || consecutiveFailures == 1 {
				log.Printf("[LOGGING-PUBLISHER] RabbitMQ connection attempt failed (%d consecutive): %v. Retrying in %v...", consecutiveFailures, err, sleepDuration)
			}

			select {
			case <-time.After(sleepDuration):
			case <-p.reconnectSig:
			case <-p.stop:
				return
			}

			currentInterval = time.Duration(float64(currentInterval) * multiplier)
			if currentInterval > maxInterval {
				currentInterval = maxInterval
			}
		}
	}
}

func (p *RabbitPublisher) connect() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.resetConnectionLocked()

	conn, err := amqp.Dial(p.url)
	if err != nil {
		return err
	}
	channel, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return err
	}
	if err := channel.Confirm(false); err != nil {
		_ = channel.Close()
		_ = conn.Close()
		return err
	}
	// Publisher declares only the topic exchange (GAP-04)
	if err := channel.ExchangeDeclare(p.exchange, DefaultLoggingExchangeType, true, false, false, false, nil); err != nil {
		_ = channel.Close()
		_ = conn.Close()
		return err
	}

	p.conn = conn
	p.channel = channel
	p.connected = true
	return nil
}

func (p *RabbitPublisher) Publish(ctx context.Context, event Event) error {
	p.mu.RLock()
	if !p.connected || p.channel == nil || p.channel.IsClosed() {
		p.mu.RUnlock()
		select {
		case p.reconnectSig <- struct{}{}:
		default:
		}
		return fmt.Errorf("rabbitmq publisher is not connected")
	}
	channel := p.channel
	exchange := p.exchange
	p.mu.RUnlock()

	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}

	confirmations := channel.NotifyPublish(make(chan amqp.Confirmation, 1))
	key := fmt.Sprintf("%s.%s.%s", event.Environment, event.Service, event.Level)
	if err := channel.PublishWithContext(ctx, exchange, key, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    event.EventID,
		Body:         payload,
	}); err != nil {
		p.mu.Lock()
		p.connected = false
		p.resetConnectionLocked()
		p.mu.Unlock()
		select {
		case p.reconnectSig <- struct{}{}:
		default:
		}
		return err
	}

	select {
	case confirmation := <-confirmations:
		if !confirmation.Ack {
			p.mu.Lock()
			p.connected = false
			p.resetConnectionLocked()
			p.mu.Unlock()
			select {
			case p.reconnectSig <- struct{}{}:
			default:
			}
			return fmt.Errorf("rabbitmq publish was negatively acknowledged")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *RabbitPublisher) resetConnectionLocked() {
	if p.channel != nil {
		_ = p.channel.Close()
	}
	if p.conn != nil {
		_ = p.conn.Close()
	}
	p.channel = nil
	p.conn = nil
	p.connected = false
}

func (p *RabbitPublisher) Close() error {
	close(p.stop)
	select {
	case <-p.stopped:
	case <-time.After(2 * time.Second):
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resetConnectionLocked()
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
