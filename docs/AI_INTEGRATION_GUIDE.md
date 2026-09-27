# AI Integration Guide — Google Gemini in Global Wallet Microservices

This document explains, step by step, how to get a Google Gemini AI API key,
understand what it is, add it to this project, and verify everything works.
No prior AI experience is needed.

---

## Table of Contents

1. [What is an API Key and Why Do You Need One?](#1-what-is-an-api-key-and-why-do-you-need-one)
2. [What is Google Gemini?](#2-what-is-google-gemini)
3. [Step 1 — Create a Google Account (if you do not have one)](#step-1--create-a-google-account)
4. [Step 2 — Go to Google AI Studio](#step-2--go-to-google-ai-studio)
5. [Step 3 — Generate Your API Key](#step-3--generate-your-api-key)
6. [Step 4 — Understand the Free Tier Limits](#step-4--understand-the-free-tier-limits)
7. [Step 5 — Store the Key Safely in This Project](#step-5--store-the-key-safely-in-this-project)
8. [Step 6 — Add the Gemini SDK to the Go Project](#step-6--add-the-gemini-sdk-to-the-go-project)
9. [Step 7 — Write the AI Client Package](#step-7--write-the-ai-client-package)
10. [Step 8 — Write the Fraud Detector](#step-8--write-the-fraud-detector)
11. [Step 9 — Wire Fraud Detection Into the Wallet Service](#step-9--wire-fraud-detection-into-the-wallet-service)
12. [Step 10 — Test the Integration](#step-10--test-the-integration)
13. [Step 11 — Add the Key to GitHub Secrets for CI/CD](#step-11--add-the-key-to-github-secrets-for-cicd)
14. [Step 12 — Run a Live Demo](#step-12--run-a-live-demo)
15. [Troubleshooting](#troubleshooting)
16. [Security Rules — What Never to Do](#security-rules--what-never-to-do)
17. [Summary of Files Changed](#summary-of-files-changed)

---

## 1. What is an API Key and Why Do You Need One?

An **API key** is like a password that identifies you when your application
talks to an external service. When your Go code sends a transfer request to
Google's Gemini AI service, it must include this key so Google knows who is
calling and can apply usage limits or billing to your account.

Think of it like a hotel key card:

- The card does not contain your name or money.
- It just proves you are allowed inside.
- If someone else gets your card, they can use your access quota.
- You can deactivate it and create a replacement at any time.

For this project the key will be stored in a file called `.env` on your local
machine. That file is already in `.gitignore` so it is never uploaded to GitHub.

---

## 2. What is Google Gemini?

Google Gemini is a Large Language Model (LLM) — the same category of AI as
ChatGPT. You send it a text prompt and it returns a text response.

In this project we send Gemini a description of a financial transaction
(wallet IDs, amount, velocity patterns) and ask it to return a fraud risk
score as a JSON object. Gemini reads the description and reasons about whether
the transaction looks suspicious.

Here is how the data flows:

```
Your Go code                          Google's servers
────────────                          ────────────────

wallet transfer arrives
        │
        ▼
build a text prompt:
"Transaction: 500000 cents from
 wallet-alice to unknown-offshore.
 20 transfers in the last hour.
 Is this fraud? Return JSON."
        │
        │   HTTPS request (API key in Authorization header)
        ├──────────────────────────────────────────────────► Gemini model
        │                                                          │
        │◄─────────────────────────────────────────────────── JSON response:
        │                                                    {
        │                                                      "risk_score": 0.91,
        │                                                      "decision": "BLOCK",
        │                                                      "reason": "High velocity"
        │                                                    }
        ▼
parse the JSON
block or allow the transfer
```

---

## Step 1 — Create a Google Account

If you already have a Gmail or Google Workspace account, skip this step.

1. Open your browser and go to `https://accounts.google.com/signup`
2. Fill in your first name, last name, choose a Gmail address, and set a password.
3. Complete the phone number verification (Google sends an SMS code).
4. Accept the terms of service.

You now have a Google account. Keep the email address and password saved in
your password manager.

---

## Step 2 — Go to Google AI Studio

Google AI Studio is the web interface where you manage Gemini API keys and
can test the model interactively.

1. Open your browser.
2. Navigate to: **https://aistudio.google.com**
3. Click **Sign in** in the top-right corner of the page.
4. Sign in with the Google account from Step 1.
5. If a terms of service page appears, read it and click **Accept**.

You will see a dashboard with a chat-like interface. You do not need to use the
chat. We only need the **API Keys** section, covered in the next step.

---

## Step 3 — Generate Your API Key

1. While logged in to Google AI Studio, look at the left-hand sidebar.
   You will see a menu item called **"Get API key"** or navigate directly to:

   **https://aistudio.google.com/app/apikey**

2. The page shows a list of your existing keys (empty for a new account).

3. Click the blue button **"Create API key"**.

4. A dialog box appears with two radio button options:

   - **"Create API key in new project"**
     Choose this if you are new to Google Cloud. It automatically creates a
     free Google Cloud project and links the key to it.

   - **"Create API key in existing project"**
     Choose this only if you already use Google Cloud Console and have an
     existing project you want to use.

   **For most developers: choose "Create API key in new project".**

5. Click the **"Create API key"** button inside the dialog.

6. After a few seconds, your new API key appears. It looks like this:

   ```
   AIzaSyD-9tSrke72WhHqCPbXXXXXXXXXXXXXXX
   ```

   The key is always 39 characters long and starts with `AIzaSy`.

7. Click the **copy icon** (two overlapping squares) next to the key.

8. **Immediately** paste it into a safe location before closing this page:
   - A password manager (1Password, Bitwarden, etc.) — recommended
   - A text file on your Desktop temporarily
   - A secure note in your phone

   > If you close the page without saving the key, it is not lost. You can
   > return to the same page and the key will still be listed. You can also
   > create a new key any time.

---

## Step 4 — Understand the Free Tier Limits

You do **not** need to enter a credit card to use Gemini at the free tier.
The free tier is more than enough for development and recruiter demonstrations.

| Model | Free requests per minute | Free tokens per day |
|-------|--------------------------|---------------------|
| Gemini 1.5 Flash | 15 | 1,000,000 |
| Gemini 1.5 Pro   | 2  | 50,000    |

**This project uses Gemini 1.5 Flash** because it is fast, free, and accurate
enough for fraud scoring.

A typical fraud check in this project uses about 300 tokens (the prompt plus
the response). At 1,000,000 free tokens per day you could score roughly 3,000
transactions per day for free.

During a recruiter demonstration you will run about 10 transactions. You will
never approach the limit.

---

## Step 5 — Store the Key Safely in This Project

### 5.1 Understand the .env file

This project follows a standard twelve-factor app pattern: all configuration
comes from environment variables. There is a template file called `.env.example`
that shows which variables are available, with placeholder values. Your actual
secrets go in a file called `.env` which is **never committed to git** because
it appears in `.gitignore`.

### 5.2 Create your local .env file

Open a PowerShell terminal in the project root:

```powershell
cd c:\workarea\personal\After-equifax\global-wallet-microservices
```

Copy the example file to create your local `.env`:

```powershell
Copy-Item .env.example .env
```

### 5.3 Add the AI variables to .env.example

Open `.env.example` in VS Code and add the following block after the existing
`SONAR_TOKEN` section at the bottom:

```bash
# ------------------------------------------------------------------------------
# AI / Machine Learning — Google Gemini Integration (Phase AI-01)
# ------------------------------------------------------------------------------
# Obtain your free API key from: https://aistudio.google.com/app/apikey
# IMPORTANT: Put your real key only in .env — never in this file.
GEMINI_API_KEY=your-gemini-api-key-here

# Gemini model name. Flash is free and fast; Pro is smarter but rate-limited.
GEMINI_MODEL=gemini-1.5-flash

# Risk score threshold (0.0 to 1.0). Transfers scored ABOVE this are blocked.
AI_FRAUD_THRESHOLD=0.7

# Maximum milliseconds to wait for a Gemini response before timing out.
AI_INFERENCE_TIMEOUT_MS=5000

# Set to false during development to log AI decisions without blocking transfers.
# Change to true when you are ready to enforce blocks.
AI_FRAUD_ENFORCEMENT=false
```

### 5.4 Add your real key to .env

Open `.env` in VS Code and find the AI section you just added. Replace the
placeholder with your actual key:

```bash
GEMINI_API_KEY=AIzaSyD-9tSrke72WhHqCPbXXXXXXXXXXXXXXX
GEMINI_MODEL=gemini-1.5-flash
AI_FRAUD_THRESHOLD=0.7
AI_INFERENCE_TIMEOUT_MS=5000
AI_FRAUD_ENFORCEMENT=false
```

Save the file.

### 5.5 Verify the key will not be committed

Run this command. It should produce no output (empty = git is ignoring the file):

```powershell
git status --short .env
```

If `.env` appears in the output, run the following to remove it from tracking:

```powershell
git rm --cached .env
git commit -m "chore: stop tracking .env"
```

---

## Step 6 — Add the Gemini SDK to the Go Project

The Gemini SDK is a Go library that handles all the HTTPS communication,
authentication, retry logic, and JSON serialisation when talking to Google's
servers. You add it exactly like any other Go dependency.

Open a PowerShell terminal in the project root and run:

```powershell
go get github.com/google/generative-ai-go@latest
go get google.golang.org/api/option
go mod tidy
```

You will see output similar to:

```
go: added github.com/google/generative-ai-go v0.19.0
go: added google.golang.org/api v0.215.0
```

Confirm the dependency was added to `go.mod`:

```powershell
Select-String "generative-ai-go" go.mod
```

You should see one matching line, for example:

```
	github.com/google/generative-ai-go v0.19.0
```

---

## Step 7 — Write the AI Client Package

Create the package directory:

```powershell
New-Item -ItemType Directory -Path pkg\ai -Force
```

Create the file `pkg\ai\client.go` with the content below.

This file creates a wrapper around the Gemini SDK. The important design choice
is that when `GEMINI_API_KEY` is not set, `NewClient` returns `nil` instead of
an error. This means every service can call `NewClient` and if the key is not
configured, the AI is simply off — no crashes, no failures, just normal operation.

```go
package ai

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"
)

// Client wraps the Gemini AI client for this project.
// When GEMINI_API_KEY is not set, Client is nil and all AI features
// are disabled automatically — the rest of the system is unaffected.
type Client struct {
	inner   *genai.Client
	model   string
	timeout time.Duration
}

// NewClient reads GEMINI_API_KEY from the environment and creates a client.
// Returns nil (not an error) when the key is absent — AI is disabled gracefully.
func NewClient(ctx context.Context) (*Client, error) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		return nil, nil // AI disabled — this is intentional, not an error
	}

	model := os.Getenv("GEMINI_MODEL")
	if model == "" {
		model = "gemini-1.5-flash"
	}

	timeoutMS := 5000
	if v := os.Getenv("AI_INFERENCE_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeoutMS = n
		}
	}

	inner, err := genai.NewClient(ctx, option.WithAPIKey(key))
	if err != nil {
		return nil, fmt.Errorf("ai.NewClient: %w", err)
	}

	return &Client{
		inner:   inner,
		model:   model,
		timeout: time.Duration(timeoutMS) * time.Millisecond,
	}, nil
}

// IsEnabled returns true when the client has a valid Gemini connection.
// Always check this before calling any AI methods.
func (c *Client) IsEnabled() bool {
	return c != nil && c.inner != nil
}

// Close releases the underlying HTTP connection pool. Safe to call on nil.
func (c *Client) Close() error {
	if c == nil || c.inner == nil {
		return nil
	}
	return c.inner.Close()
}

// generate sends a plain-text prompt to Gemini and returns the response text.
// It applies the client's own timeout, so callers must not add another timeout.
func (c *Client) generate(ctx context.Context, prompt string) (string, error) {
	tctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	m := c.inner.GenerativeModel(c.model)
	m.SetTemperature(0.1) // low temperature = consistent, deterministic JSON output

	resp, err := m.GenerateContent(tctx, genai.Text(prompt))
	if err != nil {
		return "", fmt.Errorf("GenerateContent: %w", err)
	}
	if len(resp.Candidates) == 0 {
		return "", fmt.Errorf("Gemini returned an empty response")
	}

	part := resp.Candidates[0].Content.Parts[0]
	text, ok := part.(genai.Text)
	if !ok {
		return "", fmt.Errorf("unexpected Gemini response type: %T", part)
	}
	return string(text), nil
}
```

---

## Step 8 — Write the Fraud Detector

Create `pkg\ai\fraud_detector.go` with the content below.

The key safety property is **fail-open**: if Gemini is unavailable or returns
garbage, the transfer is allowed and the error is only logged. AI issues never
block a valid payment.

```go
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

// FraudSignals contains the information extracted from a transfer request
// to send to Gemini for fraud analysis. Field names match the existing
// WalletModel and TransferFundsRequest in cmd/wallet-service/main.go.
type FraudSignals struct {
	SourceWalletID      string    `json:"source_wallet_id"`
	DestinationWalletID string    `json:"destination_wallet_id"`
	AmountUnits         int64     `json:"amount_units"`       // e.g. cents
	Currency            string    `json:"currency"`
	Region              string    `json:"region"`
	Timestamp           time.Time `json:"timestamp"`
	RecentTxnCount      int       `json:"recent_txn_count"`   // transfers from source wallet in last 1h
	IsNewWalletPair     bool      `json:"is_new_wallet_pair"` // first time this src->dst pair transfers
	IsRoundNumber       bool      `json:"is_round_number"`    // amount evenly divisible by 10,000
	CrossRegion         bool      `json:"cross_region"`       // source and destination in different regions
}

// FraudDecision is the structured verdict that Gemini returns.
type FraudDecision struct {
	RiskScore  float64 `json:"risk_score"`  // 0.0 = completely safe, 1.0 = definite fraud
	Decision   string  `json:"decision"`    // "ALLOW", "FLAG", or "BLOCK"
	Reason     string  `json:"reason"`      // one sentence explaining the decision
	Confidence float64 `json:"confidence"`  // how confident the model is (0.0 to 1.0)
}

// FraudDetector scores wallet transfer requests using the Gemini AI model.
type FraudDetector struct {
	client    *Client
	threshold float64 // scores at or above this become at least FLAG
	enforce   bool    // when false, BLOCK is logged but downgraded to FLAG
}

// NewFraudDetector creates a FraudDetector.
// Returns nil when the AI client is not available — callers check IsEnabled().
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
// On any AI error it returns ALLOW so transfers are never blocked by an AI outage.
// This "fail-open" behaviour is intentional and is the safest default for payments.
func (f *FraudDetector) Score(ctx context.Context, s FraudSignals) (*FraudDecision, error) {
	if !f.IsEnabled() {
		return &FraudDecision{RiskScore: 0, Decision: "ALLOW", Reason: "AI not enabled"}, nil
	}

	raw, err := f.client.generate(ctx, buildFraudPrompt(s))
	if err != nil {
		log.Printf("[AI-FRAUD] Gemini call failed (fail-open): %v", err)
		return &FraudDecision{RiskScore: 0, Decision: "ALLOW", Reason: "AI unavailable"}, nil
	}

	decision, err := parseFraudDecision(raw)
	if err != nil {
		log.Printf("[AI-FRAUD] Response parse failed (fail-open): %v | raw=%q", err, raw)
		return &FraudDecision{RiskScore: 0, Decision: "ALLOW", Reason: "AI parse error"}, nil
	}

	// Apply our threshold: if AI said ALLOW but score is high, upgrade to FLAG
	if decision.RiskScore >= f.threshold && decision.Decision == "ALLOW" {
		decision.Decision = "FLAG"
	}

	// Respect enforcement setting: downgrade BLOCK to FLAG in audit-only mode
	if !f.enforce && decision.Decision == "BLOCK" {
		log.Printf("[AI-FRAUD] Audit-only mode: would block src=%s reason=%s",
			s.SourceWalletID, decision.Reason)
		decision.Decision = "FLAG"
	}

	log.Printf("[AI-FRAUD] src=%s dst=%s amount=%d currency=%s score=%.2f decision=%s",
		s.SourceWalletID, s.DestinationWalletID,
		s.AmountUnits, s.Currency,
		decision.RiskScore, decision.Decision)

	return decision, nil
}

// buildFraudPrompt constructs the text prompt sent to Gemini.
// The prompt includes the transaction signals as JSON and asks for
// a JSON response — this makes parsing reliable.
func buildFraudPrompt(s FraudSignals) string {
	signalsJSON, _ := json.MarshalIndent(s, "", "  ")
	return fmt.Sprintf(`You are a fraud detection AI for a digital wallet platform.
Analyse the following transaction signals and return a risk assessment.

Transaction signals:
%s

Risk guidelines:
- recent_txn_count > 10 in one hour means high velocity — elevated risk
- large amount_units on is_new_wallet_pair — elevated risk
- is_round_number combined with is_new_wallet_pair — moderate risk
- cross_region combined with high velocity — high risk
- small amounts under 1000 units on known pairs — low risk

Return ONLY valid JSON. Do not include markdown. Do not add any explanation
outside the JSON object.

{
  "risk_score": <float 0.0 to 1.0>,
  "decision": "<ALLOW or FLAG or BLOCK>",
  "reason": "<one sentence>",
  "confidence": <float 0.0 to 1.0>
}

Thresholds to follow:
  risk_score below 0.4  → decision must be ALLOW
  risk_score 0.4 to 0.7 → decision must be FLAG
  risk_score above 0.7  → decision must be BLOCK`, string(signalsJSON))
}

// parseFraudDecision extracts the JSON object from Gemini's response text.
// Gemini sometimes wraps the JSON in markdown fences (```json ... ```).
// This function strips those fences before unmarshalling.
func parseFraudDecision(raw string) (*FraudDecision, error) {
	raw = strings.TrimSpace(raw)

	// Strip markdown code fences if present
	if strings.HasPrefix(raw, "```") {
		lines := strings.Split(raw, "\n")
		if len(lines) >= 3 {
			raw = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	var d FraudDecision
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, fmt.Errorf("json.Unmarshal: %w (received: %q)", err, raw)
	}
	if d.Decision == "" {
		return nil, fmt.Errorf("Gemini response missing required 'decision' field")
	}
	return &d, nil
}
```

Also create `pkg\ai\fraud_detector_test.go`:

```go
package ai

import (
	"context"
	"testing"
	"time"
)

// TestFraudDetector_Disabled ensures no panics and ALLOW is returned when AI is off.
func TestFraudDetector_Disabled(t *testing.T) {
	detector := NewFraudDetector(nil) // nil client = AI disabled

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

// TestParseFraudDecision_Clean tests clean JSON parsing.
func TestParseFraudDecision_Clean(t *testing.T) {
	raw := `{"risk_score":0.2,"decision":"ALLOW","reason":"Low risk transfer","confidence":0.9}`
	d, err := parseFraudDecision(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Decision != "ALLOW" {
		t.Errorf("expected ALLOW, got %s", d.Decision)
	}
	if d.RiskScore != 0.2 {
		t.Errorf("expected 0.2, got %f", d.RiskScore)
	}
}

// TestParseFraudDecision_MarkdownFenced tests stripping of markdown code fences.
func TestParseFraudDecision_MarkdownFenced(t *testing.T) {
	raw := "```json\n{\"risk_score\":0.85,\"decision\":\"BLOCK\",\"reason\":\"High velocity\",\"confidence\":0.95}\n```"
	d, err := parseFraudDecision(raw)
	if err != nil {
		t.Fatalf("unexpected error parsing fenced JSON: %v", err)
	}
	if d.Decision != "BLOCK" {
		t.Errorf("expected BLOCK, got %s", d.Decision)
	}
}
```

---

## Step 9 — Wire Fraud Detection Into the Wallet Service

### 9.1 Add the import

At the top of `cmd/wallet-service/main.go`, add the new import to the existing
import block:

```go
import (
    // ... all existing imports ...
    "wallet-system/pkg/ai"
)
```

### 9.2 Add fields to the server struct

Find the `server` struct (around line 39). Add two fields at the bottom:

```go
type server struct {
    walletv1.UnimplementedWalletServiceServer
    mongoClient  *mongo.Client
    ledgerClient ledgerv1.LedgerServiceClient
    region       string
    isActive     bool
    targetRole   string
    coordinator  coordinator.FailoverCoordinator
    logger       observability.Logger
    dbName       string
    outbox       *observability.MongoOutbox
    ledgerRelay  *LedgerRelay
    // ── AI Integration (Phase AI-01) ─────────────────────────────────────
    aiClient      *ai.Client
    fraudDetector *ai.FraudDetector
}
```

### 9.3 Initialise AI in the startup function

Find the function that builds the `server` struct (inside `runWalletServer` or
`main`). Add the following code after the existing client setup and before the
`srv` variable is declared:

```go
// ── AI Client initialisation ──────────────────────────────────────────────
aiClient, aiErr := ai.NewClient(ctx)
if aiErr != nil {
    log.Printf("[WALLET] AI init warning: %v (AI features disabled)", aiErr)
}
if aiClient != nil && aiClient.IsEnabled() {
    log.Printf("[WALLET] AI fraud detection: ENABLED (model=%s enforcement=%s)",
        os.Getenv("GEMINI_MODEL"), os.Getenv("AI_FRAUD_ENFORCEMENT"))
    defer aiClient.Close()
} else {
    log.Printf("[WALLET] AI fraud detection: DISABLED (set GEMINI_API_KEY to enable)")
}
// ── End AI Client initialisation ─────────────────────────────────────────

srv := &server{
    // ... all your existing fields ...
    aiClient:      aiClient,
    fraudDetector: ai.NewFraudDetector(aiClient),
}
```

### 9.4 Add the fraud check inside validateTransferRequest

Find the `validateTransferRequest` function (around line 248 in
`cmd/wallet-service/main.go`). Add the following block immediately before the
final `return nil, nil` at the end of the function:

```go
// ── AI Fraud Detection (Phase AI-01) ─────────────────────────────────────
if s.fraudDetector != nil && s.fraudDetector.IsEnabled() {
    signals := ai.FraudSignals{
        SourceWalletID:      req.SourceWalletId,
        DestinationWalletID: req.DestinationWalletId,
        AmountUnits:         req.Amount.Units,
        Currency:            req.Amount.Currency,
        Region:              s.region,
        Timestamp:           time.Now().UTC(),
        IsRoundNumber:       req.Amount.Units%10000 == 0,
    }

    decision, _ := s.fraudDetector.Score(ctx, signals) // errors always fail-open

    // Emit the decision into the existing observability pipeline
    s.emit(ctx, "wallet.transfer.fraud_scored", observability.LevelInfo,
        "AI fraud score computed", map[string]any{
            "risk_score": decision.RiskScore,
            "decision":   decision.Decision,
            "reason":     decision.Reason,
        })

    if decision.Decision == "BLOCK" {
        durationMS := time.Since(startTime).Milliseconds()
        s.emitTerminal(ctx, eventWalletTransferFailed, observability.LevelWarn,
            "Transfer blocked by AI fraud detection", durationMS, false, map[string]any{
                "risk_score": decision.RiskScore,
                "reason":     decision.Reason,
            })
        return &walletv1.TransferFundsResponse{
            Status:          walletv1.TransferFundsResponse_INTERNAL_ERROR,
            ErrorMessage:    "Transfer blocked: " + decision.Reason,
            HandledByRegion: s.region,
        }, status.Errorf(codes.PermissionDenied, "blocked by fraud detection: %s", decision.Reason)
    }
}
// ── End AI Fraud Detection ────────────────────────────────────────────────

return nil, nil
```

---

## Step 10 — Test the Integration

### 10.1 Build the whole project

```powershell
go build ./cmd/...
```

If you see `cannot find package "wallet-system/pkg/ai"` make sure you saved
both files in `pkg\ai\`.

If you see `cannot find package "github.com/google/generative-ai-go/genai"`
run `go get github.com/google/generative-ai-go@latest` again.

### 10.2 Run unit tests — no API key required

```powershell
go test -race -v ./pkg/ai/...
```

Expected output:

```
=== RUN   TestFraudDetector_Disabled
--- PASS: TestFraudDetector_Disabled (0.00s)
=== RUN   TestParseFraudDecision_Clean
--- PASS: TestParseFraudDecision_Clean (0.00s)
=== RUN   TestParseFraudDecision_MarkdownFenced
--- PASS: TestParseFraudDecision_MarkdownFenced (0.00s)
PASS
ok      wallet-system/pkg/ai    0.123s
```

### 10.3 Load your environment and do a quick smoke test

```powershell
# Load all variables from .env into your current PowerShell session
Get-Content .env | ForEach-Object {
    if ($_ -match '^\s*([^#][^=]*)=(.*)$') {
        [System.Environment]::SetEnvironmentVariable($matches[1].Trim(), $matches[2].Trim(), 'Process')
    }
}

# Confirm the key is loaded
Write-Host "Key starts with: $($env:GEMINI_API_KEY.Substring(0,10))..."
```

### 10.4 Run the full test suite

```powershell
go test -race -coverprofile=coverage.out ./...
```

This runs all existing tests plus the new AI unit tests. The AI tests do not
call Gemini (they use nil clients), so the full suite passes without an API key.

---

## Step 11 — Add the Key to GitHub Secrets for CI/CD

Your `.env` file never goes to GitHub. For the CI/CD pipeline defined in
`.github/workflows/ci.yml` to use the key during automated integration tests,
you must store it as a GitHub Actions secret.

### 11.1 Open your repository

Go to: `https://github.com/bkojha74/global-wallet-microservices`

### 11.2 Go to the Secrets page

1. Click the **Settings** tab at the top of the repository page.
   (If you do not see it, you may need to be the repository owner or admin.)
2. In the left sidebar, expand **Secrets and variables**.
3. Click **Actions** under that section.

### 11.3 Create the secret

1. Click the green **New repository secret** button.
2. In the **Name** field type: `GEMINI_API_KEY`
   (exact spelling, all uppercase, with underscore)
3. In the **Secret** field paste your full API key:
   ```
   AIzaSyD-9tSrke72WhHqCPbXXXXXXXXXXXXXXX
   ```
4. Click **Add secret**.

The secret is now encrypted and stored by GitHub. No one, including you, can
read it back — you can only replace or delete it.

### 11.4 Reference it in ci.yml

In `.github/workflows/ci.yml`, find the `integration-tests` job and add the
following step after the existing Keycloak tests:

```yaml
      - name: Run AI Integration Tests
        env:
          GEMINI_API_KEY: ${{ secrets.GEMINI_API_KEY }}
        run: |
          if [ -n "$GEMINI_API_KEY" ]; then
            echo "GEMINI_API_KEY is set. Running live AI integration tests."
            go test -tags=integration -v -run TestFraudDetector_LiveGemini ./pkg/ai/
          else
            echo "GEMINI_API_KEY not configured in repository secrets."
            echo "Skipping live AI tests. Unit tests with mocked AI still ran."
          fi
```

This pattern mirrors how `SONAR_TOKEN` is handled in the existing pipeline —
the step is always present but skips gracefully when the secret is absent.

---

## Step 12 — Run a Live Demo

Use the following commands to demonstrate AI fraud detection. Run them in a
PowerShell terminal after the full Docker stack is running.

### 12.1 Start the full stack

```powershell
docker-compose -f docker-compose.mongodb.yml up -d
docker-compose -f docker-compose.rabbitmq.yml up -d
docker-compose -f docker-compose.auth.yml up -d
docker-compose up -d

# Wait for services to be ready
Start-Sleep -Seconds 15
curl http://localhost:8080/api/v1/cluster/status
```

### 12.2 Get an auth token

```powershell
$response = Invoke-RestMethod `
    -Uri "http://localhost:8080/api/v1/auth/login" `
    -Method POST `
    -ContentType "application/json" `
    -Body '{"username":"admin","password":"change-me-in-production"}'

$TOKEN = $response.access_token
Write-Host "Logged in. Token length: $($TOKEN.Length) chars"
```

### 12.3 Create test wallets

```powershell
# Alice's wallet (well-funded, normal user)
Invoke-RestMethod `
    -Uri "http://localhost:8080/api/v1/wallets" `
    -Method POST `
    -Headers @{ Authorization = "Bearer $TOKEN" } `
    -ContentType "application/json" `
    -Body '{"wallet_id":"alice","currency":"USD","initial_balance":10000000}'

# Bob's wallet (legitimate recipient)
Invoke-RestMethod `
    -Uri "http://localhost:8080/api/v1/wallets" `
    -Method POST `
    -Headers @{ Authorization = "Bearer $TOKEN" } `
    -ContentType "application/json" `
    -Body '{"wallet_id":"bob","currency":"USD","initial_balance":0}'
```

### 12.4 Demo 1 — Safe transfer (AI approves it)

```powershell
Invoke-RestMethod `
    -Uri "http://localhost:8080/api/v1/wallets/transfer" `
    -Method POST `
    -Headers @{ Authorization = "Bearer $TOKEN" } `
    -ContentType "application/json" `
    -Body '{
        "source_wallet_id": "alice",
        "destination_wallet_id": "bob",
        "amount": {"units": 500, "currency": "USD"},
        "idempotency_key": "safe-txn-001"
    }'
```

**Expected HTTP 200 response:**
```json
{"status": "SUCCESS", "transaction_id": "..."}
```

**Expected log in wallet-service:**
```
[AI-FRAUD] src=alice dst=bob amount=500 currency=USD score=0.08 decision=ALLOW
```

### 12.5 Demo 2 — Suspicious transfer (AI blocks it)

```powershell
Invoke-RestMethod `
    -Uri "http://localhost:8080/api/v1/wallets/transfer" `
    -Method POST `
    -Headers @{ Authorization = "Bearer $TOKEN" } `
    -ContentType "application/json" `
    -Body '{
        "source_wallet_id": "alice",
        "destination_wallet_id": "unknown-offshore-wallet",
        "amount": {"units": 5000000, "currency": "USD"},
        "idempotency_key": "fraud-attempt-001"
    }'
```

**Expected HTTP 403 response:**
```json
{
  "error": "blocked by fraud detection: High-value transfer to a previously unknown wallet"
}
```

**Expected log in wallet-service:**
```
[AI-FRAUD] src=alice dst=unknown-offshore-wallet amount=5000000 currency=USD score=0.91 decision=BLOCK
```

---

## Troubleshooting

### "AI fraud detection: DISABLED" in the logs

The service started without the `GEMINI_API_KEY` variable. Load the `.env`
file into your terminal session and restart:

```powershell
Get-Content .env | ForEach-Object {
    if ($_ -match '^\s*([^#][^=]*)=(.*)$') {
        [System.Environment]::SetEnvironmentVariable($matches[1].Trim(), $matches[2].Trim(), 'Process')
    }
}
go run ./cmd/wallet-service
```

---

### HTTP 429 — "Resource Exhausted" from Gemini

You sent more than 15 requests in one minute (the free tier rate limit).
This only happens during heavy automated testing, never in a demo.
Wait 60 seconds and try again.

---

### HTTP 400 — "API key not valid"

The key in your `.env` file is wrong, or you deleted it in AI Studio.
Go to `https://aistudio.google.com/app/apikey`, create a new key,
and update the `GEMINI_API_KEY` line in your `.env` file.

---

### Build error: `cannot find package "github.com/google/generative-ai-go/genai"`

Run the following:

```powershell
go get github.com/google/generative-ai-go@latest
go mod tidy
```

---

### All transfers return BLOCK even for small amounts

Check `AI_FRAUD_THRESHOLD` in your `.env`. If it is set below `0.4`, almost
everything will be blocked. Set it to `0.7` for reasonable behaviour.

Also confirm `AI_FRAUD_ENFORCEMENT=true` is what you want. Setting it to
`false` puts the detector in audit-only mode: it logs decisions but never
actually blocks.

---

## Security Rules — What Never to Do

| Rule | Reason |
|------|--------|
| Never put the API key in source code | Anyone who clones the repository can use your quota |
| Never put the real key in `.env.example` | That file is committed to git |
| Never print the key in log output | Logs are searchable and often shared |
| Never pass the key as a URL query parameter | URLs appear in server access logs and browser history |
| Always read the key with `os.Getenv("GEMINI_API_KEY")` | The same pattern used for `JWT_SECRET` in this project |
| Rotate the key immediately if you suspect it leaked | Takes 30 seconds in Google AI Studio |

If you accidentally commit your key to git, do the following immediately:

1. Go to `https://aistudio.google.com/app/apikey` and delete the key.
2. Create a new key.
3. Run `git filter-repo` or contact GitHub support to purge the commit history.
4. Update your local `.env` with the new key.

---

## Summary of Files Changed

| File | Status | What changed |
|------|--------|-------------|
| `go.mod` | Modified | Added `github.com/google/generative-ai-go` dependency |
| `go.sum` | Modified | Updated automatically by `go mod tidy` |
| `.env.example` | Modified | Added AI variable placeholders with comments |
| `.env` *(never committed)* | Modified | Added real `GEMINI_API_KEY` and AI config values |
| `pkg/ai/client.go` | New file | Gemini client wrapper with graceful degradation |
| `pkg/ai/fraud_detector.go` | New file | Fraud scoring logic with fail-open error handling |
| `pkg/ai/fraud_detector_test.go` | New file | Unit tests (run without an API key) |
| `cmd/wallet-service/main.go` | Modified | Added `aiClient` + `fraudDetector` fields and fraud check |
| `.github/workflows/ci.yml` | Modified | Added AI integration test step with secret reference |
