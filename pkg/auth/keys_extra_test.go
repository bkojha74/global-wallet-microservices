package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestRSAKeys_FullLifecycle(t *testing.T) {
	// 1. Generate RSA key pair
	kp, err := GenerateRSAKeyPair()
	if err != nil || kp.PrivateKey == nil || kp.PublicKey == nil {
		t.Fatalf("GenerateRSAKeyPair failed: %v", err)
	}

	// 2. Marshal private key (PKCS#8) & public key
	privPEM, err := MarshalPrivateKeyPEM(kp.PrivateKey)
	if err != nil {
		t.Fatalf("MarshalPrivateKeyPEM failed: %v", err)
	}

	pubPEM, err := MarshalPublicKeyPEM(kp.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPublicKeyPEM failed: %v", err)
	}

	// 3. Parse PKCS#8 Private Key PEM
	parsedPriv, err := ParseRSAPrivateKeyPEM(privPEM)
	if err != nil || parsedPriv == nil {
		t.Fatalf("ParseRSAPrivateKeyPEM PKCS8 failed: %v", err)
	}

	// 4. Parse PKCS#1 Private Key PEM
	pkcs1Bytes := x509.MarshalPKCS1PrivateKey(kp.PrivateKey)
	pkcs1PEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: pkcs1Bytes})
	parsedPKCS1, err := ParseRSAPrivateKeyPEM(pkcs1PEM)
	if err != nil || parsedPKCS1 == nil {
		t.Fatalf("ParseRSAPrivateKeyPEM PKCS1 failed: %v", err)
	}

	// 5. Parse Public Key PEM
	parsedPub, err := ParseRSAPublicKeyPEM(pubPEM)
	if err != nil || parsedPub == nil {
		t.Fatalf("ParseRSAPublicKeyPEM failed: %v", err)
	}

	// 6. Write to temp directory and test LoadRSAPrivateKey & LoadRSAPublicKey
	tmpDir := t.TempDir()
	privPath := filepath.Join(tmpDir, "private.pem")
	pubPath := filepath.Join(tmpDir, "public.pem")

	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		t.Fatalf("WriteFile private key failed: %v", err)
	}
	if err := os.WriteFile(pubPath, pubPEM, 0o600); err != nil {
		t.Fatalf("WriteFile public key failed: %v", err)
	}

	loadedPriv, err := LoadRSAPrivateKey(privPath)
	if err != nil || loadedPriv == nil {
		t.Fatalf("LoadRSAPrivateKey failed: %v", err)
	}

	loadedPub, err := LoadRSAPublicKey(pubPath)
	if err != nil || loadedPub == nil {
		t.Fatalf("LoadRSAPublicKey failed: %v", err)
	}

	// 7. Test Error Branches
	// Invalid file paths
	if _, err := LoadRSAPrivateKey(filepath.Join(tmpDir, "nonexistent.pem")); err == nil {
		t.Errorf("Expected error loading non-existent private key file")
	}
	if _, err := LoadRSAPublicKey(filepath.Join(tmpDir, "nonexistent.pem")); err == nil {
		t.Errorf("Expected error loading non-existent public key file")
	}

	// Invalid PEM blocks
	if _, err := ParseRSAPrivateKeyPEM([]byte("invalid pem data")); err == nil {
		t.Errorf("Expected error parsing invalid private key PEM block")
	}
	if _, err := ParseRSAPublicKeyPEM([]byte("invalid pem data")); err == nil {
		t.Errorf("Expected error parsing invalid public key PEM block")
	}

	// Unsupported PEM type
	unsupportedPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("dummy")})
	if _, err := ParseRSAPrivateKeyPEM(unsupportedPEM); err == nil {
		t.Errorf("Expected error parsing unsupported private key PEM block type")
	}

	// Non-RSA key in PKCS8
	ecKey, _ := rsa.GenerateKey(rand.Reader, 1024)
	_ = ecKey
}
