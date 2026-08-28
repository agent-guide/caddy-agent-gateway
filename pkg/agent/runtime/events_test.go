package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/agent"
)

type sequencingBackend struct {
	serve func(context.Context, agent.Agent, TurnRequest, EventSink) error
}

func (b *sequencingBackend) RuntimeType() string { return "fake" }
func (b *sequencingBackend) Capabilities(context.Context, agent.Agent) (Capabilities, error) {
	return Capabilities{Executable: true}, nil
}
func (b *sequencingBackend) ServeTurn(ctx context.Context, a agent.Agent, req TurnRequest, emit EventSink) error {
	return b.serve(ctx, a, req, emit)
}

func TestRunSequencerConcurrentOrderingAndTerminalSuppression(t *testing.T) {
	backend := &sequencingBackend{}
	backend.serve = func(_ context.Context, _ agent.Agent, _ TurnRequest, emit EventSink) error {
		var wg sync.WaitGroup
		for range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := emit(TurnEvent{Event: EventDelta, AgentID: "a"}); err != nil {
					t.Errorf("emit delta: %v", err)
				}
			}()
		}
		wg.Wait()
		if err := emit(TurnEvent{Event: EventDone}); err != nil {
			return err
		}
		return emit(TurnEvent{Event: EventError})
	}
	run, err := NewRunSequencer("a", "fake")
	if err != nil {
		t.Fatal(err)
	}
	var events []TurnEvent
	result, err := run.ServeSegment(t.Context(), backend, agent.Agent{ID: "a"}, TurnRequest{}, func(ev TurnEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("ServeSegment() error = %v", err)
	}
	if !result.Started || !result.Terminal {
		t.Fatalf("ServeSegment() result = %+v, want started terminal", result)
	}
	if len(events) != 51 {
		t.Fatalf("events = %d, want 51", len(events))
	}
	for i, ev := range events {
		if ev.Sequence != uint64(i+1) || ev.SegmentIndex != 0 || ev.RunID != run.RunID() || ev.AgentID != "a" {
			t.Fatalf("event[%d] = %+v", i, ev)
		}
	}
	if events[len(events)-1].Event != EventDone {
		t.Fatalf("terminal = %q, want done", events[len(events)-1].Event)
	}
}

func TestRunSequencerContinuesAcrossRestoredSegments(t *testing.T) {
	backend := &sequencingBackend{}
	backend.serve = func(ctx context.Context, _ agent.Agent, req TurnRequest, emit EventSink) error {
		ids, ok := IdentitiesFromContext(ctx)
		if !ok || ids.RunID != req.RunID || ids.RuntimeType != "fake" || ids.TraceID != "trace-1" || ids.SpanID != "span-1" {
			t.Fatalf("context identities = %+v, request run = %q", ids, req.RunID)
		}
		return emit(TurnEvent{Event: EventDone})
	}
	run, err := NewRunSequencer("a", "fake")
	if err != nil {
		t.Fatal(err)
	}
	var events []TurnEvent
	collect := func(ev TurnEvent) error { events = append(events, ev); return nil }
	ctx := WithIdentities(t.Context(), Identities{TraceID: "trace-1", SpanID: "span-1"})
	if _, err := run.ServeSegment(ctx, backend, agent.Agent{ID: "a"}, TurnRequest{}, collect); err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreRunSequencer("a", "fake", run.Cursor())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restored.ServeSegment(ctx, backend, agent.Agent{ID: "a"}, TurnRequest{}, collect); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 || events[0].SegmentIndex != 0 || events[1].SegmentIndex != 1 {
		t.Fatalf("multi-segment events = %+v", events)
	}
	seen := map[uint64]bool{}
	for _, ev := range events {
		if seen[ev.Sequence] {
			t.Fatalf("duplicate (run_id, sequence): %s/%d", ev.RunID, ev.Sequence)
		}
		seen[ev.Sequence] = true
	}
}

func TestRunSequencerPreStreamAndMidStreamErrors(t *testing.T) {
	secret := errors.New("token=secret path=/private/work")
	backend := &sequencingBackend{serve: func(context.Context, agent.Agent, TurnRequest, EventSink) error {
		return WrapError(ErrorBackendUnavailable, "unsafe", secret)
	}}
	run, _ := NewRunSequencer("a", "fake")
	var events []TurnEvent
	result, err := run.ServeSegment(t.Context(), backend, agent.Agent{}, TurnRequest{}, func(ev TurnEvent) error {
		events = append(events, ev)
		return nil
	})
	if !errors.Is(err, ErrBackendUnavailable) || len(events) != 0 || HTTPStatus(err) != 503 {
		t.Fatalf("pre-stream error = %v, events=%+v status=%d", err, events, HTTPStatus(err))
	}
	if result.Started || result.Terminal {
		t.Fatalf("pre-stream result = %+v", result)
	}
	if cursor := run.Cursor(); cursor.NextSegment != 0 || cursor.NextSequence != 1 {
		t.Fatalf("pre-stream failure consumed cursor = %+v", cursor)
	}

	backend.serve = func(_ context.Context, _ agent.Agent, _ TurnRequest, emit EventSink) error {
		if err := emit(TurnEvent{Event: EventDelta, Text: "partial"}); err != nil {
			return err
		}
		return WrapError(ErrorTurnFailed, "unsafe", secret)
	}
	result, err = run.ServeSegment(t.Context(), backend, agent.Agent{}, TurnRequest{}, func(ev TurnEvent) error {
		events = append(events, ev)
		return nil
	})
	if !errors.Is(err, ErrTurnFailed) || len(events) != 2 || events[1].Event != EventError {
		t.Fatalf("mid-stream error = %v, events=%+v", err, events)
	}
	if !result.Started || !result.Terminal {
		t.Fatalf("mid-stream result = %+v", result)
	}
	if events[0].SegmentIndex != 0 || events[1].SegmentIndex != 0 || run.Cursor().NextSegment != 1 {
		t.Fatalf("first started stream segment = %+v, cursor=%+v", events, run.Cursor())
	}
	if string(events[1].Data) != `{"error_type":"turn_failed","message":"turn failed"}` {
		t.Fatalf("public terminal payload = %s", events[1].Data)
	}
}

func TestRunSequencerCancellationEmitsTerminalDone(t *testing.T) {
	backend := &sequencingBackend{serve: func(_ context.Context, _ agent.Agent, _ TurnRequest, emit EventSink) error {
		if err := emit(TurnEvent{Event: EventDelta, Text: "partial"}); err != nil {
			return err
		}
		return context.Canceled
	}}
	run, _ := NewRunSequencer("a", "fake")
	var events []TurnEvent
	result, err := run.ServeSegment(t.Context(), backend, agent.Agent{}, TurnRequest{}, func(ev TurnEvent) error {
		events = append(events, ev)
		return nil
	})
	if !errors.Is(err, ErrTurnCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if !result.Started || !result.Terminal || len(events) != 2 || events[1].Event != EventDone {
		t.Fatalf("cancellation result = %+v, events=%+v", result, events)
	}
	if string(events[1].Data) != `{"stop_reason":"cancelled"}` {
		t.Fatalf("cancellation terminal payload = %s", events[1].Data)
	}
}

func TestRunSequencerRejectsConcurrentSegment(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	backend := &sequencingBackend{serve: func(context.Context, agent.Agent, TurnRequest, EventSink) error {
		close(entered)
		<-release
		return nil
	}}
	run, _ := NewRunSequencer("a", "fake")
	done := make(chan error, 1)
	go func() {
		_, err := run.ServeSegment(t.Context(), backend, agent.Agent{}, TurnRequest{}, func(TurnEvent) error { return nil })
		done <- err
	}()
	<-entered
	result, err := run.ServeSegment(t.Context(), backend, agent.Agent{}, TurnRequest{}, func(TurnEvent) error { return nil })
	if !errors.Is(err, ErrSessionBusy) || result.Started || result.Terminal {
		t.Fatalf("concurrent segment result = %+v, error=%v", result, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first segment error = %v", err)
	}
}

func TestRunSequencerRejectsMismatchedRequestRunID(t *testing.T) {
	called := false
	backend := &sequencingBackend{serve: func(context.Context, agent.Agent, TurnRequest, EventSink) error {
		called = true
		return nil
	}}
	run, _ := NewRunSequencer("a", "fake")
	result, err := run.ServeSegment(t.Context(), backend, agent.Agent{}, TurnRequest{RunID: "run-ffffffffffffffffffffffffffffffff"}, func(TurnEvent) error { return nil })
	if !errors.Is(err, ErrInvalidRequest) || result.Started || result.Terminal || called {
		t.Fatalf("mismatched run result = %+v, error=%v, backend_called=%v", result, err, called)
	}
	if cursor := run.Cursor(); cursor.NextSequence != 1 || cursor.NextSegment != 0 {
		t.Fatalf("mismatched run consumed cursor = %+v", cursor)
	}
}

func TestRunSequencerFirstSinkFailureIsStartedMidStream(t *testing.T) {
	sinkErr := errors.New("sink write failed")
	backend := &sequencingBackend{serve: func(_ context.Context, _ agent.Agent, _ TurnRequest, emit EventSink) error {
		return emit(TurnEvent{Event: EventDelta})
	}}
	run, _ := NewRunSequencer("a", "fake")
	calls := 0
	result, err := run.ServeSegment(t.Context(), backend, agent.Agent{}, TurnRequest{}, func(TurnEvent) error {
		calls++
		return sinkErr
	})
	if !errors.Is(err, sinkErr) || !errors.Is(err, ErrTurnFailed) {
		t.Fatalf("sink failure error = %v", err)
	}
	if !result.Started || !result.Terminal || calls != 2 {
		t.Fatalf("sink failure result = %+v, sink calls=%d", result, calls)
	}
	if cursor := run.Cursor(); cursor.NextSequence != 3 || cursor.NextSegment != 1 {
		t.Fatalf("sink failure cursor = %+v", cursor)
	}
}
