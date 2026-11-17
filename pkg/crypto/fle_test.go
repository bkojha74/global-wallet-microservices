package crypto

import (
	"context"
	"strings"
	"testing"
)

func TestEnvelopeCipher_Roundtrip(t *testing.T) {
	kms := NewLocalKMSKeyManager("super-secret-master-key-2026")
	cipher := NewEnvelopeCipher(kms)

	ctx := context.Background()
	plaintexts := []string{
		"US-TAX-123456789",
		"4111222233334444",
		"100 Main Street, Suite 400, New York, NY 10001",
		"GB74BARK20000012345678",
	}

	for _, pt := range plaintexts {
		enc, err := cipher.EncryptField(ctx, pt)
		if err != nil {
			t.Fatalf("encryption failed for %q: %v", pt, err)
		}
		if !strings.HasPrefix(enc, "kms:v1:") {
			t.Fatalf("expected prefix 'kms:v1:', got: %s", enc)
		}

		dec, err := cipher.DecryptField(ctx, enc)
		if err != nil {
			t.Fatalf("decryption failed for %q: %v", pt, err)
		}
		if dec != pt {
			t.Fatalf("roundtrip mismatch: expected %q, got %q", pt, dec)
		}
	}
}

func TestEnvelopeCipher_KeyRotation(t *testing.T) {
	kms := NewLocalKMSKeyManager("master-key-version-1")
	cipher := NewEnvelopeCipher(kms)
	ctx := context.Background()

	// 1. Encrypt with v1
	v1Plain := "National-ID-Encrypted-Under-V1"
	encV1, err := cipher.EncryptField(ctx, v1Plain)
	if err != nil {
		t.Fatalf("v1 encryption failed: %v", err)
	}
	if !strings.HasPrefix(encV1, "kms:v1:") {
		t.Fatalf("expected v1 prefix, got %s", encV1)
	}

	// 2. Rotate to v2
	newKey := []byte("new-master-key-version-2-32bytes")
	if err := kms.AddKeyVersion("v2", newKey, true); err != nil {
		t.Fatalf("failed to rotate key: %v", err)
	}
	if kms.CurrentKeyVersion() != "v2" {
		t.Fatalf("expected current version 'v2', got %s", kms.CurrentKeyVersion())
	}

	// 3. Encrypt new data under v2
	v2Plain := "National-ID-Encrypted-Under-V2"
	encV2, err := cipher.EncryptField(ctx, v2Plain)
	if err != nil {
		t.Fatalf("v2 encryption failed: %v", err)
	}
	if !strings.HasPrefix(encV2, "kms:v2:") {
		t.Fatalf("expected v2 prefix, got %s", encV2)
	}

	// 4. Verify BOTH v1 and v2 can still be decrypted seamlessly
	decV1, err := cipher.DecryptField(ctx, encV1)
	if err != nil {
		t.Fatalf("failed to decrypt v1 payload after rotation: %v", err)
	}
	if decV1 != v1Plain {
		t.Fatalf("v1 decryption mismatch: expected %q, got %q", v1Plain, decV1)
	}

	decV2, err := cipher.DecryptField(ctx, encV2)
	if err != nil {
		t.Fatalf("failed to decrypt v2 payload: %v", err)
	}
	if decV2 != v2Plain {
		t.Fatalf("v2 decryption mismatch: expected %q, got %q", v2Plain, decV2)
	}
}

func TestEnvelopeCipher_TamperResistance(t *testing.T) {
	kms := NewLocalKMSKeyManager("tamper-master-key")
	cipher := NewEnvelopeCipher(kms)
	ctx := context.Background()

	enc, err := cipher.EncryptField(ctx, "SecretBankingRecord")
	if err != nil {
		t.Fatalf("encryption failed: %v", err)
	}

	parts := strings.Split(enc, ":")
	if len(parts) != 6 {
		t.Fatalf("unexpected envelope format: %s", enc)
	}

	// 1. Tamper with ciphertext
	tamperedCipher := parts[0] + ":" + parts[1] + ":" + parts[2] + ":AAAA" + parts[3][4:] + ":" + parts[4] + ":" + parts[5]
	_, err = cipher.DecryptField(ctx, tamperedCipher)
	if err == nil {
		t.Fatalf("expected decryption error on tampered ciphertext, but got nil")
	}

	// 2. Tamper with tag
	tamperedTag := parts[0] + ":" + parts[1] + ":" + parts[2] + ":" + parts[3] + ":BBBB" + parts[4][4:] + ":" + parts[5]
	_, err = cipher.DecryptField(ctx, tamperedTag)
	if err == nil {
		t.Fatalf("expected decryption error on tampered tag, but got nil")
	}

	// 3. Unknown key version
	unknownVersion := "kms:v999:" + parts[2] + ":" + parts[3] + ":" + parts[4] + ":" + parts[5]
	_, err = cipher.DecryptField(ctx, unknownVersion)
	if err == nil {
		t.Fatalf("expected error for unknown key version, got nil")
	}

	// 4. Invalid envelope format
	_, err = cipher.DecryptField(ctx, "invalid-envelope-string")
	if err != ErrInvalidCipherFormat {
		t.Fatalf("expected ErrInvalidCipherFormat, got %v", err)
	}
}

func TestMaskSensitive(t *testing.T) {
	tests := []struct {
		input          string
		visiblePrefix  int
		visibleSuffix  int
		expectedOutput string
	}{
		{"1234567890", 2, 4, "12****7890"},
		{"US-TAX-998877", 3, 4, "US-******8877"},
		{"short", 3, 3, "*****"},
		{"exact6", 3, 3, "******"},
		{"", 2, 2, ""},
	}

	for _, tc := range tests {
		actual := MaskSensitive(tc.input, tc.visiblePrefix, tc.visibleSuffix)
		if actual != tc.expectedOutput {
			t.Errorf("MaskSensitive(%q, %d, %d): expected %q, got %q", tc.input, tc.visiblePrefix, tc.visibleSuffix, tc.expectedOutput, actual)
		}
	}
}
