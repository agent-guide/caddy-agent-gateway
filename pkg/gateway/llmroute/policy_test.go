package llmroute

import (
	"context"
	"strings"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/credential"
	"github.com/agent-guide/agent-gateway/pkg/gateway/modelcatalog"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

type testModelCatalogResolver struct {
	models map[string]modelcatalog.ResolvedManagedModel
}

func (r testModelCatalogResolver) GetManagedModel(_ context.Context, providerID string, upstreamModel string) (*modelcatalog.ManagedModel, bool, error) {
	view, ok := r.models[providerID+"\x00"+upstreamModel]
	if !ok {
		return nil, false, nil
	}
	model := view.ManagedModel
	return &model, true, nil
}

func (r testModelCatalogResolver) GetResolvedManagedModel(_ context.Context, providerID string, upstreamModel string) (*modelcatalog.ResolvedManagedModel, bool, error) {
	view, ok := r.models[providerID+"\x00"+upstreamModel]
	if !ok {
		return nil, false, nil
	}
	return &view, true, nil
}

type testProviderConfigResolver struct {
	configs map[string]provider.ProviderConfig
}

func (r testProviderConfigResolver) GetConfig(_ context.Context, providerID string) (provider.ProviderConfig, error) {
	return r.configs[providerID], nil
}

func TestCredentialTypeOrderDefaultsToAPIKeyThenOAuthToken(t *testing.T) {
	got := (RouteTargetPolicyCommon{}).CredentialTypeOrder()
	want := []RouteCredentialType{RouteCredentialTypeAPIKey, RouteCredentialTypeOAuthToken}
	if len(got) != len(want) {
		t.Fatalf("CredentialTypeOrder() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("CredentialTypeOrder() = %v, want %v", got, want)
		}
	}
}

func TestResolveTargetFiltersAnthropicNativeCapability(t *testing.T) {
	provider.RegisterProviderTypeCapabilities("native-test", provider.ProviderTypeCapabilities{
		NativeDialects: provider.NewProtocolDialectSet(provider.ProtocolDialectAnthropic),
	})
	route := LLMRoute{
		AgentRouteConfig: AgentRouteConfig{ID: "native-route"},
		TargetPolicy: &RouteLogicalModelTargetPolicy{
			DefaultModel: "claude",
			ModelTargets: []RouteModelTarget{{
				Name: "claude",
				Candidates: []RouteModelCandidate{
					{ProviderID: "generic", UpstreamModel: "model", Priority: 1, Weight: 1},
					{ProviderID: "native", UpstreamModel: "model", Priority: 2, Weight: 1},
				},
			}},
		},
	}
	catalog := testModelCatalogResolver{models: map[string]modelcatalog.ResolvedManagedModel{
		"generic\x00model": {ManagedModel: modelcatalog.ManagedModel{ProviderID: "generic", UpstreamModel: "model", Enabled: true}, Capabilities: provider.ModelCapabilities{Tools: true}},
		"native\x00model":  {ManagedModel: modelcatalog.ManagedModel{ProviderID: "native", UpstreamModel: "model", Enabled: true}, Capabilities: provider.ModelCapabilities{Tools: true}},
	}}
	configs := testProviderConfigResolver{configs: map[string]provider.ProviderConfig{
		"generic": {Id: "generic", ProviderType: "generic-test"},
		"native":  {Id: "native", ProviderType: "native-test"},
	}}

	requirements := RequestRequirements{RequireTools: true}.WithNativeDialect(provider.ProtocolDialectAnthropic)
	target, err := route.ResolveTarget(context.Background(), catalog, configs, requirements)
	if err != nil {
		t.Fatalf("ResolveTarget() error = %v", err)
	}
	if target.ProviderID != "native" {
		t.Fatalf("provider = %q, want native", target.ProviderID)
	}
}

func TestDirectProviderRejectsMissingAnthropicNativeCapability(t *testing.T) {
	route := LLMRoute{
		AgentRouteConfig: AgentRouteConfig{ID: "native-direct"},
		TargetPolicy:     &RouteDirectProviderPolicy{ProviderTarget: DirectProviderTarget{ProviderID: "generic"}},
	}
	_, err := route.ResolveTarget(context.Background(), testModelCatalogResolver{}, testProviderConfigResolver{configs: map[string]provider.ProviderConfig{
		"generic": {Id: "generic", ProviderType: "generic-test"},
	}}, RequestRequirements{}.WithNativeDialect(provider.ProtocolDialectAnthropic))
	if err == nil {
		t.Fatal("ResolveTarget() error = nil, want native capability rejection")
	}
}

func TestDirectAnthropicProviderNativeRejectionSuggestsClaudeCode(t *testing.T) {
	route := LLMRoute{
		AgentRouteConfig: AgentRouteConfig{ID: "anthropic-direct"},
		TargetPolicy:     &RouteDirectProviderPolicy{ProviderTarget: DirectProviderTarget{ProviderID: "anthropic"}},
	}
	_, err := route.ResolveTarget(context.Background(), testModelCatalogResolver{}, testProviderConfigResolver{configs: map[string]provider.ProviderConfig{
		"anthropic": {Id: "anthropic", ProviderType: "anthropic"},
	}}, RequestRequirements{}.WithNativeDialect(provider.ProtocolDialectAnthropic))
	if err == nil || !strings.Contains(err.Error(), `provider_type "claudecode"`) {
		t.Fatalf("ResolveTarget() error = %v, want claudecode guidance", err)
	}
}

func TestLogicalModelNoEligibleBindingsNamesRequirements(t *testing.T) {
	route := LLMRoute{
		AgentRouteConfig: AgentRouteConfig{ID: "tools-route"},
		TargetPolicy: &RouteLogicalModelTargetPolicy{
			DefaultModel: "chat",
			ModelTargets: []RouteModelTarget{{
				Name:       "chat",
				Candidates: []RouteModelCandidate{{ProviderID: "generic", UpstreamModel: "model"}},
			}},
		},
	}
	catalog := testModelCatalogResolver{models: map[string]modelcatalog.ResolvedManagedModel{
		"generic\x00model": {
			ManagedModel: modelcatalog.ManagedModel{ProviderID: "generic", UpstreamModel: "model", Enabled: true},
		},
	}}
	configs := testProviderConfigResolver{configs: map[string]provider.ProviderConfig{
		"generic": {Id: "generic", ProviderType: "generic-test"},
	}}

	_, err := route.ResolveTarget(context.Background(), catalog, configs, RequestRequirements{RequireTools: true})
	if err == nil || !strings.Contains(err.Error(), "request requirements: tools") {
		t.Fatalf("ResolveTarget() error = %v, want tools capability diagnostic", err)
	}
}

func TestDirectProviderUsesNarrowAnthropicReasoningCapability(t *testing.T) {
	provider.RegisterProviderTypeCapabilities("reasoning-test", provider.ProviderTypeCapabilities{
		ReasoningDialects: provider.NewProtocolDialectSet(provider.ProtocolDialectAnthropic),
	})
	route := LLMRoute{
		AgentRouteConfig: AgentRouteConfig{ID: "reasoning-direct"},
		TargetPolicy:     &RouteDirectProviderPolicy{ProviderTarget: DirectProviderTarget{ProviderID: "reasoning"}},
	}
	configs := testProviderConfigResolver{configs: map[string]provider.ProviderConfig{
		"reasoning": {Id: "reasoning", ProviderType: "reasoning-test"},
	}}
	if _, err := route.ResolveTarget(context.Background(), testModelCatalogResolver{}, configs,
		RequestRequirements{}.WithReasoningDialect(provider.ProtocolDialectAnthropic)); err != nil {
		t.Fatalf("reasoning-capable provider rejected: %v", err)
	}
	if _, err := route.ResolveTarget(context.Background(), testModelCatalogResolver{}, configs,
		RequestRequirements{}.WithNativeDialect(provider.ProtocolDialectAnthropic)); err == nil {
		t.Fatal("reasoning-only provider accepted full Anthropic-native state")
	}
}

func TestAgentRouteResolveTargetUsesRouteDefaultModel(t *testing.T) {
	route := LLMRoute{
		AgentRouteConfig: AgentRouteConfig{ID: "chat-prod"},
		TargetPolicy: &RouteLogicalModelTargetPolicy{
			DefaultModel: "chat-fast",
			ModelTargets: []RouteModelTarget{{
				Name: "chat-fast",
				Candidates: []RouteModelCandidate{{
					ProviderID:    "openai-main",
					UpstreamModel: "gpt-4.1-mini",
				}},
			}},
		},
	}

	target, err := route.ResolveTarget(
		context.Background(),
		testModelCatalogResolver{
			models: map[string]modelcatalog.ResolvedManagedModel{
				"openai-main\x00gpt-4.1-mini": {
					ManagedModel: modelcatalog.ManagedModel{
						ProviderID:      "openai-main",
						UpstreamModel:   "gpt-4.1-mini",
						CredentialScope: credential.ProviderIDCredentialScope("openai-main"),
						Enabled:         true,
					},
					Capabilities: provider.ModelCapabilities{Streaming: true},
				},
			},
		},
		testProviderConfigResolver{
			configs: map[string]provider.ProviderConfig{
				"openai-main": {Id: "openai-main", ProviderType: "openai"},
			},
		},
		RequestRequirements{RequireStreaming: true},
	)
	if err != nil {
		t.Fatalf("ResolveTarget returned error: %v", err)
	}
	if target.LogicalModel != "chat-fast" {
		t.Fatalf("LogicalModel = %q, want chat-fast", target.LogicalModel)
	}
	if target.ProviderID != "openai-main" {
		t.Fatalf("ProviderID = %q, want openai-main", target.ProviderID)
	}
	if target.UpstreamModel != "gpt-4.1-mini" {
		t.Fatalf("UpstreamModel = %q, want gpt-4.1-mini", target.UpstreamModel)
	}
	if target.CredentialScope != credential.ProviderIDCredentialScope("openai-main") {
		t.Fatalf("CredentialScope = %q, want %q", target.CredentialScope, credential.ProviderIDCredentialScope("openai-main"))
	}
}

func TestAgentRouteResolveTargetRejectsUnknownModel(t *testing.T) {
	route := LLMRoute{
		AgentRouteConfig: AgentRouteConfig{ID: "chat-prod"},
		TargetPolicy: &RouteLogicalModelTargetPolicy{
			ModelTargets: []RouteModelTarget{{Name: "chat-fast"}},
		},
	}

	if _, err := route.ResolveTarget(
		context.Background(),
		testModelCatalogResolver{},
		testProviderConfigResolver{},
		RequestRequirements{Model: "chat-safe"},
	); err == nil {
		t.Fatal("ResolveTarget returned nil error, want unknown model rejection")
	}
}

func TestAgentRouteResolveTargetUsesDirectProvider(t *testing.T) {
	route := LLMRoute{
		AgentRouteConfig: AgentRouteConfig{ID: "chat-prod"},
		TargetPolicy: &RouteDirectProviderPolicy{
			ProviderTarget: DirectProviderTarget{ProviderID: "openai-main"},
		},
	}

	target, err := route.ResolveTarget(
		context.Background(),
		testModelCatalogResolver{
			models: map[string]modelcatalog.ResolvedManagedModel{
				"openai-main\x00gpt-4.1": {
					ManagedModel: modelcatalog.ManagedModel{
						ProviderID:      "openai-main",
						UpstreamModel:   "gpt-4.1",
						CredentialScope: credential.ProviderIDCredentialScope("tenant-a"),
					},
				},
			},
		},
		testProviderConfigResolver{
			configs: map[string]provider.ProviderConfig{
				"openai-main": {Id: "openai-main", ProviderType: "openai"},
			},
		},
		RequestRequirements{Model: "gpt-4.1"},
	)
	if err != nil {
		t.Fatalf("ResolveTarget returned error: %v", err)
	}
	if target.LogicalModel != "" {
		t.Fatalf("LogicalModel = %q, want empty in direct-provider mode", target.LogicalModel)
	}
	if target.ProviderID != "openai-main" {
		t.Fatalf("ProviderID = %q, want openai-main", target.ProviderID)
	}
	if target.ProviderType != "openai" {
		t.Fatalf("ProviderType = %q, want openai", target.ProviderType)
	}
	if target.UpstreamModel != "gpt-4.1" {
		t.Fatalf("UpstreamModel = %q, want gpt-4.1", target.UpstreamModel)
	}
	if target.CredentialScope != credential.ProviderIDCredentialScope("openai-main") {
		t.Fatalf("CredentialScope = %q, want %q", target.CredentialScope, credential.ProviderIDCredentialScope("openai-main"))
	}
}

func TestAgentRouteResolveTargetUsesDirectProviderWhenModelNameIsPresent(t *testing.T) {
	route := LLMRoute{
		AgentRouteConfig: AgentRouteConfig{ID: "chat-prod"},
		TargetPolicy: &RouteDirectProviderPolicy{
			ProviderTarget: DirectProviderTarget{ProviderID: "openai-main"},
		},
	}

	target, err := route.ResolveTarget(
		context.Background(),
		testModelCatalogResolver{},
		testProviderConfigResolver{
			configs: map[string]provider.ProviderConfig{
				"openai-main": {Id: "openai-main", ProviderType: "openai"},
			},
		},
		RequestRequirements{Model: "gpt-4.1"},
	)
	if err != nil {
		t.Fatalf("ResolveTarget returned error: %v", err)
	}
	if target.ProviderID != "openai-main" {
		t.Fatalf("ProviderID = %q, want openai-main", target.ProviderID)
	}
	if target.UpstreamModel != "gpt-4.1" {
		t.Fatalf("UpstreamModel = %q, want gpt-4.1", target.UpstreamModel)
	}
	if target.LogicalModel != "" {
		t.Fatalf("LogicalModel = %q, want empty in direct-provider mode", target.LogicalModel)
	}
}

func TestAgentRouteResolveTargetDirectProviderEmptyModelUsesProviderDefault(t *testing.T) {
	route := LLMRoute{
		AgentRouteConfig: AgentRouteConfig{ID: "chat-prod"},
		TargetPolicy: &RouteDirectProviderPolicy{
			ProviderTarget: DirectProviderTarget{ProviderID: "openai-main"},
		},
	}

	target, err := route.ResolveTarget(
		context.Background(),
		testModelCatalogResolver{},
		testProviderConfigResolver{
			configs: map[string]provider.ProviderConfig{
				"openai-main": {Id: "openai-main", ProviderType: "openai", DefaultModel: "gpt-4.1"},
			},
		},
		RequestRequirements{},
	)
	if err != nil {
		t.Fatalf("ResolveTarget returned error: %v", err)
	}
	if target.UpstreamModel != "gpt-4.1" {
		t.Fatalf("UpstreamModel = %q, want the provider default gpt-4.1", target.UpstreamModel)
	}
}
