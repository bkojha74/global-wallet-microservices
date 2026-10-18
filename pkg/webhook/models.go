package webhook

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Supported outbound partner webhook event types.
const (
	EventTransferCompleted   = "transfer.completed"
	EventFraudFlagged        = "fraud.flagged"
	EventDepositCompleted    = "deposit.completed"
	EventWithdrawalCompleted = "withdrawal.completed"
	EventTestPing            = "webhook.test"
)

// DeliveryStatus represents the delivery lifecycle of an outbound webhook event.
type DeliveryStatus string

const (
	DeliveryStatusPending   DeliveryStatus = "pending"
	DeliveryStatusDelivered DeliveryStatus = "delivered"
	DeliveryStatusFailed    DeliveryStatus = "failed"
	DeliveryStatusRetrying  DeliveryStatus = "retry_pending"
)

// WebhookSubscription stores registered partner webhook endpoints and their shared signing secrets.
type WebhookSubscription struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	URL       string             `bson:"url" json:"url"`
	Secret    string             `bson:"secret" json:"secret"` // HMAC-SHA256 signing secret
	Events    []string           `bson:"events" json:"events"` // Subscribed event types, e.g. ["transfer.completed", "fraud.flagged"] or ["*"]
	IsActive  bool               `bson:"is_active" json:"is_active"`
	OwnerID   string             `bson:"owner_id,omitempty" json:"owner_id,omitempty"`
	CreatedAt time.Time          `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time          `bson:"updated_at" json:"updated_at"`
}

// MatchesEvent returns true if the subscription is active and listens for the given event type.
func (s *WebhookSubscription) MatchesEvent(event string) bool {
	if !s.IsActive {
		return false
	}
	for _, e := range s.Events {
		if e == "*" || e == event {
			return true
		}
	}
	return false
}

// WebhookPayload represents the standard JSON envelope delivered to external partner endpoints.
type WebhookPayload struct {
	ID        string      `json:"id"`
	Event     string      `json:"event"`
	Timestamp string      `json:"timestamp"` // ISO8601 UTC
	Data      interface{} `json:"data"`
}

// WebhookDelivery logs individual delivery attempts for auditing, tracing, and exponential retry.
type WebhookDelivery struct {
	ID             primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	SubscriptionID primitive.ObjectID `bson:"subscription_id" json:"subscription_id"`
	URL            string             `bson:"url" json:"url"`
	Event          string             `bson:"event" json:"event"`
	Payload        []byte             `bson:"payload" json:"payload"`
	Status         DeliveryStatus     `bson:"status" json:"status"`
	Attempts       int                `bson:"attempts" json:"attempts"`
	MaxAttempts    int                `bson:"max_attempts" json:"max_attempts"`
	LastStatusCode int                `bson:"last_status_code,omitempty" json:"last_status_code,omitempty"`
	LastError      string             `bson:"last_error,omitempty" json:"last_error,omitempty"`
	NextRetryAt    time.Time          `bson:"next_retry_at,omitempty" json:"next_retry_at,omitempty"`
	DeliveredAt    *time.Time         `bson:"delivered_at,omitempty" json:"delivered_at,omitempty"`
	CreatedAt      time.Time          `bson:"created_at" json:"created_at"`
	UpdatedAt      time.Time          `bson:"updated_at" json:"updated_at"`
}
