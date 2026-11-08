package webhook

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	DefaultMaxAttempts = 5
	DefaultTimeout     = 5 * time.Second
	DefaultPollPeriod  = 2 * time.Second

	CollectionSubscriptions = "webhook_subscriptions"
	CollectionDeliveries    = "webhook_deliveries"

	DefaultWebhookExchange  = "wallet.events.v1"
	DefaultWebhookQueue     = "wallet.webhooks.dispatch.v1"
	DefaultWebhookDLQ       = "wallet.webhooks.dead.v1"
	DefaultRoutingKeyPrefix = "webhook.dispatch."
)

// lookupIPFunc is the DNS IP lookup function used by ValidateWebhookURL.
// It defaults to net.LookupIP and can be overridden in tests to support air-gapped environments.
var lookupIPFunc = net.LookupIP

// ValidateWebhookURL validates that targetURL is a valid HTTP/HTTPS URL and prevents
// Server-Side Request Forgery (SSRF) by blocking internal, loopback, private, and cloud-metadata addresses.
func ValidateWebhookURL(targetURL string) error {
	trimmed := strings.TrimSpace(targetURL)
	if trimmed == "" {
		return errors.New("webhook url is required")
	}

	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("valid webhook url (http/https) is required")
	}

	host := u.Hostname()
	if host == "" {
		return errors.New("invalid webhook host")
	}

	// Allow loopback/private addresses only when explicitly permitted in dev/test environments
	if os.Getenv("ALLOW_LOCAL_WEBHOOKS") == "true" {
		return nil
	}

	// Block localhost and numeric forms
	if strings.EqualFold(host, "localhost") {
		return errors.New("webhook destination targets restricted private/loopback IP")
	}

	// If host is a literal IP address
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return errors.New("webhook destination targets restricted private/loopback IP")
		}
		return nil
	}

	// Resolve host IPs to defend against DNS rebinding & private IP targeting
	ips, err := lookupIPFunc(host)
	if err != nil {
		return fmt.Errorf("failed to resolve webhook host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("no IP addresses found for webhook host %q", host)
	}

	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return errors.New("webhook destination targets restricted private/loopback IP")
		}
	}

	return nil
}

// WebhookJob represents an asynchronous dispatch job serialized to RabbitMQ.
type WebhookJob struct {
	DeliveryID     string          `json:"delivery_id"`
	SubscriptionID string          `json:"subscription_id"`
	URL            string          `json:"url"`
	Secret         string          `json:"secret"`
	Event          string          `json:"event"`
	Payload        json.RawMessage `json:"payload"`
	Attempts       int             `json:"attempts"`
	MaxAttempts    int             `json:"max_attempts"`
	CreatedAt      time.Time       `json:"created_at"`
}

// WebhookQueueConfig configures the decoupled RabbitMQ queue dispatcher.
type WebhookQueueConfig struct {
	URL       string
	Exchange  string
	Queue     string
	DLQ       string
	Workers   int
	TLSConfig *tls.Config
}

// Dispatcher manages outbound partner webhook subscriptions, deliveries, and retries.
type Dispatcher struct {
	db          *mongo.Database
	httpClient  *http.Client
	maxAttempts int
	mu          sync.RWMutex
	memSubs     map[primitive.ObjectID]*WebhookSubscription

	// Decoupled RabbitMQ queue pipeline
	amqpConn  *amqp.Connection
	amqpChan  *amqp.Channel
	amqpExch  string
	amqpQueue string
	amqpDLQ   string
	hasAMQP   bool
	amqpStop  chan struct{}
	closeOnce sync.Once
}

// NewDispatcher initializes a new Webhook Dispatcher instance.
func NewDispatcher(db *mongo.Database, client *http.Client) *Dispatcher {
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}
	return &Dispatcher{
		db:          db,
		httpClient:  client,
		maxAttempts: DefaultMaxAttempts,
		memSubs:     make(map[primitive.ObjectID]*WebhookSubscription),
		amqpStop:    make(chan struct{}),
	}
}

func (d *Dispatcher) subsCol() *mongo.Collection {
	if d.db == nil {
		return nil
	}
	return d.db.Collection(CollectionSubscriptions)
}

func (d *Dispatcher) deliveriesCol() *mongo.Collection {
	if d.db == nil {
		return nil
	}
	return d.db.Collection(CollectionDeliveries)
}

// RegisterSubscription stores a new partner webhook subscription.
func (d *Dispatcher) RegisterSubscription(ctx context.Context, sub *WebhookSubscription) error {
	if sub == nil {
		return fmt.Errorf("subscription is required")
	}
	u := strings.TrimSpace(sub.URL)
	if err := ValidateWebhookURL(u); err != nil {
		return err
	}
	sub.URL = u

	if len(sub.Events) == 0 {
		return fmt.Errorf("at least one event must be subscribed to")
	}

	if sub.Secret == "" {
		sec, err := GenerateSecret()
		if err != nil {
			return err
		}
		sub.Secret = sec
	}

	sub.IsActive = true
	now := time.Now().UTC()
	sub.CreatedAt = now
	sub.UpdatedAt = now

	if d.subsCol() == nil {
		if sub.ID.IsZero() {
			sub.ID = primitive.NewObjectID()
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		subCopy := *sub
		d.memSubs[sub.ID] = &subCopy
		return nil
	}

	res, err := d.subsCol().InsertOne(ctx, sub)
	if err != nil {
		return fmt.Errorf("failed to save webhook subscription: %w", err)
	}
	if oid, ok := res.InsertedID.(primitive.ObjectID); ok {
		sub.ID = oid
	}
	return nil
}

// ListSubscriptions retrieves all active subscriptions, optionally filtered by owner.
func (d *Dispatcher) ListSubscriptions(ctx context.Context, ownerID string) ([]WebhookSubscription, error) {
	if d.subsCol() == nil {
		d.mu.RLock()
		defer d.mu.RUnlock()
		var res []WebhookSubscription
		for _, s := range d.memSubs {
			if !s.IsActive {
				continue
			}
			if ownerID != "" && s.OwnerID != ownerID {
				continue
			}
			res = append(res, *s)
		}
		return res, nil
	}
	filter := bson.M{"is_active": true}
	if ownerID != "" {
		filter["owner_id"] = ownerID
	}

	cursor, err := d.subsCol().Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var subs []WebhookSubscription
	if err := cursor.All(ctx, &subs); err != nil {
		return nil, err
	}
	return subs, nil
}

// DeleteSubscription marks a webhook subscription as inactive.
func (d *Dispatcher) DeleteSubscription(ctx context.Context, id primitive.ObjectID) error {
	if d.subsCol() == nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		if s, ok := d.memSubs[id]; ok {
			s.IsActive = false
			return nil
		}
		return mongo.ErrNoDocuments
	}
	filter := bson.M{"_id": id}
	update := bson.M{
		"$set": bson.M{
			"is_active":  false,
			"updated_at": time.Now().UTC(),
		},
	}
	res, err := d.subsCol().UpdateOne(ctx, filter, update)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return nil
}

// computeBackoff calculates exponential backoff with full jitter:
// Base: 2 seconds -> 2s, 4s, 8s, 16s, 32s (capped at 60s)
func computeBackoff(attempt int) time.Duration {
	baseSeconds := int64(1 << attempt) // 2^attempt (2, 4, 8, 16...)
	if baseSeconds > 60 {
		baseSeconds = 60
	}
	// Add full jitter (0 to baseSeconds)
	jitter, _ := rand.Int(rand.Reader, big.NewInt(baseSeconds+1))
	return time.Duration(baseSeconds+jitter.Int64()) * time.Second
}

func newEventID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "whevt_" + hex.EncodeToString(b)
}

// EnableAMQPQueue connects to RabbitMQ and declares the durable exchange, queue, and DLQ.
func (d *Dispatcher) EnableAMQPQueue(ctx context.Context, cfg WebhookQueueConfig) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if cfg.URL == "" {
		return errors.New("rabbitmq url is required")
	}
	if cfg.Exchange == "" {
		cfg.Exchange = DefaultWebhookExchange
	}
	if cfg.Queue == "" {
		cfg.Queue = DefaultWebhookQueue
	}
	if cfg.DLQ == "" {
		cfg.DLQ = DefaultWebhookDLQ
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}

	var conn *amqp.Connection
	var err error
	if cfg.TLSConfig != nil {
		conn, err = amqp.DialTLS(cfg.URL, cfg.TLSConfig)
	} else {
		conn, err = amqp.Dial(cfg.URL)
	}
	if err != nil {
		return fmt.Errorf("failed to dial rabbitmq for webhook queue: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to open channel for webhook queue: %w", err)
	}

	// 1. Declare Dead Letter Queue (DLQ)
	_, err = ch.QueueDeclare(
		cfg.DLQ,
		true,  // durable
		false, // auto-delete
		false, // exclusive
		false, // no-wait
		nil,
	)
	if err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return fmt.Errorf("failed to declare DLQ %s: %w", cfg.DLQ, err)
	}

	// 2. Declare Topic Exchange
	if err := ch.ExchangeDeclare(
		cfg.Exchange,
		"topic",
		true,  // durable
		false, // auto-delete
		false, // internal
		false, // no-wait
		nil,
	); err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return fmt.Errorf("failed to declare webhook exchange %s: %w", cfg.Exchange, err)
	}

	// 3. Declare Primary Dispatch Queue with Dead Letter routing to DLQ
	queueArgs := amqp.Table{
		"x-dead-letter-exchange":    "",
		"x-dead-letter-routing-key": cfg.DLQ,
	}
	q, err := ch.QueueDeclare(
		cfg.Queue,
		true,  // durable
		false, // auto-delete
		false, // exclusive
		false, // no-wait
		queueArgs,
	)
	if err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return fmt.Errorf("failed to declare webhook queue %s: %w", cfg.Queue, err)
	}

	// 4. Bind Queue to Exchange
	if err := ch.QueueBind(q.Name, DefaultRoutingKeyPrefix+"#", cfg.Exchange, false, nil); err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return fmt.Errorf("failed to bind queue to exchange: %w", err)
	}

	d.amqpConn = conn
	d.amqpChan = ch
	d.amqpExch = cfg.Exchange
	d.amqpQueue = cfg.Queue
	d.amqpDLQ = cfg.DLQ
	d.hasAMQP = true

	// 5. Start Worker Pool
	for i := 1; i <= cfg.Workers; i++ {
		workerChan, err := conn.Channel()
		if err != nil {
			log.Printf("[WEBHOOK-QUEUE] Warning: failed to open channel for worker %d: %v", i, err)
			continue
		}
		_ = workerChan.Qos(5, 0, false)
		msgs, err := workerChan.Consume(
			q.Name,
			fmt.Sprintf("worker-%d", i),
			false, // manual ack
			false,
			false,
			false,
			nil,
		)
		if err != nil {
			log.Printf("[WEBHOOK-QUEUE] Warning: worker %d failed to consume: %v", i, err)
			continue
		}
		go d.startAMQPWorker(ctx, i, msgs)
	}

	log.Printf("[WEBHOOK-QUEUE] RabbitMQ decoupled dispatcher active: exchange=%s, queue=%s, dlq=%s, workers=%d",
		cfg.Exchange, cfg.Queue, cfg.DLQ, cfg.Workers)
	return nil
}

func (d *Dispatcher) hasAMQPQueue() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.hasAMQP && d.amqpChan != nil && !d.amqpChan.IsClosed()
}

func (d *Dispatcher) publishToQueue(ctx context.Context, job WebhookJob) error {
	d.mu.RLock()
	ch := d.amqpChan
	exch := d.amqpExch
	d.mu.RUnlock()

	if ch == nil || ch.IsClosed() {
		return errors.New("amqp channel unavailable")
	}

	body, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("failed to marshal webhook job: %w", err)
	}

	routingKey := DefaultRoutingKeyPrefix + job.Event
	return ch.PublishWithContext(ctx, exch, routingKey, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent, // durable messages
		Timestamp:    job.CreatedAt,
		Body:         body,
	})
}

func (d *Dispatcher) startAMQPWorker(ctx context.Context, workerID int, msgs <-chan amqp.Delivery) {
	log.Printf("[WEBHOOK-WORKER-%d] Worker started, listening on %s", workerID, d.amqpQueue)
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.amqpStop:
			return
		case msg, ok := <-msgs:
			if !ok {
				log.Printf("[WEBHOOK-WORKER-%d] Queue consumer channel closed", workerID)
				return
			}

			var job WebhookJob
			if err := json.Unmarshal(msg.Body, &job); err != nil {
				log.Printf("[WEBHOOK-WORKER-%d] Malformed job body: %v", workerID, err)
				_ = msg.Nack(false, false) // discard malformed message to DLQ
				continue
			}

			job.Attempts++
			statusCode, sendErr := d.sendHTTP(ctx, job.URL, job.Secret, job.Event, job.DeliveryID, job.Payload)

			var delID primitive.ObjectID
			if objID, err := primitive.ObjectIDFromHex(job.DeliveryID); err == nil {
				delID = objID
			}

			if sendErr == nil && statusCode >= 200 && statusCode < 300 {
				now := time.Now().UTC()
				if d.deliveriesCol() != nil && !delID.IsZero() {
					_, _ = d.deliveriesCol().UpdateOne(ctx, bson.M{"_id": delID}, bson.M{
						"$set": bson.M{
							"status":           DeliveryStatusDelivered,
							"delivered_at":     now,
							"last_status_code": statusCode,
							"attempts":         job.Attempts,
							"updated_at":       now,
							"last_error":       "",
						},
					})
				}
				_ = msg.Ack(false)
			} else {
				errMsg := ""
				if sendErr != nil {
					errMsg = sendErr.Error()
				} else {
					errMsg = fmt.Sprintf("HTTP status %d", statusCode)
				}

				if job.Attempts >= job.MaxAttempts {
					log.Printf("[WEBHOOK-WORKER-%d] Delivery %s exceeded max attempts (%d), routing to DLQ: %s",
						workerID, job.DeliveryID, job.MaxAttempts, errMsg)
					now := time.Now().UTC()
					if d.deliveriesCol() != nil && !delID.IsZero() {
						_, _ = d.deliveriesCol().UpdateOne(ctx, bson.M{"_id": delID}, bson.M{
							"$set": bson.M{
								"status":           DeliveryStatusFailed,
								"last_status_code": statusCode,
								"attempts":         job.Attempts,
								"last_error":       errMsg,
								"updated_at":       now,
							},
						})
					}
					_ = msg.Nack(false, false)
				} else {
					now := time.Now().UTC()
					nextRetry := now.Add(computeBackoff(job.Attempts))
					if d.deliveriesCol() != nil && !delID.IsZero() {
						_, _ = d.deliveriesCol().UpdateOne(ctx, bson.M{"_id": delID}, bson.M{
							"$set": bson.M{
								"status":           DeliveryStatusRetrying,
								"last_status_code": statusCode,
								"attempts":         job.Attempts,
								"next_retry_at":    nextRetry,
								"last_error":       errMsg,
								"updated_at":       now,
							},
						})
					}
					_ = msg.Ack(false)
					go func(j WebhookJob, delay time.Duration) {
						select {
						case <-time.After(delay):
							_ = d.publishToQueue(context.Background(), j)
						case <-d.amqpStop:
						case <-ctx.Done():
						}
					}(job, computeBackoff(job.Attempts))
				}
			}
		}
	}
}

// Close gracefully closes all AMQP channels and connections.
func (d *Dispatcher) Close() error {
	if d == nil {
		return nil
	}
	var err error
	d.closeOnce.Do(func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.amqpStop != nil {
			close(d.amqpStop)
		}
		if d.amqpChan != nil {
			_ = d.amqpChan.Close()
		}
		if d.amqpConn != nil {
			err = d.amqpConn.Close()
		}
		d.hasAMQP = false
	})
	return err
}

// Dispatch broadcasts an event to all matching active partner subscriptions.
func (d *Dispatcher) Dispatch(ctx context.Context, event string, data interface{}) error {
	subs, err := d.ListSubscriptions(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to fetch active subscriptions: %w", err)
	}

	now := time.Now().UTC()
	payloadObj := WebhookPayload{
		ID:        newEventID(),
		Event:     event,
		Timestamp: now.Format(time.RFC3339),
		Data:      data,
	}

	payloadBytes, err := json.Marshal(payloadObj)
	if err != nil {
		return fmt.Errorf("failed to serialize webhook payload: %w", err)
	}

	// REQ-ASYNC-01: If AMQP queue is enabled, publish jobs asynchronously without blocking!
	if d.hasAMQPQueue() {
		for _, sub := range subs {
			if !sub.MatchesEvent(event) {
				continue
			}
			deliveryID := primitive.NewObjectID()
			if d.deliveriesCol() != nil {
				_, _ = d.deliveriesCol().InsertOne(context.Background(), WebhookDelivery{
					ID:             deliveryID,
					SubscriptionID: sub.ID,
					URL:            sub.URL,
					Event:          event,
					Payload:        payloadBytes,
					Status:         DeliveryStatusPending,
					Attempts:       0,
					MaxAttempts:    d.maxAttempts,
					CreatedAt:      now,
					UpdatedAt:      now,
				})
			}
			job := WebhookJob{
				DeliveryID:     deliveryID.Hex(),
				SubscriptionID: sub.ID.Hex(),
				URL:            sub.URL,
				Secret:         sub.Secret,
				Event:          event,
				Payload:        payloadBytes,
				Attempts:       0,
				MaxAttempts:    d.maxAttempts,
				CreatedAt:      now,
			}
			if pubErr := d.publishToQueue(ctx, job); pubErr != nil {
				log.Printf("[WEBHOOK-QUEUE] Failed to publish job to queue: %v, falling back to direct dispatch", pubErr)
				go d.dispatchSingle(ctx, sub, event, payloadBytes)
			}
		}
		return nil
	}

	var wg sync.WaitGroup
	for _, sub := range subs {
		if !sub.MatchesEvent(event) {
			continue
		}
		wg.Add(1)
		go func(s WebhookSubscription) {
			defer wg.Done()
			d.dispatchSingle(ctx, s, event, payloadBytes)
		}(sub)
	}
	wg.Wait()
	return nil
}

// dispatchSingle handles immediate delivery and records status or schedules retry.
func (d *Dispatcher) dispatchSingle(ctx context.Context, sub WebhookSubscription, event string, payload []byte) {
	deliveryID := primitive.NewObjectID()
	delivery := WebhookDelivery{
		ID:             deliveryID,
		SubscriptionID: sub.ID,
		URL:            sub.URL,
		Event:          event,
		Payload:        payload,
		Status:         DeliveryStatusPending,
		Attempts:       1,
		MaxAttempts:    d.maxAttempts,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}

	statusCode, sendErr := d.sendHTTP(ctx, sub.URL, sub.Secret, event, deliveryID.Hex(), payload)
	delivery.LastStatusCode = statusCode

	if sendErr == nil && statusCode >= 200 && statusCode < 300 {
		now := time.Now().UTC()
		delivery.Status = DeliveryStatusDelivered
		delivery.DeliveredAt = &now
	} else {
		errMsg := ""
		if sendErr != nil {
			errMsg = sendErr.Error()
		} else {
			errMsg = fmt.Sprintf("HTTP status %d", statusCode)
		}
		delivery.LastError = errMsg
		if delivery.Attempts >= delivery.MaxAttempts {
			delivery.Status = DeliveryStatusFailed
		} else {
			delivery.Status = DeliveryStatusRetrying
			delivery.NextRetryAt = time.Now().UTC().Add(computeBackoff(delivery.Attempts))
		}
	}

	if d.deliveriesCol() != nil {
		_, _ = d.deliveriesCol().InsertOne(context.Background(), delivery)
	}
}

// sendHTTP issues the signed HTTP POST request to the partner endpoint.
func (d *Dispatcher) sendHTTP(ctx context.Context, targetURL, secret, event, deliveryID string, payload []byte) (int, error) {
	ts := time.Now().UTC().Unix()
	sigHeader := SignPayload(secret, ts, payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Global-Wallet-Webhook-Dispatcher/1.5")
	req.Header.Set("X-Wallet-Signature", sigHeader)
	req.Header.Set("X-Wallet-Event", event)
	req.Header.Set("X-Wallet-Delivery", deliveryID)
	req.Header.Set("X-Wallet-Timestamp", fmt.Sprintf("%d", ts))

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	return resp.StatusCode, nil
}

// ProcessPendingRetries executes a single iteration of re-sending failed webhooks whose backoff elapsed.
func (d *Dispatcher) ProcessPendingRetries(ctx context.Context) (int, error) {
	if d.deliveriesCol() == nil || d.subsCol() == nil {
		return 0, nil
	}

	now := time.Now().UTC()
	filter := bson.M{
		"status":        DeliveryStatusRetrying,
		"next_retry_at": bson.M{"$lte": now},
	}
	findOpts := options.Find().SetLimit(50)

	cursor, err := d.deliveriesCol().Find(ctx, filter, findOpts)
	if err != nil {
		return 0, err
	}
	defer cursor.Close(ctx)

	var deliveries []WebhookDelivery
	if err := cursor.All(ctx, &deliveries); err != nil {
		return 0, err
	}

	processed := 0
	for _, del := range deliveries {
		var sub WebhookSubscription
		if err := d.subsCol().FindOne(ctx, bson.M{"_id": del.SubscriptionID}).Decode(&sub); err != nil {
			// Subscription no longer exists or inactive
			_, _ = d.deliveriesCol().UpdateOne(ctx, bson.M{"_id": del.ID}, bson.M{
				"$set": bson.M{
					"status":     DeliveryStatusFailed,
					"last_error": "subscription no longer exists",
					"updated_at": time.Now().UTC(),
				},
			})
			continue
		}

		del.Attempts++
		code, sendErr := d.sendHTTP(ctx, sub.URL, sub.Secret, del.Event, del.ID.Hex(), del.Payload)
		update := bson.M{
			"last_status_code": code,
			"attempts":         del.Attempts,
			"updated_at":       time.Now().UTC(),
		}

		if sendErr == nil && code >= 200 && code < 300 {
			nowDel := time.Now().UTC()
			update["status"] = DeliveryStatusDelivered
			update["delivered_at"] = nowDel
			update["last_error"] = ""
		} else {
			errMsg := ""
			if sendErr != nil {
				errMsg = sendErr.Error()
			} else {
				errMsg = fmt.Sprintf("HTTP status %d", code)
			}
			update["last_error"] = errMsg
			if del.Attempts >= del.MaxAttempts {
				update["status"] = DeliveryStatusFailed
			} else {
				update["status"] = DeliveryStatusRetrying
				update["next_retry_at"] = time.Now().UTC().Add(computeBackoff(del.Attempts))
			}
		}

		_, _ = d.deliveriesCol().UpdateOne(ctx, bson.M{"_id": del.ID}, bson.M{"$set": update})
		processed++
	}

	return processed, nil
}

// StartRetryWorker runs the background polling loop for failed webhook retries.
func (d *Dispatcher) StartRetryWorker(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultPollPeriod
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("[WEBHOOK-WORKER] Background retry worker active (interval: %v)", interval)
	for {
		select {
		case <-ctx.Done():
			log.Println("[WEBHOOK-WORKER] Background retry worker stopped.")
			return
		case <-ticker.C:
			count, err := d.ProcessPendingRetries(ctx)
			if err != nil {
				log.Printf("[WEBHOOK-WORKER] Error processing retries: %v", err)
			} else if count > 0 {
				log.Printf("[WEBHOOK-WORKER] Processed %d pending webhook retries", count)
			}
		}
	}
}

// TestEndpoint dispatches a synchronous test ping event to an endpoint and returns the status and latency.
func (d *Dispatcher) TestEndpoint(ctx context.Context, targetURL, secret string) (int, time.Duration, error) {
	if err := ValidateWebhookURL(targetURL); err != nil {
		return 0, 0, err
	}
	start := time.Now()
	testPayload := WebhookPayload{
		ID:        newEventID(),
		Event:     EventTestPing,
		Timestamp: start.UTC().Format(time.RFC3339),
		Data: map[string]string{
			"message": "Global Wallet Microservices Webhook Ping",
			"status":  "TEST_OK",
		},
	}
	payloadBytes, _ := json.Marshal(testPayload)
	code, err := d.sendHTTP(ctx, targetURL, secret, EventTestPing, "test_ping", payloadBytes)
	latency := time.Since(start)
	return code, latency, err
}
