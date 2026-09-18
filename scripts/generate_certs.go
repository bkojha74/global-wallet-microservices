package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

func main() {
	outDir := "certs"
	if len(os.Args) > 1 {
		outDir = os.Args[1]
	}

	if err := os.MkdirAll(outDir, 0755); err != nil {
		fmt.Printf("Failed to create output dir: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("[CERT-GEN] Generating Zero-Trust mTLS certificates into %s/...\n", outDir)

	// 1. Root Certificate Authority (CA)
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Global Wallet Microservices"},
			CommonName:   "Global Wallet Root CA",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caBytes, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		panic(err)
	}
	writePEM(filepath.Join(outDir, "ca.crt"), "CERTIFICATE", caBytes)
	writeKey(filepath.Join(outDir, "ca.key"), caKey)

	// 2. Server Certificate (Shared by microservices in dev)
	serverKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			Organization: []string{"Global Wallet Microservices"},
			CommonName:   "wallet-cluster-services",
		},
		DNSNames: []string{
			"localhost",
			"wallet-primary",
			"wallet-standby",
			"ledger-service",
			"api-gateway",
		},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		NotBefore:   time.Now().Add(-1 * time.Hour),
		NotAfter:    time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	serverBytes, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		panic(err)
	}
	writePEM(filepath.Join(outDir, "server.crt"), "CERTIFICATE", serverBytes)
	writeKey(filepath.Join(outDir, "server.key"), serverKey)

	// 3. Client Certificate (Used by API Gateway and Wallet Service outbound client)
	clientKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject: pkix.Name{
			Organization: []string{"Global Wallet Microservices"},
			CommonName:   "wallet-client",
		},
		NotBefore:   time.Now().Add(-1 * time.Hour),
		NotAfter:    time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientBytes, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientKey.PublicKey, caKey)
	if err != nil {
		panic(err)
	}
	writePEM(filepath.Join(outDir, "client.crt"), "CERTIFICATE", clientBytes)
	writeKey(filepath.Join(outDir, "client.key"), clientKey)

	fmt.Println("[CERT-GEN] Generated successfully:")
	fmt.Printf("  - Root CA:     %s, %s\n", filepath.Join(outDir, "ca.crt"), filepath.Join(outDir, "ca.key"))
	fmt.Printf("  - Server Cert: %s, %s\n", filepath.Join(outDir, "server.crt"), filepath.Join(outDir, "server.key"))
	fmt.Printf("  - Client Cert: %s, %s\n", filepath.Join(outDir, "client.crt"), filepath.Join(outDir, "client.key"))
}

func writePEM(filename, blockType string, data []byte) {
	f, err := os.Create(filename)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	_ = pem.Encode(f, &pem.Block{Type: blockType, Bytes: data})
}

func writeKey(filename string, key *ecdsa.PrivateKey) {
	bytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		panic(err)
	}
	writePEM(filename, "EC PRIVATE KEY", bytes)
}
