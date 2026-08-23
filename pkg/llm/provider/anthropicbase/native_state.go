package anthropicbase

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/cloudwego/eino/schema"
)

func init() {
	_, err := provider.NewProtocolRequirementSet(map[provider.ProtocolFeature][]provider.RequirementReason{
		provider.FeatureAnthropicServerToolRequest:   {provider.ReasonAnthropicServerTool},
		provider.FeatureAnthropicNativeResponse:      {provider.ReasonAnthropicServerTool},
		provider.FeatureAnthropicNativeHistoryReplay: {provider.ReasonAnthropicServerTool},
	})
	if err != nil {
		panic("anthropicbase: invalid protocol requirement registration: " + err.Error())
	}
}

// AnthropicStreamEvent is the typed adapter view over one neutral stream-event
// envelope. The envelope remains the fidelity authority.
type AnthropicStreamEvent struct {
	Event string
	Data  json.RawMessage
}

func AttachAnthropicContentBlocks(msg *schema.Message, blocks []json.RawMessage) *schema.Message {
	if msg == nil || len(blocks) == 0 {
		return msg
	}
	state := provider.ProtocolStateFromMessage(msg)
	if state == nil {
		state = &provider.ProtocolState{}
	}
	for i, raw := range blocks {
		state.Envelopes = append(state.Envelopes, provider.NativeEnvelope{
			Dialect: provider.ProtocolDialectAnthropic, Scope: provider.NativeScopeMessageHistory, Kind: provider.NativeKindContentBlock,
			Location: provider.NativeLocation{ContentIndex: i}, Raw: append(json.RawMessage(nil), raw...),
		})
	}
	return provider.AttachMessageProtocolState(msg, state)
}

func AnthropicContentBlocksFromMessage(msg *schema.Message) []json.RawMessage {
	state := provider.ProtocolStateFromMessage(msg)
	if state == nil {
		return nil
	}
	var blocks []json.RawMessage
	for _, envelope := range state.Envelopes {
		if envelope.Dialect == provider.ProtocolDialectAnthropic && envelope.Scope == provider.NativeScopeMessageHistory && envelope.Kind == provider.NativeKindContentBlock {
			blocks = append(blocks, append(json.RawMessage(nil), envelope.Raw...))
		}
	}
	return blocks
}

func RewriteAnthropicContentToolNames(msg *schema.Message, names map[string]string) {
	if msg == nil || len(names) == 0 {
		return
	}
	state := provider.ProtocolStateFromMessage(msg)
	if state == nil {
		return
	}
	changed := false
	for i := range state.Envelopes {
		envelope := &state.Envelopes[i]
		if envelope.Dialect != provider.ProtocolDialectAnthropic || envelope.Scope != provider.NativeScopeMessageHistory || envelope.Kind != provider.NativeKindContentBlock {
			continue
		}
		var block map[string]any
		if json.Unmarshal(envelope.Raw, &block) != nil || block["type"] != "tool_use" {
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
		envelope.Raw = updated
		changed = true
	}
	if changed {
		provider.AttachMessageProtocolState(msg, state)
	}
}

// RewriteAnthropicRelayToolNames restores client-visible tool names in the raw
// stream lifecycle used by same-dialect relay. The relay envelope is the wire
// authority, so updating only the generic ToolCalls projection is insufficient.
func RewriteAnthropicRelayToolNames(msg *schema.Message, names map[string]string) error {
	if msg == nil || len(names) == 0 {
		return nil
	}
	state := provider.ProtocolStateFromMessage(msg)
	if state == nil {
		return nil
	}
	changed := false
	for i := range state.Envelopes {
		envelope := &state.Envelopes[i]
		if envelope.Dialect != provider.ProtocolDialectAnthropic || envelope.Scope != provider.NativeScopeStreamEvent || envelope.Kind != provider.NativeKindStreamEvent || envelope.Location.Event != "content_block_start" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(envelope.Raw, &event); err != nil {
			return fmt.Errorf("decode anthropic relay content_block_start: %w", err)
		}
		contentBlock, _ := event["content_block"].(map[string]any)
		if contentBlock["type"] != "tool_use" {
			continue
		}
		name, _ := contentBlock["name"].(string)
		replacement, ok := names[name]
		if !ok {
			continue
		}
		contentBlock["name"] = replacement
		updated, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("encode anthropic relay content_block_start: %w", err)
		}
		envelope.Raw = updated
		changed = true
	}
	if changed {
		provider.AttachMessageProtocolState(msg, state)
	}
	return nil
}

func AttachAnthropicStreamEvent(msg *schema.Message, event string, data json.RawMessage) *schema.Message {
	if msg == nil {
		msg = &schema.Message{Role: schema.Assistant}
	}
	state := provider.ProtocolStateFromMessage(msg)
	if state == nil {
		state = &provider.ProtocolState{}
	}
	index, _ := nativeJSONIndex(data)
	state.Envelopes = append(state.Envelopes, provider.NativeEnvelope{
		Dialect: provider.ProtocolDialectAnthropic, Scope: provider.NativeScopeStreamEvent, Kind: provider.NativeKindStreamProjection,
		Location: provider.NativeLocation{Event: event, SourceIndex: index}, Raw: append(json.RawMessage(nil), data...),
	})
	return provider.AttachMessageProtocolState(msg, state)
}

func AnthropicStreamEventsFromMessage(msg *schema.Message) []AnthropicStreamEvent {
	state := provider.ProtocolStateFromMessage(msg)
	if state == nil {
		return nil
	}
	var events []AnthropicStreamEvent
	for _, envelope := range state.Envelopes {
		if envelope.Dialect == provider.ProtocolDialectAnthropic && envelope.Scope == provider.NativeScopeStreamEvent && envelope.Kind == provider.NativeKindStreamProjection {
			events = append(events, AnthropicStreamEvent{Event: envelope.Location.Event, Data: append(json.RawMessage(nil), envelope.Raw...)})
		}
	}
	return events
}

func AttachAnthropicRelayStreamEvent(msg *schema.Message, event string, data json.RawMessage) *schema.Message {
	if msg == nil {
		msg = &schema.Message{Role: schema.Assistant}
	}
	state := provider.ProtocolStateFromMessage(msg)
	if state == nil {
		state = &provider.ProtocolState{}
	}
	index, _ := nativeJSONIndex(data)
	state.Envelopes = append(state.Envelopes, provider.NativeEnvelope{Dialect: provider.ProtocolDialectAnthropic, Scope: provider.NativeScopeStreamEvent, Kind: provider.NativeKindStreamEvent, Location: provider.NativeLocation{Event: event, SourceIndex: index}, Raw: append(json.RawMessage(nil), data...)})
	return provider.AttachMessageProtocolState(msg, state)
}

func AnthropicRelayStreamEventsFromMessage(msg *schema.Message) []AnthropicStreamEvent {
	state := provider.ProtocolStateFromMessage(msg)
	if state == nil {
		return nil
	}
	var events []AnthropicStreamEvent
	for _, envelope := range state.Envelopes {
		if envelope.Dialect == provider.ProtocolDialectAnthropic && envelope.Scope == provider.NativeScopeStreamEvent && envelope.Kind == provider.NativeKindStreamEvent {
			events = append(events, AnthropicStreamEvent{Event: envelope.Location.Event, Data: append(json.RawMessage(nil), envelope.Raw...)})
		}
	}
	return events
}

func AttachAnthropicResponseBody(msg *schema.Message, body json.RawMessage) *schema.Message {
	if msg == nil || len(body) == 0 {
		return msg
	}
	state := provider.ProtocolStateFromMessage(msg)
	if state == nil {
		state = &provider.ProtocolState{}
	}
	state.Envelopes = append(state.Envelopes, provider.NativeEnvelope{
		Dialect: provider.ProtocolDialectAnthropic, Scope: provider.NativeScopeResponseEphemeral, Kind: provider.NativeKindResponseBody,
		Raw: append(json.RawMessage(nil), body...),
	})
	return provider.AttachMessageProtocolState(msg, state)
}

func AnthropicResponseBodyFromMessage(msg *schema.Message) json.RawMessage {
	state := provider.ProtocolStateFromMessage(msg)
	if state == nil {
		return nil
	}
	for _, envelope := range state.Envelopes {
		if envelope.Dialect == provider.ProtocolDialectAnthropic && envelope.Scope == provider.NativeScopeResponseEphemeral && envelope.Kind == provider.NativeKindResponseBody {
			return append(json.RawMessage(nil), envelope.Raw...)
		}
	}
	return nil
}

func AnthropicRequestTools(state *provider.ProtocolState) ([]json.RawMessage, json.RawMessage) {
	if state == nil {
		return nil, nil
	}
	var tools []json.RawMessage
	var choice json.RawMessage
	for _, envelope := range state.Envelopes {
		if envelope.Dialect != provider.ProtocolDialectAnthropic || envelope.Scope != provider.NativeScopeRequest {
			continue
		}
		switch envelope.Kind {
		case provider.NativeKindToolDefinition:
			tools = append(tools, append(json.RawMessage(nil), envelope.Raw...))
		case provider.NativeKindToolChoice:
			choice = append(json.RawMessage(nil), envelope.Raw...)
		}
	}
	return tools, choice
}

func NewAnthropicRequestProtocolState(tools []json.RawMessage, choice json.RawMessage) *provider.ProtocolState {
	if len(tools) == 0 && len(choice) == 0 {
		return nil
	}
	reasons := map[provider.ProtocolFeature][]provider.RequirementReason{}
	for _, raw := range tools {
		var tool struct{ Type string }
		if json.Unmarshal(raw, &tool) == nil && strings.TrimSpace(tool.Type) != "" && strings.TrimSpace(tool.Type) != "custom" {
			reasons[provider.FeatureAnthropicServerToolRequest] = []provider.RequirementReason{provider.ReasonAnthropicServerTool}
			reasons[provider.FeatureAnthropicNativeResponse] = []provider.RequirementReason{provider.ReasonAnthropicServerTool}
			reasons[provider.FeatureAnthropicNativeHistoryReplay] = []provider.RequirementReason{provider.ReasonAnthropicServerTool}
		}
	}
	requirements, err := provider.NewProtocolRequirementSet(reasons)
	if err != nil {
		// All possible features are asserted during package initialization.
		return nil
	}
	state := &provider.ProtocolState{Requirements: requirements}
	for i, raw := range tools {
		state.Envelopes = append(state.Envelopes, provider.NativeEnvelope{Dialect: provider.ProtocolDialectAnthropic, Scope: provider.NativeScopeRequest, Kind: provider.NativeKindToolDefinition, Location: provider.NativeLocation{ToolIndex: i}, Raw: append(json.RawMessage(nil), raw...)})
	}
	if len(choice) > 0 {
		state.Envelopes = append(state.Envelopes, provider.NativeEnvelope{Dialect: provider.ProtocolDialectAnthropic, Scope: provider.NativeScopeRequest, Kind: provider.NativeKindToolChoice, Raw: append(json.RawMessage(nil), choice...)})
	}
	return state
}

func HasAnthropicNativeReasoning(messages []*schema.Message) bool {
	for _, msg := range messages {
		for _, part := range provider.ReasoningPartsFromMessage(msg) {
			switch part.Type {
			case schema.ChatMessagePartTypeReasoning:
				if part.Reasoning != nil && part.Reasoning.Signature != "" && !provider.IsGatewayThinkingSignature(part.Reasoning.Signature) {
					return true
				}
			case provider.ChatMessagePartTypeEncryptedReasoning:
				if provider.EncryptedReasoningData(part) != "" {
					return true
				}
			}
		}
	}
	return false
}

func HasAnthropicNativeContent(messages []*schema.Message) bool {
	for _, msg := range messages {
		if len(AnthropicContentBlocksFromMessage(msg)) > 0 {
			return true
		}
	}
	return false
}

func nativeJSONIndex(data json.RawMessage) (int, bool) {
	var value struct{ Index *int }
	if json.Unmarshal(data, &value) != nil || value.Index == nil {
		return 0, false
	}
	return *value.Index, true
}
