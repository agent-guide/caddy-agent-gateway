package llmroute

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/gateway/modelcatalog"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/cloudwego/eino/schema"
)

const (
	fixtureDialect       provider.ProtocolDialect = "fixture.raw.v1"
	fixtureReplayFeature provider.ProtocolFeature = "fixture.raw_replay"
	fixtureRelayFeature  provider.ProtocolFeature = "fixture.native_stream_relay"
)

type fixtureProjection struct {
	Text string
}

type fixtureDialectCodec struct{}

func (fixtureDialectCodec) Capture(input provider.NativeCaptureInput) (provider.NativeEnvelope, error) {
	if !json.Valid(input.Raw) {
		return provider.NativeEnvelope{}, fmt.Errorf("fixture codec: invalid JSON")
	}
	projection, ok := input.Modeled.(fixtureProjection)
	if !ok {
		return provider.NativeEnvelope{}, fmt.Errorf("fixture codec: wrong projection %T", input.Modeled)
	}
	baseline, err := provider.DigestModeledField("text", true, projection.Text)
	if err != nil {
		return provider.NativeEnvelope{}, err
	}
	return provider.NativeEnvelope{
		Dialect: fixtureDialect, Scope: input.Scope, Kind: input.Kind, Location: input.Location,
		Raw: append(json.RawMessage(nil), input.Raw...), Baselines: []provider.ModeledFieldBaseline{baseline},
	}, nil
}

func (fixtureDialectCodec) Overlay(input provider.NativeOverlayInput) (json.RawMessage, error) {
	if input.Envelope.Dialect != fixtureDialect {
		return nil, fmt.Errorf("fixture codec: wrong dialect %q", input.Envelope.Dialect)
	}
	projection, ok := input.Current.(fixtureProjection)
	if !ok {
		return nil, fmt.Errorf("fixture codec: wrong projection %T", input.Current)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(input.Envelope.Raw, &raw); err != nil {
		return nil, err
	}
	current, err := provider.DigestModeledField("text", true, projection.Text)
	if err != nil {
		return nil, err
	}
	if len(input.Envelope.Baselines) != 1 || input.Envelope.Baselines[0].Digest != current.Digest {
		raw["text"], err = json.Marshal(projection.Text)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(raw)
}

func (fixtureDialectCodec) FoldResponse(envelopes []provider.NativeEnvelope) ([]provider.NativeEnvelope, error) {
	var folded []provider.NativeEnvelope
	for _, envelope := range envelopes {
		if envelope.Dialect != fixtureDialect || envelope.Scope != provider.NativeScopeResponseEphemeral || envelope.Kind != provider.NativeKindResponseBody {
			folded = append(folded, envelope)
			continue
		}
		var response struct {
			Content []json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(envelope.Raw, &response); err != nil {
			return nil, err
		}
		for index, raw := range response.Content {
			folded = append(folded, provider.NativeEnvelope{
				Dialect: fixtureDialect, Scope: provider.NativeScopeMessageHistory, Kind: provider.NativeKindContentBlock,
				Location: provider.NativeLocation{ContentIndex: index}, Raw: append(json.RawMessage(nil), raw...),
			})
		}
	}
	return folded, nil
}

func (codec fixtureDialectCodec) FoldStreamEvents(envelopes []provider.NativeEnvelope) ([]provider.NativeEnvelope, error) {
	if err := codec.ValidateOrder(envelopes); err != nil {
		return nil, err
	}
	var folded []provider.NativeEnvelope
	var text string
	for _, envelope := range envelopes {
		if envelope.Dialect != fixtureDialect || envelope.Scope != provider.NativeScopeStreamEvent || envelope.Kind != provider.NativeKindStreamEvent {
			folded = append(folded, envelope)
			continue
		}
		if envelope.Location.Event != "item_delta" {
			continue
		}
		var event struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(envelope.Raw, &event); err != nil {
			return nil, err
		}
		text += event.Delta
	}
	if text != "" {
		raw, err := json.Marshal(map[string]any{"type": "fixture_text", "text": text, "future": true})
		if err != nil {
			return nil, err
		}
		folded = append(folded, provider.NativeEnvelope{
			Dialect: fixtureDialect, Scope: provider.NativeScopeMessageHistory, Kind: provider.NativeKindContentBlock,
			Raw: raw,
		})
	}
	return folded, nil
}

func (fixtureDialectCodec) ValidateFragments(kind provider.NativeStateKind, envelopes []provider.NativeEnvelope) error {
	if kind != provider.NativeKindStreamEvent {
		return fmt.Errorf("fixture codec: duplicate %s envelope", kind)
	}
	if len(envelopes) < 2 {
		return fmt.Errorf("fixture codec: fragment sequence has %d envelope", len(envelopes))
	}
	return nil
}

func (fixtureDialectCodec) ValidateOrder(envelopes []provider.NativeEnvelope) error {
	stopped := false
	hasEvents := false
	for _, envelope := range envelopes {
		if envelope.Dialect != fixtureDialect || envelope.Scope != provider.NativeScopeStreamEvent || envelope.Kind != provider.NativeKindStreamEvent {
			continue
		}
		hasEvents = true
		if stopped {
			return fmt.Errorf("fixture codec: event after item_stop")
		}
		switch envelope.Location.Event {
		case "item_delta":
		case "item_stop":
			stopped = true
		default:
			return fmt.Errorf("fixture codec: unsupported event %q", envelope.Location.Event)
		}
	}
	if hasEvents && !stopped {
		return fmt.Errorf("fixture codec: missing item_stop")
	}
	return nil
}

func TestSecondDialectUsesGenericProtocolExtensionPoints(t *testing.T) {
	provider.RegisterDialectCodec(fixtureDialect, fixtureDialectCodec{})
	provider.RegisterProtocolFeature(provider.ProtocolFeatureDefinition{
		ID: fixtureReplayFeature, Dialect: fixtureDialect, Class: provider.ProtocolFeatureClassRequirement,
	})
	provider.RegisterProtocolFeature(provider.ProtocolFeatureDefinition{
		ID: fixtureRelayFeature, Dialect: fixtureDialect, Class: provider.ProtocolFeatureClassModeSelection,
	})
	provider.RegisterProviderTypeCapabilities("fixture-raw", provider.ProviderTypeCapabilities{
		Dialect:          fixtureDialect,
		ProtocolFeatures: map[provider.ProtocolFeature]struct{}{fixtureReplayFeature: {}, fixtureRelayFeature: {}},
	})

	codec, err := provider.DialectCodecFor(fixtureDialect)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("capture_overlay_preserves_unknown_fields", func(t *testing.T) {
		envelope, err := codec.Capture(provider.NativeCaptureInput{
			Scope: provider.NativeScopeMessageHistory, Kind: provider.NativeKindContentBlock,
			Raw:     json.RawMessage(`{"type":"fixture_text","text":"before","future":{"v":1}}`),
			Modeled: fixtureProjection{Text: "before"},
		})
		if err != nil {
			t.Fatal(err)
		}
		overlaid, err := codec.Overlay(provider.NativeOverlayInput{Envelope: envelope, Current: fixtureProjection{Text: "after"}})
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(overlaid, &got); err != nil {
			t.Fatal(err)
		}
		if string(got["text"]) != `"after"` || string(got["future"]) != `{"v":1}` {
			t.Fatalf("overlay = %s", overlaid)
		}
	})

	t.Run("response_folds_into_shared_history_envelope", func(t *testing.T) {
		message := provider.AttachMessageProtocolState(schema.AssistantMessage("", nil), &provider.ProtocolState{Envelopes: []provider.NativeEnvelope{{
			Dialect: fixtureDialect, Scope: provider.NativeScopeResponseEphemeral, Kind: provider.NativeKindResponseBody,
			Raw: json.RawMessage(`{"content":[{"type":"future_block","value":7}]}`),
		}}})
		if err := provider.FoldMessageProtocolState(message); err != nil {
			t.Fatal(err)
		}
		state := provider.ProtocolStateFromMessage(message)
		if state == nil || len(state.Envelopes) != 1 || state.Envelopes[0].Scope != provider.NativeScopeMessageHistory || string(state.Envelopes[0].Raw) != `{"type":"future_block","value":7}` {
			t.Fatalf("folded state = %+v", state)
		}
	})

	t.Run("native_events_use_shared_encoder_facing_contract", func(t *testing.T) {
		chunks := []*provider.ProtocolState{
			{Envelopes: []provider.NativeEnvelope{{Dialect: fixtureDialect, Scope: provider.NativeScopeStreamEvent, Kind: provider.NativeKindStreamEvent, Location: provider.NativeLocation{Event: "item_delta", SourceIndex: 0}, Raw: json.RawMessage(`{"delta":"hel"}`)}}},
			{Envelopes: []provider.NativeEnvelope{{Dialect: fixtureDialect, Scope: provider.NativeScopeStreamEvent, Kind: provider.NativeKindStreamEvent, Location: provider.NativeLocation{Event: "item_delta", SourceIndex: 1}, Raw: json.RawMessage(`{"delta":"lo"}`)}}},
			{Envelopes: []provider.NativeEnvelope{{Dialect: fixtureDialect, Scope: provider.NativeScopeStreamEvent, Kind: provider.NativeKindStreamEvent, Location: provider.NativeLocation{Event: "item_stop", SourceIndex: 2}, Raw: json.RawMessage(`{}`)}}},
		}
		merged, err := provider.MergeMessageProtocolStates(chunks...)
		if err != nil {
			t.Fatal(err)
		}
		if len(merged.Envelopes) != 1 || merged.Envelopes[0].Kind != provider.NativeKindContentBlock || string(merged.Envelopes[0].Raw) != `{"future":true,"text":"hello","type":"fixture_text"}` {
			t.Fatalf("merged native events = %+v", merged.Envelopes)
		}
	})

	t.Run("generic_route_filter_selects_capable_provider", func(t *testing.T) {
		requirements, err := provider.NewProtocolRequirementSet(map[provider.ProtocolFeature][]provider.RequirementReason{
			fixtureReplayFeature: {"fixture_history"},
		})
		if err != nil {
			t.Fatal(err)
		}
		route := LLMRoute{
			AgentRouteConfig: AgentRouteConfig{ID: "fixture-route"},
			TargetPolicy: &RouteLogicalModelTargetPolicy{DefaultModel: "fixture", ModelTargets: []RouteModelTarget{{
				Name: "fixture", Candidates: []RouteModelCandidate{
					{ProviderID: "generic", UpstreamModel: "model", Priority: 1, Weight: 1},
					{ProviderID: "fixture", UpstreamModel: "model", Priority: 2, Weight: 1},
				},
			}}},
		}
		catalog := testModelCatalogResolver{models: map[string]modelcatalog.ResolvedManagedModel{
			"generic\x00model": {ManagedModel: modelcatalog.ManagedModel{ProviderID: "generic", UpstreamModel: "model", Enabled: true}},
			"fixture\x00model": {ManagedModel: modelcatalog.ManagedModel{ProviderID: "fixture", UpstreamModel: "model", Enabled: true}},
		}}
		configs := testProviderConfigResolver{configs: map[string]provider.ProviderConfig{
			"generic": {Id: "generic", ProviderType: "generic-test"},
			"fixture": {Id: "fixture", ProviderType: "fixture-raw"},
		}}
		target, err := route.ResolveTarget(context.Background(), catalog, configs, RequestRequirements{ProtocolRequirements: requirements})
		if err != nil {
			t.Fatal(err)
		}
		if target.ProviderID != "fixture" {
			t.Fatalf("provider = %q, want fixture", target.ProviderID)
		}
	})
}
