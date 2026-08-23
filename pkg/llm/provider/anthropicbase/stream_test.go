package anthropicbase

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

func TestReadMessageStreamPreservesNativeServerToolAndCitationEvents(t *testing.T) {
	body := strings.Join([]string{
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"srv_1\",\"name\":\"web_search\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\\\"latest\\\"}\"}}\n\n",
		"event: content_block_stop\ndata: {\"index\":0}\n\n",
		"event: content_block_delta\ndata: {\"index\":1,\"delta\":{\"type\":\"citations_delta\",\"citation\":{\"url\":\"https://example.com\"}}}\n\n",
	}, "")
	sr, sw := schema.Pipe[*schema.Message](8)
	go ReadMessageStream(io.NopCloser(strings.NewReader(body)), sw, "test")
	var events []AnthropicStreamEvent
	for {
		msg, err := sr.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}
		events = append(events, AnthropicStreamEventsFromMessage(msg)...)
	}
	if len(events) != 4 {
		t.Fatalf("events = %+v, want four native events", events)
	}
	if events[0].Event != "content_block_start" || events[3].Event != "content_block_delta" {
		t.Fatalf("events = %+v", events)
	}
	var citation map[string]any
	if err := json.Unmarshal(events[3].Data, &citation); err != nil || citation["index"] != float64(1) {
		t.Fatalf("citation event = %s, %v", events[3].Data, err)
	}
}

func TestReadMessageStreamPreservesThinkingSignatureAndRedactedData(t *testing.T) {
	body := strings.Join([]string{
		"event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":7}}}\n\n",
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"inspect\"}}\n\n",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"opaque-signature\"}}\n\n",
		"event: content_block_stop\ndata: {\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"index\":1,\"content_block\":{\"type\":\"redacted_thinking\",\"data\":\"opaque-redacted\"}}\n\n",
		"event: content_block_stop\ndata: {\"index\":1}\n\n",
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n",
	}, "")

	sr, sw := schema.Pipe[*schema.Message](16)
	go ReadMessageStream(io.NopCloser(strings.NewReader(body)), sw, "test")

	var chunks []*schema.Message
	for {
		chunk, err := sr.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}
		if len(AnthropicRelayStreamEventsFromMessage(chunk)) > 0 {
			continue
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 6 {
		t.Fatalf("chunk count = %d, want 6: %+v", len(chunks), chunks)
	}
	if parts := provider.ReasoningPartsFromMessage(chunks[1]); chunks[1].ReasoningContent != "inspect" || len(parts) != 1 || parts[0].Reasoning.Text != "inspect" {
		t.Fatalf("thinking delta = %+v", chunks[1])
	}
	if got := provider.ReasoningPartsFromMessage(chunks[2])[0].Reasoning.Signature; got != "opaque-signature" {
		t.Fatalf("signature = %q", got)
	}
	if got := provider.ReasoningPartsFromMessage(chunks[3])[0].Type; got != provider.ChatMessagePartTypeReasoningEnd {
		t.Fatalf("reasoning end type = %q", got)
	}
	redacted := provider.ReasoningPartsFromMessage(chunks[4])[0]
	if redacted.Type != provider.ChatMessagePartTypeEncryptedReasoning || provider.EncryptedReasoningData(redacted) != "opaque-redacted" {
		t.Fatalf("redacted part = %+v", redacted)
	}
	merged, err := schema.ConcatMessages(chunks)
	if err != nil {
		t.Fatalf("ConcatMessages() error = %v", err)
	}
	mergedParts := provider.ReasoningPartsFromMessage(merged)
	if merged.ReasoningContent != "inspect" || len(mergedParts) != 3 {
		t.Fatalf("merged stream = %+v, want reasoning, end marker, and encrypted block", merged)
	}
	if got := mergedParts[0].Reasoning.Signature; got != "opaque-signature" {
		t.Fatalf("merged signature = %q", got)
	}
	if len(merged.AssistantGenMultiContent) != 0 {
		t.Fatalf("merged AssistantGenMultiContent = %+v, want gateway reasoning isolated in Extra", merged.AssistantGenMultiContent)
	}
}
