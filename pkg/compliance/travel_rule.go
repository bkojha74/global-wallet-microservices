package compliance

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrMissingTravelRulePayload  = errors.New("FATF Travel Rule violation: cross-border transfer exceeds threshold ($1,000) but missing required legal party payload")
	ErrIncompleteOriginatorInfo  = errors.New("FATF Travel Rule violation: originator must specify full legal name and physical address or national ID")
	ErrIncompleteBeneficiaryInfo = errors.New("FATF Travel Rule violation: beneficiary must specify full legal name and account number")
	ErrSanctionsBlocked          = errors.New("transaction blocked: matched international sanctions screening list")
)

// Default thresholds for FATF Recommendation 16 cross-border travel rule
const (
	ThresholdUSD = 1000.0
	ThresholdEUR = 1000.0
	ThresholdGBP = 1000.0
)

// TravelRuleParty represents the validated legal entity in a cross-border wire/transfer.
type TravelRuleParty struct {
	LegalName       string `json:"legal_name"`
	PhysicalAddress string `json:"physical_address,omitempty"`
	AccountNumber   string `json:"account_number"`
	NationalID      string `json:"national_id,omitempty"` // Passport, SSN, or national tax ID
	Country         string `json:"country"`               // ISO 3166-1 alpha-2
	BICFI           string `json:"bic_fi,omitempty"`      // Intermediary / Agent BIC
}

// TravelRulePayload encapsulates FATF Recommendation 16 originator and beneficiary details.
type TravelRulePayload struct {
	TransferReference      string          `json:"transfer_reference"`
	Amount                 float64         `json:"amount"`
	Currency               string          `json:"currency"`
	Originator             TravelRuleParty `json:"originator"`
	Beneficiary            TravelRuleParty `json:"beneficiary"`
	RegulatoryJurisdiction string          `json:"regulatory_jurisdiction,omitempty"`
}

// RequiresTravelRule checks whether the given transfer amount and currency reach the FATF reporting threshold.
func RequiresTravelRule(amount float64, currency string) bool {
	upperCcy := strings.ToUpper(strings.TrimSpace(currency))
	switch upperCcy {
	case "USD":
		return amount >= ThresholdUSD
	case "EUR":
		return amount >= ThresholdEUR
	case "GBP":
		return amount >= ThresholdGBP
	default:
		// For other currencies, use conservative $1,000 USD equivalent standard
		return amount >= ThresholdUSD
	}
}

// ValidateTravelRule enforces FATF Recommendation 16 requirements for cross-border transactions.
func ValidateTravelRule(amount float64, currency string, payload *TravelRulePayload) error {
	if !RequiresTravelRule(amount, currency) {
		return nil
	}

	if payload == nil {
		return ErrMissingTravelRulePayload
	}

	// Validate Originator
	origName := strings.TrimSpace(payload.Originator.LegalName)
	origAddr := strings.TrimSpace(payload.Originator.PhysicalAddress)
	origNatID := strings.TrimSpace(payload.Originator.NationalID)
	origAcc := strings.TrimSpace(payload.Originator.AccountNumber)

	if origName == "" || (origAddr == "" && origNatID == "") || origAcc == "" {
		return fmt.Errorf("%w: legal name, account number, and (physical address or national ID) required", ErrIncompleteOriginatorInfo)
	}

	// Validate Beneficiary
	benefName := strings.TrimSpace(payload.Beneficiary.LegalName)
	benefAcc := strings.TrimSpace(payload.Beneficiary.AccountNumber)

	if benefName == "" || benefAcc == "" {
		return fmt.Errorf("%w: legal name and account number required", ErrIncompleteBeneficiaryInfo)
	}

	return nil
}
