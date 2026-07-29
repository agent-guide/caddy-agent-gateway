package usage

import (
	"strings"
	"testing"
)

type stubStats struct {
	dropped uint64
	failed  uint64
}

func (s stubStats) DroppedEvents() uint64 { return s.dropped }
func (s stubStats) WriteFailures() uint64 { return s.failed }

func TestRenderPrometheus(t *testing.T) {
	snap := PrometheusSnapshot{
		Requests: map[PrometheusLabels]int64{{RouteKind: "llm"}: 5, {RouteKind: "agent", RuntimeType: "builtin"}: 2},
		Failures: map[PrometheusLabels]int64{{RouteKind: "llm"}: 1},
		Tokens:   map[PrometheusLabels]int64{{RouteKind: "llm"}: 700},
	}
	out := RenderPrometheus(snap, stubStats{dropped: 3, failed: 4})

	for _, want := range []string{
		`agentgateway_usage_requests_total{route_kind="llm",runtime_type=""} 5`,
		`agentgateway_usage_requests_total{route_kind="agent",runtime_type="builtin"} 2`,
		`agentgateway_usage_failures_total{route_kind="llm",runtime_type=""} 1`,
		`agentgateway_usage_tokens_total{route_kind="llm",runtime_type=""} 700`,
		"agentgateway_usage_dropped_events_total 3",
		"agentgateway_usage_write_failures_total 4",
		"# TYPE agentgateway_usage_requests_total counter",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("exposition missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderPrometheusNilStats(t *testing.T) {
	out := RenderPrometheus(PrometheusSnapshot{}, nil)
	if !strings.Contains(out, "agentgateway_usage_dropped_events_total 0") {
		t.Fatalf("expected zeroed counters with nil stats, got:\n%s", out)
	}
}
