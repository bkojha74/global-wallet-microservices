package multientity

import (
	"testing"
)

func TestEntityRegistry_DefaultsAndListing(t *testing.T) {
	reg := NewEntityRegistry()

	entities := reg.ListEntities()
	if len(entities) != 4 {
		t.Fatalf("expected 4 global entities, got %d", len(entities))
	}

	// Test default heuristic mappings
	usEntity, err := reg.GetWalletEntity("alice_usd")
	if err != nil || usEntity.ID != EntityUSCorp {
		t.Errorf("expected %s for alice_usd, got %v (err: %v)", EntityUSCorp, usEntity, err)
	}

	euEntity, err := reg.GetWalletEntity("wallet_de_user_1")
	if err != nil || euEntity.ID != EntityEUGmbh {
		t.Errorf("expected %s for wallet_de_user_1, got %v (err: %v)", EntityEUGmbh, euEntity, err)
	}

	ukEntity, err := reg.GetWalletEntity("wallet_gbp_user_2")
	if err != nil || ukEntity.ID != EntityGBLtd {
		t.Errorf("expected %s for wallet_gbp_user_2, got %v (err: %v)", EntityGBLtd, ukEntity, err)
	}

	sgEntity, err := reg.GetWalletEntity("wallet_sgd_merchant")
	if err != nil || sgEntity.ID != EntitySGPte {
		t.Errorf("expected %s for wallet_sgd_merchant, got %v (err: %v)", EntitySGPte, sgEntity, err)
	}
}

func TestEntityRegistry_ExplicitBindingAndConflicts(t *testing.T) {
	reg := NewEntityRegistry()

	walletID := "custom-wallet-999"
	err := reg.RegisterWalletEntity(walletID, EntityEUGmbh)
	if err != nil {
		t.Fatalf("RegisterWalletEntity failed: %v", err)
	}

	ent, err := reg.GetWalletEntity(walletID)
	if err != nil || ent.ID != EntityEUGmbh {
		t.Fatalf("expected entity %s, got %v (err: %v)", EntityEUGmbh, ent, err)
	}

	// Registering same entity again is idempotent
	err = reg.RegisterWalletEntity(walletID, EntityEUGmbh)
	if err != nil {
		t.Errorf("expected idempotent re-registration to succeed, got %v", err)
	}

	// Conflicting binding must fail
	err = reg.RegisterWalletEntity(walletID, EntityUSCorp)
	if err != ErrWalletAlreadyBound {
		t.Errorf("expected ErrWalletAlreadyBound, got %v", err)
	}

	// Invalid entity ID
	err = reg.RegisterWalletEntity("wallet-fail", "INVALID_ENTITY")
	if err != ErrEntityNotFound {
		t.Errorf("expected ErrEntityNotFound, got %v", err)
	}
}

func TestInterEntitySettler_IntraEntity(t *testing.T) {
	reg := NewEntityRegistry()
	settler := NewInterEntitySettler(reg)

	// Both wallets default to US_CORP
	res, err := settler.CoordinateCrossEntitySettlement("tx-intra-01", "alice_usd", "bob_usd", 150.00, "USD")
	if err != nil {
		t.Fatalf("CoordinateCrossEntitySettlement failed: %v", err)
	}
	if res.IsCrossEntity {
		t.Errorf("expected IsCrossEntity to be false for intra-entity transfer")
	}
	if len(res.OmnibusPostings) != 0 {
		t.Errorf("expected 0 omnibus postings for intra-entity transfer, got %d", len(res.OmnibusPostings))
	}
	if res.Status != "SETTLED" {
		t.Errorf("expected status SETTLED, got %s", res.Status)
	}
}

func TestInterEntitySettler_CrossEntity4LeggedSettlement(t *testing.T) {
	reg := NewEntityRegistry()
	_ = reg.RegisterWalletEntity("alice_us", EntityUSCorp)
	_ = reg.RegisterWalletEntity("bob_eu", EntityEUGmbh)

	settler := NewInterEntitySettler(reg)
	amount := 5000.00
	currency := "EUR"

	res, err := settler.CoordinateCrossEntitySettlement("tx-cross-01", "alice_us", "bob_eu", amount, currency)
	if err != nil {
		t.Fatalf("CoordinateCrossEntitySettlement failed: %v", err)
	}

	if !res.IsCrossEntity {
		t.Fatalf("expected IsCrossEntity to be true")
	}
	if res.SourceEntityID != EntityUSCorp || res.TargetEntityID != EntityEUGmbh {
		t.Fatalf("unexpected entities: src=%s target=%s", res.SourceEntityID, res.TargetEntityID)
	}
	if len(res.OmnibusPostings) != 4 {
		t.Fatalf("expected 4 omnibus postings, got %d", len(res.OmnibusPostings))
	}

	// Validate double-entry balance in each entity
	// Entity US: Leg 1 Debit Customer, Leg 2 Credit Due-To EU
	leg1 := res.OmnibusPostings[0]
	leg2 := res.OmnibusPostings[1]
	if leg1.EntityID != EntityUSCorp || leg1.Direction != "DEBIT" || leg1.AccountNumber != "alice_us" {
		t.Errorf("invalid Leg 1: %+v", leg1)
	}
	if leg2.EntityID != EntityUSCorp || leg2.Direction != "CREDIT" || leg2.AccountNumber != "2100-DUE-TO-EU_GMBH" {
		t.Errorf("invalid Leg 2: %+v", leg2)
	}

	// Entity EU: Leg 3 Debit Due-From US, Leg 4 Credit Customer
	leg3 := res.OmnibusPostings[2]
	leg4 := res.OmnibusPostings[3]
	if leg3.EntityID != EntityEUGmbh || leg3.Direction != "DEBIT" || leg3.AccountNumber != "1300-DUE-FROM-US_CORP" {
		t.Errorf("invalid Leg 3: %+v", leg3)
	}
	if leg4.EntityID != EntityEUGmbh || leg4.Direction != "CREDIT" || leg4.AccountNumber != "bob_eu" {
		t.Errorf("invalid Leg 4: %+v", leg4)
	}

	// Negative amount validation
	_, err = settler.CoordinateCrossEntitySettlement("tx-err", "alice_us", "bob_eu", -10, "USD")
	if err != ErrInvalidAmount {
		t.Errorf("expected ErrInvalidAmount, got %v", err)
	}
}
