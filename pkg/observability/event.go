package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"
)

// Level constants — must be used instead of raw string literals (GAP-02)
const (
	LevelAudit = "AUDIT"
	LevelError = "ERROR"
	LevelWarn  = "WARN"
	LevelInfo  = "INFO"
	LevelDebug = "DEBUG"
)

type Event struct {
	SchemaVersion  int            `json:"schema_version" bson:"schema_version"`
	EventID        string         `json:"event_id" bson:"event_id"`
	OccurredAt     time.Time      `json:"occurred_at" bson:"occurred_at"`
	Service        string         `json:"service" bson:"service"`
	InstanceID     string         `json:"instance_id,omitempty" bson:"instance_id,omitempty"`
	Environment    string         `json:"environment" bson:"environment"`
	Region         string         `json:"region,omitempty" bson:"region,omitempty"`
	Level          string         `json:"level" bson:"level"`
	EventType      string         `json:"event_type" bson:"event_type"`
	Message        string         `json:"message,omitempty" bson:"message,omitempty"`
	AssociationID  string         `json:"association_id" bson:"association_id"`
	TransactionID  string         `json:"transaction_id,omitempty" bson:"transaction_id,omitempty"`
	IdempotencyKey string         `json:"idempotency_key,omitempty" bson:"idempotency_key,omitempty"`
	ParentEventID  string         `json:"parent_event_id,omitempty" bson:"parent_event_id,omitempty"`
	DurationMS     int64          `json:"duration_ms,omitempty" bson:"duration_ms,omitempty"`
	Success        *bool          `json:"success,omitempty" bson:"success,omitempty"`
	Attributes     map[string]any `json:"attributes,omitempty" bson:"attributes,omitempty"`
}

// ResolveInstanceID returns INSTANCE_ID from environment or falls back to os.Hostname(). (GAP-01)
func ResolveInstanceID() string {
	if id := os.Getenv("INSTANCE_ID"); id != "" {
		return id
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "unknown-instance"
}

// ValidateEvent checks all required fields in the event envelope (GAP-09).
func ValidateEvent(e Event) error {
	if e.SchemaVersion <= 0 {
		return errors.New("schema_version must be greater than 0")
	}
	if e.EventID == "" {
		return errors.New("event_id is required")
	}
	if e.OccurredAt.IsZero() {
		return errors.New("occurred_at is required")
	}
	if e.Service == "" {
		return errors.New("service is required")
	}
	if e.Environment == "" {
		return errors.New("environment is required")
	}
	switch e.Level {
	case LevelAudit, LevelError, LevelWarn, LevelInfo, LevelDebug:
		// valid level
	default:
		return fmt.Errorf("invalid level %q: must be AUDIT, ERROR, WARN, INFO, or DEBUG", e.Level)
	}
	if e.EventType == "" {
		return errors.New("event_type is required")
	}
	if e.AssociationID == "" {
		return errors.New("association_id is required")
	}
	return nil
}

type Logger interface {
	Emit(context.Context, Event)
	Sync(context.Context) error
	Close(context.Context) error
}

type StructuredLogger struct {
	mu          sync.Mutex
	writer      io.Writer
	service     string
	environment string
	region      string
	instanceID  string
	standard    *log.Logger
}

func NewStructuredLogger(service, environment, region string, writer io.Writer) *StructuredLogger {
	return NewStructuredLoggerWithInstance(service, environment, region, ResolveInstanceID(), writer)
}

func NewStructuredLoggerWithInstance(service, environment, region, instanceID string, writer io.Writer) *StructuredLogger {
	if instanceID == "" {
		instanceID = ResolveInstanceID()
	}
	return &StructuredLogger{
		writer:      writer,
		service:     service,
		environment: environment,
		region:      region,
		instanceID:  instanceID,
		standard:    log.New(writer, "", 0),
	}
}

func (l *StructuredLogger) Emit(ctx context.Context, event Event) {
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
		event.Service = l.service
	}
	if event.InstanceID == "" {
		event.InstanceID = l.instanceID
	}
	if event.Environment == "" {
		event.Environment = l.environment
	}
	if event.Region == "" {
		event.Region = l.region
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
	payload, err := json.Marshal(event)
	if err != nil {
		l.standard.Printf("{\"service\":%q,\"event_type\":\"logging.serialization_failed\",\"error\":%q}", l.service, err.Error())
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.writer.Write(append(payload, '\n'))
}

func (l *StructuredLogger) Sync(context.Context) error {
	return nil
}

func (l *StructuredLogger) Close(context.Context) error {
	return nil
}

type MemoryLogger struct {
	mu     sync.Mutex
	events []Event
}

func NewMemoryLogger() *MemoryLogger {
	return &MemoryLogger{}
}

func (l *MemoryLogger) Emit(ctx context.Context, event Event) {
	correlation := FromContext(ctx)
	if event.AssociationID == "" {
		event.AssociationID = correlation.AssociationID
	}
	if event.TransactionID == "" {
		event.TransactionID = correlation.TransactionID
	}
	if event.IdempotencyKey == "" {
		event.IdempotencyKey = correlation.IdempotencyKey
	}
	if event.InstanceID == "" {
		event.InstanceID = ResolveInstanceID()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *MemoryLogger) Events() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	copyOfEvents := make([]Event, len(l.events))
	copy(copyOfEvents, l.events)
	return copyOfEvents
}

func (l *MemoryLogger) Sync(context.Context) error {
	return nil
}

func (l *MemoryLogger) Close(context.Context) error {
	return nil
}

func NoopLogger() Logger {
	return NewStructuredLogger("", "", "", io.Discard)
}

