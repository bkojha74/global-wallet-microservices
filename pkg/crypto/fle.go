package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

var (
	ErrInvalidCipherFormat = errors.New("invalid field-level encryption format: expected kms:<version>:<iv>:<ciphertext>:<tag>")
	ErrDecryptionFailed    = errors.New("decryption failed: authentication tag mismatch or corrupted ciphertext")
	ErrKeyVersionNotFound  = errors.New("key encryption key (KEK) version not found")
	ErrEmptyPlaintext      = errors.New("plaintext cannot be empty")
)

const (
	// Prefix for field-level encrypted strings
	EnvelopePrefix = "kms"
	// KeySize256 is 32 bytes for AES-256
	KeySize256 = 32
	// NonceSize is 12 bytes standard for AES-GCM
	NonceSize = 12
	// TagSize is 16 bytes for standard GCM tag
	TagSize = 16
)

// KeyManager defines the interface for KMS / Vault Transit Master Key Encryption.
type KeyManager interface {
	WrapKey(ctx context.Context, dek []byte) (wrappedKey []byte, keyVersion string, err error)
	UnwrapKey(ctx context.Context, wrappedKey []byte, keyVersion string) (dek []byte, err error)
	CurrentKeyVersion() string
}

// LocalKMSKeyManager provides an in-memory and software-backed Key Management Service
// supporting master key encryption and versioning.
type LocalKMSKeyManager struct {
	mu             sync.RWMutex
	currentVersion string
	keys           map[string][]byte
}

// NewLocalKMSKeyManager initializes a KMS manager with an initial version "v1"
// derived from masterSecret (or cryptographically generated if empty).
func NewLocalKMSKeyManager(masterSecret string) *LocalKMSKeyManager {
	mgr := &LocalKMSKeyManager{
		currentVersion: "v1",
		keys:           make(map[string][]byte),
	}

	var key []byte
	if strings.TrimSpace(masterSecret) != "" {
		hash := sha256.Sum256([]byte(masterSecret))
		key = hash[:]
	} else {
		key = make([]byte, KeySize256)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			panic(fmt.Sprintf("failed to initialize cryptographically secure KMS key: %v", err))
		}
	}
	mgr.keys["v1"] = key
	return mgr
}

// AddKeyVersion adds or rotates a new master key version.
func (k *LocalKMSKeyManager) AddKeyVersion(version string, key []byte, setAsCurrent bool) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	if len(key) != KeySize256 {
		hash := sha256.Sum256(key)
		key = hash[:]
	}

	k.keys[version] = key
	if setAsCurrent {
		k.currentVersion = version
	}
	return nil
}

func (k *LocalKMSKeyManager) CurrentKeyVersion() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.currentVersion
}

// WrapKey encrypts an ephemeral Data Encryption Key (DEK) with the active Key Encryption Key (KEK).
func (k *LocalKMSKeyManager) WrapKey(_ context.Context, dek []byte) ([]byte, string, error) {
	k.mu.RLock()
	version := k.currentVersion
	kek, exists := k.keys[version]
	k.mu.RUnlock()

	if !exists {
		return nil, "", ErrKeyVersionNotFound
	}

	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, "", err
	}

	wrapped := gcm.Seal(nonce, nonce, dek, nil)
	return wrapped, version, nil
}

// UnwrapKey decrypts a wrapped Data Encryption Key (DEK) using the specified KEK version.
func (k *LocalKMSKeyManager) UnwrapKey(_ context.Context, wrappedKey []byte, keyVersion string) ([]byte, error) {
	k.mu.RLock()
	kek, exists := k.keys[keyVersion]
	k.mu.RUnlock()

	if !exists {
		return nil, ErrKeyVersionNotFound
	}

	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(wrappedKey) < nonceSize {
		return nil, ErrDecryptionFailed
	}

	nonce, ciphertext := wrappedKey[:nonceSize], wrappedKey[nonceSize:]
	dek, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrDecryptionFailed
	}
	return dek, nil
}

// EnvelopeCipher handles field-level encryption (FLE) for PII and sensitive banking records.
type EnvelopeCipher struct {
	kms KeyManager
}

// NewEnvelopeCipher creates a new field-level encryption cipher.
func NewEnvelopeCipher(kms KeyManager) *EnvelopeCipher {
	return &EnvelopeCipher{kms: kms}
}

// EncryptField encrypts a plaintext string into a versioned envelope string:
// kms:<version>:<base64-iv>:<base64-ciphertext>:<base64-tag>
func (c *EnvelopeCipher) EncryptField(ctx context.Context, plaintext string) (string, error) {
	if plaintext == "" {
		return "", ErrEmptyPlaintext
	}

	// 1. Generate an ephemeral 256-bit Data Encryption Key (DEK)
	dek := make([]byte, KeySize256)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return "", fmt.Errorf("failed to generate ephemeral DEK: %w", err)
	}

	// 2. Wrap DEK with KMS
	wrappedDEK, version, err := c.kms.WrapKey(ctx, dek)
	if err != nil {
		return "", fmt.Errorf("failed to wrap DEK: %w", err)
	}

	// 3. Encrypt plaintext with DEK using AES-256-GCM
	block, err := aes.NewCipher(dek)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	iv := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", err
	}

	// Seal appends ciphertext + 16-byte tag
	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	if len(sealed) < TagSize {
		return "", errors.New("ciphertext too short")
	}

	ciphertext := sealed[:len(sealed)-TagSize]
	tag := sealed[len(sealed)-TagSize:]

	// Format: kms:<version>:<iv_b64>:<ciphertext_b64>:<tag_b64>:<wrapped_dek_b64>
	envelope := fmt.Sprintf("%s:%s:%s:%s:%s:%s",
		EnvelopePrefix,
		version,
		base64.StdEncoding.EncodeToString(iv),
		base64.StdEncoding.EncodeToString(ciphertext),
		base64.StdEncoding.EncodeToString(tag),
		base64.StdEncoding.EncodeToString(wrappedDEK),
	)

	return envelope, nil
}

// DecryptField decrypts a versioned envelope string back to plaintext.
func (c *EnvelopeCipher) DecryptField(ctx context.Context, envelope string) (string, error) {
	parts := strings.Split(envelope, ":")
	if len(parts) != 6 || parts[0] != EnvelopePrefix {
		return "", ErrInvalidCipherFormat
	}

	version := parts[1]
	iv, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("failed to decode IV: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return "", fmt.Errorf("failed to decode ciphertext: %w", err)
	}
	tag, err := base64.StdEncoding.DecodeString(parts[4])
	if err != nil {
		return "", fmt.Errorf("failed to decode tag: %w", err)
	}
	wrappedDEK, err := base64.StdEncoding.DecodeString(parts[5])
	if err != nil {
		return "", fmt.Errorf("failed to decode wrapped DEK: %w", err)
	}

	// 1. Unwrap DEK using KMS
	dek, err := c.kms.UnwrapKey(ctx, wrappedDEK, version)
	if err != nil {
		return "", fmt.Errorf("failed to unwrap DEK: %w", err)
	}

	// 2. Decrypt ciphertext with unwrapped DEK
	block, err := aes.NewCipher(dek)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	sealed := append(ciphertext, tag...)
	plaintextBytes, err := gcm.Open(nil, iv, sealed, nil)
	if err != nil {
		return "", ErrDecryptionFailed
	}

	return string(plaintextBytes), nil
}

// MaskSensitive masks sensitive PII for audit logging and display, showing only prefix and suffix.
// Example: "1234567890" with prefix=2, suffix=4 -> "12****7890"
func MaskSensitive(val string, visiblePrefix, visibleSuffix int) string {
	trimmed := strings.TrimSpace(val)
	if len(trimmed) <= visiblePrefix+visibleSuffix {
		return strings.Repeat("*", len(trimmed))
	}

	prefix := trimmed[:visiblePrefix]
	suffix := trimmed[len(trimmed)-visibleSuffix:]
	maskLen := len(trimmed) - visiblePrefix - visibleSuffix
	return prefix + strings.Repeat("*", maskLen) + suffix
}
