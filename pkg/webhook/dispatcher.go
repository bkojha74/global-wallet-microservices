package webhook

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

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
)

// Dispatcher manages outbound partner webhook subscriptions, deliveries, and retries.
type Dispatcher struct {
	db          *mongo.Database
	httpClient  *http.Client
	maxAttempts int
	mu          sync.RWMutex
	memSubs     map[primitive.ObjectID]*WebhookSubscription
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
	if u == "" || (!strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://")) {
		return fmt.Errorf("valid webhook url (http/https) is required")
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
