package webhook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultSignatureTolerance is the maximum permissible age of a webhook payload to prevent replay attacks.
const DefaultSignatureTolerance = 5 * time.Minute

// GenerateSecret produces a cryptographically secure 32-byte hexadecimal signing secret.
func GenerateSecret() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate secure random secret: %w", err)
	}
	return "whsec_" + hex.EncodeToString(bytes), nil
}

// ComputeSignature calculates the HMAC-SHA256 digest over the canonical string:
// canonical = "<timestamp>.<payload>"
func ComputeSignature(secret string, timestamp int64, payload []byte) string {
	cleanSecret := strings.TrimPrefix(secret, "whsec_")
	mac := hmac.New(sha256.New, []byte(cleanSecret))
	canonical := fmt.Sprintf("%d.%s", timestamp, string(payload))
	mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

// FormatSignatureHeader constructs the industry-standard X-Wallet-Signature header string.
func FormatSignatureHeader(timestamp int64, signatureHex string) string {
	return fmt.Sprintf("t=%d,v1=%s", timestamp, signatureHex)
}

// SignPayload computes the HMAC-SHA256 signature and returns the full X-Wallet-Signature header value.
func SignPayload(secret string, timestamp int64, payload []byte) string {
	sig := ComputeSignature(secret, timestamp, payload)
	return FormatSignatureHeader(timestamp, sig)
}

// ParseSignatureHeader extracts timestamp and v1 hex signature from the X-Wallet-Signature header.
func ParseSignatureHeader(header string) (int64, string, error) {
	if header == "" {
		return 0, "", fmt.Errorf("missing signature header")
	}

	parts := strings.Split(header, ",")
	var timestamp int64
	var signature string

	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts, err := strconv.ParseInt(kv[1], 10, 64)
			if err != nil {
				return 0, "", fmt.Errorf("invalid timestamp format: %w", err)
			}
			timestamp = ts
		case "v1":
			signature = kv[1]
		}
	}

	if timestamp == 0 || signature == "" {
		return 0, "", fmt.Errorf("incomplete signature header (requires t= and v1=)")
	}

	return timestamp, signature, nil
}

// VerifySignature validates that:
// 1. The timestamp is within the acceptable clock skew tolerance window (mitigating replay attacks).
// 2. The HMAC-SHA256 digest matches the payload in constant time (mitigating timing attacks).
func VerifySignature(secret, header string, payload []byte, tolerance time.Duration) bool {
	timestamp, receivedSig, err := ParseSignatureHeader(header)
	if err != nil {
		return false
	}

	if tolerance > 0 {
		now := time.Now().UTC().Unix()
		diff := now - timestamp
		if diff < 0 {
			diff = -diff
		}
		if time.Duration(diff)*time.Second > tolerance {
			return false // Stale or future timestamp - potential replay attack
		}
	}

	expectedSig := ComputeSignature(secret, timestamp, payload)

	// Constant-time comparison prevents timing analysis side-channel attacks
	return hmac.Equal([]byte(receivedSig), []byte(expectedSig))
}
