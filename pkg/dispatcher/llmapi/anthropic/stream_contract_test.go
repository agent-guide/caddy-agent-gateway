package anthropic

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	table[stateStreamOpened][inputUsage] = "accept"
	table[stateStreamOpened][inputPing] = "drop_ping"
	for _, input := range []contractInput{inputEOF, inputProviderError, inputCancel, inputSinkError} {
		table[stateStreamOpened][input] = "fail_http"
	}

	for _, state := range []contractState{stateIdle, stateReasoning, stateText, stateToolUse, stateNativeBlock} {
		table[state][inputUsage] = "accept"
		table[state][inputPing] = "emit_ping"
		table[state][inputEOF] = "complete"
		for _, input := range []contractInput{inputProviderError, inputCancel, inputSinkError} {
			table[state][input] = "fail_sse"
		}
	}
	table[stateIdle][inputGenericReason] = "open_reasoning"
	table[stateIdle][inputGenericText] = "open_text"
	table[stateIdle][inputGenericTool] = "open_tool_use"
	table[stateIdle][inputNativeStart] = "open_native_block"

	table[stateReasoning][inputGenericReason] = "continue_block"
	table[stateText][inputGenericText] = "continue_block"
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
	table[stateToolUse][inputGenericTool] = "continue_block"
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
