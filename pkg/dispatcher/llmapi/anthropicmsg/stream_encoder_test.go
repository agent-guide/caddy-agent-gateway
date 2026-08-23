package anthropicmsg

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropicbase"
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
	if err := encoder.Accept(providerStreamEvent{Native: &anthropicbase.AnthropicStreamEvent{Event: "ping", Data: json.RawMessage(`{"type":"ping"}`)}}); err != nil {
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
	encoder, span := newTestStreamEncoder(t, sink)
	index := 0
	if err := encoder.Accept(providerStreamEvent{Generic: &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{Index: &index, ID: "toolu_1", Function: schema.FunctionCall{Name: "lookup", Arguments: `{"q":"`}}}}}); err != nil {
		t.Fatal(err)
	}
	err := encoder.Accept(providerStreamEvent{Generic: schema.AssistantMessage(strings.Repeat("x", maxDeferredTextBytes+1), nil)})
	if err == nil || !strings.Contains(err.Error(), "deferred_text") || !strings.Contains(err.Error(), fmt.Sprintf("%d bytes", maxDeferredTextBytes+1)) {
		t.Fatalf("overflow error = %v", err)
	}
	_ = encoder.Fail(err)
	assertLastResponseOutcome(t, span, "invalid_state")
}

func TestStreamEncoderRejectsUnboundedToolArguments(t *testing.T) {
	sink := &memoryStreamSink{}
	encoder, span := newTestStreamEncoder(t, sink)
	index := 0
	err := encoder.Accept(providerStreamEvent{Generic: &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
		Index: &index, ID: "toolu_1", Function: schema.FunctionCall{Name: "lookup", Arguments: strings.Repeat("x", maxToolArgumentBytes+1)},
	}}}})
	if err == nil || !strings.Contains(err.Error(), "tool_arguments") || !strings.Contains(err.Error(), fmt.Sprintf("%d bytes", maxToolArgumentBytes+1)) {
		t.Fatalf("overflow error = %v", err)
	}
	_ = encoder.Fail(err)
	assertLastResponseOutcome(t, span, "invalid_state")
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
	assertLastResponseOutcome(t, span, "sink_error")
}

func assertLastResponseOutcome(t *testing.T, span *recordingInteractionSpan, want string) {
	t.Helper()
	for i := len(span.exts) - 1; i >= 0; i-- {
		if extension, ok := span.exts[i].(usage.LLMExtension); ok && extension.ResponseOutcome != "" {
			if extension.ResponseOutcome != want {
				t.Fatalf("response outcome = %q, want %q", extension.ResponseOutcome, want)
			}
			return
		}
	}
	t.Fatalf("response outcome %q was not recorded in %#v", want, span.exts)
}

func TestStreamEncoderRelaysNativeLifecycleWithoutReindexing(t *testing.T) {
	inputs := readNativeStreamFixture(t)
	sink := &memoryStreamSink{}
	span := &recordingInteractionSpan{}
	lifecycle := newSpanResponseLifecycle(span, "stream")
	encoder := newAnthropicStreamEncoder(t.Context(), streamEncoderOptions{Model: "client-model", Mode: streamModeNativeRelay}, sink, lifecycle)
	if err := encoder.Open(); err != nil {
		t.Fatal(err)
	}
	for i := range inputs {
		if err := encoder.Accept(providerStreamEvent{Native: &inputs[i]}); err != nil {
			t.Fatalf("accept %s: %v", inputs[i].Event, err)
		}
	}
	if err := encoder.Finish(); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != len(inputs) {
		t.Fatalf("relayed events = %d, want %d", len(sink.events), len(inputs))
	}
	var start struct {
		Message struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"message"`
	}
	if err := json.Unmarshal(sink.events[0].Data, &start); err != nil {
		t.Fatal(err)
	}
	if start.Message.ID != "msg_fixture_native_1" || start.Message.Model != "client-model" {
		t.Fatalf("message_start = %+v", start.Message)
	}
	for i := range inputs {
		if sink.events[i].Event != inputs[i].Event {
			t.Fatalf("event %d = %s, want %s", i, sink.events[i].Event, inputs[i].Event)
		}
	}
}

func TestStreamEncoderConcurrentCancelAndEOFHasOneTerminalOutcome(t *testing.T) {
	sink := &memoryStreamSink{}
	encoder, span := newTestStreamEncoder(t, sink)
	if err := encoder.Accept(providerStreamEvent{Generic: schema.AssistantMessage("hello", nil)}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_ = encoder.Finish()
	}()
	go func() {
		defer wg.Done()
		<-start
		_ = encoder.Cancel(context.Canceled)
	}()
	close(start)
	wg.Wait()
	if len(span.finishes) != 1 {
		t.Fatalf("terminal finishes = %d, want 1", len(span.finishes))
	}
}

func FuzzAnthropicStreamEncoderControlledTermination(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4})
	f.Add([]byte{1, 2, 2, 0, 0, 5})
	f.Fuzz(func(t *testing.T, sequence []byte) {
		if len(sequence) > 1024 {
			sequence = sequence[:1024]
		}
		sink := &memoryStreamSink{}
		encoder, _ := newTestStreamEncoder(t, sink)
		toolIndex := 0
		for _, symbol := range sequence {
			var event providerStreamEvent
			switch symbol % 6 {
			case 0:
				event.Generic = schema.AssistantMessage(strings.Repeat("x", 4096), nil)
			case 1:
				event.Generic = &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
					Index: &toolIndex, ID: "toolu_fuzz", Function: schema.FunctionCall{Name: "lookup", Arguments: `{"q":"`},
				}}}
			case 2:
				event.Generic = &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
					Index: &toolIndex, Function: schema.FunctionCall{Arguments: strings.Repeat("y", 4096)},
				}}}
			case 3:
				event.Native = &anthropicbase.AnthropicStreamEvent{Event: "ping", Data: json.RawMessage(`{"type":"ping"}`)}
			case 4:
				event.Native = &anthropicbase.AnthropicStreamEvent{Event: "message_delta", Data: json.RawMessage(`{"type":"message_delta","usage":{"output_tokens":1}}`)}
			case 5:
				event.Generic = schema.AssistantMessage("tail", nil)
			}
			if err := encoder.Accept(event); err != nil {
				_ = encoder.Fail(err)
				break
			}
			if encoder.deferredText.Len() > maxDeferredTextBytes {
				t.Fatalf("deferred text retained %d bytes", encoder.deferredText.Len())
			}
			for _, block := range encoder.toolBlocks {
				if block != nil && block.arguments.Len() > maxToolArgumentBytes {
					t.Fatalf("tool arguments retained %d bytes", block.arguments.Len())
				}
			}
		}
		if !encoder.ended {
			_ = encoder.Finish()
		}
		for _, event := range sink.events {
			if event.Event == "" || !json.Valid(event.Data) {
				t.Fatalf("invalid downstream event: %+v", event)
			}
		}
	})
}

func readNativeStreamFixture(t *testing.T) []anthropicbase.AnthropicStreamEvent {
	t.Helper()
	file, err := os.Open("testdata/characterization/native_stream.sse")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var inputs []anthropicbase.AnthropicStreamEvent
	var event string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") {
			inputs = append(inputs, anthropicbase.AnthropicStreamEvent{Event: event, Data: json.RawMessage(strings.TrimPrefix(line, "data: "))})
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return inputs
}
