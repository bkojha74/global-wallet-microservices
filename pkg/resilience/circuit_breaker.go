package resilience

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/sony/gobreaker/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CircuitBreakerConfig encapsulates settings for outbound gRPC client circuit breakers.
type CircuitBreakerConfig struct {
	Name             string
	MaxRequests      uint32
	Interval         time.Duration
	Timeout          time.Duration
	FailureThreshold uint32
	FailureRatio     float64
	OnStateChange    func(name string, from, to string)
}

// DefaultCircuitBreakerConfig returns resilient production defaults for gRPC client connections.
func DefaultCircuitBreakerConfig(name string) CircuitBreakerConfig {
	return CircuitBreakerConfig{
		Name:             name,
		MaxRequests:      3,                // Maximum requests allowed in half-open state
		Interval:         10 * time.Second, // Cyclic clearance interval for error counts in closed state
		Timeout:          5 * time.Second,  // Duration breaker stays open before testing in half-open
		FailureThreshold: 5,                // Trip after 5 consecutive failures
		FailureRatio:     0.50,             // Trip if >= 50% failures when >= 5 requests in window
	}
}

// NewClientCircuitBreaker creates an initialized gobreaker.CircuitBreaker for gRPC client protection.
func NewClientCircuitBreaker(cfg CircuitBreakerConfig) *gobreaker.CircuitBreaker[any] {
	if cfg.MaxRequests == 0 {
		cfg.MaxRequests = 3
	}
	if cfg.Interval == 0 {
		cfg.Interval = 10 * time.Second
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.FailureThreshold == 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.FailureRatio <= 0 {
		cfg.FailureRatio = 0.50
	}

	st := gobreaker.Settings{
		Name:        cfg.Name,
		MaxRequests: cfg.MaxRequests,
		Interval:    cfg.Interval,
		Timeout:     cfg.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			// Trip if consecutive failures meet threshold or overall failure ratio exceeds limit
			if counts.ConsecutiveFailures >= cfg.FailureThreshold {
				return true
			}
			if counts.Requests >= 5 {
				ratio := float64(counts.TotalFailures) / float64(counts.Requests)
				return ratio >= cfg.FailureRatio
			}
			return false
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			fromStr := from.String()
			toStr := to.String()
			log.Printf("[CIRCUIT-BREAKER] '%s' transitioned: %s -> %s", name, fromStr, toStr)
			if cfg.OnStateChange != nil {
				cfg.OnStateChange(name, fromStr, toStr)
			}
		},
		IsSuccessful: func(err error) bool {
			if err == nil {
				return true
			}
			// Distinguish transport/server infrastructure failures from client-side business rejections
			s, ok := status.FromError(err)
			if !ok {
				// Non-gRPC network/context failure
				return false
			}
			switch s.Code() {
			case codes.OK,
				codes.InvalidArgument,
				codes.NotFound,
				codes.AlreadyExists,
				codes.PermissionDenied,
				codes.Unauthenticated,
				codes.FailedPrecondition:
				// Downstream service answered normally with a business validation response
				return true
			case codes.Unavailable,
				codes.DeadlineExceeded,
				codes.Internal,
				codes.Unknown,
				codes.ResourceExhausted:
				// Downstream infrastructure outage or saturation
				return false
			default:
				return false
			}
		},
	}

	return gobreaker.NewCircuitBreaker[any](st)
}

// UnaryClientCircuitBreakerInterceptor returns a gRPC unary client interceptor guarded by the given circuit breaker.
func UnaryClientCircuitBreakerInterceptor(cb *gobreaker.CircuitBreaker[any]) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		if cb == nil {
			return invoker(ctx, method, req, reply, cc, opts...)
		}

		_, err := cb.Execute(func() (any, error) {
			invErr := invoker(ctx, method, req, reply, cc, opts...)
			return nil, invErr
		})

		if err != nil {
			if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
				return status.Errorf(
					codes.Unavailable,
					"circuit breaker '%s' is %s: downstream service unavailable",
					cb.Name(),
					cb.State().String(),
				)
			}
			return err
		}

		return nil
	}
}
