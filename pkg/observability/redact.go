package observability

import (
	"strings"
)

var sensitiveKeys = map[string]struct{}{
	"password":      {},
	"passwd":        {},
	"secret":        {},
	"token":         {},
	"access_token":  {},
	"refresh_token": {},
	"authorization": {},
	"auth":          {},
	"api_key":       {},
	"apikey":        {},
	"pin":           {},
	"cvv":           {},
	"credit_card":   {},
	"card_number":   {},
}

// RedactAttributes returns a shallow or recursive copy of attributes with sensitive key values masked as "[REDACTED]".
func RedactAttributes(attributes map[string]any) map[string]any {
	if attributes == nil {
		return nil
	}
	redacted := make(map[string]any, len(attributes))
	for k, v := range attributes {
		lowerKey := strings.ToLower(strings.TrimSpace(k))
		if _, isSensitive := sensitiveKeys[lowerKey]; isSensitive {
			redacted[k] = "[REDACTED]"
			continue
		}
		// If nested map, redact recursively
		if nestedMap, ok := v.(map[string]any); ok {
			redacted[k] = RedactAttributes(nestedMap)
		} else {
			redacted[k] = v
		}
	}
	return redacted
}
