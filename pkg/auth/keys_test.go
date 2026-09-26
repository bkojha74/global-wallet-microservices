package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRSAKeyPairLifecycle(t *testing.T) {
	// 1. Generate key pair
	pair, err := GenerateRSAKeyPair()
	if err != nil {
		t.Fatalf("failed to generate RSA key pair: %v", err)
	}
	if pair.PrivateKey == nil || pair.PublicKey == nil {
		t.Fatal("expected non-nil private and public keys")
	}

	// 2. Marshal private key to PKCS#8 PEM
	privPEM, err := MarshalPrivateKeyPEM(pair.PrivateKey)
	if err != nil {
		t.Fatalf("failed to marshal private key: %v", err)
	}

	// 3. Marshal public key to PKIX PEM
	pubPEM, err := MarshalPublicKeyPEM(pair.PublicKey)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}

	// 4. Parse PEM bytes
	parsedPriv, err := ParseRSAPrivateKeyPEM(privPEM)
	if err != nil {
		t.Fatalf("failed to parse private key PEM: %v", err)
	}
	if parsedPriv.N.Cmp(pair.PrivateKey.N) != 0 {
		t.Fatal("parsed private key modulus mismatch")
	}

	parsedPub, err := ParseRSAPublicKeyPEM(pubPEM)
	if err != nil {
		t.Fatalf("failed to parse public key PEM: %v", err)
	}
	if parsedPub.N.Cmp(pair.PublicKey.N) != 0 {
		t.Fatal("parsed public key modulus mismatch")
	}

	// 5. Save to disk and test LoadRSAPrivateKey / LoadRSAPublicKey
	tmpDir := t.TempDir()
	privPath := filepath.Join(tmpDir, "private.pem")
	pubPath := filepath.Join(tmpDir, "public.pem")

	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		t.Fatalf("write private pem: %v", err)
	}
	if err := os.WriteFile(pubPath, pubPEM, 0o600); err != nil {
		t.Fatalf("write public pem: %v", err)
	}

	loadedPriv, err := LoadRSAPrivateKey(privPath)
	if err != nil {
		t.Fatalf("load private key: %v", err)
	}
	if loadedPriv.N.Cmp(pair.PrivateKey.N) != 0 {
		t.Fatal("loaded private key modulus mismatch")
	}

	loadedPub, err := LoadRSAPublicKey(pubPath)
	if err != nil {
		t.Fatalf("load public key: %v", err)
	}
	if loadedPub.N.Cmp(pair.PublicKey.N) != 0 {
		t.Fatal("loaded public key modulus mismatch")
	}

	// 6. Test invalid PEM parsing
	if _, err := ParseRSAPrivateKeyPEM([]byte("invalid pem data")); err == nil {
		t.Fatal("expected error parsing invalid private PEM")
	}
	if _, err := ParseRSAPublicKeyPEM([]byte("invalid pem data")); err == nil {
		t.Fatal("expected error parsing invalid public PEM")
	}
}
