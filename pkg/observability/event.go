package observability

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"sync"
	"time"
)

type Event struct {
	SchemaVersion  int            `json:"schema_version"`
	EventID        string         `json:"event_id"`
	OccurredAt     time.Time      `json:"occurred_at"`
	Service        string         `json:"service"`
	Environment    string         `json:"environment"`
	Region         string         `json:"region,omitempty"`
	Level          string         `json:"level"`
	EventType      string         `json:"event_type"`
	Message        string         `json:"message,omitempty"`
	AssociationID  string         `json:"association_id"`
	TransactionID  string         `json:"transaction_id,omitempty"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
	ParentEventID  string         `json:"parent_event_id,omitempty"`
	DurationMS     int64          `json:"duration_ms,omitempty"`
	Success        *bool          `json:"success,omitempty"`
	Attributes     map[string]any `json:"attributes,omitempty"`
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
	standard    *log.Logger
}

func NewStructuredLogger(service, environment, region string, writer io.Writer) *StructuredLogger {
	return &StructuredLogger{
		writer:      writer,
		service:     service,
		environment: environment,
		region:      region,
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
