package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestInitTracerAndExtractTraceID(t *testing.T) {
	shutdown, err := InitTracer("test-service")
	if err != nil {
		t.Fatalf("failed to init tracer: %v", err)
	}
	defer func() {
		_ = shutdown(context.Background())
	}()

	ctx := context.Background()
	traceID := ExtractTraceID(ctx)
	// Without span or correlation, returns empty
	if traceID != "" {
		t.Errorf("expected empty trace ID, got %s", traceID)
	}

	ctx = WithCorrelation(ctx, Correlation{AssociationID: "my-test-assoc-id"})
	if got := ExtractTraceID(ctx); got != "my-test-assoc-id" {
		t.Errorf("expected my-test-assoc-id, got %s", got)
	}
}

func TestTraceHTTPMiddlewareInjectsHeaders(t *testing.T) {
	_, _ = InitTracer("api-gateway")

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		corr := FromContext(r.Context())
		if corr.AssociationID == "" {
			t.Error("expected AssociationID in context")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	wrapped := TraceHTTPMiddleware("api-gateway", handler)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/wallets", nil)
	rec := httptest.NewRecorder()

	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	traceparent := rec.Header().Get("traceparent")
	if traceparent == "" {
		t.Error("expected traceparent header in response")
	}

	assocID := rec.Header().Get(AssociationIDHeader)
	if assocID == "" {
		t.Error("expected X-Association-ID header in response")
	}
}

func TestGRPCTracingInterceptorsPropagateContext(t *testing.T) {
	_, _ = InitTracer("wallet-service")

	clientInterceptor := UnaryClientTraceInterceptor("api-gateway")
	serverInterceptor := UnaryServerTraceInterceptor("wallet-service")

	var serverReceivedTraceID string

	// Mock server handler
	mockServerHandler := func(ctx context.Context, req any) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			t.Error("expected incoming metadata on server")
		}
		serverReceivedTraceID = md.Get("traceparent")[0]
		return "server-response", nil
	}

	// Mock client invoker that passes outgoing metadata to incoming metadata (simulating network transport)
	mockInvoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		md, ok := metadata.FromOutgoingContext(ctx)
		if !ok {
			t.Error("expected outgoing metadata on client")
		}

		// Simulate server receiving the request with the transmitted metadata
		serverCtx := metadata.NewIncomingContext(context.Background(), md)
		info := &grpc.UnaryServerInfo{FullMethod: method}
		_, err := serverInterceptor(serverCtx, req, info, mockServerHandler)
		return err
	}

	ctx := context.Background()
	err := clientInterceptor(ctx, "/wallet.v1.WalletService/GetBalance", "req-body", nil, nil, mockInvoker)
	if err != nil {
		t.Fatalf("unexpected interceptor error: %v", err)
	}

	if serverReceivedTraceID == "" {
		t.Fatal("expected server to receive propagated traceparent from client interceptor")
	}
}

func TestFormatW3CTraceParent(t *testing.T) {
	formatted := FormatW3CTraceParent("4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7")
	expected := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	if formatted != expected {
		t.Errorf("expected %s, got %s", expected, formatted)
	}
}
