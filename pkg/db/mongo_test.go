package db

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestConnectWithRetryRejectsNonPositiveRetryCount(t *testing.T) {
	client, err := ConnectWithRetry(context.Background(), "mongodb://127.0.0.1:27017", 0)

	if client != nil {
		t.Fatalf("expected no client, got %v", client)
	}
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "maxRetries must be greater than zero") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDefaultMongoURI(t *testing.T) {
	t.Setenv("MONGO_URI", "mongodb://custom-mongo:27017")
	if uri := DefaultMongoURI(); uri != "mongodb://custom-mongo:27017" {
		t.Fatalf("expected custom uri, got %s", uri)
	}

	t.Setenv("MONGO_URI", "")
	uriFallback := DefaultMongoURI()
	if uriFallback == "" {
		t.Fatal("expected non-empty default MONGO_URI")
	}
}

func TestConnectWithRetryAndEnsureIndexes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := ConnectWithRetry(ctx, DefaultMongoURI(), 1)
	if err != nil {
		t.Skipf("MongoDB not available: %v", err)
		return
	}
	defer client.Disconnect(ctx)

	testDB := client.Database("test_db_indexes_pkg")
	_ = testDB.Drop(ctx)

	if err := EnsureLedgerIndexes(ctx, testDB); err != nil {
		t.Fatalf("EnsureLedgerIndexes failed: %v", err)
	}

	if err := EnsureWalletIndexes(ctx, testDB); err != nil {
		t.Fatalf("EnsureWalletIndexes failed: %v", err)
	}
}
