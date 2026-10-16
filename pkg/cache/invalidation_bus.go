package cache

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	DefaultCacheExchange   = "wallet.cache.events"
	DefaultExchangeType    = "fanout"
	EventBalanceInvalidate = "balance.invalidated"
)

// InvalidationEvent is the broadcast payload emitted across all replica pods.
type InvalidationEvent struct {
	Event            string    `json:"event"`
	WalletIDs        []string  `json:"wallet_ids"`
	Timestamp        time.Time `json:"timestamp"`
	OriginInstanceID string    `json:"origin_instance_id"`
}

// InvalidationBus defines publishing and subscription for multi-pod cache coherence.
type InvalidationBus interface {
	PublishInvalidation(ctx context.Context, walletIDs ...string) error
	Subscribe(ctx context.Context, onInvalidate func(walletIDs []string)) error
	Close() error
}

// InvalidationBusConfig configures an AMQPInvalidationBus.
type InvalidationBusConfig struct {
	URL        string
	Exchange   string
	InstanceID string
	TLSConfig  *tls.Config
}

// AMQPInvalidationBus implements InvalidationBus using a RabbitMQ fanout exchange.
type AMQPInvalidationBus struct {
	url        string
	exchange   string
	instanceID string
	tlsConfig  *tls.Config

	mu           sync.RWMutex
	conn         *amqp.Connection
	pubChan      *amqp.Channel
	subChan      *amqp.Channel
	stop         chan struct{}
	closeOnce    sync.Once
	isClosed     bool
	reconnectSig chan struct{}
}

// NewAMQPInvalidationBus constructs a new AMQPInvalidationBus.
func NewAMQPInvalidationBus(cfg InvalidationBusConfig) *AMQPInvalidationBus {
	if cfg.Exchange == "" {
		cfg.Exchange = DefaultCacheExchange
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = fmt.Sprintf("wallet-pod-%d", time.Now().UnixNano())
	}

	return &AMQPInvalidationBus{
		url:          cfg.URL,
		exchange:     cfg.Exchange,
		instanceID:   cfg.InstanceID,
		tlsConfig:    cfg.TLSConfig,
		stop:         make(chan struct{}),
		reconnectSig: make(chan struct{}, 1),
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

// PublishInvalidation broadcasts a balance invalidation event to all subscriber pods.
func (b *AMQPInvalidationBus) PublishInvalidation(ctx context.Context, walletIDs ...string) error {
	if b == nil || len(walletIDs) == 0 {
		return nil
	}

	b.mu.RLock()
	if b.isClosed || b.pubChan == nil || b.pubChan.IsClosed() {
		b.mu.RUnlock()
		return errors.New("amqp publish channel is not connected")
	}
	pubChan := b.pubChan
	exchange := b.exchange
	instanceID := b.instanceID
	b.mu.RUnlock()

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

// Subscribe binds an exclusive, auto-delete queue to the fanout exchange to consume invalidations.
func (b *AMQPInvalidationBus) Subscribe(ctx context.Context, onInvalidate func(walletIDs []string)) error {
	if b == nil || onInvalidate == nil {
		return errors.New("invalid subscriber parameters")
	}

	b.mu.Lock()
	if b.conn == nil || b.conn.IsClosed() {
		if err := b.connectLocked(); err != nil {
			b.mu.Unlock()
			return err
		}
	}

	subChan, err := b.conn.Channel()
	if err != nil {
		b.mu.Unlock()
		return fmt.Errorf("failed to open subscribe channel: %w", err)
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
		b.mu.Unlock()
		return fmt.Errorf("failed to declare invalidation queue: %w", err)
	}

	if err := subChan.QueueBind(q.Name, "", b.exchange, false, nil); err != nil {
		b.mu.Unlock()
		return fmt.Errorf("failed to bind invalidation queue to exchange %s: %w", b.exchange, err)
	}
	b.mu.Unlock()

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
		return fmt.Errorf("failed to consume invalidation queue: %w", err)
	}

	go func() {
		for {
			select {
			case <-b.stop:
				return
			case <-ctx.Done():
				return
			case d, ok := <-msgs:
				if !ok {
					log.Printf("[CACHE-BUS] Invalidation consumer channel closed")
					return
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
	}()

	return nil
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
		close(b.stop)

		if b.subChan != nil {
			_ = b.subChan.Close()
		}
		if b.pubChan != nil {
			_ = b.pubChan.Close()
		}
		if b.conn != nil {
			err = b.conn.Close()
		}
		b.mu.Unlock()
	})
	return err
}
