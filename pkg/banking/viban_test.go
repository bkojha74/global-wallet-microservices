package banking

import (
	"context"
	"testing"
)

func TestGenerateVirtualIBAN(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVirtualIBANStore()

	// 1. Generate UK vIBAN
	ukVIBAN, err := GenerateVirtualIBAN("GB", "GBP", "wal_alice_uk", "Alice Primary UK Account", 12345678)
	if err != nil {
		t.Fatalf("failed to generate UK vIBAN: %v", err)
	}
	if err := ValidateIBAN(ukVIBAN.IBAN); err != nil {
		t.Fatalf("generated UK vIBAN is invalid per ISO 13616: %v (IBAN: %s)", err, ukVIBAN.IBAN)
	}

	// 2. Generate German vIBAN
	deVIBAN, err := GenerateVirtualIBAN("DE", "EUR", "wal_bob_de", "Bob Euro Clearing Account", 9876543210)
	if err != nil {
		t.Fatalf("failed to generate German vIBAN: %v", err)
	}
	if err := ValidateIBAN(deVIBAN.IBAN); err != nil {
		t.Fatalf("generated German vIBAN is invalid per ISO 13616: %v (IBAN: %s)", err, deVIBAN.IBAN)
	}

	// 3. Generate French vIBAN
	frVIBAN, err := GenerateVirtualIBAN("FR", "EUR", "wal_charlie_fr", "Charlie French Merchant", 11223344556)
	if err != nil {
		t.Fatalf("failed to generate French vIBAN: %v", err)
	}
	if err := ValidateIBAN(frVIBAN.IBAN); err != nil {
		t.Fatalf("generated French vIBAN is invalid per ISO 13616: %v (IBAN: %s)", err, frVIBAN.IBAN)
	}

	// 4. Test Store Operations
	if err := store.IssueVirtualIBAN(ctx, ukVIBAN); err != nil {
		t.Fatalf("failed to issue vIBAN into store: %v", err)
	}
	// Duplicate issue should fail
	if err := store.IssueVirtualIBAN(ctx, ukVIBAN); err != ErrVirtualIBANExists {
		t.Fatalf("expected ErrVirtualIBANExists on duplicate issue, got: %v", err)
	}

	retrieved, err := store.GetVirtualIBAN(ctx, ukVIBAN.IBAN)
	if err != nil {
		t.Fatalf("failed to retrieve stored vIBAN: %v", err)
	}
	if retrieved.WalletID != "wal_alice_uk" || retrieved.Currency != "GBP" {
		t.Fatalf("unexpected retrieved vIBAN data: %+v", retrieved)
	}

	// 5. List vIBANs
	list, err := store.ListVirtualIBANs(ctx, "wal_alice_uk")
	if err != nil || len(list) != 1 {
		t.Fatalf("expected 1 vIBAN in list, got %d (err: %v)", len(list), err)
	}

	// 6. Revoke vIBAN
	if err := store.RevokeVirtualIBAN(ctx, ukVIBAN.IBAN); err != nil {
		t.Fatalf("failed to revoke vIBAN: %v", err)
	}
	_, err = store.GetVirtualIBAN(ctx, ukVIBAN.IBAN)
	if err != ErrVirtualIBANRevoked {
		t.Fatalf("expected ErrVirtualIBANRevoked after revocation, got: %v", err)
	}
}
