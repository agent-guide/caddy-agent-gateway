package provider

import (
	"encoding/json"
	"strings"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

const (
	anthropicContentBlocksKey = "_agent_gateway_anthropic_content_blocks"
	anthropicStreamEventKey   = "_agent_gateway_anthropic_stream_event"
)

// AnthropicStreamEvent carries one native Messages SSE event that cannot be
// represented by the generic eino message model.
type AnthropicStreamEvent struct {
	Event string
	Data  json.RawMessage
}

// AnthropicContentBlocks is the gateway-owned representation of an exact
// Anthropic Messages content sequence. A named type keeps its stream concat
// behavior scoped to this protocol state instead of all []json.RawMessage
// values stored in message extras.
type AnthropicContentBlocks []json.RawMessage

// AnthropicStreamEvents accumulates exact Anthropic SSE events when eino
// materializes a streamed response into one message.
type AnthropicStreamEvents []AnthropicStreamEvent

func init() {
	compose.RegisterStreamChunkConcatFunc(func(groups []AnthropicContentBlocks) (AnthropicContentBlocks, error) {
		var merged AnthropicContentBlocks
		for _, group := range groups {
			for _, raw := range group {
				merged = append(merged, append(json.RawMessage(nil), raw...))
			}
		}
		return merged, nil
	})
	compose.RegisterStreamChunkConcatFunc(func(groups []AnthropicStreamEvents) (AnthropicStreamEvents, error) {
		var merged AnthropicStreamEvents
		for _, group := range groups {
			for _, event := range group {
				merged = append(merged, cloneAnthropicStreamEvent(event))
			}
		}
		return merged, nil
	})
}

func AttachAnthropicContentBlocks(msg *schema.Message, blocks []json.RawMessage) *schema.Message {
	if msg == nil || len(blocks) == 0 {
		return msg
	}
	if msg.Extra == nil {
		msg.Extra = map[string]any{}
	}
	cloned := make(AnthropicContentBlocks, len(blocks))
	for i, raw := range blocks {
		cloned[i] = append(json.RawMessage(nil), raw...)
	}
	msg.Extra[anthropicContentBlocksKey] = cloned
	return msg
}

func AnthropicContentBlocksFromMessage(msg *schema.Message) []json.RawMessage {
	if msg == nil || msg.Extra == nil {
		return nil
	}
	blocks, _ := msg.Extra[anthropicContentBlocksKey].(AnthropicContentBlocks)
	cloned := make([]json.RawMessage, len(blocks))
	for i, raw := range blocks {
		cloned[i] = append(json.RawMessage(nil), raw...)
	}
	return cloned
}

// RewriteAnthropicContentToolNames updates client tool_use blocks while
// retaining every provider-specific field in the native content sequence.
func RewriteAnthropicContentToolNames(msg *schema.Message, names map[string]string) {
	if msg == nil || len(names) == 0 {
		return
	}
	blocks := AnthropicContentBlocksFromMessage(msg)
	changed := false
	for i, raw := range blocks {
		var block map[string]any
		if json.Unmarshal(raw, &block) != nil || block["type"] != "tool_use" {
			continue
		}
		name, _ := block["name"].(string)
		replacement, ok := names[name]
		if !ok {
			continue
		}
		block["name"] = replacement
		updated, err := json.Marshal(block)
		if err != nil {
			continue
		}
		blocks[i] = updated
		changed = true
	}
	if changed {
		AttachAnthropicContentBlocks(msg, blocks)
	}
}

func AttachAnthropicStreamEvent(msg *schema.Message, event string, data json.RawMessage) *schema.Message {
	if msg == nil {
		msg = &schema.Message{Role: schema.Assistant}
	}
	if msg.Extra == nil {
		msg.Extra = map[string]any{}
	}
	msg.Extra[anthropicStreamEventKey] = AnthropicStreamEvents{{
		Event: event,
		Data:  append(json.RawMessage(nil), data...),
	}}
	return msg
}

func AnthropicStreamEventsFromMessage(msg *schema.Message) AnthropicStreamEvents {
	if msg == nil || msg.Extra == nil {
		return nil
	}
	events, ok := msg.Extra[anthropicStreamEventKey].(AnthropicStreamEvents)
	if !ok {
		return nil
	}
	cloned := make(AnthropicStreamEvents, len(events))
	for i, event := range events {
		cloned[i] = cloneAnthropicStreamEvent(event)
	}
	return cloned
}

func cloneAnthropicStreamEvent(event AnthropicStreamEvent) AnthropicStreamEvent {
	event.Data = append(json.RawMessage(nil), event.Data...)
	return event
}

// HasAnthropicServerTools reports whether a request contains versioned
// Anthropic-native tools that cannot be represented by a generic provider.
func HasAnthropicServerTools(opts ...einomodel.Option) bool {
	extra := ChatExtraFieldsFromOptions(opts...)
	if extra == nil {
		return false
	}
	for _, raw := range extra.AnthropicTools {
		var tool struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &tool) != nil {
			continue
		}
		typ := strings.TrimSpace(tool.Type)
		if typ != "" && typ != "custom" {
			return true
		}
	}
	return false
}

// HasAnthropicNativeReasoning reports whether a message carries authentic
// Anthropic reasoning state that cannot safely cross provider or model
// boundaries. Gateway-generated placeholder signatures remain portable.
func HasAnthropicNativeReasoning(messages []*schema.Message) bool {
	for _, msg := range messages {
		for _, part := range ReasoningPartsFromMessage(msg) {
			switch part.Type {
			case schema.ChatMessagePartTypeReasoning:
				if part.Reasoning != nil && part.Reasoning.Signature != "" &&
					!IsGatewayThinkingSignature(part.Reasoning.Signature) {
					return true
				}
			case ChatMessagePartTypeEncryptedReasoning:
				if EncryptedReasoningData(part) != "" {
					return true
				}
			}
		}
	}
	return false
}

func HasAnthropicNativeContent(messages []*schema.Message) bool {
	for _, msg := range messages {
		if hasAnthropicContentBlocks(msg) {
			return true
		}
	}
	return false
}

func hasAnthropicContentBlocks(msg *schema.Message) bool {
	if msg == nil || msg.Extra == nil {
		return false
	}
	blocks, _ := msg.Extra[anthropicContentBlocksKey].(AnthropicContentBlocks)
	return len(blocks) > 0
}
