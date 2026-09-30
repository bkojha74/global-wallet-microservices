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

func TestConvertUnitsScaleBankers(t *testing.T) {
	// 1. Same scale (USD -> EUR, both scale 2)
	// $10.00 USD (1000 cents) at 0.92 = 920 cents EUR
	res := ConvertUnitsScaleBankers(1000, "USD", "EUR", 0.92)
	if res != 920 {
		t.Fatalf("expected 920 EUR cents, got %d", res)
	}

	// 2. High scale to low scale (USD [scale 2] -> JPY [scale 0])
	// $1.00 USD (100 cents) at 150.00 = 150 JPY
	resJPY := ConvertUnitsScaleBankers(100, "USD", "JPY", 150.0)
	if resJPY != 150 {
		t.Fatalf("expected 150 JPY, got %d", resJPY)
	}

	// 3. Low scale to high scale (JPY [scale 0] -> USD [scale 2])
	// 150 JPY at 1/150 (0.00666667) = 100 cents USD ($1.00)
	resUSD := ConvertUnitsScaleBankers(150, "JPY", "USD", 1.0/150.0)
	if resUSD != 100 {
		t.Fatalf("expected 100 USD cents, got %d", resUSD)
	}

	// 4. Low scale to 3-decimal scale (USD [scale 2] -> BHD [scale 3])
	// $1.00 USD (100 cents) at 0.376 = 376 fils BHD
	resBHD := ConvertUnitsScaleBankers(100, "USD", "BHD", 0.376)
	if resBHD != 376 {
		t.Fatalf("expected 376 BHD fils, got %d", resBHD)
	}
}
