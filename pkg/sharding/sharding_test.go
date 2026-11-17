package sharding

import (
	"testing"
)

func TestRouter_ResidencyOverride(t *testing.T) {
	router := NewRouter()

	// Register explicit EU statutory residency for GDPR compliance
	walletEU := "wallet-gdpr-user-456"
	err := router.RegisterResidency(walletEU, RegionEUWest)
	if err != nil {
		t.Fatalf("RegisterResidency failed: %v", err)
	}

	reg, err := router.GetHomeRegion(walletEU)
	if err != nil {
		t.Fatalf("GetHomeRegion failed: %v", err)
	}
	if reg.ID != RegionEUWest {
		t.Errorf("expected region %s, got %s", RegionEUWest, reg.ID)
	}

	// Register US statutory residency
	walletUS := "wallet-us-fincen-789"
	err = router.RegisterResidency(walletUS, RegionUSEast)
	if err != nil {
		t.Fatalf("RegisterResidency failed: %v", err)
	}

	regUS, err := router.GetHomeRegion(walletUS)
	if err != nil {
		t.Fatalf("GetHomeRegion failed: %v", err)
	}
	if regUS.ID != RegionUSEast {
		t.Errorf("expected region %s, got %s", RegionUSEast, regUS.ID)
	}
}

func TestRouter_ConsistentHashing(t *testing.T) {
	router := NewRouter()

	// Consistent hashing should return the exact same region deterministically for the same wallet ID
	walletID := "unregistered-wallet-random-999"
	reg1, err := router.GetHomeRegion(walletID)
	if err != nil {
		t.Fatalf("GetHomeRegion failed: %v", err)
	}

	reg2, err := router.GetHomeRegion(walletID)
	if err != nil {
		t.Fatalf("GetHomeRegion failed: %v", err)
	}

	if reg1.ID != reg2.ID {
		t.Errorf("expected consistent region, got %s and %s", reg1.ID, reg2.ID)
	}

	validRegions := map[string]bool{
		RegionUSEast:      true,
		RegionEUWest:      true,
		RegionAPSoutheast: true,
	}
	if !validRegions[reg1.ID] {
		t.Errorf("expected valid regional cluster ID, got %s", reg1.ID)
	}
}

func TestRouter_FailoverOnRegionUnhealthy(t *testing.T) {
	router := NewRouter()

	// Mark EU and AP as unhealthy, leaving only US East
	err := router.SetRegionHealth(RegionEUWest, false)
	if err != nil {
		t.Fatalf("SetRegionHealth failed: %v", err)
	}
	err = router.SetRegionHealth(RegionAPSoutheast, false)
	if err != nil {
		t.Fatalf("SetRegionHealth failed: %v", err)
	}

	// Any unassigned wallet must route to the only healthy region (RegionUSEast)
	for i := 0; i < 20; i++ {
		wID := "test-wallet-" + string(rune('A'+i))
		reg, err := router.GetHomeRegion(wID)
		if err != nil {
			t.Fatalf("GetHomeRegion failed: %v", err)
		}
		if reg.ID != RegionUSEast {
			t.Errorf("expected failover to %s, got %s", RegionUSEast, reg.ID)
		}
	}

	// Mark all unhealthy -> should return ErrNoHealthyRegions
	err = router.SetRegionHealth(RegionUSEast, false)
	if err != nil {
		t.Fatalf("SetRegionHealth failed: %v", err)
	}
	_, err = router.GetHomeRegion("any-wallet")
	if err != ErrNoHealthyRegions {
		t.Errorf("expected ErrNoHealthyRegions, got %v", err)
	}
}

func TestRouter_CoordinateCrossRegionTransfer(t *testing.T) {
	router := NewRouter()

	// Register explicit residencies for deterministic test
	srcUS := "wallet-alice-us"
	dstEU := "wallet-bob-eu"
	dstUS := "wallet-charlie-us"

	_ = router.RegisterResidency(srcUS, RegionUSEast)
	_ = router.RegisterResidency(dstEU, RegionEUWest)
	_ = router.RegisterResidency(dstUS, RegionUSEast)

	// 1. Intra-region transfer (Alice US -> Charlie US)
	resLocal, err := router.CoordinateCrossRegionTransfer("tx-local-01", srcUS, dstUS, 150.0, "USD")
	if err != nil {
		t.Fatalf("CoordinateCrossRegionTransfer intra-region failed: %v", err)
	}
	if resLocal.TransferType != TransferTypeIntraRegion {
		t.Errorf("expected %s, got %s", TransferTypeIntraRegion, resLocal.TransferType)
	}
	if resLocal.SourceRegion != RegionUSEast || resLocal.TargetRegion != RegionUSEast {
		t.Errorf("unexpected regions: src=%s dst=%s", resLocal.SourceRegion, resLocal.TargetRegion)
	}
	if resLocal.Status != "SUCCESS" {
		t.Errorf("expected SUCCESS, got %s", resLocal.Status)
	}

	// 2. Cross-region transfer (Alice US -> Bob EU)
	resCross, err := router.CoordinateCrossRegionTransfer("tx-cross-01", srcUS, dstEU, 500.0, "EUR")
	if err != nil {
		t.Fatalf("CoordinateCrossRegionTransfer cross-region failed: %v", err)
	}
	if resCross.TransferType != TransferTypeCrossRegion {
		t.Errorf("expected %s, got %s", TransferTypeCrossRegion, resCross.TransferType)
	}
	if resCross.SourceRegion != RegionUSEast || resCross.TargetRegion != RegionEUWest {
		t.Errorf("unexpected regions: src=%s dst=%s", resCross.SourceRegion, resCross.TargetRegion)
	}
	if resCross.Phase1Status != "PREPARED_DEBIT_HELD" || resCross.Phase2Status != "COMMITTED_CREDIT_FINAL" {
		t.Errorf("unexpected 2PC phases: %s, %s", resCross.Phase1Status, resCross.Phase2Status)
	}
}

func TestRouter_ValidationAndListing(t *testing.T) {
	router := NewRouter()

	// Invalid empty wallet
	_, err := router.GetHomeRegion("")
	if err != ErrInvalidWalletID {
		t.Errorf("expected ErrInvalidWalletID, got: %v", err)
	}

	err = router.RegisterResidency("", RegionUSEast)
	if err != ErrInvalidWalletID {
		t.Errorf("expected ErrInvalidWalletID, got: %v", err)
	}

	err = router.RegisterResidency("wallet-1", "invalid-region")
	if err != ErrUnknownRegion {
		t.Errorf("expected ErrUnknownRegion, got: %v", err)
	}

	regions := router.ListRegions()
	if len(regions) != 3 {
		t.Errorf("expected 3 regions, got %d", len(regions))
	}
}
