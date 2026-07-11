package usage

import "testing"

type captureEventSink struct {
	events []any
}

func (s *captureEventSink) Enqueue(v any) bool {
	s.events = append(s.events, v)
	return true
}

func TestObserverGeneratesW3CTraceAndSpanIDs(t *testing.T) {
	sink := &captureEventSink{}
	observer := NewObserver(sink)

	span, _ := observer.Begin(t.Context(), InteractionDimensions{RouteKind: "acp"})
	span.Finish(InteractionOutcome{Success: true, StatusCode: 200})

	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want 1", len(sink.events))
	}
	ev, ok := sink.events[0].(ACPUsageEvent)
	if !ok {
		t.Fatalf("event type = %T, want ACPUsageEvent", sink.events[0])
	}
	if !ValidTraceID(ev.TraceID) {
		t.Fatalf("trace_id = %q, want 32 lowercase hex chars and non-zero", ev.TraceID)
	}
	if !ValidSpanID(ev.SpanID) {
		t.Fatalf("span_id = %q, want 16 lowercase hex chars and non-zero", ev.SpanID)
	}
}
