package pipeline

import (
	"testing"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
)

func TestPrometheusSinkSnapshot(t *testing.T) {
	sink := NewPrometheusSink()
	if err := sink.Write(usage.LLMUsageEvent{
		InteractionEvent: usage.InteractionEvent{RouteKind: "llm", Success: true},
		TotalTokens:      7,
	}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	snap := sink.Snapshot()
	labels := usage.PrometheusLabels{RouteKind: "llm"}
	if snap.Requests[labels] != 1 || snap.Tokens[labels] != 7 {
		t.Fatalf("snapshot = %+v, want one llm request and 7 tokens", snap)
	}
	if err := sink.Write(usage.ACPUsageEvent{InteractionEvent: usage.InteractionEvent{
		RouteKind: "agent", RouteProtocol: "agent", RuntimeType: "acp", AgentID: "high-cardinality-agent-id", Success: false,
	}}); err != nil {
		t.Fatalf("Write(agent) error = %v", err)
	}
	snap = sink.Snapshot()
	agentLabels := usage.PrometheusLabels{RouteKind: "agent", RuntimeType: "acp"}
	if snap.Requests[agentLabels] != 1 || snap.Failures[agentLabels] != 1 || len(snap.Requests) != 2 {
		t.Fatalf("bounded Agent labels snapshot = %+v", snap)
	}
}

type captureExporter struct {
	events int
	closed bool
}

func (e *captureExporter) ExportUsageEvent(any) error {
	e.events++
	return nil
}

func (e *captureExporter) Close() error {
	e.closed = true
	return nil
}

func TestOpenTelemetrySinkDelegates(t *testing.T) {
	exporter := &captureExporter{}
	sink := NewOpenTelemetrySink(exporter)
	if err := sink.Write("event"); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if exporter.events != 1 || !exporter.closed {
		t.Fatalf("exporter = %+v, want one event and closed", exporter)
	}
}
