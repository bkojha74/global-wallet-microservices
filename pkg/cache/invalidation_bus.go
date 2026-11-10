package cache

import (
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	DefaultCacheExchange   = "wallet.cache.events"
	DefaultExchangeType    = "fanout"
	EventBalanceInvalidate = "balance.invalidated"

	DefaultBaseBackoff = 100 * time.Millisecond
	DefaultMaxBackoff  = 10 * time.Second
)

// InvalidationEvent is the broadcast payload emitted across all replica pods.
type InvalidationEvent struct {
	Event            string    `json:"event"`
	WalletIDs        []string  `json:"wallet_ids"`
	Timestamp        time.Time `json:"timestamp"`
	OriginInstanceID string    `json:"origin_instance_id"`
}

// StatusCallback is invoked when connection state changes between connected and disconnected.
type StatusCallback func(connected bool)

// InvalidationBus defines publishing and subscription for multi-pod cache coherence.
type InvalidationBus interface {
	PublishInvalidation(ctx context.Context, walletIDs ...string) error
	Subscribe(ctx context.Context, onInvalidate func(walletIDs []string)) error
	Close() error
	IsConnected() bool
	OnConnectionStatusChange(cb StatusCallback)
}

// InvalidationBusConfig configures an AMQPInvalidationBus.
type InvalidationBusConfig struct {
	URL         string
	Exchange    string
	InstanceID  string
	TLSConfig   *tls.Config
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// AMQPInvalidationBus implements InvalidationBus using a RabbitMQ fanout exchange
// with full-jitter exponential backoff auto-reconnection.
type AMQPInvalidationBus struct {
	url         string
	exchange    string
	instanceID  string
	tlsConfig   *tls.Config
	baseBackoff time.Duration
	maxBackoff  time.Duration

	mu                sync.RWMutex
	conn              *amqp.Connection
	pubChan           *amqp.Channel
	subChan           *amqp.Channel
	stop              chan struct{}
	closeOnce         sync.Once
	isClosed          bool
	isConnected       bool
	onStatusCallbacks []StatusCallback
}

// NewAMQPInvalidationBus constructs a new AMQPInvalidationBus.
func NewAMQPInvalidationBus(cfg InvalidationBusConfig) *AMQPInvalidationBus {
	if cfg.Exchange == "" {
		cfg.Exchange = DefaultCacheExchange
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = fmt.Sprintf("wallet-pod-%d", time.Now().UnixNano())
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = DefaultBaseBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}

	return &AMQPInvalidationBus{
		url:         cfg.URL,
		exchange:    cfg.Exchange,
		instanceID:  cfg.InstanceID,
		tlsConfig:   cfg.TLSConfig,
		baseBackoff: cfg.BaseBackoff,
		maxBackoff:  cfg.MaxBackoff,
		stop:        make(chan struct{}),
	}
}

// Connect initializes connection and declares the fanout exchange.
func (b *AMQPInvalidationBus) Connect() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.connectLocked()
}

func (b *AMQPInvalidationBus) connectLocked() error {
	if b.isClosed {
		return errors.New("invalidation bus is closed")
	}

	var conn *amqp.Connection
	var err error
	if b.tlsConfig != nil {
		conn, err = amqp.DialTLS(b.url, b.tlsConfig)
	} else {
		conn, err = amqp.Dial(b.url)
	}
	if err != nil {
		return fmt.Errorf("failed to dial rabbitmq: %w", err)
	}

	pubChan, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to open publish channel: %w", err)
	}

	if err := pubChan.ExchangeDeclare(
		b.exchange,
		DefaultExchangeType,
		true,  // durable
		false, // auto-deleted
		false, // internal
		false, // no-wait
		nil,
	); err != nil {
		_ = pubChan.Close()
		_ = conn.Close()
		return fmt.Errorf("failed to declare fanout exchange %s: %w", b.exchange, err)
	}

	b.conn = conn
	b.pubChan = pubChan
	return nil
}

// ComputeJitterBackoff calculates a full-jitter exponential backoff duration for the given attempt.
func (b *AMQPInvalidationBus) ComputeJitterBackoff(attempt int) time.Duration {
	base := b.baseBackoff
	if base <= 0 {
		base = DefaultBaseBackoff
	}
	maxB := b.maxBackoff
	if maxB <= 0 {
		maxB = DefaultMaxBackoff
	}

	if attempt > 30 {
		attempt = 30
	}
	multiplier := time.Duration(1 << attempt)
	temp := base * multiplier
	if temp > maxB || temp <= 0 { // overflow protection
		temp = maxB
	}

	// Full jitter: uniformly random between [0, temp]
	nBig, err := crand.Int(crand.Reader, big.NewInt(int64(temp)))
	if err != nil || nBig == nil {
		return temp / 2
	}
	return time.Duration(nBig.Int64())
}

// setConnected updates connection state and fires registered callbacks.
func (b *AMQPInvalidationBus) setConnected(connected bool) {
	b.mu.Lock()
	changed := b.isConnected != connected
	b.isConnected = connected
	callbacks := make([]StatusCallback, len(b.onStatusCallbacks))
	copy(callbacks, b.onStatusCallbacks)
	b.mu.Unlock()

	if changed {
		for _, cb := range callbacks {
			if cb != nil {
				cb(connected)
			}
		}
	}
}

// IsConnected returns whether the invalidation bus currently has an active connection.
func (b *AMQPInvalidationBus) IsConnected() bool {
	if b == nil {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.isConnected && !b.isClosed
}

// OnConnectionStatusChange registers a callback triggered when connection state transitions.
func (b *AMQPInvalidationBus) OnConnectionStatusChange(cb StatusCallback) {
	if b == nil || cb == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onStatusCallbacks = append(b.onStatusCallbacks, cb)
}

// PublishInvalidation broadcasts a balance invalidation event to all subscriber pods.
func (b *AMQPInvalidationBus) PublishInvalidation(ctx context.Context, walletIDs ...string) error {
	if b == nil || len(walletIDs) == 0 {
		return nil
	}

	b.mu.Lock()
	if b.isClosed {
		b.mu.Unlock()
		return errors.New("invalidation bus is closed")
	}
	if b.pubChan == nil || b.pubChan.IsClosed() || b.conn == nil || b.conn.IsClosed() {
		if err := b.connectLocked(); err != nil {
			b.mu.Unlock()
			b.setConnected(false)
			return fmt.Errorf("amqp publish channel is not connected: %w", err)
		}
	}
	pubChan := b.pubChan
	exchange := b.exchange
	instanceID := b.instanceID
	b.mu.Unlock()

	evt := InvalidationEvent{
		Event:            EventBalanceInvalidate,
		WalletIDs:        walletIDs,
		Timestamp:        time.Now().UTC(),
		OriginInstanceID: instanceID,
	}

	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("failed to marshal invalidation event: %w", err)
	}

	return pubChan.PublishWithContext(ctx, exchange, "", false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Transient, // fast memory broadcast
		Timestamp:    evt.Timestamp,
		Body:         payload,
	})
}

// initSubscription sets up or reconnects the subscriber channel, transient queue, and binding.
func (b *AMQPInvalidationBus) initSubscription() (<-chan amqp.Delivery, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.isClosed {
		return nil, errors.New("invalidation bus is closed")
	}

	if b.subChan != nil {
		_ = b.subChan.Close()
		b.subChan = nil
	}
	if b.conn == nil || b.conn.IsClosed() {
		if err := b.connectLocked(); err != nil {
			return nil, err
		}
	}

	subChan, err := b.conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("failed to open subscribe channel: %w", err)
	}
	b.subChan = subChan

	// Declare transient, exclusive auto-delete queue for this pod instance
	q, err := subChan.QueueDeclare(
		"",    // random unique queue name assigned by broker
		false, // durable
		true,  // auto-delete when pod disconnects
		true,  // exclusive to this connection
		false, // no-wait
		nil,
	)
	if err != nil {
		_ = subChan.Close()
		b.subChan = nil
		return nil, fmt.Errorf("failed to declare invalidation queue: %w", err)
	}

	if err := subChan.QueueBind(q.Name, "", b.exchange, false, nil); err != nil {
		_ = subChan.Close()
		b.subChan = nil
		return nil, fmt.Errorf("failed to bind invalidation queue to exchange %s: %w", b.exchange, err)
	}

	msgs, err := subChan.Consume(
		q.Name,
		"",    // consumer tag
		true,  // auto-ack
		false, // exclusive
		false, // no-local
		false, // no-wait
		nil,
	)
	if err != nil {
		_ = subChan.Close()
		b.subChan = nil
		return nil, fmt.Errorf("failed to consume invalidation queue: %w", err)
	}

	return msgs, nil
}

// Subscribe starts an automated reconnection and consumption loop with full-jitter exponential backoff.
func (b *AMQPInvalidationBus) Subscribe(ctx context.Context, onInvalidate func(walletIDs []string)) error {
	if b == nil || onInvalidate == nil {
		return errors.New("invalid subscriber parameters")
	}

	// Launch background auto-reconnect consumer loop
	go b.runSubscriberLoop(ctx, onInvalidate)
	return nil
}

func (b *AMQPInvalidationBus) runSubscriberLoop(ctx context.Context, onInvalidate func(walletIDs []string)) {
	var attempt int

	for {
		select {
		case <-b.stop:
			return
		case <-ctx.Done():
			return
		default:
		}

		msgs, err := b.initSubscription()
		if err != nil {
			b.setConnected(false)
			sleepDur := b.ComputeJitterBackoff(attempt)
			log.Printf("[CACHE-BUS] Invalidation subscriber connect attempt %d failed: %v. Retrying in %v...", attempt+1, err, sleepDur)
			attempt++

			select {
			case <-b.stop:
				return
			case <-ctx.Done():
				return
			case <-time.After(sleepDur):
				continue
			}
		}

		// Connected successfully
		attempt = 0
		b.setConnected(true)
		log.Printf("[CACHE-BUS] Invalidation subscriber connected and listening on exchange %s", b.exchange)

		// Consume until channel closes or context cancelled
		channelClosed := false
		for !channelClosed {
			select {
			case <-b.stop:
				return
			case <-ctx.Done():
				return
			case d, ok := <-msgs:
				if !ok {
					log.Printf("[CACHE-BUS] Invalidation consumer channel closed, scheduling reconnect...")
					channelClosed = true
					b.setConnected(false)
					break
				}

				var evt InvalidationEvent
				if err := json.Unmarshal(d.Body, &evt); err != nil {
					log.Printf("[CACHE-BUS] Warning: failed to unmarshal invalidation event: %v", err)
					continue
				}

				// Echo suppression: skip if event originated from this pod
				if evt.OriginInstanceID != "" && evt.OriginInstanceID == b.instanceID {
					continue
				}

				if len(evt.WalletIDs) > 0 {
					onInvalidate(evt.WalletIDs)
				}
			}
		}

		// Channel dropped: jitter before re-attempting connection
		sleepDur := b.ComputeJitterBackoff(0)
		select {
		case <-b.stop:
			return
		case <-ctx.Done():
			return
		case <-time.After(sleepDur):
		}
	}
}

// Close gracefully closes subscriber and publisher channels and connection.
func (b *AMQPInvalidationBus) Close() error {
	if b == nil {
		return nil
	}

	var err error
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.isClosed = true
		b.isConnected = false
		close(b.stop)

		if b.subChan != nil {
			_ = b.subChan.Close()
			b.subChan = nil
		}
		if b.pubChan != nil {
			_ = b.pubChan.Close()
			b.pubChan = nil
		}
		if b.conn != nil {
			err = b.conn.Close()
			b.conn = nil
		}
		b.mu.Unlock()
	})
	return err
}
