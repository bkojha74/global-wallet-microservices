package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc/credentials"
)

const (
	errFailedToAppendCACert = "failed to append CA cert"
	pemTypeECPrivateKey     = "EC PRIVATE KEY"
)

// NewServerTransportCredentials loads TLS credentials for a gRPC server.
// If caFile is provided, it enforces mutual TLS (mTLS) requiring and verifying client certificates.
func NewServerTransportCredentials(certFile, keyFile, caFile string) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load server keypair: %w", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	if caFile != "" {
		cleanCA := filepath.Clean(caFile)
		// #nosec G304 -- CA certificate path is loaded from trusted system or test configuration
		caBytes, err := os.ReadFile(cleanCA)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate: %w", err)
		}
		certPool := x509.NewCertPool()
		if !certPool.AppendCertsFromPEM(caBytes) {
			return nil, errors.New("failed to parse CA certificate into pool")
		}
		tlsConfig.ClientCAs = certPool
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return credentials.NewTLS(tlsConfig), nil
}

// NewClientTransportCredentials loads TLS credentials for an outbound gRPC client.
// If certFile and keyFile are provided, client certificates are sent for mTLS authentication.
func NewClientTransportCredentials(certFile, keyFile, caFile, serverName string) (credentials.TransportCredentials, error) {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: serverName,
	}

	if certFile != "" && keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client keypair: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	if caFile != "" {
		cleanCA := filepath.Clean(caFile)
		// #nosec G304 -- CA certificate path is loaded from trusted system or test configuration
		caBytes, err := os.ReadFile(cleanCA)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate: %w", err)
		}
		certPool := x509.NewCertPool()
		if !certPool.AppendCertsFromPEM(caBytes) {
			return nil, errors.New("failed to parse CA certificate into pool")
		}
		tlsConfig.RootCAs = certPool
	}

	return credentials.NewTLS(tlsConfig), nil
}

// TestCertBundle holds in-memory generated certificates for testing mTLS.
type TestCertBundle struct {
	CACertPEM              []byte
	ServerCertPEM          []byte
	ServerKeyPEM           []byte
	ClientCertPEM          []byte
	ClientKeyPEM           []byte
	UntrustedClientCertPEM []byte
	UntrustedClientKeyPEM  []byte
}

// ServerTLSConfig returns a tls.Config configured for strict mTLS on the server.
func (b *TestCertBundle) ServerTLSConfig() (*tls.Config, error) {
	serverCert, err := tls.X509KeyPair(b.ServerCertPEM, b.ServerKeyPEM)
	if err != nil {
		return nil, err
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(b.CACertPEM) {
		return nil, errors.New(errFailedToAppendCACert)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientTLSConfig returns a tls.Config configured for mTLS on the client.
func (b *TestCertBundle) ClientTLSConfig(serverName string) (*tls.Config, error) {
	clientCert, err := tls.X509KeyPair(b.ClientCertPEM, b.ClientKeyPEM)
	if err != nil {
		return nil, err
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(b.CACertPEM) {
		return nil, errors.New(errFailedToAppendCACert)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caPool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// UntrustedClientTLSConfig returns a client tls.Config using certificates from an untrusted rogue CA.
func (b *TestCertBundle) UntrustedClientTLSConfig(serverName string) (*tls.Config, error) {
	clientCert, err := tls.X509KeyPair(b.UntrustedClientCertPEM, b.UntrustedClientKeyPEM)
	if err != nil {
		return nil, err
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(b.CACertPEM) {
		return nil, errors.New(errFailedToAppendCACert)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caPool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// GenerateTestCertificates generates an in-memory CA, Server, and Client certificate hierarchy for testing.
func GenerateTestCertificates() (*TestCertBundle, error) {
	// 1. Root CA
	caPrivKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Wallet Testing CA"},
			CommonName:   "Wallet Test Root CA",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caBytes, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return nil, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caBytes})

	// 2. Server Certificate
	serverPrivKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			Organization: []string{"Wallet Testing Service"},
			CommonName:   "localhost",
		},
		DNSNames:    []string{"localhost", "wallet-primary", "wallet-standby", "ledger-service"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		NotBefore:   time.Now().Add(-1 * time.Hour),
		NotAfter:    time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverBytes, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return nil, err
	}
	serverCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverBytes})
	serverKeyBytes, _ := x509.MarshalECPrivateKey(serverPrivKey)
	serverKeyPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeECPrivateKey, Bytes: serverKeyBytes})

	// 3. Client Certificate
	clientPrivKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject: pkix.Name{
			Organization: []string{"Wallet Testing Client"},
			CommonName:   "api-gateway",
		},
		NotBefore:   time.Now().Add(-1 * time.Hour),
		NotAfter:    time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientBytes, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return nil, err
	}
	clientCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientBytes})
	clientKeyBytes, _ := x509.MarshalECPrivateKey(clientPrivKey)
	clientKeyPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeECPrivateKey, Bytes: clientKeyBytes})

	// 4. Untrusted Client Certificate (signed by a rogue CA)
	roguePrivKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rogueTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(99),
		Subject:               pkix.Name{CommonName: "Rogue CA"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	rogueCABytes, _ := x509.CreateCertificate(rand.Reader, rogueTemplate, rogueTemplate, &roguePrivKey.PublicKey, roguePrivKey)
	rogueCACert, _ := x509.ParseCertificate(rogueCABytes)

	untrustedPrivKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	untrustedTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(100),
		Subject:      pkix.Name{CommonName: "malicious-client"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	untrustedBytes, _ := x509.CreateCertificate(rand.Reader, untrustedTemplate, rogueCACert, &untrustedPrivKey.PublicKey, roguePrivKey)
	untrustedCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: untrustedBytes})
	untrustedKeyBytes, _ := x509.MarshalECPrivateKey(untrustedPrivKey)
	untrustedKeyPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeECPrivateKey, Bytes: untrustedKeyBytes})

	return &TestCertBundle{
		CACertPEM:              caPEM,
		ServerCertPEM:          serverCertPEM,
		ServerKeyPEM:           serverKeyPEM,
		ClientCertPEM:          clientCertPEM,
		ClientKeyPEM:           clientKeyPEM,
		UntrustedClientCertPEM: untrustedCertPEM,
		UntrustedClientKeyPEM:  untrustedKeyPEM,
	}, nil
}
