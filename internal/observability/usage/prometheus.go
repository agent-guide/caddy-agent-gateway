package usage

import (
	"fmt"
	"sort"
	"strings"
)

// PrometheusSnapshot is a low-cardinality counter view of usage events keyed by
// interaction kind (llm/mcp/acp). It lives in this package so both the sink that
// produces it (internal/observability/pipeline) and the Admin API that renders it can share
// the type without an import cycle.
type PrometheusSnapshot struct {
	RequestsByKind map[string]int64
	FailuresByKind map[string]int64
	TokensByKind   map[string]int64
}

// PrometheusProvider is implemented by the in-process Prometheus sink and lets
// the Admin API expose an O(1) /metrics scrape without re-aggregating SQLite.
type PrometheusProvider interface {
	PrometheusSnapshot() PrometheusSnapshot
}

// RenderPrometheus formats a snapshot plus pipeline health counters as
// Prometheus text exposition (version 0.0.4).
func RenderPrometheus(snap PrometheusSnapshot, stats RuntimeStats) string {
	var b strings.Builder
	writeCounter(&b, "agentgateway_usage_requests_total", "Total gateway usage events by kind.", snap.RequestsByKind)
	writeCounter(&b, "agentgateway_usage_failures_total", "Failed gateway usage events by kind.", snap.FailuresByKind)
	writeCounter(&b, "agentgateway_usage_tokens_total", "Total LLM tokens accounted by kind.", snap.TokensByKind)

	var dropped, failures uint64
	if stats != nil {
		dropped = stats.DroppedEvents()
		failures = stats.WriteFailures()
	}
	fmt.Fprintf(&b, "# HELP agentgateway_usage_dropped_events_total Usage events dropped because the pipeline buffer was full.\n")
	fmt.Fprintf(&b, "# TYPE agentgateway_usage_dropped_events_total counter\n")
	fmt.Fprintf(&b, "agentgateway_usage_dropped_events_total %d\n", dropped)
	fmt.Fprintf(&b, "# HELP agentgateway_usage_write_failures_total Usage events that a sink failed to persist.\n")
	fmt.Fprintf(&b, "# TYPE agentgateway_usage_write_failures_total counter\n")
	fmt.Fprintf(&b, "agentgateway_usage_write_failures_total %d\n", failures)
	return b.String()
}

func writeCounter(b *strings.Builder, name, help string, values map[string]int64) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s counter\n", name)
	kinds := make([]string, 0, len(values))
	for kind := range values {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		fmt.Fprintf(b, "%s{kind=%q} %d\n", name, kind, values[kind])
	}
}
