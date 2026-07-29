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

func TestObserverCarriesCommonRuntimeIdentity(t *testing.T) {
	sink := &captureEventSink{}
	observer := NewObserver(sink)
	span, ctx := observer.Begin(t.Context(), InteractionDimensions{
		RouteKind: "builtin", AgentID: "agent-1", RuntimeType: "builtin", RunID: "run-1",
	})
	dims, ok := DimensionsFromContext(ctx)
	if !ok || dims.AgentID != "agent-1" || dims.RuntimeType != "builtin" || dims.RunID != "run-1" {
		t.Fatalf("context dimensions = %+v, present=%v", dims, ok)
	}
	span.SetExtension(CommonExtension{RunID: "run-2"})
	span.Finish(InteractionOutcome{Success: true, StatusCode: 200})
	ev := sink.events[0].(BuiltinUsageEvent)
	if ev.InteractionEvent.RunID != "run-2" || ev.RunID != "run-2" || ev.RuntimeType != "builtin" {
		t.Fatalf("common usage identity = %+v", ev)
	}
}

func TestObserverSelectsTypedStoreForUnifiedAgentRoute(t *testing.T) {
	for _, tc := range []struct {
		runtimeType string
		wantType    string
	}{
		{runtimeType: "acp", wantType: "acp"},
		{runtimeType: "builtin", wantType: "builtin"},
	} {
		t.Run(tc.runtimeType, func(t *testing.T) {
			sink := &captureEventSink{}
			span, _ := NewObserver(sink).Begin(t.Context(), InteractionDimensions{
				RouteKind: "agent", RouteProtocol: "agent", AgentID: "a1", RuntimeType: tc.runtimeType, RunID: "run-1",
			})
			span.Finish(InteractionOutcome{Success: true, StatusCode: 200})
			if len(sink.events) != 1 {
				t.Fatalf("events = %d", len(sink.events))
			}
			switch tc.wantType {
			case "acp":
				if _, ok := sink.events[0].(ACPUsageEvent); !ok {
					t.Fatalf("event type = %T, want ACPUsageEvent", sink.events[0])
				}
			case "builtin":
				if _, ok := sink.events[0].(BuiltinUsageEvent); !ok {
					t.Fatalf("event type = %T, want BuiltinUsageEvent", sink.events[0])
				}
			}
		})
	}
}

func TestFinishFallbackErrorTypeClassifiesByStatusClass(t *testing.T) {
	sink := &captureEventSink{}
	observer := NewObserver(sink)

	span, _ := observer.Begin(t.Context(), InteractionDimensions{RouteKind: "builtin"})
	span.Finish(InteractionOutcome{Success: false, StatusCode: 400})
	span, _ = observer.Begin(t.Context(), InteractionDimensions{RouteKind: "builtin"})
	span.Finish(InteractionOutcome{Success: false, StatusCode: 502})

	if len(sink.events) != 2 {
		t.Fatalf("events = %d, want 2", len(sink.events))
	}
	if got := sink.events[0].(BuiltinUsageEvent).ErrorType; got != "client_error" {
		t.Fatalf("unannotated 4xx error_type = %q, want client_error", got)
	}
	if got := sink.events[1].(BuiltinUsageEvent).ErrorType; got != "internal_error" {
		t.Fatalf("unannotated 5xx error_type = %q, want internal_error", got)
	}
}

func TestDiscardSuppressesTheEvent(t *testing.T) {
	sink := &captureEventSink{}
	observer := NewObserver(sink)

	span, _ := observer.Begin(t.Context(), InteractionDimensions{RouteKind: "builtin"})
	span.Discard()
	span.Finish(InteractionOutcome{Success: true, StatusCode: 200})

	if len(sink.events) != 0 {
		t.Fatalf("events = %d, want 0 after Discard", len(sink.events))
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
