package provider_test

import (
	"encoding/json"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropic"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropicbase"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/claudecode"
	"github.com/cloudwego/eino/schema"
)

func TestDeclaredAnthropicFeaturesHaveIndexedConformanceCases(t *testing.T) {
	checks := map[provider.ProtocolFeature]func(*testing.T){
		provider.FeatureAnthropicServerToolRequest: func(t *testing.T) {
			state := anthropicbase.NewAnthropicRequestProtocolState([]json.RawMessage{json.RawMessage(`{"type":"web_search_20250305","name":"web_search","future":true}`)}, nil)
			tools, _ := anthropicbase.AnthropicRequestTools(state)
			if len(tools) != 1 || !json.Valid(tools[0]) {
				t.Fatalf("server tool round trip = %q", tools)
			}
		},
		provider.FeatureAnthropicNativeResponse: func(t *testing.T) {
			msg := anthropicbase.AttachAnthropicContentBlocks(schema.AssistantMessage("", nil), []json.RawMessage{json.RawMessage(`{"type":"future_block","opaque":true}`)})
			if got := anthropicbase.AnthropicContentBlocksFromMessage(msg); len(got) != 1 {
				t.Fatalf("native response blocks = %q", got)
			}
		},
		provider.FeatureAnthropicNativeHistoryReplay: func(t *testing.T) {
			block := json.RawMessage(`{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"},"future":true}`)
			msg := anthropicbase.AttachAnthropicContentBlocks(&schema.Message{Role: schema.Assistant}, []json.RawMessage{block})
			request := &anthropicbase.MessagesRequest{}
			items := anthropicbase.ConvertMessages([]*schema.Message{msg}, request, false, nil)
			encoded, _ := json.Marshal(items)
			if !json.Valid(encoded) || !containsJSONField(encoded, "future") {
				t.Fatalf("history replay = %s", encoded)
			}
		},
		provider.FeatureAnthropicReasoningReplay: func(t *testing.T) {
			msg := provider.AttachReasoningParts(&schema.Message{Role: schema.Assistant}, provider.NewReasoningOutputPart("inspect", "authentic-signature", nil))
			if !anthropicbase.HasAnthropicNativeReasoning([]*schema.Message{msg}) {
				t.Fatal("authentic reasoning was not retained")
			}
		},
		provider.FeatureAnthropicStreamRelay: func(t *testing.T) {
			msg := anthropicbase.AttachAnthropicRelayStreamEvent(nil, "message_start", json.RawMessage(`{"type":"message_start","message":{"id":"msg_1"}}`))
			if events := anthropicbase.AnthropicRelayStreamEventsFromMessage(msg); len(events) != 1 || events[0].Event != "message_start" {
				t.Fatalf("relay events = %+v", events)
			}
		},
		provider.FeatureAnthropicBodyRelay: func(t *testing.T) {
			msg := anthropicbase.AttachAnthropicResponseBody(schema.AssistantMessage("answer", nil), json.RawMessage(`{"id":"msg_1","content":[],"future":true}`))
			if body := anthropicbase.AnthropicResponseBodyFromMessage(msg); !containsJSONField(body, "future") {
				t.Fatalf("response body = %s", body)
			}
		},
	}

	expected := map[string][]provider.ProtocolFeature{
		"anthropic": {provider.FeatureAnthropicReasoningReplay, provider.FeatureAnthropicBodyRelay},
		"claudecode": {
			provider.FeatureAnthropicServerToolRequest, provider.FeatureAnthropicNativeResponse,
			provider.FeatureAnthropicNativeHistoryReplay, provider.FeatureAnthropicReasoningReplay,
			provider.FeatureAnthropicStreamRelay, provider.FeatureAnthropicBodyRelay,
		},
	}
	for providerType, expectedFeatures := range expected {
		capabilities := provider.CapabilitiesForProviderType(providerType)
		if len(capabilities.ProtocolFeatures) != len(expectedFeatures) {
			t.Fatalf("provider %s features = %v, want %v", providerType, capabilities.ProtocolFeatures, expectedFeatures)
		}
		for _, feature := range expectedFeatures {
			if !capabilities.SupportsProtocolFeature(feature) {
				t.Fatalf("provider %s omitted required declaration %s", providerType, feature)
			}
		}
		for feature := range capabilities.ProtocolFeatures {
			check := checks[feature]
			if check == nil {
				t.Fatalf("provider %s declares %s without a conformance case", providerType, feature)
			}
			t.Run(providerType+"/"+string(feature), check)
		}
	}
}

func containsJSONField(raw []byte, field string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	encoded, _ := json.Marshal(value)
	return len(encoded) > 0 && string(encoded) != "null" && jsonFieldExists(value, field)
}

func jsonFieldExists(value any, field string) bool {
	switch typed := value.(type) {
	case map[string]any:
		if _, ok := typed[field]; ok {
			return true
		}
		for _, child := range typed {
			if jsonFieldExists(child, field) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if jsonFieldExists(child, field) {
				return true
			}
		}
	}
	return false
}
