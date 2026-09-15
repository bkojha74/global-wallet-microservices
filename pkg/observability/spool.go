package observability

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type EventPublisher interface {
	Publish(context.Context, Event) error
	Close() error
}

type SpoolConfig struct {
	MaxBytes int64
	MaxAge   time.Duration
	Fsync    bool
}

// DefaultSpoolConfig reads from environment or uses default constants (GAP-06)
func DefaultSpoolConfig() SpoolConfig {
	maxBytes := int64(50 * 1024 * 1024) // 50 MiB
	if val := os.Getenv("LOGGING_SPOOL_MAX_BYTES"); val != "" {
		if parsed, err := strconv.ParseInt(val, 10, 64); err == nil && parsed > 0 {
			maxBytes = parsed
		}
	}
	maxAge := 72 * time.Hour // 72 hours
	if val := os.Getenv("LOGGING_SPOOL_MAX_AGE_HOURS"); val != "" {
		if parsed, err := strconv.ParseInt(val, 10, 64); err == nil && parsed > 0 {
			maxAge = time.Duration(parsed) * time.Hour
		}
	}
	fsync := true
	if val := os.Getenv("LOGGING_SPOOL_FSYNC"); val != "" {
		if parsed, err := strconv.ParseBool(val); err == nil {
			fsync = parsed
		}
	}
	return SpoolConfig{
		MaxBytes: maxBytes,
		MaxAge:   maxAge,
		Fsync:    fsync,
	}
}

type FileSpool struct {
	mu     sync.Mutex
	path   string
	config SpoolConfig
}

func NewFileSpool(path string) *FileSpool {
	return NewFileSpoolWithConfig(path, DefaultSpoolConfig())
}

func NewFileSpoolWithConfig(path string, config SpoolConfig) *FileSpool {
	if config.MaxBytes <= 0 {
		config.MaxBytes = 50 * 1024 * 1024
	}
	if config.MaxAge <= 0 {
		config.MaxAge = 72 * time.Hour
	}
	return &FileSpool{
		path:   path,
		config: config,
	}
}

func (s *FileSpool) Append(event Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// OS-level file lock (GAP-05)
	lock, err := acquireFileLock(s.path + ".lock")
	if err != nil {
		return fmt.Errorf("acquire spool lock: %w", err)
	}
	defer lock.Release()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}

	// Check if pruning is necessary before append
	fi, err := os.Stat(s.path)
	if err == nil && fi.Size()+int64(len(payload))+1 > s.config.MaxBytes {
		_ = s.pruneUnderLock()
	}

	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()

	if _, err := file.Write(append(payload, '\n')); err != nil {
		return err
	}

	if s.config.Fsync {
		return file.Sync()
	}
	return nil
}

func (s *FileSpool) Replay(ctx context.Context, publisher EventPublisher) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// OS-level file lock (GAP-05)
	lock, err := acquireFileLock(s.path + ".lock")
	if err != nil {
		return fmt.Errorf("acquire spool lock: %w", err)
	}
	defer lock.Release()

	file, err := os.Open(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	var remaining []Event
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			file.Close()
			return fmt.Errorf("decode spool event: %w", err)
		}
		if err := publisher.Publish(ctx, event); err != nil {
			remaining = append(remaining, event)
			for scanner.Scan() {
				var pending Event
				if err := json.Unmarshal(scanner.Bytes(), &pending); err != nil {
					file.Close()
					return fmt.Errorf("decode pending spool event: %w", err)
				}
				remaining = append(remaining, pending)
			}
			break
		}
	}
	scanErr := scanner.Err()
	closeErr := file.Close()
	if scanErr != nil {
		return scanErr
	}
	if closeErr != nil {
		return closeErr
	}

	if len(remaining) == 0 {
		return os.Remove(s.path)
	}

	temporaryPath := s.path + ".tmp"
	temporary, err := os.OpenFile(temporaryPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(temporary)
	for _, event := range remaining {
		if err := encoder.Encode(event); err != nil {
			temporary.Close()
			return err
		}
	}
	if s.config.Fsync {
		if err := temporary.Sync(); err != nil {
			temporary.Close()
			return err
		}
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, s.path)
}

// pruneUnderLock removes expired events and, if still above budget, drops oldest non-audit events.
func (s *FileSpool) pruneUnderLock() error {
	file, err := os.Open(s.path)
	if err != nil {
		return err
	}

	var events []Event
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err == nil {
			// Filter out expired events
			if time.Since(e.OccurredAt) <= s.config.MaxAge {
				events = append(events, e)
			}
		}
	}
	_ = file.Close()

	// If events still exceed max bytes, drop oldest non-audit events
	targetBytes := s.config.MaxBytes * 8 / 10 // target 80% capacity
	var filtered []Event
	var keptBytes int64
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		eBytes := int64(len(e.EventType) + len(e.Message) + 200) // approx JSON size
		if keptBytes+eBytes > targetBytes && e.Level != LevelAudit {
			// drop non-audit event
			continue
		}
		filtered = append([]Event{e}, filtered...)
		keptBytes += eBytes
	}

	temporaryPath := s.path + ".prune.tmp"
	tempFile, err := os.OpenFile(temporaryPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(tempFile)
	for _, e := range filtered {
		_ = encoder.Encode(e)
	}
	_ = tempFile.Close()
	return os.Rename(temporaryPath, s.path)
}

// Size returns current file size of spool file in bytes
func (s *FileSpool) Size() int64 {
	fi, err := os.Stat(s.path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// OldestAge returns age of oldest event in spool file
func (s *FileSpool) OldestAge() time.Duration {
	file, err := os.Open(s.path)
	if err != nil {
		return 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err == nil && !e.OccurredAt.IsZero() {
			return time.Since(e.OccurredAt)
		}
	}
	return 0
}
