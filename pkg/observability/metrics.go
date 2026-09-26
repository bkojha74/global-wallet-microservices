package observability

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// MetricsRegistry tracks the 6 Prometheus-format metrics specified in the architecture
// document Section 7 (GAP-07). All metric names match the specification exactly.
type MetricsRegistry struct {
	mu sync.RWMutex

	// Counters
	eventsEmitted   map[string]int64 // logging_events_emitted_total{service,level,event_type}
	publishFailures map[string]int64 // logging_publish_failures_total{service,reason}
	eventsDropped   map[string]int64 // logging_events_dropped_total{service}
	spoolReplayed   map[string]int64 // logging_spool_replay_events_total{service}

	// Gauges
	queueDepth     map[string]int64   // logging_queue_depth{service}
	spoolBytes     map[string]int64   // logging_spool_bytes{service}
	spoolOldestAge map[string]float64 // logging_spool_oldest_event_age_seconds{service}

	// Phase 3 (GAP-OBS-02): gRPC & cluster metrics
	grpcRequests        map[string]int64   // grpc_requests_total{service,method,code}
	grpcLatencySum      map[string]float64 // grpc_request_duration_seconds_sum{service,method}
	grpcLatencyCount    map[string]int64   // grpc_request_duration_seconds_count{service,method}
	clusterActiveTarget map[string]int64   // cluster_active_target{service,target}
}

// DefaultMetrics is the process-wide singleton registry used by all AsyncLogger instances.
var DefaultMetrics = NewMetricsRegistry()

func NewMetricsRegistry() *MetricsRegistry {
	return &MetricsRegistry{
		eventsEmitted:       make(map[string]int64),
		publishFailures:     make(map[string]int64),
		eventsDropped:       make(map[string]int64),
		spoolReplayed:       make(map[string]int64),
		queueDepth:          make(map[string]int64),
		spoolBytes:          make(map[string]int64),
		spoolOldestAge:      make(map[string]float64),
		grpcRequests:        make(map[string]int64),
		grpcLatencySum:      make(map[string]float64),
		grpcLatencyCount:    make(map[string]int64),
		clusterActiveTarget: make(map[string]int64),
	}
}

// --- Counter increments ---

func (m *MetricsRegistry) IncEventsEmitted(service, level, eventType string) {
	key := fmt.Sprintf("service=%q,level=%q,event_type=%q", service, level, eventType)
	m.mu.Lock()
	m.eventsEmitted[key]++
	m.mu.Unlock()
}

const labelServiceFmt = "service=%q"

func (m *MetricsRegistry) IncPublishFailures(service, reason string) {
	key := fmt.Sprintf("service=%q,reason=%q", service, reason)
	m.mu.Lock()
	m.publishFailures[key]++
	m.mu.Unlock()
}

func (m *MetricsRegistry) IncEventsDropped(service string) {
	key := fmt.Sprintf(labelServiceFmt, service)
	m.mu.Lock()
	m.eventsDropped[key]++
	m.mu.Unlock()
}

func (m *MetricsRegistry) AddSpoolReplayed(service string, count int64) {
	key := fmt.Sprintf(labelServiceFmt, service)
	m.mu.Lock()
	m.spoolReplayed[key] += count
	m.mu.Unlock()
}

// --- Gauge setters ---

func (m *MetricsRegistry) SetQueueDepth(service string, depth int) {
	key := fmt.Sprintf(labelServiceFmt, service)
	m.mu.Lock()
	m.queueDepth[key] = int64(depth)
	m.mu.Unlock()
}

func (m *MetricsRegistry) SetSpoolBytes(service string, bytes int64) {
	key := fmt.Sprintf(labelServiceFmt, service)
	m.mu.Lock()
	m.spoolBytes[key] = bytes
	m.mu.Unlock()
}

func (m *MetricsRegistry) SetSpoolOldestAge(service string, seconds float64) {
	key := fmt.Sprintf(labelServiceFmt, service)
	m.mu.Lock()
	m.spoolOldestAge[key] = seconds
	m.mu.Unlock()
}

func (m *MetricsRegistry) ObserveGRPCRequest(service, method, code string, durationSeconds float64) {
	reqKey := fmt.Sprintf("service=%q,method=%q,code=%q", service, method, code)
	latKey := fmt.Sprintf("service=%q,method=%q", service, method)

	m.mu.Lock()
	m.grpcRequests[reqKey]++
	m.grpcLatencySum[latKey] += durationSeconds
	m.grpcLatencyCount[latKey]++
	m.mu.Unlock()
}

func (m *MetricsRegistry) SetClusterActiveTarget(service, target string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.clusterActiveTarget {
		if strings.HasPrefix(k, fmt.Sprintf("service=%q,", service)) {
			m.clusterActiveTarget[k] = 0
		}
	}
	key := fmt.Sprintf("service=%q,target=%q", service, target)
	m.clusterActiveTarget[key] = 1
}

// UnaryServerMetricsInterceptor returns a gRPC server interceptor recording metrics.
func (m *MetricsRegistry) UnaryServerMetricsInterceptor(serviceName string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		duration := time.Since(start).Seconds()

		code := "OK"
		if err != nil {
			s, _ := status.FromError(err)
			code = s.Code().String()
		}
		m.ObserveGRPCRequest(serviceName, info.FullMethod, code, duration)
		return resp, err
	}
}

// Handler returns an http.Handler that renders metrics in Prometheus text exposition format.
// Mount this at GET /metrics on the management port of each service (GAP-07).
func (m *MetricsRegistry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		m.mu.RLock()
		defer m.mu.RUnlock()

		var b strings.Builder
		writeMetricInts(&b, "logging_events_emitted_total", "Total events successfully emitted.", "counter", m.eventsEmitted)
		writeMetricInts(&b, "logging_queue_depth", "Current number of events waiting in the in-memory channel.", "gauge", m.queueDepth)
		writeMetricInts(&b, "logging_spool_bytes", "Current size of the local spool file in bytes.", "gauge", m.spoolBytes)
		writeMetricInts(&b, "logging_publish_failures_total", "Total RabbitMQ publish failures since startup.", "counter", m.publishFailures)
		writeMetricInts(&b, "logging_events_dropped_total", "Total events dropped due to full channel and spool error.", "counter", m.eventsDropped)
		writeMetricInts(&b, "logging_spool_replay_events_total", "Total events successfully replayed from spool.", "counter", m.spoolReplayed)
		writeMetricFloats(&b, "logging_spool_oldest_event_age_seconds", "Age in seconds of the oldest event in the spool.", "gauge", "%.2f", m.spoolOldestAge)
		writeMetricInts(&b, "grpc_requests_total", "Total gRPC requests by service, method, and response status code.", "counter", m.grpcRequests)
		writeMetricFloats(&b, "grpc_request_duration_seconds_sum", "Total duration of gRPC requests in seconds.", "counter", "%.4f", m.grpcLatencySum)
		writeMetricInts(&b, "grpc_request_duration_seconds_count", "Total count of recorded gRPC requests.", "counter", m.grpcLatencyCount)
		writeMetricInts(&b, "cluster_active_target", "Cluster active routing target (1 for active, 0 for standby).", "gauge", m.clusterActiveTarget)

		_, _ = w.Write([]byte(b.String()))
	})
}

func writeMetricInts(b *strings.Builder, name, help, typeName string, data map[string]int64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typeName)
	for _, k := range sortedKeys(data) {
		fmt.Fprintf(b, "%s{%s} %d\n", name, k, data[k])
	}
}

func writeMetricFloats(b *strings.Builder, name, help, typeName, format string, data map[string]float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typeName)
	for _, k := range sortedFloatKeys(data) {
		b.WriteString(name + "{" + k + "} ")
		fmt.Fprintf(b, format+"\n", data[k])
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedFloatKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
