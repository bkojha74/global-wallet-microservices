package observability

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
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

// AsyncLogger delivers log events asynchronously via a bounded in-memory channel.
// A background worker drains the channel, publishing to the broker or falling back
// to the local FileSpool. Metrics are reported to the provided MetricsRegistry so
// that each service can expose them at GET /metrics (GAP-07).
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
	metrics     *MetricsRegistry
}

func NewAsyncLogger(service, environment, region string, publisher EventPublisher, spool *FileSpool, bufferSize int, writer io.Writer) *AsyncLogger {
	return NewAsyncLoggerWithInstance(service, environment, region, ResolveInstanceID(), publisher, spool, bufferSize, writer)
}

func NewAsyncLoggerWithInstance(service, environment, region, instanceID string, publisher EventPublisher, spool *FileSpool, bufferSize int, writer io.Writer) *AsyncLogger {
	return NewAsyncLoggerFull(service, environment, region, instanceID, publisher, spool, bufferSize, writer, DefaultMetrics)
}

// NewAsyncLoggerFull is the primary constructor; callers can supply a custom MetricsRegistry
// for isolated testing. Production code should use NewAsyncLoggerWithInstance (uses DefaultMetrics).
func NewAsyncLoggerFull(service, environment, region, instanceID string, publisher EventPublisher, spool *FileSpool, bufferSize int, writer io.Writer, metrics *MetricsRegistry) *AsyncLogger {
	if instanceID == "" {
		instanceID = ResolveInstanceID()
	}
	if bufferSize <= 0 {
		bufferSize = 256
	}
	if metrics == nil {
		metrics = NewMetricsRegistry()
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
		metrics:     metrics,
	}
	go logger.run()
	return logger
}

// MetricsHandler returns the Prometheus-format HTTP handler for this logger's registry.
// Mount at GET /metrics on the service's management port (GAP-07).
func (l *AsyncLogger) MetricsHandler() http.Handler {
	return l.metrics.Handler()
}

// MetricsRegistry returns the underlying registry for direct mounting.
func (l *AsyncLogger) MetricsRegistry() *MetricsRegistry {
	return l.metrics
}

// Emit is non-blocking. It places the event on the bounded in-memory channel.
// If the channel is full, it falls back directly to the local spool.
// If the spool also fails, the event is dropped and the drop counter is incremented (GAP-07).
func (l *AsyncLogger) Emit(ctx context.Context, event Event) {
	event = normalizeEvent(ctx, event, l.service, l.environment, l.region, l.instanceID)
	l.metrics.IncEventsEmitted(l.service, event.Level, event.EventType)
	select {
	case l.queue <- event:
		// successfully queued
	default:
		// channel full — fall back to spool immediately
		if err := l.spool.Append(event); err != nil {
			l.standard.Printf("{\"service\":%q,\"event_type\":\"logging.spool_failed\",\"error\":%q}", l.service, err.Error())
			l.metrics.IncEventsDropped(l.service)
		} else {
			l.metrics.SetSpoolBytes(l.service, l.spool.Size())
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
			// Update queue-depth gauge after draining one event
			l.metrics.SetQueueDepth(l.service, len(l.queue))

		case <-ticker.C:
			// Periodic gauge refresh
			l.metrics.SetQueueDepth(l.service, len(l.queue))
			l.metrics.SetSpoolBytes(l.service, l.spool.Size())
			oldestAge := l.spool.OldestAge()
			if oldestAge > 0 {
				l.metrics.SetSpoolOldestAge(l.service, oldestAge.Seconds())
			}
			// Attempt spool replay; count replayed events
			_ = l.replayAndCount(context.Background())

		case <-l.stop:
			// Drain remaining queued events before exiting
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

// replayAndCount wraps spool replay and updates the replay counter.
func (l *AsyncLogger) replayAndCount(ctx context.Context) error {
	counter := &countingPublisher{inner: l.publisher}
	err := l.spool.Replay(ctx, counter)
	if counter.count > 0 {
		l.metrics.AddSpoolReplayed(l.service, counter.count)
		l.standard.Printf("{\"service\":%q,\"event_type\":\"logging.replay_succeeded\",\"events_replayed\":%d}", l.service, counter.count)
	}
	// Refresh spool bytes after replay
	l.metrics.SetSpoolBytes(l.service, l.spool.Size())
	return err
}

func (l *AsyncLogger) publish(event Event) {
	if err := l.publisher.Publish(context.Background(), event); err != nil {
		l.metrics.IncPublishFailures(l.service, err.Error())
		if spoolErr := l.spool.Append(event); spoolErr != nil {
			l.standard.Printf("{\"service\":%q,\"event_type\":\"logging.spool_failed\",\"error\":%q}", l.service, spoolErr.Error())
			l.metrics.IncEventsDropped(l.service)
		} else {
			l.metrics.SetSpoolBytes(l.service, l.spool.Size())
		}
	}
}

func (l *AsyncLogger) Sync(ctx context.Context) error {
	select {
	case <-l.stopped:
		return l.replayAndCount(ctx)
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

// countingPublisher wraps an EventPublisher and counts successful publishes.
type countingPublisher struct {
	inner EventPublisher
	count int64
}

func (c *countingPublisher) Publish(ctx context.Context, e Event) error {
	err := c.inner.Publish(ctx, e)
	if err == nil {
		c.count++
	}
	return err
}

func (c *countingPublisher) Close() error { return c.inner.Close() }

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

// ─── RabbitPublisher ────────────────────────────────────────────────────────

// RabbitPublisher publishes events to RabbitMQ with publisher confirms enabled.
// It runs a reconnect loop with exponential backoff and ±20% jitter (GAP-08).
// The publisher declares only the topic exchange; the logging service owns
// the queue and DLQ topology (GAP-04).
//
// TLS: set tlsCfg to a non-nil *tls.Config to dial over TLS (Phase 5).
// Build one with TLSConfigFromEnv() or supply your own.
type RabbitPublisher struct {
	mu           sync.RWMutex
	url          string
	exchange     string
	tlsCfg       *tls.Config // nil → plaintext; non-nil → TLS
	conn         *amqp.Connection
	channel      *amqp.Channel
	stop         chan struct{}
	stopped      chan struct{}
	connected    bool
	reconnectSig chan struct{}
}

// NewRabbitPublisher creates a plaintext AMQP publisher.
func NewRabbitPublisher(url, exchange string) *RabbitPublisher {
	return NewRabbitPublisherWithTLS(url, exchange, nil)
}

// NewRabbitPublisherWithTLS creates an AMQP publisher that dials with TLS when
// tlsCfg is non-nil. Pass nil for plaintext connections.
func NewRabbitPublisherWithTLS(url, exchange string, tlsCfg *tls.Config) *RabbitPublisher {
	if exchange == "" {
		exchange = DefaultLoggingExchange
	}
	p := &RabbitPublisher{
		url:          url,
		exchange:     exchange,
		tlsCfg:       tlsCfg,
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

// reconnectLoop implements exponential backoff with ±20% jitter (GAP-08).
// Parameters: initial 500ms, multiplier 2×, max 30s, jitter ±20%.
// Logs a warning every 10 consecutive failures to avoid log flooding.
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

	var conn *amqp.Connection
	var err error
	if p.tlsCfg != nil {
		conn, err = amqp.DialTLS(p.url, p.tlsCfg)
	} else {
		conn, err = amqp.Dial(p.url)
	}
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
	// Publisher declares only the topic exchange (GAP-04).
	// Queue and DLQ topology is owned by the logging service (Phase 3).
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

	return nil
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

// ─── Factory ─────────────────────────────────────────────────────────────────

// TLSConfigFromEnv builds a *tls.Config from the environment variables
// LOGGING_RABBITMQ_TLS_CERT, LOGGING_RABBITMQ_TLS_KEY, and LOGGING_RABBITMQ_TLS_CA.
// Returns nil when any of the three variables is absent (plaintext mode).
// The returned config requires mutual TLS when all three are set.
func TLSConfigFromEnv() (*tls.Config, error) {
	certFile := os.Getenv("LOGGING_RABBITMQ_TLS_CERT")
	keyFile := os.Getenv("LOGGING_RABBITMQ_TLS_KEY")
	caFile := os.Getenv("LOGGING_RABBITMQ_TLS_CA")
	if certFile == "" || keyFile == "" || caFile == "" {
		return nil, nil // plaintext mode
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("TLS: failed to load client cert/key (%s/%s): %w", certFile, keyFile, err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("TLS: failed to read CA cert (%s): %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("TLS: failed to parse CA cert from %s", caFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// LoggerFromEnvironment creates the appropriate Logger implementation based on
// the LOGGING_RABBITMQ_URL environment variable:
//   - If set: returns an AsyncLogger publishing to RabbitMQ with local spool fallback.
//   - If absent: returns a StructuredLogger writing JSON to writer (local mode).
//
// TLS (Phase 5): set LOGGING_RABBITMQ_TLS_CERT, LOGGING_RABBITMQ_TLS_KEY, and
// LOGGING_RABBITMQ_TLS_CA to enable mutual TLS for the RabbitMQ connection.
// All services should call this at startup and use the returned Logger everywhere.
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

	// Phase 5: build TLS config from env vars; nil means plaintext (safe default).
	tlsCfg, err := TLSConfigFromEnv()
	if err != nil {
		log.Printf("[LOGGING-SDK] TLS configuration error: %v — falling back to plaintext", err)
		tlsCfg = nil
	} else if tlsCfg != nil {
		log.Printf("[LOGGING-SDK] TLS enabled for RabbitMQ connection")
	}

	return NewAsyncLoggerFull(
		service, environment, region, instanceID,
		NewRabbitPublisherWithTLS(url, exchange, tlsCfg),
		NewFileSpool(spoolPath),
		256,
		writer,
		DefaultMetrics,
	)
}
