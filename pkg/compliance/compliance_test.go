package compliance

import (
	"encoding/json"
	"testing"
)

func TestValidateTravelRule(t *testing.T) {
	// 1. Below threshold ($500) -> Should pass without payload
	if err := ValidateTravelRule(500.0, "USD", nil); err != nil {
		t.Fatalf("expected nil error for below-threshold transfer, got: %v", err)
	}

	// 2. At or above threshold ($1,000) with nil payload -> Should fail
	if err := ValidateTravelRule(1000.0, "USD", nil); err != ErrMissingTravelRulePayload {
		t.Fatalf("expected ErrMissingTravelRulePayload for $1,000 transfer, got: %v", err)
	}

	// 3. Above threshold with valid payload -> Should pass
	validPayload := &TravelRulePayload{
		TransferReference: "TR-REF-2026-001",
		Amount:            2500.0,
		Currency:          "EUR",
		Originator: TravelRuleParty{
			LegalName:       "Alice Finance GmbH",
			PhysicalAddress: "Kaiserstrasse 12, Frankfurt",
			AccountNumber:   "DE89370400440532013000",
			Country:         "DE",
		},
		Beneficiary: TravelRuleParty{
			LegalName:     "Bob Global Corp Ltd",
			AccountNumber: "GB29NWBK60161331926819",
			Country:       "GB",
		},
	}
	if err := ValidateTravelRule(2500.0, "EUR", validPayload); err != nil {
		t.Fatalf("expected valid travel rule payload to pass, got: %v", err)
	}

	// 4. Incomplete originator (missing address and national ID) -> Should fail
	invalidOrig := *validPayload
	invalidOrig.Originator.PhysicalAddress = ""
	invalidOrig.Originator.NationalID = ""
	if err := ValidateTravelRule(2500.0, "EUR", &invalidOrig); err == nil {
		t.Fatal("expected error for missing originator physical address/national ID, got nil")
	}

	// 5. Incomplete beneficiary (missing account number) -> Should fail
	invalidBenef := *validPayload
	invalidBenef.Beneficiary.AccountNumber = ""
	if err := ValidateTravelRule(2500.0, "EUR", &invalidBenef); err == nil {
		t.Fatal("expected error for missing beneficiary account number, got nil")
	}
}

func TestSanctionsEngine(t *testing.T) {
	engine := NewSanctionsEngine()

	// 1. Exact match on designated entity -> Should BLOCK
	res1 := engine.ScreenParty("Evil Corp Cyber Syndicate", "RU")
	if !res1.IsBlocked || res1.Action != "BLOCK" || res1.MatchScore < 0.95 {
		t.Fatalf("expected BLOCK for Evil Corp, got: %+v", res1)
	}

	// 2. Match on alias -> Should BLOCK
	res2 := engine.ScreenParty("Dridex Gang", "")
	if !res2.IsBlocked || res2.Action != "BLOCK" {
		t.Fatalf("expected BLOCK for Dridex Gang alias, got: %+v", res2)
	}

	// 3. Clean benign user -> Should CLEAR
	res3 := engine.ScreenParty("Alice Smith", "US")
	if res3.IsBlocked || res3.Action != "CLEAR" || res3.MatchScore >= 0.70 {
		t.Fatalf("expected CLEAR for Alice Smith, got: %+v", res3)
	}

	// 4. Near match / fuzzy review -> Should REVIEW or BLOCK
	res4 := engine.ScreenParty("Koryo Commercial Bank LLC", "KP")
	if res4.Action != "BLOCK" && res4.Action != "REVIEW" {
		t.Fatalf("expected BLOCK or REVIEW for Koryo Commercial Bank, got: %+v", res4)
	}
}

func TestGenerateSAR(t *testing.T) {
	parties := []TravelRuleParty{
		{
			LegalName:     "Suspect Account",
			AccountNumber: "wal_suspect_1001",
			Country:       "US",
		},
	}
	sarBytes, err := GenerateSAR("SAR-2026-999", "Suspicious structuring pattern detected: 5 rapid sub-threshold wire transfers", parties, 49750.0, "USD")
	if err != nil {
		t.Fatalf("failed to generate SAR report: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(sarBytes, &parsed); err != nil {
		t.Fatalf("failed to parse generated SAR JSON: %v", err)
	}
	if parsed["report_id"] != "SAR-2026-999" || parsed["amount"] != 49750.0 {
		t.Fatalf("unexpected SAR content: %+v", parsed)
	}
}
