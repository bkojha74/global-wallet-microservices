package observability

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

type mockPublisherUnitTest struct {
	published []Event
	err       error
	closed    bool
}

func (m *mockPublisherUnitTest) Publish(ctx context.Context, e Event) error {
	if m.err != nil {
		return m.err
	}
	m.published = append(m.published, e)
	return nil
}

func (m *mockPublisherUnitTest) Close() error {
	m.closed = true
	return nil
}

func TestAsyncLoggerEmitAndFlush(t *testing.T) {
	buf := &bytes.Buffer{}
	pub := &mockPublisherUnitTest{}

	cfg := AsyncLoggerConfig{
		Service:     "test-service",
		Environment: "test-env",
		Region:      "us-east-1",
		InstanceID:  "inst-1",
		Publisher:   pub,
		BufferSize:  5,
		Writer:      buf,
		Metrics:     NewMetricsRegistry(),
	}

	logger := NewAsyncLoggerFull(cfg)
	if logger.MetricsRegistry() == nil {
		t.Fatalf("expected non-nil metrics registry")
	}

	h := logger.MetricsHandler()
	if h == nil {
		t.Fatalf("expected non-nil metrics handler")
	}

	ctx := context.Background()
	logger.Emit(ctx, Event{
		EventType: "TEST_EVENT",
		Level:     "INFO",
		Message:   "Hello World",
	})

	err := logger.Sync(ctx)
	if err != nil {
		t.Fatalf("unexpected sync error: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if err := logger.Close(ctx); err != nil {
		t.Fatalf("unexpected close error: %v", err)
	}

	if len(pub.published) == 0 {
		t.Fatalf("expected published event")
	}
}

func TestAsyncLoggerBufferOverflowAndFallback(t *testing.T) {
	buf := &bytes.Buffer{}
	failPub := &mockPublisherUnitTest{err: errors.New("publish error")}

	tmpDir, err := os.MkdirTemp("", "spool_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	spool := NewFileSpool(tmpDir + "/test.spool")

	logger := NewAsyncLogger("srv", "dev", "us-east-1", failPub, spool, 2, buf)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		logger.Emit(ctx, Event{EventType: "EVT", Message: "overflow"})
	}

	_ = logger.Close(ctx)
}

func TestCountingPublisherAndNormalizeEvent(t *testing.T) {
	inner := &mockPublisherUnitTest{}
	cp := &countingPublisher{inner: inner}

	ctx := context.Background()
	e := Event{EventType: "COUNT_EVT"}
	err := cp.Publish(ctx, e)
	if err != nil || len(inner.published) != 1 {
		t.Fatalf("failed counting publisher delegate")
	}
	_ = cp.Close()

	norm := normalizeEvent(ctx, Event{
		EventType: "EVT",
		Level:     "WARN",
	}, "srv", "env", "reg", "inst")

	if norm.Service != "srv" || norm.Environment != "env" {
		t.Fatalf("unexpected normalized event fields")
	}
}

func TestRabbitPublisherConstructorsAndTLS(t *testing.T) {
	p := NewRabbitPublisher("amqp://guest:guest@127.0.0.1:56799/", "ex")
	_ = p.IsConnected()
	_ = p.Close()

	t.Setenv("LOGGING_RABBITMQ_TLS_CERT", "")
	t.Setenv("LOGGING_RABBITMQ_TLS_KEY", "")
	t.Setenv("LOGGING_RABBITMQ_TLS_CA", "")
	tlsCfg, err := TLSConfigFromEnv()
	if err != nil || tlsCfg != nil {
		t.Fatalf("expected nil TLS config when disabled")
	}

	t.Setenv("LOGGING_RABBITMQ_TLS_CERT", "non-existent.crt")
	t.Setenv("LOGGING_RABBITMQ_TLS_KEY", "non-existent.key")
	t.Setenv("LOGGING_RABBITMQ_TLS_CA", "non-existent.ca")
	_, err = TLSConfigFromEnv()
	if err == nil {
		t.Fatalf("expected error for non-existent CA cert file")
	}
}

func TestLoggerFromEnvironment(t *testing.T) {
	buf := &bytes.Buffer{}
	l := LoggerFromEnvironment("test-service", "local", "us-west-2", buf)
	if l == nil {
		t.Fatalf("expected non-nil logger from environment")
	}
	_ = l.Close(context.Background())
}
