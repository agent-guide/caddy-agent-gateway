package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/schema"
)

type testConfigurableProvider struct {
	cfg ProviderConfig
}

func (p *testConfigurableProvider) Chat(context.Context, *ChatRequest) (*ChatResponse, error) {
	return nil, nil
}

func (p *testConfigurableProvider) StreamChat(context.Context, *ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	return nil, nil
}

func (p *testConfigurableProvider) ListModels(context.Context) ([]ModelInfo, error) {
	return nil, nil
}

func (p *testConfigurableProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{}
}

func (p *testConfigurableProvider) Config() ProviderConfig {
	return p.cfg
}

func TestProviderRegistryEnableDisableProviderType(t *testing.T) {
	const providerType = "test-registry-provider-name"
	RegisterProviderFactory(providerType, func(cfg ProviderConfig) (Provider, error) {
		return &testConfigurableProvider{cfg: cfg}, nil
	})
	// Toggle only this provider type (non-exclusive) so other registered
	// types keep their state across parallel tests.
	setEnabled := func(enabled bool) {
		if err := ConfigureProviderTypes([]ProviderTypeSetting{{ProviderType: providerType, Enabled: enabled}}, false); err != nil {
			t.Fatalf("configure provider type (enabled=%v): %v", enabled, err)
		}
	}
	defer setEnabled(true)

	enabled, ok := IsProviderTypeEnabled(providerType)
	if !ok {
		t.Fatalf("provider type %q not registered", providerType)
	}
	if !enabled {
		t.Fatalf("provider type %q enabled = false, want true", providerType)
	}

	setEnabled(false)
	enabled, ok = IsProviderTypeEnabled(providerType)
	if !ok || enabled {
		t.Fatalf("provider type state after disable: enabled=%v registered=%v", enabled, ok)
	}
	if _, err := NewProvider(ProviderConfig{ProviderType: providerType}); !errors.Is(err, ErrProviderTypeDisabled) {
		t.Fatalf("NewProvider error = %v, want ErrProviderTypeDisabled", err)
	}

	setEnabled(true)
	if _, err := NewProvider(ProviderConfig{ProviderType: providerType}); err != nil {
		t.Fatalf("NewProvider after enable: %v", err)
	}
}

func TestProviderCapabilityFeatureSetIsValidatedAndCloned(t *testing.T) {
	const providerType = "test-protocol-feature-provider"
	features := map[ProtocolFeature]struct{}{FeatureAnthropicBodyRelay: {}}
	RegisterProviderTypeCapabilities(providerType, ProviderTypeCapabilities{
		ProtocolFeatures: features,
	})
	delete(features, FeatureAnthropicBodyRelay)
	got := CapabilitiesForProviderType(providerType)
	if !got.SupportsProtocolFeature(FeatureAnthropicBodyRelay) {
		t.Fatal("registration retained caller-owned feature map")
	}
	delete(got.ProtocolFeatures, FeatureAnthropicBodyRelay)
	if !CapabilitiesForProviderType(providerType).SupportsProtocolFeature(FeatureAnthropicBodyRelay) {
		t.Fatal("capability lookup exposed registry-owned feature map")
	}
}

func TestProviderCapabilityRejectsUnknownFeature(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("unknown protocol feature registration did not panic")
		}
	}()
	RegisterProviderTypeCapabilities("test-unknown-protocol-feature", ProviderTypeCapabilities{
		ProtocolFeatures: map[ProtocolFeature]struct{}{ProtocolFeature("unknown.feature"): {}},
	})
}
