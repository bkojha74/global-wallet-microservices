package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"wallet-system/pkg/observability"
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

func (m *mockRepo) Retention(_ context.Context, _ int) (int64, error) { return 0, nil }

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

func TestBuildHTTPHandler(t *testing.T) {
	repo := &mockRepo{}
	handler := buildHTTPHandler(repo, "")

	// 1. /healthz
	reqHealth := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recHealth := httptest.NewRecorder()
	handler.ServeHTTP(recHealth, reqHealth)
	if recHealth.Code != http.StatusOK {
		t.Fatalf("expected 200 for /healthz, got %d", recHealth.Code)
	}

	// 2. /readyz when healthy
	reqReady := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	recReady := httptest.NewRecorder()
	handler.ServeHTTP(recReady, reqReady)
	if recReady.Code != http.StatusOK {
		t.Fatalf("expected 200 for /readyz, got %d", recReady.Code)
	}

	// 3. /readyz when unhealthy
	unhealthyRepo := &mockUnhealthyRepo{}
	unhealthyHandler := buildHTTPHandler(unhealthyRepo, "")
	recUnhealthy := httptest.NewRecorder()
	unhealthyHandler.ServeHTTP(recUnhealthy, reqReady)
	if recUnhealthy.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for unhealthy /readyz, got %d", recUnhealthy.Code)
	}
}

type mockUnhealthyRepo struct {
	mockRepo
}

func (m *mockUnhealthyRepo) Health(ctx context.Context) error {
	return errors.New("db disconnected")
}

func TestGetEnvOrDefault(t *testing.T) {
	t.Setenv("TEST_KEY_123", "value-abc")
	if val := getEnvOrDefault("TEST_KEY_123", "fallback"); val != "value-abc" {
		t.Fatalf("expected value-abc, got %s", val)
	}
	if val := getEnvOrDefault("NONEXISTENT_KEY_123", "fallback"); val != "fallback" {
		t.Fatalf("expected fallback, got %s", val)
	}
}

func TestRunRetention(t *testing.T) {
	repo := &mockRepo{}
	// Should run cleanly without panics
	runRetention(context.Background(), repo, 30)
}

func TestBuildHTTPHandlerWithAPIKey(t *testing.T) {
	repo := &mockRepo{}
	handler := buildHTTPHandler(repo, "my-secret-key")

	// Protected route without key -> 401
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized without API key, got %d", rec.Code)
	}

	// Protected route with valid key
	reqAuth := httptest.NewRequest(http.MethodGet, "/api/v1/logs", nil)
	reqAuth.Header.Set("Authorization", "Bearer my-secret-key")
	recAuth := httptest.NewRecorder()
	handler.ServeHTTP(recAuth, reqAuth)
	if recAuth.Code == http.StatusUnauthorized {
		t.Fatalf("expected authorization to succeed with valid API key, got %d", recAuth.Code)
	}
}

func TestStartMetricsServer(t *testing.T) {
	srv := startMetricsServer("0")
	defer srv.Close()

	// Verify healthz through handler
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for /healthz, got %d", rec.Code)
	}
}

func TestWaitBackoffAndEnsureConnection(t *testing.T) {
	// 1. Normal backoff doubles
	dur := 2 * time.Millisecond
	ok := waitBackoff(context.Background(), &dur, 10*time.Millisecond)
	if !ok || dur != 4*time.Millisecond {
		t.Fatalf("expected backoff to double to 4ms, got %v, ok=%t", dur, ok)
	}

	// 2. Cap at maxBackoff
	dur = 8 * time.Millisecond
	ok = waitBackoff(context.Background(), &dur, 10*time.Millisecond)
	if !ok || dur != 10*time.Millisecond {
		t.Fatalf("expected capped at 10ms, got %v", dur)
	}

	// 3. Cancelled context
	ctxCancel, cancel := context.WithCancel(context.Background())
	cancel()
	dur = 5 * time.Second
	ok = waitBackoff(ctxCancel, &dur, 10*time.Second)
	if ok {
		t.Fatal("expected waitBackoff to return false on cancelled context")
	}

	// 4. ensureConnection with cancelled context
	c := &Consumer{}
	bDur := 1 * time.Millisecond
	err := c.ensureConnection(ctxCancel, &bDur, 5*time.Millisecond)
	if err == nil {
		t.Fatal("expected error on ensureConnection with cancelled context")
	}
}

func TestConsumeLoopAndConsumerLifecycle(t *testing.T) {
	c := &Consumer{
		repo: &mockRepo{},
	}

	// 1. Consumer.Close and cleanup on empty consumer
	c.cleanup()
	if err := c.Close(); err != nil {
		t.Fatalf("expected nil on Close, got %v", err)
	}

	// 2. consumeLoop with closed channel
	msgs := make(chan amqp.Delivery)
	close(msgs)
	err := c.consumeLoop(context.Background(), msgs)
	if err == nil || err.Error() != "consumer channel closed" {
		t.Fatalf("expected channel closed error, got %v", err)
	}

	// 3. consumeLoop with cancelled context
	ctxCancel, cancel := context.WithCancel(context.Background())
	cancel()
	msgsUnclosed := make(chan amqp.Delivery)
	errCancel := c.consumeLoop(ctxCancel, msgsUnclosed)
	if errCancel != nil {
		t.Fatalf("expected nil on cancelled context, got %v", errCancel)
	}

	// 4. Start with cancelled context
	errStart := c.Start(ctxCancel)
	if errStart != nil {
		t.Fatalf("expected nil on Start with cancelled context, got %v", errStart)
	}
}

type unhealthyRepo struct {
	mockRepo
}

func (u *unhealthyRepo) Health(ctx context.Context) error {
	return errors.New("db disconnected")
}

func TestReadyzEndpoint(t *testing.T) {
	// 1. Healthy repo -> 200
	repoHealthy := &mockRepo{}
	handlerHealthy := buildHTTPHandler(repoHealthy, "")

	req1 := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec1 := httptest.NewRecorder()
	handlerHealthy.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for /readyz, got %d", rec1.Code)
	}

	// 2. Unhealthy repo -> 503
	repoUnhealthy := &unhealthyRepo{}
	handlerUnhealthy := buildHTTPHandler(repoUnhealthy, "")

	req2 := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec2 := httptest.NewRecorder()
	handlerUnhealthy.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable for /readyz when repo unhealthy, got %d", rec2.Code)
	}
}
