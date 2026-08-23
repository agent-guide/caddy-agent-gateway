package anthropicmsg

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropicbase"
	"github.com/cloudwego/eino/schema"
)

type contractState string
type contractInput string
type transitionClass string

const (
	stateUncommitted  contractState = "uncommitted"
	stateStreamOpened contractState = "stream_opened"
	stateIdle         contractState = "idle"
	stateReasoning    contractState = "reasoning"
	stateText         contractState = "text"
	stateToolUse      contractState = "tool_use"
	stateNativeBlock  contractState = "native_block"
	stateCompleted    contractState = "completed"
	stateFailed       contractState = "failed"
)

const (
	inputProviderOpened contractInput = "provider_opened"
	inputMessageStart   contractInput = "message_start"
	inputGenericReason  contractInput = "generic_reasoning"
	inputGenericText    contractInput = "generic_text"
	inputGenericTool    contractInput = "generic_tool"
	inputNativeStart    contractInput = "native_block_start"
	inputBlockDelta     contractInput = "block_delta"
	inputBlockStop      contractInput = "block_stop"
	inputUsage          contractInput = "usage"
	inputPing           contractInput = "ping"
	inputEOF            contractInput = "eof"
	inputProviderError  contractInput = "provider_error"
	inputCancel         contractInput = "cancel"
	inputSinkError      contractInput = "sink_error"
)

var contractStates = []contractState{
	stateUncommitted, stateStreamOpened, stateIdle, stateReasoning, stateText,
	stateToolUse, stateNativeBlock, stateCompleted, stateFailed,
}

var contractInputs = []contractInput{
	inputProviderOpened, inputMessageStart, inputGenericReason, inputGenericText,
	inputGenericTool, inputNativeStart, inputBlockDelta, inputBlockStop, inputUsage,
	inputPing, inputEOF, inputProviderError, inputCancel, inputSinkError,
}

// streamTransitionContract is test data, not a second implementation. The
// production encoder introduced in phase 1 is exercised against every row.
var streamTransitionContract = buildStreamTransitionContract()

func buildStreamTransitionContract() map[contractState]map[contractInput]transitionClass {
	table := make(map[contractState]map[contractInput]transitionClass, len(contractStates))
	for _, state := range contractStates {
		table[state] = make(map[contractInput]transitionClass, len(contractInputs))
		for _, input := range contractInputs {
			table[state][input] = "invalid_state"
		}
	}

	for _, terminal := range []contractState{stateCompleted, stateFailed} {
		for _, input := range contractInputs {
			table[terminal][input] = "terminal_error"
		}
	}
	table[stateUncommitted][inputProviderOpened] = "accept"
	table[stateUncommitted][inputPing] = "drop_ping"
	for _, input := range []contractInput{inputEOF, inputProviderError, inputCancel, inputSinkError} {
		table[stateUncommitted][input] = "fail_http"
	}

	table[stateStreamOpened][inputMessageStart] = "start_message"
	for _, input := range []contractInput{inputGenericReason, inputGenericText, inputGenericTool} {
		table[stateStreamOpened][input] = "start_message"
	}
	table[stateStreamOpened][inputNativeStart] = "start_message"
	table[stateStreamOpened][inputUsage] = "accept"
	table[stateStreamOpened][inputPing] = "drop_ping"
	for _, input := range []contractInput{inputEOF, inputProviderError, inputCancel, inputSinkError} {
		table[stateStreamOpened][input] = "fail_http"
	}

	for _, state := range []contractState{stateIdle, stateReasoning, stateText, stateToolUse, stateNativeBlock} {
		table[state][inputUsage] = "accept"
		table[state][inputPing] = "emit_ping"
		table[state][inputEOF] = "complete"
		for _, input := range []contractInput{inputProviderError, inputCancel} {
			table[state][input] = "fail_sse"
		}
		table[state][inputSinkError] = "fail_sink"
	}
	table[stateIdle][inputGenericReason] = "open_reasoning"
	table[stateIdle][inputGenericText] = "open_text"
	table[stateIdle][inputGenericTool] = "open_tool_use"
	table[stateIdle][inputNativeStart] = "open_native_block"

	table[stateReasoning][inputGenericReason] = "continue_block"
	table[stateText][inputGenericText] = "continue_block"
	table[stateText][inputBlockDelta] = "continue_block"
	table[stateToolUse][inputGenericTool] = "continue_block"
	table[stateNativeBlock][inputBlockDelta] = "continue_block"
	table[stateNativeBlock][inputBlockStop] = "close_block"

	for _, state := range []contractState{stateReasoning, stateText, stateToolUse, stateNativeBlock} {
		table[state][inputGenericReason] = "open_reasoning"
		table[state][inputGenericText] = "open_text"
		table[state][inputGenericTool] = "open_tool_use"
		table[state][inputNativeStart] = "open_native_block"
	}
	// Same-kind inputs continue instead of reopening a block.
	table[stateReasoning][inputGenericReason] = "continue_block"
	table[stateText][inputGenericText] = "continue_block"
	table[stateText][inputGenericReason] = "drop_late_reasoning"
	table[stateToolUse][inputGenericTool] = "continue_block"
	table[stateToolUse][inputGenericReason] = "drop_late_reasoning"
	table[stateNativeBlock][inputBlockDelta] = "continue_block"
	table[stateNativeBlock][inputBlockStop] = "close_block"
	return table
}

func TestStreamTransitionContractIsComplete(t *testing.T) {
	for _, state := range contractStates {
		row, ok := streamTransitionContract[state]
		if !ok {
			t.Fatalf("missing transition row for %q", state)
		}
		for _, input := range contractInputs {
			if disposition := row[input]; disposition == "" {
				t.Errorf("missing transition for state=%q input=%q", state, input)
			}
		}
		if len(row) != len(contractInputs) {
			t.Errorf("state %q has %d inputs, want %d", state, len(row), len(contractInputs))
		}
	}
}

func TestStreamTransitionContractDrivesProductionEncoder(t *testing.T) {
	for _, state := range contractStates {
		for _, input := range contractInputs {
			state, input := state, input
			t.Run(string(state)+"/"+string(input), func(t *testing.T) {
				fixture := newTransitionFixture(t, state)
				before := len(fixture.sink.events)
				err := fixture.apply(input)
				assertTransitionDisposition(t, fixture, before, err, streamTransitionContract[state][input])
			})
		}
	}
}

type transitionFixture struct {
	encoder *anthropicStreamEncoder
	sink    *memoryStreamSink
}

func newTransitionFixture(t *testing.T, state contractState) *transitionFixture {
	t.Helper()
	sink := &memoryStreamSink{}
	lifecycle := newSpanResponseLifecycle(&recordingInteractionSpan{}, "stream")
	encoder := newAnthropicStreamEncoder(t.Context(), streamEncoderOptions{Model: "client-model", MessageID: "msg_contract"}, sink, lifecycle)
	fixture := &transitionFixture{encoder: encoder, sink: sink}
	if state == stateUncommitted {
		return fixture
	}
	encoder.opened = true
	if state == stateStreamOpened {
		return fixture
	}
	encoder.started = true
	switch state {
	case stateReasoning:
		encoder.activeKind, encoder.activeIndex, encoder.reasoningSourceIndex = "reasoning", 0, 0
	case stateText:
		encoder.activeKind, encoder.activeIndex = "text", 0
	case stateToolUse:
		encoder.activeKind, encoder.activeIndex = "tool_use", 0
		encoder.toolBlocks[0] = &streamToolBlock{blockIndex: 0, started: true, id: "toolu_contract", name: "lookup"}
		encoder.toolBlockOrder = []int{0}
	case stateNativeBlock:
		encoder.activeKind, encoder.activeIndex = "native_block", 0
		encoder.nativeBlockIndices[0] = 0
	case stateCompleted:
		encoder.ended = true
		_ = lifecycle.Finish(responseFinish{StatusCode: 200, Outcome: "completed"})
	case stateFailed:
		encoder.ended = true
		_ = lifecycle.Fail(responseFailure{StatusCode: 502, Outcome: "upstream_stream_error", ErrorType: "provider_stream_failed"})
	}
	return fixture
}

func (f *transitionFixture) apply(input contractInput) error {
	switch input {
	case inputProviderOpened:
		return f.encoder.Open()
	case inputMessageStart:
		return f.encoder.Accept(providerStreamEvent{Native: &anthropicbase.AnthropicStreamEvent{Event: "message_start", Data: json.RawMessage(`{"type":"message_start","message":{"id":"msg_upstream","usage":{}}}`)}})
	case inputGenericReason:
		return f.encoder.Accept(providerStreamEvent{Generic: &schema.Message{Role: schema.Assistant, ReasoningContent: "reason"}})
	case inputGenericText:
		return f.encoder.Accept(providerStreamEvent{Generic: schema.AssistantMessage("text", nil)})
	case inputGenericTool:
		return f.encoder.Accept(providerStreamEvent{Generic: &schema.Message{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{{
				ID: "toolu_next", Function: schema.FunctionCall{Name: "lookup", Arguments: `{}`},
			}},
		}})
	case inputNativeStart:
		return f.encoder.Accept(providerStreamEvent{Native: &anthropicbase.AnthropicStreamEvent{Event: "content_block_start", Data: json.RawMessage(`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)}})
	case inputBlockDelta:
		return f.encoder.Accept(providerStreamEvent{Native: &anthropicbase.AnthropicStreamEvent{Event: "content_block_delta", Data: json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`)}})
	case inputBlockStop:
		return f.encoder.Accept(providerStreamEvent{Native: &anthropicbase.AnthropicStreamEvent{Event: "content_block_stop", Data: json.RawMessage(`{"type":"content_block_stop","index":0}`)}})
	case inputUsage:
		return f.encoder.Accept(providerStreamEvent{Native: &anthropicbase.AnthropicStreamEvent{Event: "message_delta", Data: json.RawMessage(`{"type":"message_delta","usage":{"output_tokens":1}}`)}})
	case inputPing:
		if !f.encoder.opened {
			return nil
		}
		return f.encoder.Accept(providerStreamEvent{Native: &anthropicbase.AnthropicStreamEvent{Event: "ping", Data: json.RawMessage(`{"type":"ping"}`)}})
	case inputEOF:
		return f.encoder.Finish()
	case inputProviderError:
		return f.encoder.Fail(errors.New("provider stream failed"))
	case inputCancel:
		return f.encoder.Cancel(context.Canceled)
	case inputSinkError:
		if !f.encoder.opened {
			return f.encoder.Fail(streamSinkFailure(errors.New("sink failed")))
		}
		f.sink.failAt = len(f.sink.events) + 1
		err := f.encoder.Accept(providerStreamEvent{Generic: schema.AssistantMessage("sink", nil)})
		if err != nil {
			_ = f.encoder.Fail(err)
		}
		return err
	default:
		return errors.New("unknown contract input")
	}
}

func assertTransitionDisposition(t *testing.T, fixture *transitionFixture, before int, err error, disposition transitionClass) {
	t.Helper()
	newEvents := fixture.sink.events[before:]
	hasEvent := func(name string) bool {
		for _, event := range newEvents {
			if event.Event == name {
				return true
			}
		}
		return false
	}
	switch disposition {
	case "invalid_state", "terminal_error":
		if err == nil {
			t.Fatalf("disposition %s returned nil; events=%+v", disposition, newEvents)
		}
	case "fail_http":
		if !fixture.encoder.ended || len(newEvents) != 0 {
			t.Fatalf("fail_http ended=%v events=%+v err=%v", fixture.encoder.ended, newEvents, err)
		}
	case "fail_sse":
		if !fixture.encoder.ended || !hasEvent("error") {
			t.Fatalf("fail_sse ended=%v events=%+v err=%v", fixture.encoder.ended, newEvents, err)
		}
	case "fail_sink":
		if !fixture.encoder.ended || err == nil {
			t.Fatalf("fail_sink ended=%v events=%+v err=%v", fixture.encoder.ended, newEvents, err)
		}
	case "drop_ping":
		if err != nil || len(newEvents) != 0 {
			t.Fatalf("drop_ping events=%+v err=%v", newEvents, err)
		}
	case "drop_late_reasoning":
		if err != nil || len(newEvents) != 0 {
			t.Fatalf("drop_late_reasoning events=%+v err=%v", newEvents, err)
		}
	case "emit_ping":
		if err != nil || !hasEvent("ping") {
			t.Fatalf("emit_ping events=%+v err=%v", newEvents, err)
		}
	case "complete":
		if err != nil || !fixture.encoder.ended || !hasEvent("message_stop") {
			t.Fatalf("complete ended=%v events=%+v err=%v", fixture.encoder.ended, newEvents, err)
		}
	case "start_message":
		if err != nil || !fixture.encoder.started || !hasEvent("message_start") {
			t.Fatalf("start_message started=%v events=%+v err=%v", fixture.encoder.started, newEvents, err)
		}
	case "open_reasoning":
		if err != nil || fixture.encoder.activeKind != "reasoning" {
			t.Fatalf("open_reasoning active=%q events=%+v err=%v", fixture.encoder.activeKind, newEvents, err)
		}
	case "open_text":
		if err != nil || fixture.encoder.activeKind != "text" {
			t.Fatalf("open_text active=%q events=%+v err=%v", fixture.encoder.activeKind, newEvents, err)
		}
	case "open_tool_use":
		if err != nil || !hasToolUseStart(newEvents) {
			t.Fatalf("open_tool_use events=%+v err=%v", newEvents, err)
		}
	case "open_native_block":
		if err != nil || fixture.encoder.activeKind != "native_block" {
			t.Fatalf("open_native_block active=%q events=%+v err=%v", fixture.encoder.activeKind, newEvents, err)
		}
	case "continue_block", "close_block", "accept":
		if err != nil {
			t.Fatalf("disposition %s returned %v; events=%+v", disposition, err, newEvents)
		}
	default:
		t.Fatalf("unhandled transition disposition %q", disposition)
	}
}

func hasToolUseStart(events []anthropicStreamEvent) bool {
	for _, event := range events {
		if event.Event == "content_block_start" && strings.Contains(string(event.Data), `"type":"tool_use"`) {
			return true
		}
	}
	return false
}

func TestCharacterizationFixturesAreSanitizedAndWellFormed(t *testing.T) {
	dir := filepath.Join("testdata", "characterization")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 5 {
		t.Fatalf("fixture count = %d, want at least 5", len(entries))
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(data))
		for _, forbidden := range []string{"authorization:", "x-api-key:", "sk-ant-", "bearer "} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("fixture %s contains forbidden credential marker %q", entry.Name(), forbidden)
			}
		}
		switch filepath.Ext(entry.Name()) {
		case ".json":
			if !json.Valid(data) {
				t.Errorf("fixture %s is not valid JSON", entry.Name())
			}
		case ".sse":
			assertFixtureSSE(t, entry.Name(), data)
		default:
			t.Errorf("unsupported fixture extension: %s", entry.Name())
		}
	}
}

func assertFixtureSSE(t *testing.T, name string, data []byte) {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	event := ""
	frames := 0
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if event == "" {
				t.Errorf("fixture %s has data without event", name)
			}
			if !json.Valid([]byte(strings.TrimPrefix(line, "data: "))) {
				t.Errorf("fixture %s event %s has invalid JSON", name, event)
			}
			frames++
			event = ""
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if frames == 0 {
		t.Errorf("fixture %s contains no SSE frames", name)
	}
}
