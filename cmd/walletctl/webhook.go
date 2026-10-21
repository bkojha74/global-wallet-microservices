package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wallet-system/pkg/webhook"
)

const webhookUsage = `walletctl webhook - Webhook Dispatch & Verification Diagnostics

Usage:
  walletctl webhook ping --url <target_url> [flags]

Flags:
  --url        Target webhook endpoint URL (required)
  --secret     Webhook HMAC-SHA256 signing secret (default: whsec_test_secret_001)
  --event      Webhook event name (default: webhook.test)
  --timeout    HTTP request timeout (default: 10s)
  --json       Output result as JSON
  --help, -h   Show this help message
`

type webhookPingReport struct {
	URL             string `json:"url"`
	Event           string `json:"event"`
	DeliveryID      string `json:"delivery_id"`
	Signature       string `json:"signature"`
	StatusCode      int    `json:"status_code"`
	StatusText      string `json:"status_text"`
	LatencyMS       int64  `json:"latency_ms"`
	Success         bool   `json:"success"`
	ResponsePreview string `json:"response_preview,omitempty"`
	ErrorMessage    string `json:"error,omitempty"`
}

type webhookPingPayload struct {
	Event     string `json:"event"`
	PingID    string `json:"ping_id"`
	Timestamp string `json:"timestamp"`
	Summary   string `json:"summary"`
	Source    string `json:"source"`
}

func generateDeliveryID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "whping_" + hex.EncodeToString(b)
}

func runWebhookCmd(args []string, out io.Writer, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(out, webhookUsage)
		return 0
	}

	subCmd := args[0]
	if subCmd == "--help" || subCmd == "-h" || subCmd == "help" {
		fmt.Fprint(out, webhookUsage)
		return 0
	}

	if subCmd != "ping" {
		fmt.Fprintf(errOut, "Unknown webhook subcommand %q. Supported subcommands: ping\n", subCmd)
		return 1
	}

	fs := flag.NewFlagSet("webhook ping", flag.ContinueOnError)
	fs.SetOutput(errOut)

	var (
		targetURL = fs.String("url", "", "Target webhook endpoint URL")
		secret    = fs.String("secret", "whsec_test_secret_001", "Webhook HMAC-SHA256 signing secret")
		event     = fs.String("event", "webhook.test", "Webhook event type")
		timeout   = fs.Duration("timeout", 10*time.Second, "HTTP timeout")
		jsonOut   = fs.Bool("json", false, "Output results as JSON")
	)

	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	if *targetURL == "" {
		fmt.Fprintln(errOut, "Error: --url is required")
		return 1
	}

	deliveryID := generateDeliveryID()
	now := time.Now().UTC()
	tsUnix := now.Unix()

	payloadObj := webhookPingPayload{
		Event:     *event,
		PingID:    deliveryID,
		Timestamp: now.Format(time.RFC3339),
		Summary:   "Synthetic webhook probe dispatched by walletctl utility",
		Source:    "walletctl/" + Version,
	}

	payloadBytes, err := json.Marshal(payloadObj)
	if err != nil {
		fmt.Fprintf(errOut, "Failed to serialize test payload: %v\n", err)
		return 1
	}

	sigHeader := webhook.SignPayload(*secret, tsUnix, payloadBytes)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *targetURL, bytes.NewReader(payloadBytes))
	if err != nil {
		fmt.Fprintf(errOut, "Failed to construct HTTP request: %v\n", err)
		return 1
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Global-Wallet-CLI-Webhook-Ping/"+Version)
	req.Header.Set("X-Wallet-Signature", sigHeader)
	req.Header.Set("X-Wallet-Event", *event)
	req.Header.Set("X-Wallet-Delivery", deliveryID)
	req.Header.Set("X-Wallet-Timestamp", strconv.FormatInt(tsUnix, 10))

	client := &http.Client{Timeout: *timeout}
	startTime := time.Now()
	resp, reqErr := client.Do(req)
	latency := time.Since(startTime)

	report := webhookPingReport{
		URL:        *targetURL,
		Event:      *event,
		DeliveryID: deliveryID,
		Signature:  sigHeader,
		LatencyMS:  latency.Milliseconds(),
	}

	if reqErr != nil {
		report.Success = false
		report.ErrorMessage = reqErr.Error()
	} else {
		defer resp.Body.Close()
		report.StatusCode = resp.StatusCode
		report.StatusText = resp.Status
		report.Success = (resp.StatusCode >= 200 && resp.StatusCode < 300)

		bodyPreview, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		report.ResponsePreview = strings.TrimSpace(string(bodyPreview))
	}

	if *jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		fmt.Fprintln(out, "=== Webhook Ping Probe ===")
		fmt.Fprintf(out, "Target URL:       %s\n", report.URL)
		fmt.Fprintf(out, "Event:            %s\n", report.Event)
		fmt.Fprintf(out, "Delivery ID:      %s\n", report.DeliveryID)
		fmt.Fprintf(out, "Signature Header: %s\n", report.Signature)
		fmt.Fprintf(out, "Latency:          %d ms\n", report.LatencyMS)

		if reqErr != nil {
			fmt.Fprintf(out, "Result:           [FAIL] Request error: %s\n", reqErr.Error())
		} else {
			fmt.Fprintf(out, "HTTP Status:      %d %s\n", report.StatusCode, http.StatusText(report.StatusCode))
			if report.Success {
				fmt.Fprintln(out, "Result:           [PASS] Partner endpoint acknowledged delivery")
			} else {
				fmt.Fprintln(out, "Result:           [FAIL] Partner endpoint returned non-2xx status")
			}
			if report.ResponsePreview != "" {
				fmt.Fprintf(out, "Response Preview: %s\n", report.ResponsePreview)
			}
		}
	}

	if !report.Success {
		return 1
	}
	return 0
}
