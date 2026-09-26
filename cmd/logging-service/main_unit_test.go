package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestLoggingGetEnvOrDefault(t *testing.T) {
	os.Unsetenv("TEST_KEY_123")
	if getEnvOrDefault("TEST_KEY_123", "default") != "default" {
		t.Fatalf("expected fallback value")
	}

	t.Setenv("TEST_KEY_123", "custom")
	if getEnvOrDefault("TEST_KEY_123", "default") != "custom" {
		t.Fatalf("expected custom value")
	}
}

func TestQueueTypeArgs(t *testing.T) {
	os.Unsetenv("LOGGING_QUEUE_TYPE")
	if queueTypeArgs() != nil {
		t.Fatalf("expected nil args for default classic queue")
	}

	t.Setenv("LOGGING_QUEUE_TYPE", "quorum")
	args := queueTypeArgs()
	if args == nil || args["x-queue-type"] != "quorum" {
		t.Fatalf("expected quorum queue type args")
	}
}

func TestApiKeyMiddleware(t *testing.T) {
	dummyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// 1. Empty API key (auth disabled)
	noAuthHandler := apiKeyMiddleware("", dummyHandler)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/query", nil)
	w := httptest.NewRecorder()
	noAuthHandler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK when API key disabled, got %d", w.Code)
	}

	// 2. Configured API key - missing / invalid header
	authHandler := apiKeyMiddleware("secret-token", dummyHandler)

	reqMissing := httptest.NewRequest(http.MethodGet, "/api/v1/query", nil)
	wMissing := httptest.NewRecorder()
	authHandler.ServeHTTP(wMissing, reqMissing)
	if wMissing.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for missing auth header, got %d", wMissing.Code)
	}

	reqInvalid := httptest.NewRequest(http.MethodGet, "/api/v1/query", nil)
	reqInvalid.Header.Set("Authorization", "Bearer wrong-token")
	wInvalid := httptest.NewRecorder()
	authHandler.ServeHTTP(wInvalid, reqInvalid)
	if wInvalid.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for invalid token, got %d", wInvalid.Code)
	}

	// 3. Configured API key - valid header
	reqValid := httptest.NewRequest(http.MethodGet, "/api/v1/query", nil)
	reqValid.Header.Set("Authorization", "Bearer secret-token")
	wValid := httptest.NewRecorder()
	authHandler.ServeHTTP(wValid, reqValid)
	if wValid.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for valid bearer token, got %d", wValid.Code)
	}

	// 4. Non-protected route (e.g. /healthz)
	reqHealth := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	wHealth := httptest.NewRecorder()
	authHandler.ServeHTTP(wHealth, reqHealth)
	if wHealth.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for health endpoint without token, got %d", wHealth.Code)
	}
}

func TestRetentionSchedulerAndRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	repo := &mockLogRepoImpl{}

	// Test default parameters fallback
	startRetentionScheduler(ctx, repo, 0, 0)
	time.Sleep(100 * time.Millisecond)

	// Test direct run retention
	runRetention(ctx, repo, 30)
}

func TestBuildHTTPHandlerAndMetricsServer(t *testing.T) {
	repo := &mockLogRepoImpl{}
	handler := buildHTTPHandler(repo, "key123")

	// /healthz
	reqH := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	wH := httptest.NewRecorder()
	handler.ServeHTTP(wH, reqH)
	if wH.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from healthz")
	}

	// /readyz unhealthy
	badRepo := &mockLogRepoImpl{healthErr: fmt.Errorf("db down"), retentionErr: fmt.Errorf("retention failed")}
	badHandler := buildHTTPHandler(badRepo, "")
	wRUnhealthy := httptest.NewRecorder()
	reqRUnhealthy := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	badHandler.ServeHTTP(wRUnhealthy, reqRUnhealthy)
	if wRUnhealthy.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for unhealthy readyz, got %d", wRUnhealthy.Code)
	}

	runRetention(context.Background(), badRepo, 30)

	// Metrics server
	metricsSrv := startMetricsServer("9897")
	time.Sleep(100 * time.Millisecond)
	resp, err := http.Get("http://127.0.0.1:9897/healthz")
	if err == nil {
		resp.Body.Close()
	}
	_ = metricsSrv.Shutdown(context.Background())
}

type mockAck struct {
	acked  bool
	nacked bool
}

func (m *mockAck) Ack(tag uint64, multiple bool) error                    { m.acked = true; return nil }
func (m *mockAck) Nack(tag uint64, multiple bool, requeue bool) error     { m.nacked = true; return nil }
func (m *mockAck) Reject(tag uint64, requeue bool) error                  { m.nacked = true; return nil }

func TestConsumerProcessMessage(t *testing.T) {
	oldBackoff := retryBackoff
	retryBackoff = 1 * time.Millisecond
	defer func() { retryBackoff = oldBackoff }()

	repo := &mockLogRepoImpl{}
	c := &Consumer{repo: repo}

	// 1. Invalid JSON
	ack1 := &mockAck{}
	c.processMessage(context.Background(), amqp.Delivery{Acknowledger: ack1, Body: []byte("bad-json")})
	if !ack1.nacked {
		t.Fatalf("expected nack for bad json")
	}

	// 2. Invalid schema (missing event_id)
	ack2 := &mockAck{}
	c.processMessage(context.Background(), amqp.Delivery{Acknowledger: ack2, Body: []byte(`{"event_type":"test"}`)})
	if !ack2.nacked {
		t.Fatalf("expected nack for invalid schema")
	}

	// 3. Valid event -> success Ack
	ack3 := &mockAck{}
	validEvt := `{"event_id":"evt-1","event_type":"test","service":"test","environment":"dev","region":"us-east-1","level":"INFO","occurred_at":"2026-01-01T00:00:00Z","schema_version":1,"association_id":"assoc-1"}`
	c.processMessage(context.Background(), amqp.Delivery{Acknowledger: ack3, Body: []byte(validEvt)})
	if !ack3.acked {
		t.Fatalf("expected ack for valid event")
	}

	// 4. Save error -> retries & Nack
	failRepo := &mockLogRepoImpl{saveErr: fmt.Errorf("write error")}
	cFail := &Consumer{repo: failRepo}
	ack4 := &mockAck{}
	cFail.processMessage(context.Background(), amqp.Delivery{Acknowledger: ack4, Body: []byte(validEvt)})
	if !ack4.nacked {
		t.Fatalf("expected nack for save error")
	}
}

func TestWaitBackoffAndCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled context

	backoff := 1 * time.Millisecond
	if waitBackoff(ctx, &backoff, 10*time.Millisecond) {
		t.Fatalf("expected waitBackoff to return false on cancelled context")
	}

	c := &Consumer{}
	c.cleanup()
	_ = c.Close()

	mongoRepo := &MongoLogRepository{}
	_ = mongoRepo.Close(ctx)
}

func TestRunLoggingServer_CancelledContext(t *testing.T) {
	// 1. Connection failure path
	t.Setenv("TEST_MOCK_DB", "false")
	t.Setenv("HTTP_PORT", "58090")
	t.Setenv("METRICS_PORT", "59897")
	t.Setenv("MONGO_URI", "mongodb://127.0.0.1:27019/?connectTimeoutMS=100")
	t.Setenv("RABBITMQ_URL", "amqp://guest:guest@127.0.0.1:5679/")

	ctx1, cancel1 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel1()
	_ = runLoggingServer(ctx1)

	// 2. Mock DB clean startup and shutdown path
	t.Setenv("TEST_MOCK_DB", "true")
	t.Setenv("HTTP_PORT", "58091")
	t.Setenv("METRICS_PORT", "59898")

	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()

	err := runLoggingServer(ctx2)
	if err != nil {
		t.Errorf("expected clean shutdown, got: %v", err)
	}
}

func TestConsumerConsumeLoop(t *testing.T) {
	repo := &mockLogRepoImpl{}
	c := &Consumer{repo: repo}

	msgs := make(chan amqp.Delivery, 2)
	ack := &mockAck{}
	validEvt := `{"event_id":"evt-loop-1","event_type":"test","service":"test","environment":"dev","region":"us-east-1","level":"INFO","occurred_at":"2026-01-01T00:00:00Z","schema_version":1,"association_id":"assoc-1"}`
	msgs <- amqp.Delivery{Acknowledger: ack, Body: []byte(validEvt)}
	close(msgs)

	ctx := context.Background()
	err := c.consumeLoop(ctx, msgs)
	if err == nil {
		t.Fatalf("expected error when channel closed")
	}

	// Cancelled context path
	msgs2 := make(chan amqp.Delivery, 1)
	ctxCancel, cancel := context.WithCancel(context.Background())
	cancel()
	errCancel := c.consumeLoop(ctxCancel, msgs2)
	if errCancel != nil {
		t.Fatalf("expected nil error on cancelled context")
	}

	// Start with cancelled context
	cStart := &Consumer{}
	errStart := cStart.Start(ctxCancel)
	if errStart != nil {
		t.Fatalf("expected nil error from Start on cancelled context")
	}
}


