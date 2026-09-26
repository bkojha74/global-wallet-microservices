package observability

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type dummyPublisher struct {
	published []Event
	err       error
}

func (d *dummyPublisher) Publish(_ context.Context, e Event) error {
	if d.err != nil {
		return d.err
	}
	d.published = append(d.published, e)
	return nil
}

func (d *dummyPublisher) Close() error { return nil }

func TestPublisherAndAsyncLogger_Extra(t *testing.T) {
	ctx := context.Background()

	// 1. Test TLSConfigFromEnv empty
	os.Unsetenv("LOGGING_RABBITMQ_TLS_CERT")
	os.Unsetenv("LOGGING_RABBITMQ_TLS_KEY")
	os.Unsetenv("LOGGING_RABBITMQ_TLS_CA")

	cfg, err := TLSConfigFromEnv()
	if err != nil || cfg != nil {
		t.Errorf("Expected nil tls config when env vars are unset, got %v, err=%v", cfg, err)
	}

	// TLSConfigFromEnv error branch
	os.Setenv("LOGGING_RABBITMQ_TLS_CERT", "invalid_cert.pem")
	os.Setenv("LOGGING_RABBITMQ_TLS_KEY", "invalid_key.pem")
	os.Setenv("LOGGING_RABBITMQ_TLS_CA", "invalid_ca.pem")
	_, errTls := TLSConfigFromEnv()
	if errTls == nil {
		t.Errorf("Expected error from TLSConfigFromEnv with invalid cert paths")
	}
	os.Unsetenv("LOGGING_RABBITMQ_TLS_CERT")
	os.Unsetenv("LOGGING_RABBITMQ_TLS_KEY")
	os.Unsetenv("LOGGING_RABBITMQ_TLS_CA")

	// 2. Test LoggerFromEnvironment local mode vs RabbitMQ mode
	os.Unsetenv("LOGGING_RABBITMQ_URL")
	buf := &bytes.Buffer{}
	localLogger := LoggerFromEnvironment("test-svc", "dev", "us-east-1", buf)
	if localLogger == nil {
		t.Fatalf("Expected non-nil local logger")
	}
	localLogger.Emit(ctx, Event{EventID: "ev-1", EventType: "test", Level: LevelInfo})

	os.Setenv("LOGGING_RABBITMQ_URL", "amqp://invalid:5672")
	asyncEnvLogger := LoggerFromEnvironment("test-svc", "dev", "us-east-1", buf)
	if asyncEnvLogger == nil {
		t.Fatalf("Expected non-nil async env logger")
	}
	_ = asyncEnvLogger.Close(ctx)
	os.Unsetenv("LOGGING_RABBITMQ_URL")

	// 3. Test AsyncLogger methods (Emit, MetricsHandler, Sync, Close)
	tmpDir := t.TempDir()
	spoolFile := filepath.Join(tmpDir, "async_test.spool")
	spool := NewFileSpool(spoolFile)
	pub := &dummyPublisher{}

	asyncLog := NewAsyncLogger("svc", "env", "reg", pub, spool, 16, buf)
	if asyncLog.MetricsHandler() == nil || asyncLog.MetricsRegistry() == nil {
		t.Errorf("Expected non-nil metrics handler and registry")
	}

	asyncLog.Emit(ctx, Event{
		EventID:     "ev-async-1",
		EventType:   "audit.transfer",
		Level:       LevelAudit,
		Message:     "transfer completed",
		Environment: "test",
		Service:     "svc",
	})

	// Wait for queue drain
	time.Sleep(50 * time.Millisecond)
	_ = asyncLog.Sync(ctx)
	_ = asyncLog.Close(ctx)

	if len(pub.published) == 0 {
		t.Errorf("Expected published events in dummy publisher")
	}
}
