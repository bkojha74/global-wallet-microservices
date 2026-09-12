package observability

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type EventPublisher interface {
	Publish(context.Context, Event) error
	Close() error
}

type FileSpool struct {
	mu   sync.Mutex
	path string
}

func NewFileSpool(path string) *FileSpool {
	return &FileSpool{path: path}
}

func (s *FileSpool) Append(event Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(append(payload, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

func (s *FileSpool) Replay(ctx context.Context, publisher EventPublisher) error {
	s.mu.Lock()
	defer s.mu.Unlock()

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
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, s.path)
}
