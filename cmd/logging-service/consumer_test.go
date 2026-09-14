package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"wallet-system/pkg/observability"
	amqp "github.com/rabbitmq/amqp091-go"
)

type mockRepo struct {
	saveCount   int
	lastEvent   observability.Event
	errToReturn error
}

func (m *mockRepo) Save(ctx context.Context, event observability.Event) error {
	m.saveCount++
	m.lastEvent = event
	return m.errToReturn
}

func (m *mockRepo) Find(_ context.Context, _ QueryFilter) ([]observability.Event, error) {
	return nil, nil
}

func (m *mockRepo) Health(ctx context.Context) error {
	return nil
}

type mockAcknowledger struct {
	ackCount  int
	nackCount int
	requeued  bool
}

func (m *mockAcknowledger) Ack(tag uint64, multiple bool) error {
	m.ackCount++
	return nil
}

func (m *mockAcknowledger) Nack(tag uint64, multiple bool, requeue bool) error {
	m.nackCount++
	m.requeued = requeue
	return nil
}

func (m *mockAcknowledger) Reject(tag uint64, requeue bool) error {
	return nil
}

func init() {
	retryBackoff = 0 // Fast tests
}

func TestProcessMessage(t *testing.T) {
	repo := &mockRepo{}
	consumer := &Consumer{
		repo: repo,
	}

	validEvent := observability.Event{
		SchemaVersion: 1,
		EventID:       "test-event-1",
		OccurredAt:    time.Now(),
		Service:       "test-service",
		Environment:   "test",
		Level:         observability.LevelInfo,
		EventType:     "test.event",
		AssociationID: "assoc-1",
	}
	validBody, _ := json.Marshal(validEvent)

	tests := []struct {
		name          string
		body          []byte
		repoErr       error
		expectAck     bool
		expectNack    bool
		expectRequeue bool
		expectRetries int
	}{
		{
			name:          "Valid event processes successfully",
			body:          validBody,
			repoErr:       nil,
			expectAck:     true,
			expectNack:    false,
			expectRetries: 1,
		},
		{
			name:       "Invalid JSON triggers Nack(false)",
			body:       []byte("not json"),
			repoErr:    nil,
			expectAck:  false,
			expectNack: true,
		},
		{
			name: "Invalid schema triggers Nack(false)",
			body: func() []byte {
				invalid := validEvent
				invalid.EventID = "" // invalid schema
				b, _ := json.Marshal(invalid)
				return b
			}(),
			repoErr:    nil,
			expectAck:  false,
			expectNack: true,
		},
		{
			name:          "Transient repo error retries and then Nacks to DLQ",
			body:          validBody,
			repoErr:       errors.New("db timeout"),
			expectAck:     false,
			expectNack:    true,
			expectRetries: 3,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo.saveCount = 0
			repo.errToReturn = tc.repoErr

			ack := &mockAcknowledger{}
			msg := amqp.Delivery{
				Body:         tc.body,
				Acknowledger: ack,
			}

			// Capture backoff behavior to not wait in tests
			// In consumer.go, retryBackoff is 2 * time.Second.
			// Ideally we'd inject this, but for this simple test, we will just let it run.
			// Since we have a 3 retry count, it would take 6s.
			// We can run this async or accept the delay for the failure case.
			consumer.processMessage(context.Background(), msg)

			if tc.expectAck && ack.ackCount != 1 {
				t.Errorf("Expected 1 Ack, got %d", ack.ackCount)
			}
			if tc.expectNack && ack.nackCount != 1 {
				t.Errorf("Expected 1 Nack, got %d", ack.nackCount)
			}
			if tc.expectNack && ack.requeued != tc.expectRequeue {
				t.Errorf("Expected Requeue=%v, got %v", tc.expectRequeue, ack.requeued)
			}
			if tc.repoErr != nil && repo.saveCount != tc.expectRetries {
				t.Errorf("Expected %d retries, got %d", tc.expectRetries, repo.saveCount)
			}
		})
	}
}
