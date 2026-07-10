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
		RequestsByKind: map[string]int64{"llm": 5, "mcp": 2},
		FailuresByKind: map[string]int64{"llm": 1},
		TokensByKind:   map[string]int64{"llm": 700},
	}
	out := RenderPrometheus(snap, stubStats{dropped: 3, failed: 4})

	for _, want := range []string{
		`agentgateway_usage_requests_total{kind="llm"} 5`,
		`agentgateway_usage_requests_total{kind="mcp"} 2`,
		`agentgateway_usage_failures_total{kind="llm"} 1`,
		`agentgateway_usage_tokens_total{kind="llm"} 700`,
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
