package openbanking

import (
	"context"
	"testing"
	"time"
)

func TestConsentLifecycleAndAccess(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryConsentStore()

	// 1. Create Consent
	consent := &Consent{
		ConsentID:   "consent-ob-1001",
		ClientTPPId: "tpp-plaid-fintech",
		UserID:      "alice",
		WalletIDs:   []string{"wal_alice_usd", "wal_alice_eur"},
		Permissions: []string{PermReadAccountsDetail, PermReadBalances, PermCreatePaymentInitiation},
		Status:      StatusAwaitingAuthorisation,
		CreatedAt:   time.Now().UTC(),
		ExpiresAt:   time.Now().UTC().Add(90 * 24 * time.Hour), // 90 days
	}

	if err := store.SaveConsent(ctx, consent); err != nil {
		t.Fatalf("failed to save consent: %v", err)
	}

	// 2. Validate un-authorised consent -> Should fail
	c, err := store.GetConsent(ctx, "consent-ob-1001")
	if err != nil {
		t.Fatalf("failed to get consent: %v", err)
	}
	if err := ValidateConsentAccess(c, PermReadBalances, "wal_alice_usd"); err != ErrConsentNotAuthorised {
		t.Fatalf("expected ErrConsentNotAuthorised, got: %v", err)
	}

	// 3. Authorise Consent
	if err := store.AuthoriseConsent(ctx, "consent-ob-1001", "alice"); err != nil {
		t.Fatalf("failed to authorise consent: %v", err)
	}

	c, _ = store.GetConsent(ctx, "consent-ob-1001")
	if c.Status != StatusAuthorised || c.AuthorisedAt == nil {
		t.Fatalf("consent status not updated to AUTHORISED: %+v", c)
	}

	// 4. Validate authorized access
	if err := ValidateConsentAccess(c, PermReadBalances, "wal_alice_usd"); err != nil {
		t.Fatalf("expected valid access for permitted wallet and scope, got: %v", err)
	}

	// 5. Test missing permission (e.g. PermReadTransactionsDetail not granted)
	if err := ValidateConsentAccess(c, PermReadTransactionsDetail, "wal_alice_usd"); err == nil {
		t.Fatal("expected error for ungranted scope, got nil")
	}

	// 6. Test unauthorized wallet
	if err := ValidateConsentAccess(c, PermReadBalances, "wal_bob_secret"); err == nil {
		t.Fatal("expected error for unauthorized wallet, got nil")
	}

	// 7. Revoke Consent
	if err := store.RevokeConsent(ctx, "consent-ob-1001", "alice"); err != nil {
		t.Fatalf("failed to revoke consent: %v", err)
	}
	c, _ = store.GetConsent(ctx, "consent-ob-1001")
	if err := ValidateConsentAccess(c, PermReadBalances, "wal_alice_usd"); err != ErrConsentRevoked {
		t.Fatalf("expected ErrConsentRevoked after revocation, got: %v", err)
	}

	// 8. Test Expiration
	expiredConsent := &Consent{
		ConsentID:   "consent-ob-expired",
		ClientTPPId: "tpp-yodlee",
		UserID:      "bob",
		WalletIDs:   []string{"wal_bob_usd"},
		Permissions: []string{PermReadBalances},
		Status:      StatusAuthorised,
		CreatedAt:   time.Now().UTC().Add(-100 * 24 * time.Hour),
		ExpiresAt:   time.Now().UTC().Add(-10 * 24 * time.Hour),
	}
	_ = store.SaveConsent(ctx, expiredConsent)
	cExp, _ := store.GetConsent(ctx, "consent-ob-expired")
	if err := ValidateConsentAccess(cExp, PermReadBalances, "wal_bob_usd"); err != ErrConsentExpired {
		t.Fatalf("expected ErrConsentExpired for expired consent, got: %v", err)
	}
}
