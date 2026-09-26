package main

import (
	"context"
	"testing"
	"time"

	"wallet-system/pkg/db"
	"wallet-system/pkg/observability"
)

func TestMongoLogRepositoryLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mongoURI := db.DefaultMongoURI()
	repo, err := NewMongoLogRepository(ctx, mongoURI, "test_logging_db", "test_events")
	if err != nil {
		t.Skipf("MongoDB unavailable: %v", err)
		return
	}
	defer func() { _ = repo.Close(ctx) }()

	_ = repo.collection.Drop(ctx)
	_ = repo.createIndexes(ctx)

	// 1. Save Event
	evt1 := observability.Event{
		SchemaVersion: 1,
		EventID:       "evt-repo-1",
		OccurredAt:    time.Now().UTC(),
		Service:       "wallet-service",
		Environment:   "test",
		Level:         observability.LevelInfo,
		EventType:     "wallet.test",
		TransactionID: "tx-repo-100",
		AssociationID: "assoc-repo-100",
	}

	if err := repo.Save(ctx, evt1); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// 2. Duplicate Save (idempotency: returns nil)
	if err := repo.Save(ctx, evt1); err != nil {
		t.Fatalf("Duplicate Save should return nil, got %v", err)
	}

	// 3. Find Events
	events, err := repo.Find(ctx, QueryFilter{
		TransactionID: "tx-repo-100",
		AssociationID: "assoc-repo-100",
		Service:       "wallet-service",
		Level:         observability.LevelInfo,
		Limit:         50,
	})
	if err != nil || len(events) != 1 {
		t.Fatalf("Find failed: %v, events count=%d", err, len(events))
	}

	// 4. Health
	if err := repo.Health(ctx); err != nil {
		t.Fatalf("Health failed: %v", err)
	}

	// 5. Retention
	deleted, err := repo.Retention(ctx, 30)
	if err != nil {
		t.Fatalf("Retention failed: %v, deleted=%d", err, deleted)
	}
}
