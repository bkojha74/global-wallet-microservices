package auth

import (
	"testing"
	"time"
)

func TestStepUpManager_FullFlow(t *testing.T) {
	secret := "test-stepup-secret-key-32-bytes!"
	mgr := NewStepUpManager(secret)

	// 1. Request Challenge
	userID := "user-alice-123"
	action := "transfer"
	ch, err := mgr.CreateChallenge(userID, action)
	if err != nil {
		t.Fatalf("CreateChallenge failed: %v", err)
	}
	if len(ch.Nonce) != 64 { // 32 bytes hex encoded = 64 characters
		t.Errorf("expected 64 char hex nonce, got length %d", len(ch.Nonce))
	}
	if ch.Consumed {
		t.Errorf("expected newly created challenge to not be consumed")
	}

	// 2. Verify with valid HMAC proof
	proof := ComputeHMACProof(ch.Nonce, secret)
	tokenStr, err := mgr.VerifyChallenge(ch.ID, proof)
	if err != nil {
		t.Fatalf("VerifyChallenge with HMAC failed: %v", err)
	}
	if tokenStr == "" {
		t.Fatalf("expected non-empty token string")
	}

	// 3. Validate Step-Up Token
	claims, err := mgr.ValidateStepUpToken(tokenStr)
	if err != nil {
		t.Fatalf("ValidateStepUpToken failed: %v", err)
	}
	if claims.Subject != userID {
		t.Errorf("expected subject %s, got %s", userID, claims.Subject)
	}
	if !claims.HasScope(ScopeStepUpElevated) {
		t.Errorf("expected scope %s", ScopeStepUpElevated)
	}

	// 4. One-time use: re-verifying same challenge should fail
	_, err = mgr.VerifyChallenge(ch.ID, proof)
	if err != ErrChallengeNotFound {
		t.Errorf("expected ErrChallengeNotFound for re-used challenge, got: %v", err)
	}
}

func TestStepUpManager_FIDO2AndTOTPProofs(t *testing.T) {
	secret := "test-stepup-secret-key-32-bytes!"
	mgr := NewStepUpManager(secret)

	// FIDO2 Assertion Proof
	ch1, err := mgr.CreateChallenge("user-bob", "tier_upgrade")
	if err != nil {
		t.Fatalf("CreateChallenge failed: %v", err)
	}
	token1, err := mgr.VerifyChallenge(ch1.ID, "fido2-assert:"+ch1.Nonce)
	if err != nil {
		t.Fatalf("VerifyChallenge with FIDO2 assertion failed: %v", err)
	}
	if token1 == "" {
		t.Errorf("expected valid token for FIDO2 assertion")
	}

	// TOTP 6-digit backup code
	ch2, err := mgr.CreateChallenge("user-charlie", "cluster_admin")
	if err != nil {
		t.Fatalf("CreateChallenge failed: %v", err)
	}
	token2, err := mgr.VerifyChallenge(ch2.ID, "totp:123456")
	if err != nil {
		t.Fatalf("VerifyChallenge with TOTP failed: %v", err)
	}
	if token2 == "" {
		t.Errorf("expected valid token for TOTP")
	}

	// Passkey biometric verified proof
	ch3, err := mgr.CreateChallenge("user-dave", "transfer")
	if err != nil {
		t.Fatalf("CreateChallenge failed: %v", err)
	}
	token3, err := mgr.VerifyChallenge(ch3.ID, "passkey-biometric-verified")
	if err != nil {
		t.Fatalf("VerifyChallenge with passkey failed: %v", err)
	}
	if token3 == "" {
		t.Errorf("expected valid token for passkey")
	}
}

func TestStepUpManager_InvalidProofAndExpired(t *testing.T) {
	secret := "test-stepup-secret-key-32-bytes!"
	mgr := NewStepUpManager(secret)

	ch, err := mgr.CreateChallenge("user-eve", "transfer")
	if err != nil {
		t.Fatalf("CreateChallenge failed: %v", err)
	}

	// Invalid proof
	_, err = mgr.VerifyChallenge(ch.ID, "invalid-tampered-proof")
	if err != ErrInvalidStepUpProof {
		t.Errorf("expected ErrInvalidStepUpProof, got: %v", err)
	}

	// Simulate expired challenge
	ch.ExpiresAt = time.Now().Add(-1 * time.Minute)
	_, err = mgr.VerifyChallenge(ch.ID, ComputeHMACProof(ch.Nonce, secret))
	if err != ErrChallengeExpired {
		t.Errorf("expected ErrChallengeExpired, got: %v", err)
	}
}

func TestStepUpManager_ValidateStepUpToken_RegularTokenFails(t *testing.T) {
	secret := "test-stepup-secret-key-32-bytes!"
	mgr := NewStepUpManager(secret)

	// Create regular token without ScopeStepUpElevated
	regClaims := Claims{
		Subject:   "user-frank",
		Roles:     []string{RoleUser},
		Scopes:    []string{ScopeWalletRead, ScopeWalletTransfer},
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
	}
	tokenStr, err := GenerateToken(regClaims, secret)
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}

	_, err = mgr.ValidateStepUpToken(tokenStr)
	if err != ErrStepUpRequired {
		t.Errorf("expected ErrStepUpRequired for token without elevated scope, got: %v", err)
	}
}

func TestIsStepUpRequired(t *testing.T) {
	tests := []struct {
		action   string
		amount   float64
		expected bool
	}{
		{"transfer", 50.0, false},
		{"transfer", 9999.99, false},
		{"transfer", 10000.00, true},
		{"transfer", 50000.00, true},
		{"failover", 0, true},
		{"cluster_admin", 0, true},
		{"tier_upgrade", 0, true},
		{"read", 0, false},
	}

	for _, tc := range tests {
		res := IsStepUpRequired(tc.action, tc.amount)
		if res != tc.expected {
			t.Errorf("IsStepUpRequired(%s, %f): expected %v, got %v", tc.action, tc.amount, tc.expected, res)
		}
	}
}
