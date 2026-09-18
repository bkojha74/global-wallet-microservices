package observability

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var defaultPropagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{},
	propagation.Baggage{},
)

func init() {
	otel.SetTextMapPropagator(defaultPropagator)
}

// InitTracer initializes an in-process OpenTelemetry TracerProvider with W3C propagation.
func InitTracer(serviceName string) (func(context.Context) error, error) {
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(defaultPropagator)

	return tp.Shutdown, nil
}

// MetadataCarrier adapts gRPC metadata.MD to OpenTelemetry TextMapCarrier.
type MetadataCarrier metadata.MD

func (m MetadataCarrier) Get(key string) string {
	vals := metadata.MD(m).Get(key)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

func (m MetadataCarrier) Set(key, val string) {
	metadata.MD(m).Set(key, val)
}

func (m MetadataCarrier) Keys() []string {
	keys := make([]string, 0, len(m))
	for k := range metadata.MD(m) {
		keys = append(keys, k)
	}
	return keys
}

// TraceHTTPMiddleware wraps an HTTP handler with W3C OpenTelemetry tracing.
func TraceHTTPMiddleware(serviceName string, next http.Handler) http.Handler {
	tracer := otel.Tracer(serviceName)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, fmt.Sprintf("HTTP %s %s", r.Method, r.URL.Path),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(r.Method),
				semconv.URLPath(r.URL.Path),
			),
		)
		defer span.End()

		spanCtx := span.SpanContext()
		traceID := ""
		if spanCtx.HasTraceID() {
			traceID = spanCtx.TraceID().String()
		}

		// Bridge with Correlation
		correlation := FromHTTPRequest(r)
		if traceID != "" {
			correlation.AssociationID = traceID
		}
		ctx = WithCorrelation(ctx, correlation)

		// Inject traceparent and association headers into response
		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(w.Header()))
		if correlation.AssociationID != "" {
			w.Header().Set(AssociationIDHeader, correlation.AssociationID)
		}

		rw := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r.WithContext(ctx))

		span.SetAttributes(semconv.HTTPResponseStatusCode(rw.statusCode))
		if rw.statusCode >= 500 {
			span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", rw.statusCode))
		} else {
			span.SetStatus(codes.Ok, "")
		}
	})
}

type statusResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *statusResponseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// UnaryClientTraceInterceptor injects W3C TraceContext into outgoing gRPC requests.
func UnaryClientTraceInterceptor(serviceName string) grpc.UnaryClientInterceptor {
	tracer := otel.Tracer(serviceName)
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx, span := tracer.Start(ctx, fmt.Sprintf("gRPC Client %s", method),
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(
				attribute.String("rpc.system", "grpc"),
				attribute.String("rpc.service", serviceName),
				attribute.String("rpc.method", method),
			),
		)
		defer span.End()

		md, ok := metadata.FromOutgoingContext(ctx)
		if !ok {
			md = metadata.New(nil)
		} else {
			md = md.Copy()
		}

		otel.GetTextMapPropagator().Inject(ctx, MetadataCarrier(md))
		ctx = metadata.NewOutgoingContext(ctx, md)

		err := invoker(ctx, method, req, reply, cc, opts...)
		if err != nil {
			s, _ := status.FromError(err)
			span.SetStatus(codes.Error, s.Message())
			span.SetAttributes(attribute.String("rpc.grpc.status_code", s.Code().String()))
		} else {
			span.SetStatus(codes.Ok, "")
		}
		return err
	}
}

// UnaryServerTraceInterceptor extracts W3C TraceContext from incoming gRPC requests.
func UnaryServerTraceInterceptor(serviceName string) grpc.UnaryServerInterceptor {
	tracer := otel.Tracer(serviceName)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			md = metadata.New(nil)
		}

		ctx = otel.GetTextMapPropagator().Extract(ctx, MetadataCarrier(md))
		ctx, span := tracer.Start(ctx, fmt.Sprintf("gRPC Server %s", info.FullMethod),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("rpc.system", "grpc"),
				attribute.String("rpc.service", serviceName),
				attribute.String("rpc.method", info.FullMethod),
			),
		)
		defer span.End()

		// Bridge correlation
		correlation := FromIncomingContext(ctx)
		spanCtx := span.SpanContext()
		if spanCtx.HasTraceID() {
			correlation.AssociationID = spanCtx.TraceID().String()
		}
		ctx = WithCorrelation(ctx, correlation)

		resp, err := handler(ctx, req)
		if err != nil {
			s, _ := status.FromError(err)
			span.SetStatus(codes.Error, s.Message())
			span.SetAttributes(attribute.String("rpc.grpc.status_code", s.Code().String()))
		} else {
			span.SetStatus(codes.Ok, "")
		}
		return resp, err
	}
}

// ExtractTraceID returns the hex-encoded 32-character TraceID from context, if available.
func ExtractTraceID(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)
	if span != nil && span.SpanContext().HasTraceID() {
		return span.SpanContext().TraceID().String()
	}
	return FromContext(ctx).AssociationID
}

// FormatW3CTraceParent formats a W3C traceparent string given a 32-char hex traceID and 16-char spanID.
func FormatW3CTraceParent(traceID, spanID string) string {
	if len(traceID) != 32 {
		traceID = strings.Repeat("0", 32-len(traceID)) + traceID
	}
	if len(spanID) != 16 {
		spanID = strings.Repeat("0", 16-len(spanID)) + spanID
	}
	return fmt.Sprintf("00-%s-%s-01", traceID, spanID)
}
