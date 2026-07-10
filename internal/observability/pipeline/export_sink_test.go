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
	if snap.RequestsByKind["llm"] != 1 || snap.TokensByKind["llm"] != 7 {
		t.Fatalf("snapshot = %+v, want one llm request and 7 tokens", snap)
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
