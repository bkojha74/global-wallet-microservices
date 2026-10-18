package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestSignatureLifecycle(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret failed: %v", err)
	}
	if len(secret) < 32 {
		t.Fatalf("secret too short: %s", secret)
	}

	payload := []byte(`{"id":"evt_1","event":"transfer.completed","data":{"amount":1000}}`)
	ts := time.Now().UTC().Unix()

	// 1. Valid signature
	header := SignPayload(secret, ts, payload)
	if !VerifySignature(secret, header, payload, 5*time.Minute) {
		t.Fatalf("expected signature verification to succeed")
	}

	// 2. Tampered payload
	tamperedPayload := []byte(`{"id":"evt_1","event":"transfer.completed","data":{"amount":999999}}`)
	if VerifySignature(secret, header, tamperedPayload, 5*time.Minute) {
		t.Fatalf("expected tampered payload signature verification to fail")
	}

	// 3. Stale timestamp (replay attack)
	staleTS := time.Now().UTC().Add(-10 * time.Minute).Unix()
	staleHeader := SignPayload(secret, staleTS, payload)
	if VerifySignature(secret, staleHeader, payload, 5*time.Minute) {
		t.Fatalf("expected stale signature (replay attack) to be rejected")
	}

	// 4. Future timestamp out of skew
	futureTS := time.Now().UTC().Add(10 * time.Minute).Unix()
	futureHeader := SignPayload(secret, futureTS, payload)
	if VerifySignature(secret, futureHeader, payload, 5*time.Minute) {
		t.Fatalf("expected future timestamp out of skew to be rejected")
	}

	// 5. Corrupted header
	if VerifySignature(secret, "invalid_header", payload, 5*time.Minute) {
		t.Fatalf("expected invalid header to fail")
	}
	if VerifySignature(secret, "t=123", payload, 5*time.Minute) {
		t.Fatalf("expected incomplete header to fail")
	}
}

func TestSubscriptionEventMatching(t *testing.T) {
	sub1 := WebhookSubscription{
		IsActive: true,
		Events:   []string{EventTransferCompleted, EventFraudFlagged},
	}

	if !sub1.MatchesEvent(EventTransferCompleted) {
		t.Errorf("expected sub1 to match transfer.completed")
	}
	if !sub1.MatchesEvent(EventFraudFlagged) {
		t.Errorf("expected sub1 to match fraud.flagged")
	}
	if sub1.MatchesEvent(EventDepositCompleted) {
		t.Errorf("expected sub1 NOT to match deposit.completed")
	}

	// Wildcard subscription
	subWildcard := WebhookSubscription{
		IsActive: true,
		Events:   []string{"*"},
	}
	if !subWildcard.MatchesEvent(EventDepositCompleted) {
		t.Errorf("expected wildcard subscription to match deposit.completed")
	}

	// Inactive subscription
	subInactive := WebhookSubscription{
		IsActive: false,
		Events:   []string{"*"},
	}
	if subInactive.MatchesEvent(EventTransferCompleted) {
		t.Errorf("expected inactive subscription NOT to match any event")
	}
}

func TestDispatcherSendAndVerifyWithHTTPServer(t *testing.T) {
	var receivedHeader string
	var receivedEvent string
	var receivedDelivery string
	var receivedBody []byte
	var requestCount int32

	secret := "whsec_0123456789abcdef0123456789abcdef"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		receivedHeader = r.Header.Get("X-Wallet-Signature")
		receivedEvent = r.Header.Get("X-Wallet-Event")
		receivedDelivery = r.Header.Get("X-Wallet-Delivery")
		body, _ := io.ReadAll(r.Body)
		receivedBody = body
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"received":true}`))
	}))
	defer server.Close()

	dispatcher := NewDispatcher(nil, server.Client())

	sub := WebhookSubscription{
		ID:       primitive.NewObjectID(),
		URL:      server.URL,
		Secret:   secret,
		Events:   []string{EventTransferCompleted},
		IsActive: true,
	}

	ctx := context.Background()
	payload := map[string]interface{}{
		"transfer_id": "tx_test_123",
		"amount":      5000,
		"currency":    "USD",
	}

	payloadBytes, _ := json.Marshal(payload)
	dispatcher.dispatchSingle(ctx, sub, EventTransferCompleted, payloadBytes)

	if atomic.LoadInt32(&requestCount) != 1 {
		t.Fatalf("expected exactly 1 HTTP request received, got %d", requestCount)
	}
	if receivedEvent != EventTransferCompleted {
		t.Errorf("expected X-Wallet-Event %s, got %s", EventTransferCompleted, receivedEvent)
	}
	if receivedDelivery == "" {
		t.Errorf("expected non-empty X-Wallet-Delivery")
	}
	if !VerifySignature(secret, receivedHeader, receivedBody, 5*time.Minute) {
		t.Errorf("signature verification failed on received HTTP request")
	}

	// Test endpoint ping method
	code, latency, err := dispatcher.TestEndpoint(ctx, server.URL, secret)
	if err != nil || code != http.StatusOK {
		t.Errorf("TestEndpoint failed: code=%d, err=%v", code, err)
	}
	if latency <= 0 {
		t.Errorf("expected positive latency measurement")
	}
}

func TestRegisterSubscriptionValidation(t *testing.T) {
	d := NewDispatcher(nil, nil)
	ctx := context.Background()

	// 1. Nil subscription
	if err := d.RegisterSubscription(ctx, nil); err == nil {
		t.Errorf("expected error for nil subscription")
	}

	// 2. Bad URL
	if err := d.RegisterSubscription(ctx, &WebhookSubscription{URL: "ftp://bad.url", Events: []string{"*"}}); err == nil {
		t.Errorf("expected error for non-http URL")
	}

	// 3. Empty events
	if err := d.RegisterSubscription(ctx, &WebhookSubscription{URL: "https://partner.com", Events: nil}); err == nil {
		t.Errorf("expected error for empty events")
	}

	// 4. Valid subscription
	sub := &WebhookSubscription{
		URL:    "https://partner.com/webhook",
		Events: []string{EventTransferCompleted},
	}
	if err := d.RegisterSubscription(ctx, sub); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if sub.Secret == "" || !sub.IsActive {
		t.Errorf("expected auto-generated secret and active status")
	}
}

func TestComputeBackoff(t *testing.T) {
	for i := 1; i <= 5; i++ {
		b := computeBackoff(i)
		if b < time.Duration(1<<i)*time.Second {
			t.Errorf("backoff for attempt %d too small: %v", i, b)
		}
	}
}
