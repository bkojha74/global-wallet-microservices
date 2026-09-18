package db

import (
	"testing"
)

func TestCurrencyValidation(t *testing.T) {
	validCurrencies := []string{"USD", "usd", "EUR", "gbp", "jpy", "INR"}
	for _, curr := range validCurrencies {
		if !IsValidCurrency(curr) {
			t.Errorf("expected currency %s to be valid", curr)
		}
	}

	invalidCurrencies := []string{"XYZ", "ABC", "123", "", "FAKE"}
	for _, curr := range invalidCurrencies {
		if IsValidCurrency(curr) {
			t.Errorf("expected currency %s to be invalid", curr)
		}
	}
}

func TestCurrencyScale(t *testing.T) {
	scale, err := CurrencyScale("USD")
	if err != nil || scale != 2 {
		t.Fatalf("expected USD scale 2, got %d, err: %v", scale, err)
	}

	scale, err = CurrencyScale("JPY")
	if err != nil || scale != 0 {
		t.Fatalf("expected JPY scale 0, got %d, err: %v", scale, err)
	}

	scale, err = CurrencyScale("BHD")
	if err != nil || scale != 3 {
		t.Fatalf("expected BHD scale 3, got %d, err: %v", scale, err)
	}

	_, err = CurrencyScale("INVALID")
	if err == nil {
		t.Fatal("expected error for invalid currency scale")
	}
}

func TestValidateAmount(t *testing.T) {
	if err := ValidateAmount(100, "USD"); err != nil {
		t.Errorf("expected valid amount, got %v", err)
	}

	if err := ValidateAmount(0, "USD"); err == nil {
		t.Error("expected error for zero amount")
	}

	if err := ValidateAmount(-50, "USD"); err == nil {
		t.Error("expected error for negative amount")
	}

	if err := ValidateAmount(100, "INVALID"); err == nil {
		t.Error("expected error for invalid currency")
	}
}
