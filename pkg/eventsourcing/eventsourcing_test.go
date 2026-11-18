package eventsourcing

import (
	"testing"
	"time"
)

func TestEventStore_FullLifecycleAndPointInTimeAudit(t *testing.T) {
	store := NewEventStore()
	accountID := "acct-corporate-audit-101"
	currency := "USD"

	baseTime := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

	// Event 1: Account Opened with $100 initial balance at T0
	t0 := baseTime
	err := store.AppendEvent(Event{
		EventID:     "ev-01",
		AccountID:   accountID,
		Sequence:    1,
		Type:        EventAccountOpened,
		Amount:      100.00,
		Currency:    currency,
		Description: "Account creation",
		Timestamp:   t0,
	})
	if err != nil {
		t.Fatalf("AppendEvent 1 failed: %v", err)
	}

	// Event 2: Credit of $50.00 at T0 + 1 hour
	t1 := baseTime.Add(1 * time.Hour)
	err = store.AppendEvent(Event{
		EventID:     "ev-02",
		AccountID:   accountID,
		Sequence:    2,
		Type:        EventFundsCredited,
		Amount:      50.00,
		Currency:    currency,
		Description: "Client incoming wire",
		Timestamp:   t1,
	})
	if err != nil {
		t.Fatalf("AppendEvent 2 failed: %v", err)
	}

	// Event 3: Authorisation Hold of $30.00 at T0 + 2 hours
	t2 := baseTime.Add(2 * time.Hour)
	err = store.AppendEvent(Event{
		EventID:     "ev-03",
		AccountID:   accountID,
		Sequence:    3,
		Type:        EventFundsHeld,
		Amount:      30.00,
		Currency:    currency,
		HoldID:      "hold-auth-99",
		Description: "Card authorization hold",
		Timestamp:   t2,
	})
	if err != nil {
		t.Fatalf("AppendEvent 3 failed: %v", err)
	}

	// Event 4: Debit of $20.00 at T0 + 3 hours
	t3 := baseTime.Add(3 * time.Hour)
	err = store.AppendEvent(Event{
		EventID:     "ev-04",
		AccountID:   accountID,
		Sequence:    4,
		Type:        EventFundsDebited,
		Amount:      20.00,
		Currency:    currency,
		Description: "Vendor payment fee",
		Timestamp:   t3,
	})
	if err != nil {
		t.Fatalf("AppendEvent 4 failed: %v", err)
	}

	// Event 5: Release of Hold of $30.00 at T0 + 4 hours
	t4 := baseTime.Add(4 * time.Hour)
	err = store.AppendEvent(Event{
		EventID:     "ev-05",
		AccountID:   accountID,
		Sequence:    5,
		Type:        EventFundsReleased,
		Amount:      30.00,
		Currency:    currency,
		HoldID:      "hold-auth-99",
		Description: "Release card authorization hold",
		Timestamp:   t4,
	})
	if err != nil {
		t.Fatalf("AppendEvent 5 failed: %v", err)
	}

	// === Point-in-Time Auditing Assertions ===

	// 1. Audit at T0 (Account Opening): Posted: 100, Held: 0, Available: 100
	snapT0, err := store.ReconstructBalanceAt(accountID, t0.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("ReconstructBalanceAt T0 failed: %v", err)
	}
	if snapT0.PostedBalance != 100.00 || snapT0.AvailableBalance != 100.00 || snapT0.EventCount != 1 {
		t.Errorf("T0 snapshot mismatch: %+v", snapT0)
	}

	// 2. Audit at T1 (After Credit): Posted: 150, Held: 0, Available: 150
	snapT1, err := store.ReconstructBalanceAt(accountID, t1.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("ReconstructBalanceAt T1 failed: %v", err)
	}
	if snapT1.PostedBalance != 150.00 || snapT1.AvailableBalance != 150.00 || snapT1.EventCount != 2 {
		t.Errorf("T1 snapshot mismatch: %+v", snapT1)
	}

	// 3. Audit at T2 (After Hold): Posted: 150, Held: 30, Available: 120
	snapT2, err := store.ReconstructBalanceAt(accountID, t2.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("ReconstructBalanceAt T2 failed: %v", err)
	}
	if snapT2.PostedBalance != 150.00 || snapT2.HeldBalance != 30.00 || snapT2.AvailableBalance != 120.00 || snapT2.EventCount != 3 {
		t.Errorf("T2 snapshot mismatch: %+v", snapT2)
	}

	// 4. Audit at T3 (After Debit): Posted: 130, Held: 30, Available: 100
	snapT3, err := store.ReconstructBalanceAt(accountID, t3.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("ReconstructBalanceAt T3 failed: %v", err)
	}
	if snapT3.PostedBalance != 130.00 || snapT3.HeldBalance != 30.00 || snapT3.AvailableBalance != 100.00 || snapT3.EventCount != 4 {
		t.Errorf("T3 snapshot mismatch: %+v", snapT3)
	}

	// 5. Current Balance (After Hold Released): Posted: 130, Held: 0, Available: 130
	snapCurrent, err := store.GetCurrentBalance(accountID)
	if err != nil {
		t.Fatalf("GetCurrentBalance failed: %v", err)
	}
	if snapCurrent.PostedBalance != 130.00 || snapCurrent.HeldBalance != 0.00 || snapCurrent.AvailableBalance != 130.00 || snapCurrent.EventCount != 5 {
		t.Errorf("Current snapshot mismatch: %+v", snapCurrent)
	}
}

func TestEventStore_SequenceValidation(t *testing.T) {
	store := NewEventStore()
	accountID := "acct-seq-check"

	// Event 1 with valid sequence 1
	err := store.AppendEvent(Event{AccountID: accountID, Sequence: 1, Type: EventAccountOpened, Amount: 10})
	if err != nil {
		t.Fatalf("AppendEvent failed: %v", err)
	}

	// Event with gap in sequence (expected 2, got 4) must fail
	err = store.AppendEvent(Event{AccountID: accountID, Sequence: 4, Type: EventFundsCredited, Amount: 5})
	if err == nil {
		t.Errorf("expected sequence mismatch error, got nil")
	}

	// Non-existent account
	_, err = store.GetEvents("non-existent")
	if err != ErrAccountNotFound {
		t.Errorf("expected ErrAccountNotFound, got: %v", err)
	}
}
