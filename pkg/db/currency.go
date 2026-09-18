package db

import (
	"fmt"
	"strings"
)

// SupportedCurrencies maps standard ISO-4217 codes to their standard minor unit decimal scale.
var SupportedCurrencies = map[string]int{
	"USD": 2, // US Dollar (Cents)
	"EUR": 2, // Euro (Cents)
	"GBP": 2, // British Pound (Pence)
	"CAD": 2, // Canadian Dollar (Cents)
	"AUD": 2, // Australian Dollar (Cents)
	"JPY": 0, // Japanese Yen (Zero minor units)
	"CHF": 2, // Swiss Franc (Rappen)
	"SGD": 2, // Singapore Dollar (Cents)
	"INR": 2, // Indian Rupee (Paise)
	"BHD": 3, // Bahraini Dinar (Fils)
}

// IsValidCurrency checks if the given code is a supported ISO-4217 currency.
func IsValidCurrency(code string) bool {
	_, ok := SupportedCurrencies[strings.ToUpper(strings.TrimSpace(code))]
	return ok
}

// CurrencyScale returns the decimal scale (minor unit exponent) for the given currency.
func CurrencyScale(code string) (int, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	scale, ok := SupportedCurrencies[code]
	if !ok {
		return 0, fmt.Errorf("unsupported or invalid ISO-4217 currency: %s", code)
	}
	return scale, nil
}

// ValidateAmount validates that the amount is positive, non-zero, and belongs to a supported currency.
func ValidateAmount(units int64, currency string) error {
	if units <= 0 {
		return fmt.Errorf("amount units must be strictly positive, got: %d", units)
	}
	if !IsValidCurrency(currency) {
		return fmt.Errorf("invalid or unsupported currency: %s", currency)
	}
	return nil
}
