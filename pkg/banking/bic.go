package banking

import (
	"strings"
)

// CleanBIC strips all whitespace and converts to uppercase.
func CleanBIC(raw string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(raw), " ", ""))
}

// ValidateBIC validates an ISO 9362 Bank Identifier Code (8 or 11 characters).
func ValidateBIC(raw string) error {
	bic := CleanBIC(raw)
	if len(bic) != 8 && len(bic) != 11 {
		return ErrInvalidBICFormat
	}
	if !bicRegex.MatchString(bic) {
		return ErrInvalidBICFormat
	}
	return nil
}
