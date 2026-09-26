package observability

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type failPublisher struct {
	failAfter int
	count     int
}

func (f *failPublisher) Publish(_ context.Context, _ Event) error {
	f.count++
	if f.count > f.failAfter {
		return errors.New("publisher transient failure")
	}
	return nil
}

func (f *failPublisher) Close() error { return nil }

func TestFileSpool_ExtraCoverage(t *testing.T) {
	// 1. DefaultSpoolConfig with env vars
	os.Setenv("LOGGING_SPOOL_MAX_BYTES", "1048576")
	os.Setenv("LOGGING_SPOOL_MAX_AGE_HOURS", "24")
	os.Setenv("LOGGING_SPOOL_FSYNC", "false")
	cfg := DefaultSpoolConfig()
	if cfg.MaxBytes != 1048576 || cfg.MaxAge != 24*time.Hour || cfg.Fsync != false {
		t.Errorf("DefaultSpoolConfig mismatch: %v", cfg)
	}
	os.Unsetenv("LOGGING_SPOOL_MAX_BYTES")
	os.Unsetenv("LOGGING_SPOOL_MAX_AGE_HOURS")
	os.Unsetenv("LOGGING_SPOOL_FSYNC")

	// 2. Size and OldestAge with nil & non-existent
	var nilSpool *FileSpool
	if nilSpool.Size() != 0 || nilSpool.OldestAge() != 0 {
		t.Errorf("Expected 0 size and age for nil spool")
	}
	if err := nilSpool.Replay(context.Background(), &failPublisher{}); err != nil {
		t.Errorf("Expected nil error on nil spool Replay")
	}

	tmpDir, err := os.MkdirTemp("", "spool_test_*")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	spoolFile := filepath.Join(tmpDir, "test.spool")
	spool := NewFileSpoolWithConfig(spoolFile, SpoolConfig{
		MaxBytes: 150, // very small budget to force pruning
		MaxAge:   1 * time.Hour,
		Fsync:    false,
	})

	if spool.Size() != 0 || spool.OldestAge() != 0 {
		t.Errorf("Expected 0 size and age before appending")
	}

	// 3. Append multiple events to trigger prune
	ev1 := Event{EventID: "ev-1", OccurredAt: time.Now().Add(-10 * time.Minute), EventType: "test", Level: LevelInfo}
	ev2 := Event{EventID: "ev-2", OccurredAt: time.Now().Add(-5 * time.Minute), EventType: "test", Level: LevelAudit}
	ev3 := Event{EventID: "ev-3", OccurredAt: time.Now(), EventType: "test", Level: LevelInfo}

	_ = spool.Append(ev1)
	_ = spool.Append(ev2)
	_ = spool.Append(ev3)

	if spool.Size() == 0 {
		t.Errorf("Expected non-zero spool size")
	}
	if spool.OldestAge() <= 0 {
		t.Errorf("Expected positive OldestAge")
	}

	// 4. Replay with publisher failing after 1 event
	pub := &failPublisher{failAfter: 1}
	_ = spool.Replay(context.Background(), pub)

	// Spool should still have remaining events written back
	if spool.Size() == 0 {
		t.Errorf("Expected remaining events in spool file after partial failure")
	}
}
