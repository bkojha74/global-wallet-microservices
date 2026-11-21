package eventsourcing

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Supported banking event types for immutable event sourcing
const (
	EventAccountOpened = "AccountOpened"
	EventFundsCredited = "FundsCredited"
	EventFundsDebited  = "FundsDebited"
	EventFundsHeld     = "FundsHeld"
	EventFundsReleased = "FundsReleased"
)

var (
	ErrAccountNotFound       = errors.New("account not found in event store")
	ErrInvalidSequence       = errors.New("event sequence must be strictly monotonically increasing")
	ErrNegativeBalance       = errors.New("event would cause negative posted balance")
	ErrInsufficientAvailable = errors.New("insufficient available balance for hold/debit")
	ErrInvalidAmount         = errors.New("event amount must be greater than zero")
)

// Event represents an immutable, append-only financial mutation.
type Event struct {
	EventID     string            `json:"event_id"`
	AccountID   string            `json:"account_id"`
	Sequence    int64             `json:"sequence"`
	Type        string            `json:"type"`
	Amount      float64           `json:"amount"`
	Currency    string            `json:"currency"`
	HoldID      string            `json:"hold_id,omitempty"`
	Description string            `json:"description"`
	Timestamp   time.Time         `json:"timestamp"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// BalanceSnapshot is a point-in-time materialized view of an account's financial state.
type BalanceSnapshot struct {
	AccountID        string    `json:"account_id"`
	Currency         string    `json:"currency"`
	PostedBalance    float64   `json:"posted_balance"`
	HeldBalance      float64   `json:"held_balance"`
	AvailableBalance float64   `json:"available_balance"`
	LastSequence     int64     `json:"last_sequence"`
	AsOfTimestamp    time.Time `json:"as_of_timestamp"`
	EventCount       int       `json:"event_count"`
}

// EventStore manages append-only event streams and historical point-in-time auditing.
type EventStore struct {
	mu      sync.RWMutex
	streams map[string][]Event
}

// NewEventStore creates a thread-safe EventStore.
func NewEventStore() *EventStore {
	return &EventStore{
		streams: make(map[string][]Event),
	}
}

// AppendEvent writes an event to the account's immutable stream.
func (s *EventStore) AppendEvent(ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ev.AccountID = strings.TrimSpace(ev.AccountID)
	if ev.AccountID == "" {
		return errors.New("account ID cannot be empty")
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}

	stream := s.streams[ev.AccountID]
	expectedSeq := int64(len(stream) + 1)
	if ev.Sequence == 0 {
		ev.Sequence = expectedSeq
	} else if ev.Sequence != expectedSeq {
		return fmt.Errorf("%w: expected sequence %d, got %d", ErrInvalidSequence, expectedSeq, ev.Sequence)
	}

	s.streams[ev.AccountID] = append(stream, ev)
	return nil
}

// GetEvents returns all events for a given account.
func (s *EventStore) GetEvents(accountID string) ([]Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stream, exists := s.streams[accountID]
	if !exists {
		return nil, ErrAccountNotFound
	}
	result := make([]Event, len(stream))
	copy(result, stream)
	return result, nil
}

// ReconstructBalanceAt replays events up to a given timestamp to compute a verifiable point-in-time balance snapshot.
func (s *EventStore) ReconstructBalanceAt(accountID string, cutoff time.Time) (*BalanceSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stream, exists := s.streams[accountID]
	if !exists {
		return nil, ErrAccountNotFound
	}

	snap := &BalanceSnapshot{
		AccountID:     accountID,
		AsOfTimestamp: cutoff,
	}

	for _, ev := range stream {
		// Stop when reaching events strictly after the cutoff timestamp
		if !cutoff.IsZero() && ev.Timestamp.After(cutoff) {
			break
		}

		snap.Currency = ev.Currency
		snap.LastSequence = ev.Sequence
		snap.EventCount++

		switch ev.Type {
		case EventAccountOpened:
			snap.PostedBalance = ev.Amount
		case EventFundsCredited:
			snap.PostedBalance += ev.Amount
		case EventFundsDebited:
			snap.PostedBalance -= ev.Amount
		case EventFundsHeld:
			snap.HeldBalance += ev.Amount
		case EventFundsReleased:
			snap.HeldBalance -= ev.Amount
			if snap.HeldBalance < 0 {
				snap.HeldBalance = 0
			}
		}
	}

	snap.AvailableBalance = snap.PostedBalance - snap.HeldBalance
	return snap, nil
}

// GetCurrentBalance computes the latest snapshot by replaying all committed events.
func (s *EventStore) GetCurrentBalance(accountID string) (*BalanceSnapshot, error) {
	return s.ReconstructBalanceAt(accountID, time.Time{})
}
