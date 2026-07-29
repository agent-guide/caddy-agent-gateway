package pipeline

import (
	"sync"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
)

// PrometheusSink is a dependency-free in-process sink that keeps counters in a
// shape the Admin API /metrics exposition layer renders into Prometheus text.
type PrometheusSink struct {
	mu       sync.RWMutex
	requests map[usage.PrometheusLabels]int64
	failures map[usage.PrometheusLabels]int64
	tokens   map[usage.PrometheusLabels]int64
}

func NewPrometheusSink() *PrometheusSink {
	return &PrometheusSink{
		requests: map[usage.PrometheusLabels]int64{},
		failures: map[usage.PrometheusLabels]int64{},
		tokens:   map[usage.PrometheusLabels]int64{},
	}
}

func (s *PrometheusSink) Write(ev any) error {
	if s == nil {
		return nil
	}
	labels, success, tokens := eventMetrics(ev)
	if labels.RouteKind == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests[labels]++
	if !success {
		s.failures[labels]++
	}
	if tokens > 0 {
		s.tokens[labels] += int64(tokens)
	}
	return nil
}

func (s *PrometheusSink) Close() error { return nil }

func (s *PrometheusSink) Snapshot() usage.PrometheusSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return usage.PrometheusSnapshot{
		Requests: cloneInt64Map(s.requests),
		Failures: cloneInt64Map(s.failures),
		Tokens:   cloneInt64Map(s.tokens),
	}
}

// PrometheusSnapshot satisfies usage.PrometheusProvider.
func (s *PrometheusSink) PrometheusSnapshot() usage.PrometheusSnapshot {
	return s.Snapshot()
}

func eventMetrics(ev any) (labels usage.PrometheusLabels, success bool, tokens int) {
	switch e := ev.(type) {
	case usage.LLMUsageEvent:
		return prometheusLabels(e.InteractionEvent), e.Success, e.TotalTokens
	case usage.MCPUsageEvent:
		return prometheusLabels(e.InteractionEvent), e.Success, 0
	case usage.ACPUsageEvent:
		return prometheusLabels(e.InteractionEvent), e.Success, 0
	case usage.BuiltinUsageEvent:
		return prometheusLabels(e.InteractionEvent), e.Success, 0
	default:
		return usage.PrometheusLabels{}, false, 0
	}
}

func prometheusLabels(ev usage.InteractionEvent) usage.PrometheusLabels {
	return usage.PrometheusLabels{RouteKind: ev.RouteKind, RuntimeType: ev.RuntimeType}
}

func cloneInt64Map(src map[usage.PrometheusLabels]int64) map[usage.PrometheusLabels]int64 {
	out := make(map[usage.PrometheusLabels]int64, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
