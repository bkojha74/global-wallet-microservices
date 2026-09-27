//go:build integration

package ai

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestFraudDetector_LiveGemini requires GEMINI_API_KEY to be set in the environment.
func TestFraudDetector_LiveGemini(t *testing.T) {
	if os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("Skipping live AI test: GEMINI_API_KEY is not set")
	}

	client, err := NewClient(context.Background())
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client when API key is set")
	}
	defer client.Close()

	detector := NewFraudDetector(client)

	// Test Case: Safe, small transfer between known wallets
	t.Run("SafeTransfer", func(t *testing.T) {
		signals := FraudSignals{
			SourceWalletID:      "alice",
			DestinationWalletID: "bob",
			AmountUnits:         500, // $5.00
			Currency:            "USD",
			Region:              "us-east-1",
			Timestamp:           time.Now(),
			RecentTxnCount:      1,
			IsNewWalletPair:     false,
		}

		decision, err := detector.Score(context.Background(), signals)
		if err != nil {
			t.Fatalf("unexpected error during scoring: %v", err)
		}
		t.Logf("Safe transfer decision: %s (score: %.2f) reason: %s", decision.Decision, decision.RiskScore, decision.Reason)
		if decision.Decision != "ALLOW" {
			t.Errorf("expected ALLOW for safe transfer, got %s", decision.Decision)
		}
	})

	// Test Case: Suspicious transfer with high velocity
	t.Run("SuspiciousTransfer", func(t *testing.T) {
		signals := FraudSignals{
			SourceWalletID:      "alice",
			DestinationWalletID: "unknown-offshore",
			AmountUnits:         5000000, // $50,000
			Currency:            "USD",
			Region:              "us-east-1",
			Timestamp:           time.Now(),
			RecentTxnCount:      25, // very high velocity
			IsNewWalletPair:     true,
			CrossRegion:         true,
		}

		decision, err := detector.Score(context.Background(), signals)
		if err != nil {
			t.Fatalf("unexpected error during scoring: %v", err)
		}
		t.Logf("Suspicious transfer decision: %s (score: %.2f) reason: %s", decision.Decision, decision.RiskScore, decision.Reason)
		if decision.Decision == "ALLOW" {
			t.Errorf("expected FLAG or BLOCK for suspicious transfer, got ALLOW")
		}
	})
}
