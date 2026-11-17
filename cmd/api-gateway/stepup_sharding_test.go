package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"wallet-system/pkg/auth"
	"wallet-system/pkg/sharding"
)

func TestGateway_StepUpChallengeAndVerifyFlow(t *testing.T) {
	gw := &Gateway{
		stepUpMgr:      auth.NewStepUpManager("test-stepup-secret-key-32-bytes!"),
		shardingRouter: sharding.NewRouter(),
	}

	// 1. Request Challenge
	challengeReqBody := []byte(`{"user_id":"alice-user-1","action":"transfer"}`)
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/step-up/challenge", bytes.NewReader(challengeReqBody))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()

	gw.handleStepUpChallenge(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from handleStepUpChallenge, got %d: %s", w1.Code, w1.Body.String())
	}

	var challenge auth.StepUpChallenge
	if err := json.Unmarshal(w1.Body.Bytes(), &challenge); err != nil {
		t.Fatalf("failed to parse challenge response: %v", err)
	}
	if challenge.ID == "" || challenge.Nonce == "" {
		t.Fatalf("expected challenge ID and Nonce, got id=%s nonce=%s", challenge.ID, challenge.Nonce)
	}

	// 2. Verify with invalid proof -> 401
	verifyInvalidBody := []byte(`{"challenge_id":"` + challenge.ID + `","proof":"invalid-proof"}`)
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/step-up/verify", bytes.NewReader(verifyInvalidBody))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()

	gw.handleStepUpVerify(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for invalid proof, got %d: %s", w2.Code, w2.Body.String())
	}

	// 3. Verify with valid HMAC proof -> 200 OK with step-up token
	validProof := auth.ComputeHMACProof(challenge.Nonce, "test-stepup-secret-key-32-bytes!")
	verifyValidBody := []byte(`{"challenge_id":"` + challenge.ID + `","proof":"` + validProof + `"}`)
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/step-up/verify", bytes.NewReader(verifyValidBody))
	req3.Header.Set("Content-Type", "application/json")
	w3 := httptest.NewRecorder()

	gw.handleStepUpVerify(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for valid proof, got %d: %s", w3.Code, w3.Body.String())
	}

	var verifyResp struct {
		Success     bool   `json:"success"`
		StepUpToken string `json:"step_up_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(w3.Body.Bytes(), &verifyResp); err != nil {
		t.Fatalf("failed to decode verify response: %v", err)
	}
	if !verifyResp.Success || verifyResp.StepUpToken == "" {
		t.Fatalf("expected success and non-empty step-up token")
	}

	// 4. Validate Step-Up Token contains elevated scope
	claims, err := gw.getStepUpManager().ValidateStepUpToken(verifyResp.StepUpToken)
	if err != nil {
		t.Fatalf("ValidateStepUpToken failed on issued token: %v", err)
	}
	if !claims.HasScope(auth.ScopeStepUpElevated) {
		t.Errorf("expected ScopeStepUpElevated in claims")
	}
}

func TestGateway_StepUpTransferEnforcement(t *testing.T) {
	gw := &Gateway{
		stepUpMgr: auth.NewStepUpManager("test-stepup-secret-key-32-bytes!"),
	}

	// 1. Transfer with X-Require-StepUp but without X-StepUp-Token -> 403 Forbidden
	transferBody := []byte(`{
		"source_wallet_id": "alice",
		"destination_wallet_id": "bob",
		"amount": 5000,
		"currency": "USD"
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/transfers", bytes.NewReader(transferBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Require-StepUp", "true")
	w := httptest.NewRecorder()

	gw.handleTransfer(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden without step-up token, got %d: %s", w.Code, w.Body.String())
	}

	var errResp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	if errResp["step_up_required"] != true {
		t.Errorf("expected step_up_required: true in response")
	}
}

func TestGateway_ShardingEndpoints(t *testing.T) {
	gw := &Gateway{
		shardingRouter: sharding.NewRouter(),
	}

	// 1. List regions
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/sharding/regions", nil)
	w1 := httptest.NewRecorder()
	gw.handleShardingRegions(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from handleShardingRegions, got %d: %s", w1.Code, w1.Body.String())
	}

	var regionsResp struct {
		Regions []sharding.RegionConfig `json:"regions"`
		Count   int                     `json:"count"`
	}
	if err := json.Unmarshal(w1.Body.Bytes(), &regionsResp); err != nil {
		t.Fatalf("failed to decode regions response: %v", err)
	}
	if regionsResp.Count != 3 {
		t.Errorf("expected 3 regions, got %d", regionsResp.Count)
	}

	// 2. Query home region for wallet
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/sharding/home?wallet_id=wallet-user-alpha", nil)
	w2 := httptest.NewRecorder()
	gw.handleShardingHome(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from handleShardingHome, got %d: %s", w2.Code, w2.Body.String())
	}

	var homeResp struct {
		WalletID   string                `json:"wallet_id"`
		HomeRegion sharding.RegionConfig `json:"home_region"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &homeResp); err != nil {
		t.Fatalf("failed to decode home region response: %v", err)
	}
	if homeResp.WalletID != "wallet-user-alpha" || homeResp.HomeRegion.ID == "" {
		t.Errorf("invalid home region response: %+v", homeResp)
	}

	// 3. Query home region with missing wallet_id -> 400 Bad Request
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/sharding/home", nil)
	w3 := httptest.NewRecorder()
	gw.handleShardingHome(w3, req3)
	if w3.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for missing wallet_id, got %d", w3.Code)
	}
}
