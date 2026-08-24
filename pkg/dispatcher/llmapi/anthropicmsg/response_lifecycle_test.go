package anthropicmsg

import (
	"errors"
	"sync"
	"testing"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
)

type recordingInteractionSpan struct {
	mu       sync.Mutex
	finishes []usage.InteractionOutcome
	exts     []any
	finished bool
}

func (s *recordingInteractionSpan) SetExtension(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.exts = append(s.exts, v)
}
func (*recordingInteractionSpan) AddAnnotation(string, string) {}
func (*recordingInteractionSpan) Discard()                     {}
func (s *recordingInteractionSpan) Finish(outcome usage.InteractionOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.finished = true
	s.finishes = append(s.finishes, outcome)
}

func TestResponseLifecycleFinalizesExactlyOnce(t *testing.T) {
	span := &recordingInteractionSpan{}
	lifecycle := newSpanResponseLifecycle(span, "stream")
	lifecycle.ObserveUsage(usageObservation{InputTokens: 3, OutputTokens: 2, Final: true})
	lifecycle.ObserveToolNames(map[string]struct{}{"zeta": {}, "alpha": {}})
	lifecycle.ObserveResponse(responseObservation{Mode: "native_relay", MessageIDSource: "upstream", UsageSource: "native_stream"})
	if err := lifecycle.Finish(responseFinish{StatusCode: 200, Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Fail(responseFailure{StatusCode: 502, Outcome: "upstream_stream_error"}); !errors.Is(err, errResponseLifecycleFinalized) {
		t.Fatalf("second terminal error = %v", err)
	}
	if len(span.finishes) != 1 || len(span.exts) != 1 {
		t.Fatalf("finishes=%d extensions=%d, want one each", len(span.finishes), len(span.exts))
	}
	extension, ok := span.exts[0].(usage.LLMExtension)
	if !ok || extension.ResponseMode != "native_relay" || extension.MessageIDSource != "upstream" || extension.UsageSource != "native_stream" {
		t.Fatalf("extension = %#v", span.exts[0])
	}
	if extension.ToolCallCount == nil || *extension.ToolCallCount != 2 || len(extension.ToolNames) != 2 || extension.ToolNames[0] != "alpha" || extension.ToolNames[1] != "zeta" {
		t.Fatalf("tool extension = %#v", extension)
	}
}

func TestResponseLifecycleConcurrentTerminalCallsFinishOnce(t *testing.T) {
	span := &recordingInteractionSpan{}
	lifecycle := newSpanResponseLifecycle(span, "stream")
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_ = lifecycle.Finish(responseFinish{StatusCode: 200, Outcome: "completed"})
	}()
	go func() {
		defer wg.Done()
		<-start
		_ = lifecycle.Fail(responseFailure{StatusCode: 502, Outcome: "sink_error", ErrorType: "sink_error"})
	}()
	close(start)
	wg.Wait()
	if len(span.finishes) != 1 {
		t.Fatalf("terminal finishes = %d, want 1", len(span.finishes))
	}
}
