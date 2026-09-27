package ai

import (
	"context"
	"testing"
	"time"
)

// TestFraudDetector_NilClient verifies no panic and ALLOW returned when AI is off.
func TestFraudDetector_NilClient(t *testing.T) {
	detector := NewFraudDetector(nil)

	decision, err := detector.Score(context.Background(), FraudSignals{
		SourceWalletID:      "wallet-A",
		DestinationWalletID: "wallet-B",
		AmountUnits:         5000,
		Currency:            "USD",
		Timestamp:           time.Now(),
	})

	if err != nil {
		t.Fatalf("expected nil error when AI disabled, got: %v", err)
	}
	if decision.Decision != "ALLOW" {
		t.Errorf("expected ALLOW when AI disabled, got: %s", decision.Decision)
	}
}

// TestParseFraudDecision_Clean verifies standard JSON parsing.
func TestParseFraudDecision_Clean(t *testing.T) {
	raw := `{"risk_score":0.2,"decision":"ALLOW","reason":"Low risk transfer","confidence":0.9}`
	d, err := ParseFraudDecision(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Decision != "ALLOW" {
		t.Errorf("expected ALLOW, got %s", d.Decision)
	}
	if d.RiskScore != 0.2 {
		t.Errorf("expected risk_score 0.2, got %f", d.RiskScore)
	}
}

// TestParseFraudDecision_Block verifies BLOCK is parsed correctly.
func TestParseFraudDecision_Block(t *testing.T) {
	raw := `{"risk_score":0.91,"decision":"BLOCK","reason":"High velocity transfer to new wallet","confidence":0.95}`
	d, err := ParseFraudDecision(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Decision != "BLOCK" {
		t.Errorf("expected BLOCK, got %s", d.Decision)
	}
	if d.RiskScore < 0.9 {
		t.Errorf("expected risk_score >= 0.9, got %f", d.RiskScore)
	}
}

// TestParseFraudDecision_MarkdownFenced verifies markdown fence stripping.
func TestParseFraudDecision_MarkdownFenced(t *testing.T) {
	raw := "```json\n{\"risk_score\":0.85,\"decision\":\"BLOCK\",\"reason\":\"High velocity\",\"confidence\":0.95}\n```"
	d, err := ParseFraudDecision(raw)
	if err != nil {
		t.Fatalf("unexpected error parsing fenced JSON: %v", err)
	}
	if d.Decision != "BLOCK" {
		t.Errorf("expected BLOCK, got %s", d.Decision)
	}
}

// TestParseFraudDecision_MissingDecision verifies error on malformed response.
func TestParseFraudDecision_MissingDecision(t *testing.T) {
	raw := `{"risk_score":0.5,"reason":"something"}`
	_, err := ParseFraudDecision(raw)
	if err == nil {
		t.Error("expected error for missing decision field, got nil")
	}
}
