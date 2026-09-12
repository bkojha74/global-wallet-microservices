package observability

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
)

func TestStructuredLoggerAddsEnvelopeDefaultsAndCorrelation(t *testing.T) {
	var output strings.Builder
	logger := NewStructuredLogger("wallet-service", "test", "us-east-1", &output)
	ctx := WithCorrelation(context.Background(), Correlation{
		AssociationID:  "association-1",
		TransactionID:  "transaction-1",
		IdempotencyKey: "idempotency-1",
	})

	logger.Emit(ctx, Event{Level: "INFO", EventType: "wallet.transfer.completed"})

	var event Event
	if err := json.Unmarshal([]byte(output.String()), &event); err != nil {
		t.Fatalf("decode structured event: %v", err)
	}
	if event.SchemaVersion != 1 || event.EventID == "" || event.Service != "wallet-service" || event.Environment != "test" || event.Region != "us-east-1" {
		t.Fatalf("unexpected event envelope: %+v", event)
	}
	if event.AssociationID != "association-1" || event.TransactionID != "transaction-1" || event.IdempotencyKey != "idempotency-1" {
		t.Fatalf("unexpected correlation fields: %+v", event)
	}
}

func TestFromHTTPRequestCreatesOrPreservesAssociation(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/transfers", nil)
	request.Header.Set(AssociationIDHeader, "association-from-client")
	request.Header.Set(TransactionIDHeader, "transaction-from-client")
	correlation := FromHTTPRequest(request)
	if correlation.AssociationID != "association-from-client" || correlation.TransactionID != "transaction-from-client" {
		t.Fatalf("unexpected request correlation: %+v", correlation)
	}

	generated := FromHTTPRequest(httptest.NewRequest(http.MethodGet, "/health", nil))
	if generated.AssociationID == "" {
		t.Fatal("expected generated association ID")
	}
}

func TestCorrelationRoundTripsThroughGRPCMetadata(t *testing.T) {
	correlation := Correlation{
		AssociationID:  "association-1",
		TransactionID:  "transaction-1",
		IdempotencyKey: "idempotency-1",
	}
	outgoing := WithOutgoingMetadata(context.Background(), correlation)
	md, ok := metadata.FromOutgoingContext(outgoing)
	if !ok {
		t.Fatal("expected outgoing metadata")
	}
	incoming := metadata.NewIncomingContext(context.Background(), md)
	decoded := FromIncomingContext(incoming)
	if decoded != correlation {
		t.Fatalf("unexpected metadata correlation: got %+v want %+v", decoded, correlation)
	}
}

func TestMemoryLoggerCopiesEvents(t *testing.T) {
	logger := NewMemoryLogger()
	logger.Emit(WithCorrelation(context.Background(), Correlation{AssociationID: "association-1"}), Event{EventType: "test"})
	events := logger.Events()
	if len(events) != 1 || events[0].AssociationID != "association-1" {
		t.Fatalf("unexpected events: %+v", events)
	}
	events[0].EventType = "changed-copy"
	if logger.Events()[0].EventType != "test" {
		t.Fatal("expected Events to return a copy")
	}
}

func TestStructuredLoggerIncludesInstanceIDAndTerminalFields(t *testing.T) {
	var output strings.Builder
	logger := NewStructuredLoggerWithInstance("wallet-service", "test", "us-east-1", "wallet-primary-1", &output)
	ctx := WithCorrelation(context.Background(), Correlation{
		AssociationID: "assoc-123",
		TransactionID: "txn-456",
	})
	success := true
	logger.Emit(ctx, Event{
		Level:      LevelAudit,
		EventType:  "wallet.transfer.completed",
		DurationMS: 42,
		Success:    &success,
	})

	var event Event
	if err := json.Unmarshal([]byte(output.String()), &event); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if event.InstanceID != "wallet-primary-1" {
		t.Errorf("expected instance_id 'wallet-primary-1', got %q", event.InstanceID)
	}
	if event.Level != LevelAudit {
		t.Errorf("expected level 'AUDIT', got %q", event.Level)
	}
	if event.DurationMS != 42 {
		t.Errorf("expected duration_ms 42, got %d", event.DurationMS)
	}
	if event.Success == nil || !*event.Success {
		t.Errorf("expected success true, got %+v", event.Success)
	}
}

func TestValidateEvent(t *testing.T) {
	now := time.Now().UTC()
	validEvent := Event{
		SchemaVersion: 1,
		EventID:       "evt-001",
		OccurredAt:    now,
		Service:       "wallet-service",
		Environment:   "production",
		Level:         LevelInfo,
		EventType:     "wallet.transfer.request_received",
		AssociationID: "assoc-001",
	}

	if err := ValidateEvent(validEvent); err != nil {
		t.Fatalf("expected valid event to pass, got: %v", err)
	}

	// Test invalid schema version
	invalid := validEvent
	invalid.SchemaVersion = 0
	if err := ValidateEvent(invalid); err == nil {
		t.Error("expected error for schema_version 0")
	}

	// Test missing event_id
	invalid = validEvent
	invalid.EventID = ""
	if err := ValidateEvent(invalid); err == nil {
		t.Error("expected error for empty event_id")
	}

	// Test zero occurred_at
	invalid = validEvent
	invalid.OccurredAt = time.Time{}
	if err := ValidateEvent(invalid); err == nil {
		t.Error("expected error for zero occurred_at")
	}

	// Test missing service
	invalid = validEvent
	invalid.Service = ""
	if err := ValidateEvent(invalid); err == nil {
		t.Error("expected error for empty service")
	}

	// Test missing environment
	invalid = validEvent
	invalid.Environment = ""
	if err := ValidateEvent(invalid); err == nil {
		t.Error("expected error for empty environment")
	}

	// Test invalid level
	invalid = validEvent
	invalid.Level = "VERBOSE"
	if err := ValidateEvent(invalid); err == nil {
		t.Error("expected error for invalid level VERBOSE")
	}

	// Test all valid levels
	for _, lvl := range []string{LevelAudit, LevelError, LevelWarn, LevelInfo, LevelDebug} {
		valid := validEvent
		valid.Level = lvl
		if err := ValidateEvent(valid); err != nil {
			t.Errorf("expected valid level %s to pass, got: %v", lvl, err)
		}
	}

	// Test missing event_type
	invalid = validEvent
	invalid.EventType = ""
	if err := ValidateEvent(invalid); err == nil {
		t.Error("expected error for empty event_type")
	}

	// Test missing association_id
	invalid = validEvent
	invalid.AssociationID = ""
	if err := ValidateEvent(invalid); err == nil {
		t.Error("expected error for empty association_id")
	}
}

func TestRedactAttributes(t *testing.T) {
	attrs := map[string]any{
		"wallet_id":   "w-123",
		"password":    "super-secret-pw",
		"Token":       "bearer-123",
		"credit_card": "4111-2222-3333-4444",
		"nested": map[string]any{
			"api_key": "key-xyz",
			"safe":    "safe-val",
		},
	}

	redacted := RedactAttributes(attrs)
	if redacted["wallet_id"] != "w-123" {
		t.Errorf("safe field was altered: %v", redacted["wallet_id"])
	}
	if redacted["password"] != "[REDACTED]" {
		t.Errorf("expected password to be redacted, got: %v", redacted["password"])
	}
	if redacted["Token"] != "[REDACTED]" {
		t.Errorf("expected Token to be redacted, got: %v", redacted["Token"])
	}
	if redacted["credit_card"] != "[REDACTED]" {
		t.Errorf("expected credit_card to be redacted, got: %v", redacted["credit_card"])
	}

	nested, ok := redacted["nested"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested map, got: %T", redacted["nested"])
	}
	if nested["api_key"] != "[REDACTED]" {
		t.Errorf("expected nested api_key to be redacted, got: %v", nested["api_key"])
	}
	if nested["safe"] != "safe-val" {
		t.Errorf("expected nested safe to be preserved, got: %v", nested["safe"])
	}
}

type mockPublisher struct {
	mu     sync.Mutex
	events []Event
	err    error
}

func (m *mockPublisher) Publish(ctx context.Context, e Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.events = append(m.events, e)
	return nil
}

func (m *mockPublisher) Close() error {
	return nil
}

func TestFileSpoolAppendAndReplay(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "spool-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	spoolPath := filepath.Join(tmpDir, "test.jsonl")
	spool := NewFileSpool(spoolPath)

	evt := Event{
		SchemaVersion: 1,
		EventID:       "spool-1",
		EventType:     "test.event",
		Level:         LevelInfo,
	}

	if err := spool.Append(evt); err != nil {
		t.Fatalf("spool append failed: %v", err)
	}

	pub := &mockPublisher{}
	if err := spool.Replay(context.Background(), pub); err != nil {
		t.Fatalf("spool replay failed: %v", err)
	}

	if len(pub.events) != 1 || pub.events[0].EventID != "spool-1" {
		t.Fatalf("unexpected replayed events: %+v", pub.events)
	}

	// File should be removed after successful replay
	if _, err := os.Stat(spoolPath); !os.IsNotExist(err) {
		t.Errorf("expected spool file to be deleted after full replay, stat err: %v", err)
	}
}
