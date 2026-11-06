package db

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// CurrencyInfo holds complete ISO-4217 standard metadata for a currency.
type CurrencyInfo struct {
	Code        string `json:"code" bson:"code"`                 // ISO-4217 Alphabetic code (e.g., "USD")
	NumericCode string `json:"numeric_code" bson:"numeric_code"` // ISO-4217 3-digit numeric code (e.g., "840")
	Scale       int    `json:"scale" bson:"scale"`               // Minor unit decimal exponent (0, 2, or 3)
	Symbol      string `json:"symbol" bson:"symbol"`             // Standard display symbol
	Name        string `json:"name" bson:"name"`                 // Official ISO-4217 currency name
}

// ISO4217Catalog defines the global standard metadata for all supported currencies.
var ISO4217Catalog = map[string]CurrencyInfo{
	"USD": {Code: "USD", NumericCode: "840", Scale: 2, Symbol: "$", Name: "US Dollar"},
	"EUR": {Code: "EUR", NumericCode: "978", Scale: 2, Symbol: "€", Name: "Euro"},
	"GBP": {Code: "GBP", NumericCode: "826", Scale: 2, Symbol: "£", Name: "British Pound Sterling"},
	"CAD": {Code: "CAD", NumericCode: "124", Scale: 2, Symbol: "CA$", Name: "Canadian Dollar"},
	"AUD": {Code: "AUD", NumericCode: "036", Scale: 2, Symbol: "A$", Name: "Australian Dollar"},
	"JPY": {Code: "JPY", NumericCode: "392", Scale: 0, Symbol: "¥", Name: "Japanese Yen"},
	"KRW": {Code: "KRW", NumericCode: "410", Scale: 0, Symbol: "₩", Name: "South Korean Won"},
	"CHF": {Code: "CHF", NumericCode: "756", Scale: 2, Symbol: "CHF", Name: "Swiss Franc"},
	"SGD": {Code: "SGD", NumericCode: "702", Scale: 2, Symbol: "S$", Name: "Singapore Dollar"},
	"INR": {Code: "INR", NumericCode: "356", Scale: 2, Symbol: "₹", Name: "Indian Rupee"},
	"BHD": {Code: "BHD", NumericCode: "048", Scale: 3, Symbol: "BD", Name: "Bahraini Dinar"},
	"KWD": {Code: "KWD", NumericCode: "414", Scale: 3, Symbol: "KD", Name: "Kuwaiti Dinar"},
	"TND": {Code: "TND", NumericCode: "788", Scale: 3, Symbol: "DT", Name: "Tunisian Dinar"},
}

// SupportedCurrencies maps standard ISO-4217 codes to their standard minor unit decimal scale.
var SupportedCurrencies = map[string]int{
	"USD": 2, // US Dollar (Cents)
	"EUR": 2, // Euro (Cents)
	"GBP": 2, // British Pound (Pence)
	"CAD": 2, // Canadian Dollar (Cents)
	"AUD": 2, // Australian Dollar (Cents)
	"JPY": 0, // Japanese Yen (Zero minor units)
	"KRW": 0, // South Korean Won (Zero minor units)
	"CHF": 2, // Swiss Franc (Rappen)
	"SGD": 2, // Singapore Dollar (Cents)
	"INR": 2, // Indian Rupee (Paise)
	"BHD": 3, // Bahraini Dinar (Fils)
	"KWD": 3, // Kuwaiti Dinar (Fils)
	"TND": 3, // Tunisian Dinar (Millimes)
}

// NormalizeCurrency normalizes a currency code to uppercase trimmed ISO-4217 format.
func NormalizeCurrency(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// IsValidCurrency checks if the given code is a supported ISO-4217 currency.
func IsValidCurrency(code string) bool {
	_, ok := SupportedCurrencies[NormalizeCurrency(code)]
	return ok
}

// GetCurrencyInfo returns the ISO-4217 metadata for a currency code.
func GetCurrencyInfo(code string) (CurrencyInfo, bool) {
	info, ok := ISO4217Catalog[NormalizeCurrency(code)]
	return info, ok
}

// ListSupportedCurrencies returns all supported ISO-4217 currencies sorted alphabetically.
func ListSupportedCurrencies() []CurrencyInfo {
	list := make([]CurrencyInfo, 0, len(ISO4217Catalog))
	for _, info := range ISO4217Catalog {
		list = append(list, info)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Code < list[j].Code
	})
	return list
}

// CurrencyScale returns the decimal scale (minor unit exponent) for the given currency.
func CurrencyScale(code string) (int, error) {
	code = NormalizeCurrency(code)
	scale, ok := SupportedCurrencies[code]
	if !ok {
		return 0, fmt.Errorf("unsupported or invalid ISO-4217 currency: %s", code)
	}
	return scale, nil
}

// CurrencyScaleInt32 returns the decimal scale as an int32, bounded by ISO-4217 standard exponents.
func CurrencyScaleInt32(code string) (int32, error) {
	scale, err := CurrencyScale(code)
	if err != nil {
		return 0, err
	}
	if scale < 0 || scale > 18 {
		return 0, fmt.Errorf("scale out of bounds: %d", scale)
	}
	// #nosec G115 -- ISO-4217 minor unit scale exponent is verified in [0, 18]
	return int32(scale), nil
}

// ValidateAmount validates that the amount is positive, non-zero, and belongs to a supported currency.
func ValidateAmount(units int64, currency string) error {
	return ValidateMoney(currency, units, 0, 0)
}

// ValidateMoney validates a monetary amount taking into account ISO-4217 minor unit scale and fractional nanos:
// 1. Currency code must be a valid, supported ISO-4217 currency.
// 2. Units must be >= 0, and total amount (units + nanos) must be strictly positive.
// 3. Nanos must be in range [0, 999,999,999].
// 4. Zero-decimal currencies (JPY, KRW with scale 0) reject any fractional subunits (nanos != 0).
// 5. If scale is explicitly specified (scale != 0), it must match the ISO-4217 standard scale.
func ValidateMoney(currency string, units int64, nanos int32, scale int32) error {
	normCurr := NormalizeCurrency(currency)
	if !IsValidCurrency(normCurr) {
		return fmt.Errorf("invalid or unsupported currency: %s", currency)
	}

	expectedScale, err := CurrencyScaleInt32(normCurr)
	if err != nil {
		return err
	}

	if scale != 0 && scale != expectedScale {
		return fmt.Errorf("invalid scale %d for currency %s (expected %d)", scale, normCurr, expectedScale)
	}

	if nanos < 0 || nanos > 999_999_999 {
		return fmt.Errorf("nanos out of range [0, 999999999]: %d", nanos)
	}

	if units < 0 {
		return fmt.Errorf("amount units must be non-negative, got: %d", units)
	}

	if units == 0 && nanos == 0 {
		return fmt.Errorf("amount units must be strictly positive, got: 0")
	}

	// Zero-decimal currencies (e.g., JPY, KRW) cannot have fractional nanos
	if expectedScale == 0 && nanos != 0 {
		return fmt.Errorf("zero-decimal currency %s does not allow fractional subunits (got nanos=%d)", normCurr, nanos)
	}

	return nil
}

// ConvertUnitsBankers applies an exchange rate to a positive integer amount using
// IEEE 754 Round-Half-To-Even (Banker's Rounding) to eliminate systematic rounding bias.
// Note: This assumes identical minor unit scales for both currencies. For multi-scale conversions,
// use ConvertUnitsScaleBankers.
func ConvertUnitsBankers(sourceUnits int64, effectiveRate float64) int64 {
	if sourceUnits <= 0 || effectiveRate <= 0 {
		return 0
	}
	converted := math.RoundToEven(float64(sourceUnits) * effectiveRate)
	if converted < 1 && sourceUnits > 0 {
		return 1
	}
	return int64(converted)
}

// ConvertUnitsScaleBankers converts minor units from base currency to target currency
// accounting for differences in ISO-4217 minor unit decimal scales (e.g. USD cents [scale 2] -> JPY [scale 0] -> BHD fils [scale 3])
// and applies IEEE 754 Round-Half-To-Even (Banker's Rounding).
func ConvertUnitsScaleBankers(sourceUnits int64, baseCurrency, targetCurrency string, effectiveRate float64) int64 {
	if sourceUnits <= 0 || effectiveRate <= 0 {
		return 0
	}
	baseScale, err1 := CurrencyScale(baseCurrency)
	targetScale, err2 := CurrencyScale(targetCurrency)
	if err1 != nil || err2 != nil {
		return ConvertUnitsBankers(sourceUnits, effectiveRate)
	}

	scaleDiff := targetScale - baseScale
	scaleFactor := math.Pow(10, float64(scaleDiff))
	converted := math.RoundToEven(float64(sourceUnits) * effectiveRate * scaleFactor)
	if converted < 1 && sourceUnits > 0 {
		return 1
	}
	return int64(converted)
}
