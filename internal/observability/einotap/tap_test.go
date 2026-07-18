package einotap

import (
	"testing"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
)

type captureSink struct {
	events []any
}

func (s *captureSink) Enqueue(v any) bool {
	s.events = append(s.events, v)
	return true
}

func TestTapFoldsChatModelTokenUsageIntoSpan(t *testing.T) {
	sink := &captureSink{}
	observer := usage.NewObserver(sink)
	span, ctx := observer.Begin(t.Context(), usage.InteractionDimensions{RouteKind: "llm"})

	// The tap must be reachable through the real aspect path: EnsureRunInfo
	// initializes the callback manager from the global handlers, and the OnEnd
	// aspect dispatches to them. Register is the production entry point.
	Register()
	ctx = callbacks.EnsureRunInfo(ctx, "test-model", components.ComponentOfChatModel)
	ctx = callbacks.OnStart(ctx, &model.CallbackInput{})
	callbacks.OnEnd(ctx, &model.CallbackOutput{
		TokenUsage: &model.TokenUsage{
			PromptTokens: 11,
			PromptTokenDetails: model.PromptTokenDetails{
				CachedTokens: 7,
			},
			CompletionTokens: 5,
			CompletionTokensDetails: model.CompletionTokensDetails{
				ReasoningTokens: 2,
			},
		},
	})
	span.Finish(usage.InteractionOutcome{Success: true, StatusCode: 200})

	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want exactly 1 (the tap must merge, never emit)", len(sink.events))
	}
	ev, ok := sink.events[0].(usage.LLMUsageEvent)
	if !ok {
		t.Fatalf("event type = %T, want LLMUsageEvent", sink.events[0])
	}
	if ev.InputTokens != 11 || ev.OutputTokens != 5 || ev.TotalTokens != 16 {
		t.Fatalf("tokens = %d/%d/%d, want 11/5/16", ev.InputTokens, ev.OutputTokens, ev.TotalTokens)
	}
	if ev.CachedTokens != 7 || ev.ReasoningTokens != 2 {
		t.Fatalf("token details = cached %d reasoning %d, want 7 and 2", ev.CachedTokens, ev.ReasoningTokens)
	}
	if !ev.UsageFinalized {
		t.Fatal("UsageFinalized = false, want true when tokens are present")
	}
}

func TestTapIgnoresNonChatModelComponents(t *testing.T) {
	sink := &captureSink{}
	observer := usage.NewObserver(sink)
	span, ctx := observer.Begin(t.Context(), usage.InteractionDimensions{RouteKind: "llm"})

	h := Handler()
	h.OnEnd(ctx, &callbacks.RunInfo{Component: components.ComponentOfTool}, &model.CallbackOutput{
		TokenUsage: &model.TokenUsage{PromptTokens: 99},
	})
	span.Finish(usage.InteractionOutcome{Success: true, StatusCode: 200})

	ev := sink.events[0].(usage.LLMUsageEvent)
	if ev.InputTokens != 0 {
		t.Fatalf("InputTokens = %d, want 0 (non-chat-model output must be ignored)", ev.InputTokens)
	}
}

func TestTapNeedsOnlyOnEnd(t *testing.T) {
	checker, ok := Handler().(callbacks.TimingChecker)
	if !ok {
		t.Fatal("handler does not implement TimingChecker")
	}
	for _, timing := range []callbacks.CallbackTiming{
		callbacks.TimingOnStart,
		callbacks.TimingOnError,
		callbacks.TimingOnStartWithStreamInput,
		callbacks.TimingOnEndWithStreamOutput,
	} {
		if checker.Needed(t.Context(), nil, timing) {
			t.Fatalf("Needed(%v) = true, want false for every timing except OnEnd", timing)
		}
	}
	if !checker.Needed(t.Context(), nil, callbacks.TimingOnEnd) {
		t.Fatal("Needed(OnEnd) = false, want true")
	}
}
