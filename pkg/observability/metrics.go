package observability

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
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
}

// DefaultMetrics is the process-wide singleton registry used by all AsyncLogger instances.
var DefaultMetrics = NewMetricsRegistry()

func NewMetricsRegistry() *MetricsRegistry {
	return &MetricsRegistry{
		eventsEmitted:   make(map[string]int64),
		publishFailures: make(map[string]int64),
		eventsDropped:   make(map[string]int64),
		spoolReplayed:   make(map[string]int64),
		queueDepth:      make(map[string]int64),
		spoolBytes:      make(map[string]int64),
		spoolOldestAge:  make(map[string]float64),
	}
}

// --- Counter increments ---

func (m *MetricsRegistry) IncEventsEmitted(service, level, eventType string) {
	key := fmt.Sprintf("service=%q,level=%q,event_type=%q", service, level, eventType)
	m.mu.Lock()
	m.eventsEmitted[key]++
	m.mu.Unlock()
}

func (m *MetricsRegistry) IncPublishFailures(service, reason string) {
	key := fmt.Sprintf("service=%q,reason=%q", service, reason)
	m.mu.Lock()
	m.publishFailures[key]++
	m.mu.Unlock()
}

func (m *MetricsRegistry) IncEventsDropped(service string) {
	key := fmt.Sprintf("service=%q", service)
	m.mu.Lock()
	m.eventsDropped[key]++
	m.mu.Unlock()
}

func (m *MetricsRegistry) AddSpoolReplayed(service string, count int64) {
	key := fmt.Sprintf("service=%q", service)
	m.mu.Lock()
	m.spoolReplayed[key] += count
	m.mu.Unlock()
}

// --- Gauge setters ---

func (m *MetricsRegistry) SetQueueDepth(service string, depth int) {
	key := fmt.Sprintf("service=%q", service)
	m.mu.Lock()
	m.queueDepth[key] = int64(depth)
	m.mu.Unlock()
}

func (m *MetricsRegistry) SetSpoolBytes(service string, bytes int64) {
	key := fmt.Sprintf("service=%q", service)
	m.mu.Lock()
	m.spoolBytes[key] = bytes
	m.mu.Unlock()
}

func (m *MetricsRegistry) SetSpoolOldestAge(service string, seconds float64) {
	key := fmt.Sprintf("service=%q", service)
	m.mu.Lock()
	m.spoolOldestAge[key] = seconds
	m.mu.Unlock()
}

// Handler returns an http.Handler that renders metrics in Prometheus text exposition format.
// Mount this at GET /metrics on the management port of each service (GAP-07).
func (m *MetricsRegistry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		m.mu.RLock()
		defer m.mu.RUnlock()

		var b strings.Builder

		// 1. logging_queue_depth — current in-memory channel depth (Gauge)
		b.WriteString("# HELP logging_queue_depth Current number of events waiting in the in-memory channel.\n")
		b.WriteString("# TYPE logging_queue_depth gauge\n")
		for _, k := range sortedKeys(m.queueDepth) {
			fmt.Fprintf(&b, "logging_queue_depth{%s} %d\n", k, m.queueDepth[k])
		}

		// 2. logging_spool_bytes — current spool file size (Gauge)
		b.WriteString("# HELP logging_spool_bytes Current size of the local spool file in bytes.\n")
		b.WriteString("# TYPE logging_spool_bytes gauge\n")
		for _, k := range sortedKeys(m.spoolBytes) {
			fmt.Fprintf(&b, "logging_spool_bytes{%s} %d\n", k, m.spoolBytes[k])
		}

		// 3. logging_publish_failures_total (Counter)
		b.WriteString("# HELP logging_publish_failures_total Total RabbitMQ publish failures since startup.\n")
		b.WriteString("# TYPE logging_publish_failures_total counter\n")
		for _, k := range sortedKeys(m.publishFailures) {
			fmt.Fprintf(&b, "logging_publish_failures_total{%s} %d\n", k, m.publishFailures[k])
		}

		// 4. logging_events_dropped_total (Counter)
		b.WriteString("# HELP logging_events_dropped_total Total events dropped due to full channel and spool error.\n")
		b.WriteString("# TYPE logging_events_dropped_total counter\n")
		for _, k := range sortedKeys(m.eventsDropped) {
			fmt.Fprintf(&b, "logging_events_dropped_total{%s} %d\n", k, m.eventsDropped[k])
		}

		// 5. logging_spool_replay_events_total (Counter)
		b.WriteString("# HELP logging_spool_replay_events_total Total events successfully replayed from spool.\n")
		b.WriteString("# TYPE logging_spool_replay_events_total counter\n")
		for _, k := range sortedKeys(m.spoolReplayed) {
			fmt.Fprintf(&b, "logging_spool_replay_events_total{%s} %d\n", k, m.spoolReplayed[k])
		}

		// 6. logging_spool_oldest_event_age_seconds (Gauge)
		b.WriteString("# HELP logging_spool_oldest_event_age_seconds Age in seconds of the oldest event in the spool.\n")
		b.WriteString("# TYPE logging_spool_oldest_event_age_seconds gauge\n")
		for _, k := range sortedFloatKeys(m.spoolOldestAge) {
			fmt.Fprintf(&b, "logging_spool_oldest_event_age_seconds{%s} %.2f\n", k, m.spoolOldestAge[k])
		}

		_, _ = w.Write([]byte(b.String()))
	})
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
