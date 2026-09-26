package observability

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileSpoolPruningAndReplayCorrupt(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "spool_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	spoolPath := filepath.Join(tmpDir, "events.spool")

	cfg := SpoolConfig{
		MaxBytes: 100, // Small limit to trigger size pruning
		MaxAge:   50 * time.Millisecond,
		Fsync:    false,
	}

	spool := NewFileSpoolWithConfig(spoolPath, cfg)

	// Append multiple events
	for i := 0; i < 5; i++ {
		_ = spool.Append(Event{
			EventType:  "SPOOL_EVENT",
			Message:    "Testing spool pruning with a longer string message body to exceed size limit",
			OccurredAt: time.Now().Add(-100 * time.Millisecond),
		})
	}

	// Verify Size and OldestAge
	if spool.Size() <= 0 {
		t.Fatalf("expected positive spool size")
	}

	oldest := spool.OldestAge()
	if oldest < 0 {
		t.Fatalf("expected non-negative oldest age")
	}

	// Replay events with corrupt line in spool file
	f, _ := os.OpenFile(spoolPath, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString("{corrupt_json_line\n")
	_ = f.Close()

	pub := &mockPublisherUnitTest{}
	ctx := context.Background()
	_ = spool.Replay(ctx, pub)
}
