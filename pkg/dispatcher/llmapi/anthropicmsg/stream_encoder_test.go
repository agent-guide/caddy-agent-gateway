package anthropicmsg

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/cloudwego/eino/schema"
)

type memoryStreamSink struct {
	mu     sync.Mutex
	events []anthropicStreamEvent
	failAt int
}

func (s *memoryStreamSink) Emit(_ context.Context, event anthropicStreamEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAt > 0 && len(s.events)+1 == s.failAt {
		return errors.New("fixture sink failure")
	}
	s.events = append(s.events, event)
	return nil
}

func newTestStreamEncoder(t *testing.T, sink *memoryStreamSink) (*anthropicStreamEncoder, *recordingInteractionSpan) {
	t.Helper()
	span := &recordingInteractionSpan{}
	lifecycle := newSpanResponseLifecycle(span, "stream")
	encoder := newAnthropicStreamEncoder(t.Context(), streamEncoderOptions{Model: "client-model", MessageID: "msg_fixture"}, sink, lifecycle)
	if err := encoder.Open(); err != nil {
		t.Fatal(err)
	}
	return encoder, span
}

func TestStreamEncoderDoesNotCommitUntilMeaningfulOutput(t *testing.T) {
	sink := &memoryStreamSink{}
	encoder, _ := newTestStreamEncoder(t, sink)
	if len(sink.events) != 0 {
		t.Fatalf("Open emitted %d events", len(sink.events))
	}
	if err := encoder.Accept(providerStreamEvent{Native: &provider.AnthropicStreamEvent{Event: "ping", Data: json.RawMessage(`{"type":"ping"}`)}}); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 0 {
		t.Fatal("pre-commit ping committed response")
	}
	if err := encoder.Accept(providerStreamEvent{Generic: schema.AssistantMessage("hello", nil)}); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) < 3 || sink.events[0].Event != "message_start" || sink.events[1].Event != "content_block_start" {
		t.Fatalf("events = %+v", sink.events)
	}
}

func TestStreamEncoderSerializesParallelToolCalls(t *testing.T) {
	sink := &memoryStreamSink{}
	encoder, _ := newTestStreamEncoder(t, sink)
	first, second := 0, 1
	chunk := &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
		{Index: &first, ID: "toolu_1", Function: schema.FunctionCall{Name: "one", Arguments: `{"x":1}`}},
		{Index: &second, ID: "toolu_2", Function: schema.FunctionCall{Name: "two", Arguments: `{"x":2}`}},
	}}
	if err := encoder.Accept(providerStreamEvent{Generic: chunk}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Finish(); err != nil {
		t.Fatal(err)
	}
	active := false
	starts := 0
	for _, event := range sink.events {
		switch event.Event {
		case "content_block_start":
			if active {
				t.Fatal("parallel calls produced overlapping Anthropic blocks")
			}
			active = true
			starts++
		case "content_block_stop":
			if !active {
				t.Fatal("block stopped without start")
			}
			active = false
		}
	}
	if active || starts != 2 {
		t.Fatalf("active=%v starts=%d", active, starts)
	}
}

func TestStreamEncoderRejectsUnboundedDeferredText(t *testing.T) {
	sink := &memoryStreamSink{}
	encoder, _ := newTestStreamEncoder(t, sink)
	index := 0
	if err := encoder.Accept(providerStreamEvent{Generic: &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{Index: &index, ID: "toolu_1", Function: schema.FunctionCall{Name: "lookup", Arguments: `{"q":"`}}}}}); err != nil {
		t.Fatal(err)
	}
	err := encoder.Accept(providerStreamEvent{Generic: schema.AssistantMessage(strings.Repeat("x", maxDeferredTextBytes+1), nil)})
	if err == nil || !strings.Contains(err.Error(), "deferred_text") {
		t.Fatalf("overflow error = %v", err)
	}
}

func TestStreamEncoderSinkFailureFinalizesOnce(t *testing.T) {
	sink := &memoryStreamSink{failAt: 2}
	encoder, span := newTestStreamEncoder(t, sink)
	err := encoder.Accept(providerStreamEvent{Generic: schema.AssistantMessage("hello", nil)})
	if err == nil {
		t.Fatal("sink failure was not returned")
	}
	_ = encoder.Fail(err)
	if len(span.finishes) != 1 {
		t.Fatalf("terminal finishes = %d, want 1", len(span.finishes))
	}
}
