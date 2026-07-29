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
	Requests map[PrometheusLabels]int64
	Failures map[PrometheusLabels]int64
	Tokens   map[PrometheusLabels]int64
}

// PrometheusLabels contains only bounded dimensions. RouteKind is one of the
// registered route families and RuntimeType is empty or a registered Agent
// backend type; request, session, route, endpoint, and Agent ids never appear
// as labels.
type PrometheusLabels struct {
	RouteKind   string
	RuntimeType string
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
	writeCounter(&b, "agentgateway_usage_requests_total", "Total gateway usage events by route kind and runtime type.", snap.Requests)
	writeCounter(&b, "agentgateway_usage_failures_total", "Failed gateway usage events by route kind and runtime type.", snap.Failures)
	writeCounter(&b, "agentgateway_usage_tokens_total", "Total LLM tokens accounted by route kind and runtime type.", snap.Tokens)

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

func writeCounter(b *strings.Builder, name, help string, values map[PrometheusLabels]int64) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s counter\n", name)
	labels := make([]PrometheusLabels, 0, len(values))
	for label := range values {
		labels = append(labels, label)
	}
	sort.Slice(labels, func(i, j int) bool {
		if labels[i].RouteKind == labels[j].RouteKind {
			return labels[i].RuntimeType < labels[j].RuntimeType
		}
		return labels[i].RouteKind < labels[j].RouteKind
	})
	for _, label := range labels {
		fmt.Fprintf(b, "%s{route_kind=%q,runtime_type=%q} %d\n", name, label.RouteKind, label.RuntimeType, values[label])
	}
}
