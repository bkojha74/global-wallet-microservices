package resilience

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCircuitBreaker_Success(t *testing.T) {
	cb := NewClientCircuitBreaker(DefaultCircuitBreakerConfig("test-success"))
	interceptor := UnaryClientCircuitBreakerInterceptor(cb)

	invokerCalled := false
	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		invokerCalled = true
		return nil
	}

	err := interceptor(context.Background(), "/test.Service/TestMethod", nil, nil, nil, invoker)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !invokerCalled {
		t.Fatal("expected invoker to be called")
	}
	if cb.State() != gobreaker.StateClosed {
		t.Fatalf("expected StateClosed, got %v", cb.State())
	}
}

func TestCircuitBreaker_BusinessErrorsDoNotTrip(t *testing.T) {
	cfg := DefaultCircuitBreakerConfig("test-business-err")
	cfg.FailureThreshold = 3
	cb := NewClientCircuitBreaker(cfg)
	interceptor := UnaryClientCircuitBreakerInterceptor(cb)

	// Simulate 5 client-side business errors (NotFound, InvalidArgument, AlreadyExists)
	businessCodes := []codes.Code{
		codes.NotFound,
		codes.InvalidArgument,
		codes.AlreadyExists,
		codes.Unauthenticated,
		codes.PermissionDenied,
	}

	for _, c := range businessCodes {
		invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			return status.Errorf(c, "business validation error")
		}
		err := interceptor(context.Background(), "/test.Service/TestMethod", nil, nil, nil, invoker)
		if st, ok := status.FromError(err); !ok || st.Code() != c {
			t.Fatalf("expected code %v, got %v", c, err)
		}
	}

	// Circuit breaker should remain CLOSED because business errors indicate downstream service is healthy
	if cb.State() != gobreaker.StateClosed {
		t.Fatalf("expected circuit breaker to remain closed, got state: %v", cb.State())
	}
}

func TestCircuitBreaker_TripsOnDownstreamFailures(t *testing.T) {
	var lastStateChange string
	cfg := CircuitBreakerConfig{
		Name:             "test-trip",
		MaxRequests:      1,
		Interval:         5 * time.Second,
		Timeout:          50 * time.Millisecond,
		FailureThreshold: 3,
		OnStateChange: func(name string, from, to string) {
			lastStateChange = to
		},
	}
	cb := NewClientCircuitBreaker(cfg)
	interceptor := UnaryClientCircuitBreakerInterceptor(cb)

	failingInvoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		return status.Errorf(codes.Unavailable, "downstream transport connection refused")
	}

	// 3 failures should trip the circuit breaker
	for i := 0; i < 3; i++ {
		_ = interceptor(context.Background(), "/test.Service/TestMethod", nil, nil, nil, failingInvoker)
	}

	if cb.State() != gobreaker.StateOpen {
		t.Fatalf("expected breaker to be Open, got %v", cb.State())
	}
	if lastStateChange != "open" {
		t.Fatalf("expected lastStateChange 'open', got '%s'", lastStateChange)
	}

	// 4th call should be rejected immediately without calling invoker
	called := false
	spyInvoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		called = true
		return nil
	}

	err := interceptor(context.Background(), "/test.Service/TestMethod", nil, nil, nil, spyInvoker)
	if called {
		t.Fatal("expected invoker NOT to be called when circuit breaker is open")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unavailable {
		t.Fatalf("expected codes.Unavailable, got %v", err)
	}

	// Wait for timeout to expire and transition to half-open
	time.Sleep(70 * time.Millisecond)

	// In half-open state, a successful call resets breaker to closed
	successInvoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		return nil
	}

	err = interceptor(context.Background(), "/test.Service/TestMethod", nil, nil, nil, successInvoker)
	if err != nil {
		t.Fatalf("expected success in half-open state, got %v", err)
	}
	if cb.State() != gobreaker.StateClosed {
		t.Fatalf("expected breaker to close on success in half-open, got %v", cb.State())
	}
}

func TestCircuitBreaker_NilSafe(t *testing.T) {
	interceptor := UnaryClientCircuitBreakerInterceptor(nil)
	invokerCalled := false
	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		invokerCalled = true
		return errors.New("raw error")
	}

	err := interceptor(context.Background(), "/test/Method", nil, nil, nil, invoker)
	if err == nil || !invokerCalled {
		t.Fatalf("expected nil interceptor to passthrough to invoker")
	}
}
