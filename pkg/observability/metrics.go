package observability

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

type MetricsRegistry struct {
	mu sync.RWMutex

	eventsEmitted   map[string]int64
	publishFailures map[string]int64
	spoolWrites     map[string]int64
	spoolBytes      map[string]int64
	spoolReplayed   map[string]int64
	spoolOldestAge  map[string]float64
}

var DefaultMetrics = NewMetricsRegistry()

func NewMetricsRegistry() *MetricsRegistry {
	return &MetricsRegistry{
		eventsEmitted:   make(map[string]int64),
		publishFailures: make(map[string]int64),
		spoolWrites:     make(map[string]int64),
		spoolBytes:      make(map[string]int64),
		spoolReplayed:   make(map[string]int64),
		spoolOldestAge:  make(map[string]float64),
	}
}

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

func (m *MetricsRegistry) IncSpoolWrites(service string) {
	key := fmt.Sprintf("service=%q", service)
	m.mu.Lock()
	m.spoolWrites[key]++
	m.mu.Unlock()
}

func (m *MetricsRegistry) SetSpoolBytes(service string, bytes int64) {
	key := fmt.Sprintf("service=%q", service)
	m.mu.Lock()
	m.spoolBytes[key] = bytes
	m.mu.Unlock()
}

func (m *MetricsRegistry) AddSpoolReplayed(service string, count int64) {
	key := fmt.Sprintf("service=%q", service)
	m.mu.Lock()
	m.spoolReplayed[key] += count
	m.mu.Unlock()
}

func (m *MetricsRegistry) SetSpoolOldestAge(service string, seconds float64) {
	key := fmt.Sprintf("service=%q", service)
	m.mu.Lock()
	m.spoolOldestAge[key] = seconds
	m.mu.Unlock()
}

// Handler returns an http.Handler that renders metrics in Prometheus text exposition format
func (m *MetricsRegistry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		m.mu.RLock()
		defer m.mu.RUnlock()

		var b strings.Builder

		// 1. logging_events_emitted_total
		b.WriteString("# HELP logging_events_emitted_total Total count of structured events emitted by service.\n")
		b.WriteString("# TYPE logging_events_emitted_total counter\n")
		for _, k := range sortedKeys(m.eventsEmitted) {
			fmt.Fprintf(&b, "logging_events_emitted_total{%s} %d\n", k, m.eventsEmitted[k])
		}

		// 2. logging_publish_failures_total
		b.WriteString("# HELP logging_publish_failures_total Total count of message broker publish failures.\n")
		b.WriteString("# TYPE logging_publish_failures_total counter\n")
		for _, k := range sortedKeys(m.publishFailures) {
			fmt.Fprintf(&b, "logging_publish_failures_total{%s} %d\n", k, m.publishFailures[k])
		}

		// 3. logging_spool_writes_total
		b.WriteString("# HELP logging_spool_writes_total Total count of events written to local fallback disk spool.\n")
		b.WriteString("# TYPE logging_spool_writes_total counter\n")
		for _, k := range sortedKeys(m.spoolWrites) {
			fmt.Fprintf(&b, "logging_spool_writes_total{%s} %d\n", k, m.spoolWrites[k])
		}

		// 4. logging_spool_bytes_current
		b.WriteString("# HELP logging_spool_bytes_current Current byte size of the local fallback spool file.\n")
		b.WriteString("# TYPE logging_spool_bytes_current gauge\n")
		for _, k := range sortedKeys(m.spoolBytes) {
			fmt.Fprintf(&b, "logging_spool_bytes_current{%s} %d\n", k, m.spoolBytes[k])
		}

		// 5. logging_spool_replayed_total
		b.WriteString("# HELP logging_spool_replayed_total Total count of events replayed from disk spool to broker.\n")
		b.WriteString("# TYPE logging_spool_replayed_total counter\n")
		for _, k := range sortedKeys(m.spoolReplayed) {
			fmt.Fprintf(&b, "logging_spool_replayed_total{%s} %d\n", k, m.spoolReplayed[k])
		}

		// 6. logging_spool_oldest_age_seconds
		b.WriteString("# HELP logging_spool_oldest_age_seconds Age in seconds of the oldest event pending in disk spool.\n")
		b.WriteString("# TYPE logging_spool_oldest_age_seconds gauge\n")
		for _, k := range sortedFloatKeys(m.spoolOldestAge) {
			fmt.Fprintf(&b, "logging_spool_oldest_age_seconds{%s} %.2f\n", k, m.spoolOldestAge[k])
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
