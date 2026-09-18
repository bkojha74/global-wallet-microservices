package tlsutil

import (
	"context"
	"net"
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
