package anthropicbase

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

type anthropicCodec struct{}

// ModeledObject is the Anthropic codec's typed projection. Keeping this a
// distinct type makes cross-dialect or accidental generic projections fail
// closed at the registry boundary.
type ModeledObject map[string]any

func init() {
	provider.RegisterDialectCodec(provider.ProtocolDialectAnthropic, anthropicCodec{})
}

func (anthropicCodec) Capture(input provider.NativeCaptureInput) (provider.NativeEnvelope, error) {
	if len(input.Raw) == 0 || !json.Valid(input.Raw) {
		return provider.NativeEnvelope{}, fmt.Errorf("anthropic codec: capture raw payload is invalid JSON")
	}
	modeled, err := modeledObject(input.Modeled)
	if err != nil {
		return provider.NativeEnvelope{}, err
	}
	keys := make([]string, 0, len(modeled))
	for key := range modeled {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	baselines := make([]provider.ModeledFieldBaseline, 0, len(keys))
	for _, key := range keys {
		baseline, err := provider.DigestModeledField(key, true, modeled[key])
		if err != nil {
			return provider.NativeEnvelope{}, err
		}
		baselines = append(baselines, baseline)
	}
	return provider.NativeEnvelope{Dialect: provider.ProtocolDialectAnthropic, Scope: input.Scope, Kind: input.Kind, Location: input.Location, Raw: append(json.RawMessage(nil), input.Raw...), Baselines: baselines}, nil
}

func (anthropicCodec) Overlay(input provider.NativeOverlayInput) (json.RawMessage, error) {
	if input.Envelope.Dialect != provider.ProtocolDialectAnthropic {
		return nil, fmt.Errorf("anthropic codec: wrong envelope dialect %q", input.Envelope.Dialect)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(input.Envelope.Raw, &raw); err != nil {
		return nil, fmt.Errorf("anthropic codec: overlay raw object: %w", err)
	}
	current, err := modeledObject(input.Current)
	if err != nil {
		return nil, err
	}
	baselines := map[string]provider.ModeledFieldBaseline{}
	for _, baseline := range input.Envelope.Baselines {
		baselines[baseline.Path] = baseline
	}
	for path, value := range current {
		baseline, known := baselines[path]
		now, err := provider.DigestModeledField(path, true, value)
		if err != nil {
			return nil, err
		}
		if !known || !baseline.Present || baseline.Digest != now.Digest {
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			raw[path] = encoded
		}
	}
	for path, baseline := range baselines {
		if _, present := current[path]; baseline.Present && !present {
			delete(raw, path)
		}
	}
	return json.Marshal(raw)
}

func (anthropicCodec) FoldResponse(envelopes []provider.NativeEnvelope) ([]provider.NativeEnvelope, error) {
	var folded []provider.NativeEnvelope
	for _, envelope := range envelopes {
		if envelope.Kind != provider.NativeKindResponseBody || envelope.Scope != provider.NativeScopeResponseEphemeral {
			folded = append(folded, envelope)
			continue
		}
		var response struct {
			Content []json.RawMessage
		}
		if err := json.Unmarshal(envelope.Raw, &response); err != nil {
			return nil, fmt.Errorf("anthropic codec: fold response body: %w", err)
		}
		for i, block := range response.Content {
			folded = append(folded, provider.NativeEnvelope{Dialect: provider.ProtocolDialectAnthropic, Scope: provider.NativeScopeMessageHistory, Kind: provider.NativeKindContentBlock, Location: provider.NativeLocation{ContentIndex: i}, Raw: append(json.RawMessage(nil), block...)})
		}
	}
	return folded, nil
}

func (anthropicCodec) FoldStreamEvents(envelopes []provider.NativeEnvelope) ([]provider.NativeEnvelope, error) {
	if err := (anthropicCodec{}).ValidateOrder(envelopes); err != nil {
		return nil, err
	}
	var folded []provider.NativeEnvelope
	type openBlock struct {
		location provider.NativeLocation
		value    map[string]any
		input    string
	}
	var active *openBlock
	for _, envelope := range envelopes {
		if envelope.Scope == provider.NativeScopeStreamEvent && envelope.Kind == provider.NativeKindStreamProjection {
			continue
		}
		if envelope.Scope != provider.NativeScopeStreamEvent || envelope.Kind != provider.NativeKindStreamEvent {
			folded = append(folded, envelope)
			continue
		}
		switch envelope.Location.Event {
		case "content_block_start":
			var start struct {
				Index        int            `json:"index"`
				ContentBlock map[string]any `json:"content_block"`
			}
			if err := json.Unmarshal(envelope.Raw, &start); err != nil || start.ContentBlock == nil {
				return nil, fmt.Errorf("anthropic codec: fold content_block_start")
			}
			active = &openBlock{location: provider.NativeLocation{ContentIndex: start.Index}, value: start.ContentBlock}
		case "content_block_delta":
			if active == nil {
				return nil, fmt.Errorf("anthropic codec: fold delta without active block")
			}
			if err := foldContentBlockDelta(active.value, &active.input, envelope.Raw); err != nil {
				return nil, err
			}
		case "content_block_stop":
			if active == nil {
				return nil, fmt.Errorf("anthropic codec: fold stop without active block")
			}
			if active.input != "" {
				var input any
				if err := json.Unmarshal([]byte(active.input), &input); err != nil {
					return nil, fmt.Errorf("anthropic codec: fold tool input: %w", err)
				}
				active.value["input"] = input
			}
			raw, err := json.Marshal(active.value)
			if err != nil {
				return nil, fmt.Errorf("anthropic codec: encode folded block: %w", err)
			}
			folded = append(folded, provider.NativeEnvelope{Dialect: provider.ProtocolDialectAnthropic, Scope: provider.NativeScopeMessageHistory, Kind: provider.NativeKindContentBlock, Location: active.location, Raw: raw})
			active = nil
		}
	}
	return folded, nil
}

func foldContentBlockDelta(block map[string]any, input *string, raw json.RawMessage) error {
	var event struct {
		Delta map[string]any `json:"delta"`
	}
	if err := json.Unmarshal(raw, &event); err != nil || event.Delta == nil {
		return fmt.Errorf("anthropic codec: decode content block delta")
	}
	typ, _ := event.Delta["type"].(string)
	switch typ {
	case "text_delta":
		appendStringField(block, "text", event.Delta["text"])
	case "thinking_delta":
		appendStringField(block, "thinking", event.Delta["thinking"])
	case "signature_delta":
		appendStringField(block, "signature", event.Delta["signature"])
	case "input_json_delta":
		fragment, ok := event.Delta["partial_json"].(string)
		if !ok {
			return fmt.Errorf("anthropic codec: input_json_delta has invalid fragment")
		}
		*input += fragment
	case "citations_delta":
		citation, ok := event.Delta["citation"]
		if !ok {
			return fmt.Errorf("anthropic codec: citations_delta has no citation")
		}
		citations, _ := block["citations"].([]any)
		block["citations"] = append(citations, citation)
	default:
		return fmt.Errorf("anthropic codec: unsupported delta type %q", typ)
	}
	return nil
}

func appendStringField(block map[string]any, field string, value any) {
	fragment, _ := value.(string)
	current, _ := block[field].(string)
	block[field] = current + fragment
}

func (anthropicCodec) MergeFragments(kind provider.NativeStateKind, envelopes []provider.NativeEnvelope) ([]provider.NativeEnvelope, error) {
	if kind != provider.NativeKindStreamEvent && kind != provider.NativeKindStreamProjection {
		return nil, fmt.Errorf("anthropic codec: duplicate %s envelope", kind)
	}
	merged := make([]provider.NativeEnvelope, len(envelopes))
	copy(merged, envelopes)
	return merged, nil
}

func (anthropicCodec) ValidateOrder(envelopes []provider.NativeEnvelope) error {
	started := false
	stopped := false
	hasEvents := false
	active := -1
	seen := map[int]struct{}{}
	for _, envelope := range envelopes {
		if envelope.Scope != provider.NativeScopeStreamEvent || envelope.Kind != provider.NativeKindStreamEvent {
			continue
		}
		hasEvents = true
		event := envelope.Location.Event
		index := envelope.Location.SourceIndex
		if stopped && event != "ping" {
			return fmt.Errorf("anthropic codec: event %q after message_stop", event)
		}
		switch event {
		case "message_start":
			if started {
				return fmt.Errorf("anthropic codec: message_start repeated")
			}
			started = true
		case "content_block_start":
			if !started || active >= 0 {
				return fmt.Errorf("anthropic codec: overlapping or pre-message block %d", index)
			}
			if _, duplicate := seen[index]; duplicate {
				return fmt.Errorf("anthropic codec: block %d repeated", index)
			}
			seen[index] = struct{}{}
			active = index
		case "content_block_delta":
			if active != index {
				return fmt.Errorf("anthropic codec: delta for inactive block %d", index)
			}
		case "content_block_stop":
			if active != index {
				return fmt.Errorf("anthropic codec: stop for inactive block %d", index)
			}
			active = -1
		case "message_delta":
			if !started || active >= 0 {
				return fmt.Errorf("anthropic codec: terminal event with incomplete message")
			}
		case "message_stop":
			if !started || active >= 0 {
				return fmt.Errorf("anthropic codec: terminal event with incomplete message")
			}
			stopped = true
		case "ping":
			// Keepalives do not change lifecycle state.
		case "error":
			return fmt.Errorf("anthropic codec: upstream error event cannot become history")
		default:
			return fmt.Errorf("anthropic codec: unsupported stream event %q", event)
		}
	}
	if active >= 0 {
		return fmt.Errorf("anthropic codec: block %d remains open", active)
	}
	if hasEvents && (!started || !stopped) {
		return fmt.Errorf("anthropic codec: incomplete message lifecycle")
	}
	return nil
}

func modeledObject(value provider.ModeledProjection) (map[string]any, error) {
	switch typed := value.(type) {
	case nil:
		return map[string]any{}, nil
	case ModeledObject:
		return typed, nil
	default:
		return nil, fmt.Errorf("anthropic codec: wrong modeled projection type %T", value)
	}
}
