package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
)

// RSAKeyPair holds an RSA private key (for signing) and its corresponding public key (for verification).
// Auth-service holds the private key; all other services only need the public key.
type RSAKeyPair struct {
	PrivateKey *rsa.PrivateKey
	PublicKey  *rsa.PublicKey
}

// LoadRSAPrivateKey reads and parses a PKCS#8 or PKCS#1 PEM-encoded RSA private key from disk.
func LoadRSAPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseRSAPrivateKeyPEM(data)
}

// LoadRSAPublicKey reads and parses a PEM-encoded RSA public key from disk.
func LoadRSAPublicKey(path string) (*rsa.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseRSAPublicKeyPEM(data)
}

// ParseRSAPrivateKeyPEM parses a PEM block containing an RSA private key.
// Supports PKCS#8 ("PRIVATE KEY") and PKCS#1 ("RSA PRIVATE KEY") formats.
func ParseRSAPrivateKeyPEM(pemData []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, errors.New("auth/keys: failed to decode PEM block for private key")
	}
	switch block.Type {
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("auth/keys: PKCS#8 key is not RSA")
		}
		return rsaKey, nil
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, errors.New("auth/keys: unsupported PEM block type: " + block.Type)
	}
}

// ParseRSAPublicKeyPEM parses a PEM block containing an RSA public key.
func ParseRSAPublicKeyPEM(pemData []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, errors.New("auth/keys: failed to decode PEM block for public key")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("auth/keys: public key is not RSA")
	}
	return rsaKey, nil
}

// GenerateRSAKeyPair generates a new 2048-bit RSA key pair in memory.
// Use this for testing or initial key bootstrapping.
func GenerateRSAKeyPair() (*RSAKeyPair, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	return &RSAKeyPair{
		PrivateKey: privateKey,
		PublicKey:  &privateKey.PublicKey,
	}, nil
}

// MarshalPrivateKeyPEM encodes an RSA private key to PKCS#8 PEM format.
func MarshalPrivateKeyPEM(key *rsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// MarshalPublicKeyPEM encodes an RSA public key to PKIX PEM format.
func MarshalPublicKeyPEM(key *rsa.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}
