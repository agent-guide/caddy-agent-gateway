package pipeline

import (
	"sync"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
)

// PrometheusSink is a dependency-free in-process sink that keeps counters in a
// shape the Admin API /metrics exposition layer renders into Prometheus text.
type PrometheusSink struct {
	mu       sync.RWMutex
	requests map[string]int64
	failures map[string]int64
	tokens   map[string]int64
}

func NewPrometheusSink() *PrometheusSink {
	return &PrometheusSink{
		requests: map[string]int64{},
		failures: map[string]int64{},
		tokens:   map[string]int64{},
	}
}

func (s *PrometheusSink) Write(ev any) error {
	if s == nil {
		return nil
	}
	kind, success, tokens := eventMetrics(ev)
	if kind == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests[kind]++
	if !success {
		s.failures[kind]++
	}
	if tokens > 0 {
		s.tokens[kind] += int64(tokens)
	}
	return nil
}

func (s *PrometheusSink) Close() error { return nil }

func (s *PrometheusSink) Snapshot() usage.PrometheusSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return usage.PrometheusSnapshot{
		RequestsByKind: cloneInt64Map(s.requests),
		FailuresByKind: cloneInt64Map(s.failures),
		TokensByKind:   cloneInt64Map(s.tokens),
	}
}

// PrometheusSnapshot satisfies usage.PrometheusProvider.
func (s *PrometheusSink) PrometheusSnapshot() usage.PrometheusSnapshot {
	return s.Snapshot()
}

func eventMetrics(ev any) (kind string, success bool, tokens int) {
	switch e := ev.(type) {
	case usage.LLMUsageEvent:
		return "llm", e.Success, e.TotalTokens
	case usage.MCPUsageEvent:
		return "mcp", e.Success, 0
	case usage.ACPUsageEvent:
		return "acp", e.Success, 0
	default:
		return "", false, 0
	}
}

func cloneInt64Map(src map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
