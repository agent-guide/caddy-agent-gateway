package provider

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

const protocolStateMessageKey = "_agent_gateway_protocol_state"

type NativeStateScope string

const (
	NativeScopeRequest           NativeStateScope = "request"
	NativeScopeMessageHistory    NativeStateScope = "message_history"
	NativeScopeResponseEphemeral NativeStateScope = "response_ephemeral"
	NativeScopeStreamEvent       NativeStateScope = "stream_event"
)

type NativeStateKind string

const (
	NativeKindToolDefinition   NativeStateKind = "tool_definition"
	NativeKindToolChoice       NativeStateKind = "tool_choice"
	NativeKindContentBlock     NativeStateKind = "content_block"
	NativeKindResponseBody     NativeStateKind = "response_body"
	NativeKindStreamEvent      NativeStateKind = "stream_event"
	NativeKindStreamProjection NativeStateKind = "stream_projection"
)

type NativeLocation struct {
	MessageIndex int    `json:"message_index,omitempty"`
	ContentIndex int    `json:"content_index,omitempty"`
	ToolIndex    int    `json:"tool_index,omitempty"`
	SourceIndex  int    `json:"source_index,omitempty"`
	Event        string `json:"event,omitempty"`
}

type ModeledFieldBaseline struct {
	Path    string   `json:"path"`
	Present bool     `json:"present"`
	Digest  [32]byte `json:"digest"`
}

type NativeEnvelope struct {
	Dialect   ProtocolDialect        `json:"dialect"`
	Scope     NativeStateScope       `json:"scope"`
	Kind      NativeStateKind        `json:"kind"`
	Location  NativeLocation         `json:"location"`
	Raw       json.RawMessage        `json:"raw"`
	Baselines []ModeledFieldBaseline `json:"baselines,omitempty"`
}

type ProtocolState struct {
	Envelopes    []NativeEnvelope       `json:"envelopes,omitempty"`
	Requirements ProtocolRequirementSet `json:"requirements,omitempty"`
}

type MessageProtocolState ProtocolState

func init() {
	compose.RegisterStreamChunkConcatFunc(func(groups []MessageProtocolState) (MessageProtocolState, error) {
		states := make([]*ProtocolState, 0, len(groups))
		for i := range groups {
			state := ProtocolState(groups[i])
			states = append(states, &state)
		}
		merged, err := MergeMessageProtocolStates(states...)
		if err != nil {
			return MessageProtocolState{}, err
		}
		if merged == nil {
			return MessageProtocolState{}, nil
		}
		return MessageProtocolState(*merged), nil
	})
}

func AttachMessageProtocolState(msg *schema.Message, state *ProtocolState) *schema.Message {
	if msg == nil || state == nil || len(state.Envelopes) == 0 {
		return msg
	}
	if msg.Extra == nil {
		msg.Extra = map[string]any{}
	}
	normalized := CloneProtocolState(state)
	msg.Extra[protocolStateMessageKey] = MessageProtocolState(*normalized)
	return msg
}

func ProtocolStateFromMessage(msg *schema.Message) *ProtocolState {
	if msg == nil || msg.Extra == nil {
		return nil
	}
	stored, ok := msg.Extra[protocolStateMessageKey].(MessageProtocolState)
	if !ok {
		return nil
	}
	state := ProtocolState(stored)
	if len(state.Envelopes) == 0 && state.Requirements.Empty() {
		return nil
	}
	return CloneProtocolState(&state)
}

func CloneProtocolState(state *ProtocolState) *ProtocolState {
	if state == nil {
		return nil
	}
	out := &ProtocolState{Requirements: cloneProtocolRequirements(state.Requirements)}
	out.Envelopes = make([]NativeEnvelope, len(state.Envelopes))
	for i := range state.Envelopes {
		out.Envelopes[i] = cloneNativeEnvelope(state.Envelopes[i])
	}
	return out
}

func cloneProtocolRequirements(set ProtocolRequirementSet) ProtocolRequirementSet {
	return CloneProtocolRequirementSet(set)
}

func cloneNativeEnvelope(envelope NativeEnvelope) NativeEnvelope {
	envelope.Raw = append(json.RawMessage(nil), envelope.Raw...)
	envelope.Baselines = append([]ModeledFieldBaseline(nil), envelope.Baselines...)
	return envelope
}

func MergeMessageProtocolStates(states ...*ProtocolState) (*ProtocolState, error) {
	merged := &ProtocolState{}
	seen := map[string]NativeEnvelope{}
	for _, state := range states {
		if state == nil {
			continue
		}
		if !state.Requirements.Empty() {
			return nil, fmt.Errorf("message protocol state cannot carry request requirements")
		}
		for _, envelope := range state.Envelopes {
			if envelope.Scope == NativeScopeRequest {
				return nil, fmt.Errorf("request envelope cannot be attached to a message")
			}
			key := fmt.Sprintf("%s\x00%s\x00%s\x00%+v", envelope.Dialect, envelope.Scope, envelope.Kind, envelope.Location)
			if previous, duplicate := seen[key]; duplicate {
				codec, err := DialectCodecFor(envelope.Dialect)
				if err != nil {
					return nil, err
				}
				if err := codec.ValidateFragments(envelope.Kind, []NativeEnvelope{previous, envelope}); err != nil {
					return nil, fmt.Errorf("duplicate protocol envelope %s: %w", key, err)
				}
				// Fragment validation proves this duplicate key is an ordered
				// sequence, not a conflict. Retain its original global
				// position; grouping duplicates here would reorder interleaved
				// events such as pings and block deltas.
				merged.Envelopes = append(merged.Envelopes, cloneNativeEnvelope(envelope))
				seen[key] = envelope
				continue
			}
			seen[key] = envelope
			merged.Envelopes = append(merged.Envelopes, cloneNativeEnvelope(envelope))
		}
	}
	if len(merged.Envelopes) == 0 {
		return nil, nil
	}
	dialects := map[ProtocolDialect]struct{}{}
	for _, envelope := range merged.Envelopes {
		dialects[envelope.Dialect] = struct{}{}
	}
	if len(dialects) > 1 {
		return nil, fmt.Errorf("message protocol state contains mixed dialect lifecycles")
	}
	for dialect := range dialects {
		codec, err := DialectCodecFor(dialect)
		if err != nil {
			return nil, err
		}
		folded, err := codec.FoldStreamEvents(merged.Envelopes)
		if err != nil {
			return nil, err
		}
		merged.Envelopes = folded
	}
	if len(merged.Envelopes) == 0 {
		return nil, nil
	}
	return merged, nil
}

func FoldMessageProtocolState(msg *schema.Message) error {
	state := ProtocolStateFromMessage(msg)
	if state == nil {
		return nil
	}
	dialects := map[ProtocolDialect]struct{}{}
	for _, envelope := range state.Envelopes {
		dialects[envelope.Dialect] = struct{}{}
	}
	if len(dialects) > 1 {
		return fmt.Errorf("message protocol state contains mixed dialects")
	}
	for dialect := range dialects {
		codec, err := DialectCodecFor(dialect)
		if err != nil {
			return err
		}
		state.Envelopes, err = codec.FoldResponse(state.Envelopes)
		if err != nil {
			return err
		}
		state.Envelopes, err = codec.FoldStreamEvents(state.Envelopes)
		if err != nil {
			return err
		}
	}
	if msg.Extra == nil {
		msg.Extra = map[string]any{}
	}
	if len(state.Envelopes) == 0 {
		delete(msg.Extra, protocolStateMessageKey)
		return nil
	}
	msg.Extra[protocolStateMessageKey] = MessageProtocolState(*CloneProtocolState(state))
	return nil
}

type ModeledProjection any

type NativeCaptureInput struct {
	Scope    NativeStateScope
	Kind     NativeStateKind
	Location NativeLocation
	Raw      json.RawMessage
	Modeled  ModeledProjection
}

type NativeOverlayInput struct {
	Envelope NativeEnvelope
	Current  ModeledProjection
}

type DialectCodec interface {
	Capture(NativeCaptureInput) (NativeEnvelope, error)
	Overlay(NativeOverlayInput) (json.RawMessage, error)
	FoldResponse([]NativeEnvelope) ([]NativeEnvelope, error)
	FoldStreamEvents([]NativeEnvelope) ([]NativeEnvelope, error)
	ValidateFragments(NativeStateKind, []NativeEnvelope) error
	ValidateOrder([]NativeEnvelope) error
}

var dialectCodecs = struct {
	sync.RWMutex
	values map[ProtocolDialect]DialectCodec
}{values: map[ProtocolDialect]DialectCodec{}}

func RegisterDialectCodec(dialect ProtocolDialect, codec DialectCodec) {
	if dialect == "" || codec == nil {
		panic("provider: dialect codec registration requires dialect and codec")
	}
	dialectCodecs.Lock()
	defer dialectCodecs.Unlock()
	if _, exists := dialectCodecs.values[dialect]; exists {
		panic("provider: duplicate dialect codec registration: " + string(dialect))
	}
	dialectCodecs.values[dialect] = codec
}

func DialectCodecFor(dialect ProtocolDialect) (DialectCodec, error) {
	dialectCodecs.RLock()
	codec := dialectCodecs.values[dialect]
	dialectCodecs.RUnlock()
	if codec == nil {
		return nil, fmt.Errorf("provider: dialect %q has no registered codec", dialect)
	}
	return codec, nil
}

func DigestModeledField(path string, present bool, value any) (ModeledFieldBaseline, error) {
	baseline := ModeledFieldBaseline{Path: path, Present: present}
	if !present {
		return baseline, nil
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return baseline, err
	}
	baseline.Digest = sha256.Sum256(canonical)
	return baseline, nil
}
