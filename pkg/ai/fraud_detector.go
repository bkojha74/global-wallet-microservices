package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// FraudSignals contains the data extracted from a wallet transfer request
// and sent to Gemini for fraud analysis.
// Field names mirror WalletModel and TransferFundsRequest in cmd/wallet-service/main.go.
type FraudSignals struct {
	SourceWalletID      string    `json:"source_wallet_id"`
	DestinationWalletID string    `json:"destination_wallet_id"`
	AmountUnits         int64     `json:"amount_units"`       // smallest currency unit, e.g. cents
	Currency            string    `json:"currency"`
	Region              string    `json:"region"`
	Timestamp           time.Time `json:"timestamp"`
	RecentTxnCount      int       `json:"recent_txn_count"`   // transfers from source wallet in last 1h
	IsNewWalletPair     bool      `json:"is_new_wallet_pair"` // first time this src→dst pair transacts
	IsRoundNumber       bool      `json:"is_round_number"`    // amount divisible by 10,000
	CrossRegion         bool      `json:"cross_region"`       // source and destination in different regions
}

// FraudDecision is the structured verdict returned by Gemini.
type FraudDecision struct {
	RiskScore  float64 `json:"risk_score"`  // 0.0 = safe, 1.0 = definite fraud
	Decision   string  `json:"decision"`    // "ALLOW", "FLAG", or "BLOCK"
	Reason     string  `json:"reason"`      // one-sentence explanation
	Confidence float64 `json:"confidence"`  // model confidence 0.0–1.0
}

// FraudDetector scores wallet transfer requests using the Gemini AI model.
type FraudDetector struct {
	client    *Client
	threshold float64 // risk scores at or above this become at least FLAG
	enforce   bool    // when false, BLOCK is logged but downgraded to FLAG (audit-only)
}

// NewFraudDetector creates a FraudDetector.
// Returns nil when the AI client is unavailable — callers check IsEnabled().
func NewFraudDetector(client *Client) *FraudDetector {
	if !client.IsEnabled() {
		return nil
	}

	threshold := 0.7
	if v := os.Getenv("AI_FRAUD_THRESHOLD"); v != "" {
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil && f > 0 && f <= 1.0 {
			threshold = f
		}
	}

	enforce := os.Getenv("AI_FRAUD_ENFORCEMENT") != "false"

	return &FraudDetector{client: client, threshold: threshold, enforce: enforce}
}

// IsEnabled returns true when fraud detection is active.
func (f *FraudDetector) IsEnabled() bool {
	return f != nil && f.client.IsEnabled()
}

// Score evaluates a transfer and returns a risk decision.
// KEY SAFETY PROPERTY — fail-open: if Gemini is unavailable or returns
// unparseable output, the function returns ALLOW and logs the error.
// AI issues never block a valid payment.
func (f *FraudDetector) Score(ctx context.Context, s FraudSignals) (*FraudDecision, error) {
	if !f.IsEnabled() {
		return &FraudDecision{RiskScore: 0, Decision: "ALLOW", Reason: "AI not enabled"}, nil
	}

	raw, err := f.client.Generate(ctx, buildFraudPrompt(s))
	if err != nil {
		log.Printf("[AI-FRAUD] Gemini call failed (fail-open): %v", err)
		return &FraudDecision{RiskScore: 0, Decision: "ALLOW", Reason: "AI unavailable"}, nil
	}

	decision, err := ParseFraudDecision(raw)
	if err != nil {
		log.Printf("[AI-FRAUD] Response parse failed (fail-open): %v | raw=%q", err, raw)
		return &FraudDecision{RiskScore: 0, Decision: "ALLOW", Reason: "AI parse error"}, nil
	}

	// Upgrade ALLOW → FLAG when score exceeds our threshold
	if decision.RiskScore >= f.threshold && decision.Decision == "ALLOW" {
		decision.Decision = "FLAG"
	}

	// Audit-only mode: log but do not enforce BLOCK
	if !f.enforce && decision.Decision == "BLOCK" {
		log.Printf("[AI-FRAUD] Audit-only: would BLOCK src=%s reason=%s",
			s.SourceWalletID, decision.Reason)
		decision.Decision = "FLAG"
	}

	log.Printf("[AI-FRAUD] src=%s dst=%s amount=%d %s score=%.2f decision=%s",
		s.SourceWalletID, s.DestinationWalletID,
		s.AmountUnits, s.Currency,
		decision.RiskScore, decision.Decision)

	return decision, nil
}

// buildFraudPrompt constructs the text prompt sent to Gemini.
// Signals are embedded as JSON so Gemini receives structured data.
func buildFraudPrompt(s FraudSignals) string {
	signalsJSON, _ := json.MarshalIndent(s, "", "  ")
	return fmt.Sprintf(`You are a fraud detection AI for a digital wallet platform.
Analyse the following transaction signals and return a risk assessment.

Transaction signals:
%s

Risk guidelines:
- recent_txn_count > 10 in one hour = high velocity, elevated risk
- large amount_units on is_new_wallet_pair = elevated risk
- is_round_number combined with is_new_wallet_pair = moderate risk
- cross_region combined with high velocity = high risk
- small amounts under 1000 units on known pairs = low risk

Return ONLY valid JSON. No markdown fences. No explanation outside the JSON.

{
  "risk_score": <float 0.0 to 1.0>,
  "decision": "<ALLOW or FLAG or BLOCK>",
  "reason": "<one sentence>",
  "confidence": <float 0.0 to 1.0>
}

Thresholds:
  risk_score < 0.4  → ALLOW
  risk_score < 0.7  → FLAG
  risk_score >= 0.7 → BLOCK`, string(signalsJSON))
}

// ParseFraudDecision extracts the FraudDecision JSON from Gemini's response.
// Exported so it can be used in tests. Strips markdown fences if present.
func ParseFraudDecision(raw string) (*FraudDecision, error) {
	raw = strings.TrimSpace(raw)

	// Gemini sometimes wraps output in ```json ... ``` fences — strip them
	if strings.HasPrefix(raw, "```") {
		lines := strings.Split(raw, "\n")
		if len(lines) >= 3 {
			raw = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	var d FraudDecision
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, fmt.Errorf("json.Unmarshal: %w | input: %q", err, raw)
	}
	if d.Decision == "" {
		return nil, fmt.Errorf("response missing required 'decision' field")
	}
	return &d, nil
}
