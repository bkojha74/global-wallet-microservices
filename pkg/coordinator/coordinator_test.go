package coordinator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"wallet-system/pkg/db"
)

func TestMemoryFailoverCoordinatorDefaultAndSwitch(t *testing.T) {
	coord := NewMemoryFailoverCoordinator("")
	ctx := context.Background()

	target, err := coord.GetActiveTarget(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target != TargetPrimary {
		t.Fatalf("expected default %s, got %s", TargetPrimary, target)
	}

	// Switch to STANDBY
	if err := coord.SetActiveTarget(ctx, TargetStandby); err != nil {
		t.Fatalf("failed to set active target: %v", err)
	}

	target, err = coord.GetActiveTarget(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target != TargetStandby {
		t.Fatalf("expected %s, got %s", TargetStandby, target)
	}

	// Reject invalid target
	err = coord.SetActiveTarget(ctx, "INVALID_TARGET")
	if !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("expected ErrInvalidTarget, got %v", err)
	}
}

func TestMemoryFailoverCoordinatorConcurrency(t *testing.T) {
	coord := NewMemoryFailoverCoordinator(TargetPrimary)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			target := TargetPrimary
			if idx%2 == 0 {
				target = TargetStandby
			}
			_ = coord.SetActiveTarget(ctx, target)
			got, err := coord.GetActiveTarget(ctx)
			if err != nil {
				t.Errorf("unexpected error on get: %v", err)
			}
			if got != TargetPrimary && got != TargetStandby {
				t.Errorf("unexpected target: %s", got)
			}
		}(i)
	}
	wg.Wait()
}

func TestMongoFailoverCoordinatorLiveIfAvailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 2)
	if err != nil {
		t.Skipf("MongoDB not available for live coordinator test: %v", err)
		return
	}
	defer func() { _ = client.Disconnect(ctx) }()

	database := client.Database("test_banking_coord_db")
	_ = database.Drop(ctx)
	defer func() { _ = database.Drop(ctx) }()

	coord := NewMongoFailoverCoordinator(database, 100*time.Millisecond)

	// First get should initialize to PRIMARY
	target, err := coord.GetActiveTarget(ctx)
	if err != nil {
		t.Fatalf("expected successful get, got %v", err)
	}
	if target != TargetPrimary {
		t.Fatalf("expected %s, got %s", TargetPrimary, target)
	}

	// Switch to STANDBY
	if err := coord.SetActiveTarget(ctx, TargetStandby); err != nil {
		t.Fatalf("failed to set target to STANDBY: %v", err)
	}

	// Read back through cache
	target, err = coord.GetActiveTarget(ctx)
	if err != nil {
		t.Fatalf("unexpected get error: %v", err)
	}
	if target != TargetStandby {
		t.Fatalf("expected %s, got %s", TargetStandby, target)
	}

	// Create a second coordinator instance pointing to the same collection (simulating a 2nd gateway replica)
	// Wait for cache expiry to ensure reading from Mongo
	time.Sleep(150 * time.Millisecond)
	coord2 := NewMongoFailoverCoordinator(database, 100*time.Millisecond)
	target2, err := coord2.GetActiveTarget(ctx)
	if err != nil {
		t.Fatalf("coord2 failed to get active target: %v", err)
	}
	if target2 != TargetStandby {
		t.Fatalf("coord2 replica saw %s, expected %s from shared MongoDB", target2, TargetStandby)
	}
}
