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

func TestLLMExtensionTokenDetailsReachEvent(t *testing.T) {
	sink := &captureEventSink{}
	observer := NewObserver(sink)

	span, _ := observer.Begin(t.Context(), InteractionDimensions{RouteKind: "llm"})
	span.SetExtension(LLMExtension{
		InputTokens:     Int(10),
		OutputTokens:    Int(5),
		TotalTokens:     Int(15),
		CachedTokens:    Int(4),
		ReasoningTokens: Int(2),
	})
	// A later partial merge must not clear the detail fields.
	span.SetExtension(LLMExtension{ToolCallCount: Int(0)})
	span.Finish(InteractionOutcome{Success: true, StatusCode: 200})

	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want 1", len(sink.events))
	}
	ev, ok := sink.events[0].(LLMUsageEvent)
	if !ok {
		t.Fatalf("event type = %T, want LLMUsageEvent", sink.events[0])
	}
	if ev.CachedTokens != 4 || ev.ReasoningTokens != 2 {
		t.Fatalf("event token details = cached %d reasoning %d, want 4 and 2", ev.CachedTokens, ev.ReasoningTokens)
	}
	if ev.InputTokens != 10 || ev.OutputTokens != 5 || ev.TotalTokens != 15 {
		t.Fatalf("event tokens = %+v, want 10/5/15", ev)
	}
}
