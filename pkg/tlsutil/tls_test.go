package tlsutil

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	walletv1 "wallet-system/proto/wallet"
)

type dummyWalletServer struct {
	walletv1.UnimplementedWalletServiceServer
}

func (s *dummyWalletServer) HealthCheck(ctx context.Context, req *walletv1.HealthRequest) (*walletv1.HealthResponse, error) {
	return &walletv1.HealthResponse{Status: "OK"}, nil
}

func TestMutualTLSHandshakeSuccess(t *testing.T) {
	bundle, err := GenerateTestCertificates()
	if err != nil {
		t.Fatalf("failed to generate test certs: %v", err)
	}

	serverTLS, err := bundle.ServerTLSConfig()
	if err != nil {
		t.Fatalf("failed to build server TLS: %v", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	walletv1.RegisterWalletServiceServer(grpcServer, &dummyWalletServer{})
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	// Connect with trusted client cert
	clientTLS, err := bundle.ClientTLSConfig("localhost")
	if err != nil {
		t.Fatalf("failed to build client TLS: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)), grpc.WithBlock())
	if err != nil {
		t.Fatalf("mTLS dial failed: %v", err)
	}
	defer conn.Close()

	client := walletv1.NewWalletServiceClient(conn)
	resp, err := client.HealthCheck(ctx, &walletv1.HealthRequest{})
	if err != nil {
		t.Fatalf("health check failed over mTLS: %v", err)
	}
	if resp.Status != "OK" {
		t.Fatalf("expected OK, got %s", resp.Status)
	}
}

func TestMutualTLSRejectsUntrustedClient(t *testing.T) {
	bundle, err := GenerateTestCertificates()
	if err != nil {
		t.Fatalf("failed to generate test certs: %v", err)
	}

	serverTLS, err := bundle.ServerTLSConfig()
	if err != nil {
		t.Fatalf("failed to build server TLS: %v", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	walletv1.RegisterWalletServiceServer(grpcServer, &dummyWalletServer{})
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	// Connect with untrusted rogue client cert
	untrustedTLS, err := bundle.UntrustedClientTLSConfig("localhost")
	if err != nil {
		t.Fatalf("failed to build untrusted client TLS: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(untrustedTLS)), grpc.WithBlock())
	if err == nil {
		// If dial succeeded (lazy handshake), RPC must fail
		client := walletv1.NewWalletServiceClient(conn)
		_, rpcErr := client.HealthCheck(ctx, &walletv1.HealthRequest{})
		conn.Close()
		if rpcErr == nil {
			t.Fatal("expected RPC failure for untrusted client certificate, but it succeeded")
		}
	}
}

func TestFileBasedTransportCredentials(t *testing.T) {
	bundle, err := GenerateTestCertificates()
	if err != nil {
		t.Fatalf("generate test certs: %v", err)
	}

	tmpDir := t.TempDir()
	caPath := filepath.Join(tmpDir, "ca.pem")
	srvCertPath := filepath.Join(tmpDir, "server.pem")
	srvKeyPath := filepath.Join(tmpDir, "server.key")
	cliCertPath := filepath.Join(tmpDir, "client.pem")
	cliKeyPath := filepath.Join(tmpDir, "client.key")

	_ = os.WriteFile(caPath, bundle.CACertPEM, 0o644)
	_ = os.WriteFile(srvCertPath, bundle.ServerCertPEM, 0o644)
	_ = os.WriteFile(srvKeyPath, bundle.ServerKeyPEM, 0o600)
	_ = os.WriteFile(cliCertPath, bundle.ClientCertPEM, 0o644)
	_ = os.WriteFile(cliKeyPath, bundle.ClientKeyPEM, 0o600)

	// Server credentials with CA
	srvCreds, err := NewServerTransportCredentials(srvCertPath, srvKeyPath, caPath)
	if err != nil || srvCreds == nil {
		t.Fatalf("expected server transport credentials, got %v", err)
	}

	// Client credentials with CA and client cert
	cliCreds, err := NewClientTransportCredentials(cliCertPath, cliKeyPath, caPath, "localhost")
	if err != nil || cliCreds == nil {
		t.Fatalf("expected client transport credentials, got %v", err)
	}
}

func TestCACertAppendErrors(t *testing.T) {
	bundle, err := GenerateTestCertificates()
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}

	// Corrupt CA cert
	bundle.CACertPEM = []byte("not-a-valid-ca-pem")

	if _, err := bundle.ServerTLSConfig(); err == nil {
		t.Fatal("expected error on corrupt CA for ServerTLSConfig")
	}

	if _, err := bundle.ClientTLSConfig("localhost"); err == nil {
		t.Fatal("expected error on corrupt CA for ClientTLSConfig")
	}

	if _, err := bundle.UntrustedClientTLSConfig("localhost"); err == nil {
		t.Fatal("expected error on corrupt CA for UntrustedClientTLSConfig")
	}
}

