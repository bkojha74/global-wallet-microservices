package db

import (
	"context"
	"strings"
	"testing"
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
