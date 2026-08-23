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
}

func (s *recordingInteractionSpan) SetExtension(v any) {
	s.mu.Lock()
	s.exts = append(s.exts, v)
	s.mu.Unlock()
}
func (*recordingInteractionSpan) AddAnnotation(string, string) {}
func (*recordingInteractionSpan) Discard()                     {}
func (s *recordingInteractionSpan) Finish(outcome usage.InteractionOutcome) {
	s.mu.Lock()
	s.finishes = append(s.finishes, outcome)
	s.mu.Unlock()
}

func TestResponseLifecycleFinalizesExactlyOnce(t *testing.T) {
	span := &recordingInteractionSpan{}
	lifecycle := newSpanResponseLifecycle(span, "stream")
	lifecycle.ObserveUsage(usageObservation{InputTokens: 3, OutputTokens: 2, Final: true})
	if err := lifecycle.Finish(responseFinish{StatusCode: 200, Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Fail(responseFailure{StatusCode: 502, Outcome: "upstream_stream_error"}); !errors.Is(err, errResponseLifecycleFinalized) {
		t.Fatalf("second terminal error = %v", err)
	}
	if len(span.finishes) != 1 || len(span.exts) != 1 {
		t.Fatalf("finishes=%d extensions=%d, want one each", len(span.finishes), len(span.exts))
	}
}
