package anthropicmsg

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropicbase"
	"github.com/cloudwego/eino/schema"
)

type memoryBodySink struct {
	body responseBody
}

func (s *memoryBodySink) Emit(_ context.Context, body responseBody) error {
	s.body = body
	return nil
}

func TestResponseEncoderRelaysNativeBodyAndClosedRewriteSet(t *testing.T) {
	raw, err := os.ReadFile("testdata/characterization/native_response.json")
	if err != nil {
		t.Fatal(err)
	}
	message := &schema.Message{Role: schema.Assistant, ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 19, CompletionTokens: 7}}}
	anthropicbase.AttachAnthropicResponseBody(message, raw)
	span := &recordingInteractionSpan{}
	lifecycle := newSpanResponseLifecycle(span, "batch")
	encoder := newAnthropicResponseEncoder(lifecycle)
	sink := &memoryBodySink{}
	if err := encoder.Emit(t.Context(), responseOpen{Mode: responseModeNativeRelay, RewriteSet: rewriteSet{ClientModel: "client-model"}}, &provider.ChatResponse{Message: message}, sink); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(sink.body.Payload, &response); err != nil {
		t.Fatal(err)
	}
	if response["id"] != "msg_fixture_native_1" || response["model"] != "client-model" || response["future_fixture_top_level"] != "preserve-me" {
		t.Fatalf("relayed response = %#v", response)
	}
}

func TestNormalizedBatchResponseHasMessageID(t *testing.T) {
	response := (&Converter{}).FromInternal(&provider.ChatResponse{Message: schema.AssistantMessage("answer", nil)}, "client-model")
	if response.ID == "" {
		t.Fatal("normalized batch response has empty message id")
	}
}

func TestNativeBatchAndStreamMaterializeEquivalentContent(t *testing.T) {
	raw, err := os.ReadFile("testdata/characterization/native_response.json")
	if err != nil {
		t.Fatal(err)
	}
	var batch struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(raw, &batch); err != nil {
		t.Fatal(err)
	}
	events := readNativeStreamFixture(t)
	chunks := make([]*schema.Message, 0, len(events))
	for _, event := range events {
		chunks = append(chunks, anthropicbase.AttachAnthropicRelayStreamEvent(nil, event.Event, event.Data))
	}
	materialized, err := schema.ConcatMessages(chunks)
	if err != nil {
		t.Fatal(err)
	}
	blocks := anthropicbase.AnthropicContentBlocksFromMessage(materialized)
	if len(blocks) != 1 {
		t.Fatalf("stream history blocks = %d, want 1", len(blocks))
	}
	var streamBlock map[string]any
	if err := json.Unmarshal(blocks[0], &streamBlock); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(streamBlock, batch.Content[0]) {
		t.Fatalf("stream block = %#v, batch block = %#v", streamBlock, batch.Content[0])
	}
}
