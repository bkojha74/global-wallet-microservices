package observability

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
