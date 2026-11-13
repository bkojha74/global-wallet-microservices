// Package banking provides international banking identifier validation and parsing
// complying with ISO 13616 (IBAN) and ISO 9362 (BIC / SWIFT).
package banking

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

var (
	ErrInvalidIBANFormat   = errors.New("invalid IBAN format")
	ErrInvalidIBANLength   = errors.New("invalid IBAN length for country")
	ErrInvalidIBANChecksum = errors.New("invalid IBAN Mod 97-10 checksum")
	ErrInvalidBICFormat    = errors.New("invalid BIC/SWIFT format")
	ErrInvalidCountryCode  = errors.New("invalid ISO country code")

	ibanRegex = regexp.MustCompile(`^[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}$`)
	bicRegex  = regexp.MustCompile(`^[A-Z]{4}[A-Z]{2}[A-Z0-9]{2}([A-Z0-9]{3})?$`)

	// Standard IBAN lengths per ISO 13616
	countryIBANLengths = map[string]int{
		"AL": 28, "AD": 24, "AT": 20, "AZ": 28, "BH": 22, "BY": 28, "BE": 16,
		"BA": 20, "BR": 29, "BG": 22, "CR": 22, "HR": 21, "CY": 28, "CZ": 24,
		"DK": 18, "DO": 28, "EE": 20, "FO": 18, "FI": 18, "FR": 27, "GE": 22,
		"DE": 22, "GI": 23, "GR": 27, "GL": 18, "GT": 28, "HU": 28, "IS": 26,
		"IE": 22, "IL": 23, "IT": 27, "JO": 30, "KZ": 20, "KW": 30, "LV": 21,
		"LB": 28, "LI": 21, "LT": 20, "LU": 20, "MK": 19, "MT": 31, "MR": 27,
		"MU": 30, "MD": 24, "MC": 27, "ME": 22, "NL": 18, "NO": 15, "PK": 24,
		"PS": 29, "PL": 28, "PT": 25, "QA": 29, "RO": 24, "SM": 27, "SA": 24,
		"RS": 22, "SK": 24, "SI": 19, "ES": 24, "SE": 24, "CH": 21, "TN": 24,
		"TR": 26, "AE": 23, "GB": 22, "US": 24, // US Virtual IBAN standard for BaaS
	}
)

// CleanIBAN removes whitespace and hyphens, and converts to uppercase.
func CleanIBAN(raw string) string {
	cleaned := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(raw, " ", ""), "-", ""))
	return cleaned
}

// ValidateIBAN performs syntax, length, and ISO 7064 Mod 97-10 checksum validation.
func ValidateIBAN(raw string) error {
	iban := CleanIBAN(raw)
	if len(iban) < 15 || len(iban) > 34 {
		return ErrInvalidIBANFormat
	}
	if !ibanRegex.MatchString(iban) {
		return ErrInvalidIBANFormat
	}

	country := iban[:2]
	expectedLen, known := countryIBANLengths[country]
	if known && len(iban) != expectedLen {
		return fmt.Errorf("%w: country %s expected %d chars, got %d", ErrInvalidIBANLength, country, expectedLen, len(iban))
	}

	// Rearrange: move the first 4 characters to the end
	rearranged := iban[4:] + iban[:4]

	// Convert letters A-Z to numbers 10-35
	var numericStr strings.Builder
	for _, char := range rearranged {
		if char >= '0' && char <= '9' {
			numericStr.WriteRune(char)
		} else if char >= 'A' && char <= 'Z' {
			fmt.Fprintf(&numericStr, "%d", char-'A'+10)
		} else {
			return ErrInvalidIBANFormat
		}
	}

	// Compute Mod 97 using math/big to prevent 64-bit integer overflow
	num := new(big.Int)
	num, ok := num.SetString(numericStr.String(), 10)
	if !ok {
		return ErrInvalidIBANFormat
	}

	mod97 := new(big.Int)
	mod97.Mod(num, big.NewInt(97))

	if mod97.Int64() != 1 {
		return ErrInvalidIBANChecksum
	}

	return nil
}

// GenerateIBAN computes the ISO 7064 Mod 97-10 check digits for a given country code and BBAN.
func GenerateIBAN(countryCode, bban string) (string, error) {
	countryCode = strings.ToUpper(strings.TrimSpace(countryCode))
	bban = CleanIBAN(bban)
	if len(countryCode) != 2 {
		return "", ErrInvalidCountryCode
	}

	// Expected total length
	expectedLen, known := countryIBANLengths[countryCode]
	if known && len(bban)+4 != expectedLen {
		return "", fmt.Errorf("%w: for %s BBAN must be %d chars, got %d", ErrInvalidIBANLength, countryCode, expectedLen-4, len(bban))
	}

	// Rearrange with check digits "00" at the end
	rearranged := bban + countryCode + "00"

	var numericStr strings.Builder
	for _, char := range rearranged {
		if char >= '0' && char <= '9' {
			numericStr.WriteRune(char)
		} else if char >= 'A' && char <= 'Z' {
			fmt.Fprintf(&numericStr, "%d", char-'A'+10)
		} else {
			return "", ErrInvalidIBANFormat
		}
	}

	num := new(big.Int)
	num, ok := num.SetString(numericStr.String(), 10)
	if !ok {
		return "", ErrInvalidIBANFormat
	}

	mod97 := new(big.Int)
	mod97.Mod(num, big.NewInt(97))

	// Check digits = 98 - (rearranged_num mod 97)
	checkDigits := 98 - mod97.Int64()

	return fmt.Sprintf("%s%02d%s", countryCode, checkDigits, bban), nil
}
