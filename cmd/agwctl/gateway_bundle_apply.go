package main

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
	"github.com/agent-guide/agent-gateway/pkg/adminclient"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	acproute "github.com/agent-guide/agent-gateway/pkg/gateway/acproute"
	builtinroute "github.com/agent-guide/agent-gateway/pkg/gateway/builtinroute"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	mcproute "github.com/agent-guide/agent-gateway/pkg/gateway/mcproute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/modelcatalog"
	virtualkeypkg "github.com/agent-guide/agent-gateway/pkg/gateway/virtualkey"
	"github.com/agent-guide/agent-gateway/pkg/gatewaybundle"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	mcpservice "github.com/agent-guide/agent-gateway/pkg/mcp/service"
)

type gatewayApplySummary struct {
	File    string                    `json:"file"`
	Status  string                    `json:"status"`
	Actions []gatewayApplyAction      `json:"actions"`
	Counts  gatewayApplySummaryCounts `json:"counts"`
}

type gatewayApplyAction struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Action string `json:"action"`
	Error  string `json:"error,omitempty"`
}

type gatewayApplySummaryCounts struct {
	Create int `json:"create"`
	Update int `json:"update"`
	Skip   int `json:"skip"`
	Error  int `json:"error"`
}

func runGatewayApply(ctx context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("--file is required")
	}
	bundle, err := gatewaybundle.LoadFile(path)
	if err != nil {
		return err
	}
	if err := bundle.ValidateForConfigStore(); err != nil {
		return err
	}

	client := newGatewayClient()
	summary := gatewayApplySummary{
		File:   path,
		Status: "ok",
	}

	applyErrs := []error{}
	record := func(kind, id, action string, err error) {
		item := gatewayApplyAction{Kind: kind, ID: id, Action: action}
		switch action {
		case "create":
			summary.Counts.Create++
		case "update":
			summary.Counts.Update++
		case "skip":
			summary.Counts.Skip++
		case "error":
			summary.Counts.Error++
		}
		if err != nil {
			item.Error = err.Error()
			applyErrs = append(applyErrs, err)
			summary.Status = "error"
		}
		summary.Actions = append(summary.Actions, item)
	}

	if err := applyProviders(ctx, client, bundle, record); err != nil {
		return err
	}
	if err := applyManagedModels(ctx, client, bundle, record); err != nil {
		return err
	}
	if err := applyLLMRoutes(ctx, client, bundle, record); err != nil {
		return err
	}
	if err := applyVirtualKeys(ctx, client, bundle, record); err != nil {
		return err
	}
	if err := applyCLIAuthAuthenticators(ctx, client, bundle, record); err != nil {
		return err
	}
	if err := applyMCPServices(ctx, client, bundle, record); err != nil {
		return err
	}
	if err := applyMCPRoutes(ctx, client, bundle, record); err != nil {
		return err
	}
	if err := applyACPServices(ctx, client, bundle, record); err != nil {
		return err
	}
	if err := applyACPRoutes(ctx, client, bundle, record); err != nil {
		return err
	}
	if err := applyBuiltinRoutes(ctx, client, bundle, record); err != nil {
		return err
	}
	// Agents are pure references over the objects above, so they apply last,
	// after every referenced object is created/resolved.
	if err := applyAgents(ctx, client, bundle, record); err != nil {
		return err
	}

	if outputFormat == "json" {
		if err := printJSON(summary); err != nil {
			return err
		}
	} else {
		printGatewayApplySummary(summary)
	}
	if len(applyErrs) > 0 {
		return fmt.Errorf("gateway apply finished with %d error(s)", len(applyErrs))
	}
	return nil
}

func ensureProviderTypesEnabled(ctx context.Context, client *adminclient.Client, bundle *gatewaybundle.GatewayBundle) error {
	items, err := client.ListProviderTypes(ctx)
	if err != nil {
		return err
	}
	current := map[string]bool{}
	for _, item := range items {
		current[strings.ToLower(strings.TrimSpace(item.ProviderType))] = item.Enabled
	}
	for _, item := range bundle.Providers {
		providerType := strings.ToLower(strings.TrimSpace(item.ProviderType))
		if providerType == "" {
			continue
		}
		enabled, ok := current[providerType]
		if !ok {
			return fmt.Errorf("providers[%q]: unknown provider_type %q", item.Id, providerType)
		}
		if !enabled {
			return fmt.Errorf("providers[%q]: provider_type %q is not enabled by this gateway runtime", item.Id, providerType)
		}
	}
	return nil
}

func applyProviders(ctx context.Context, client *adminclient.Client, bundle *gatewaybundle.GatewayBundle, record func(kind, id, action string, err error)) error {
	if err := ensureProviderTypesEnabled(ctx, client, bundle); err != nil {
		return err
	}
	items, err := client.ListProviders(ctx, adminclient.ProviderListOptions{})
	if err != nil {
		return err
	}
	current := map[string]adminclient.Provider{}
	for _, item := range items {
		current[item.Id] = item
	}
	for _, desired := range bundle.Providers {
		desired = provider.NormalizeConfig(desired, desired.Id, desired.ProviderType)
		item, ok := current[desired.Id]
		if !ok {
			if _, err := client.CreateProvider(ctx, desired); err != nil {
				record("provider", desired.Id, "error", fmt.Errorf("provider %q create: %w", desired.Id, err))
			} else {
				record("provider", desired.Id, "create", nil)
			}
			continue
		}
		currentCfg := provider.NormalizeConfig(item.ProviderConfig, item.Id, item.ProviderType)
		if providerConfigsEqual(currentCfg, desired) {
			record("provider", desired.Id, "skip", nil)
			continue
		}
		if item.ReadOnly {
			record("provider", desired.Id, "error", fmt.Errorf("provider %q is read-only", desired.Id))
			continue
		}
		if _, err := client.UpdateProvider(ctx, desired.Id, desired); err != nil {
			record("provider", desired.Id, "error", fmt.Errorf("provider %q update: %w", desired.Id, err))
		} else {
			record("provider", desired.Id, "update", nil)
		}
	}
	return nil
}

func applyManagedModels(ctx context.Context, client *adminclient.Client, bundle *gatewaybundle.GatewayBundle, record func(kind, id, action string, err error)) error {
	items, err := client.ListManagedModels(ctx, adminclient.ManagedModelListOptions{})
	if err != nil {
		return err
	}
	current := map[string]modelcatalog.ManagedModel{}
	for _, item := range items {
		model := item.ManagedModel
		model.Normalize()
		current[managedModelKey(model.ProviderID, model.UpstreamModel)] = model
	}
	for _, desired := range bundle.ManagedModels {
		desired.Normalize()
		key := managedModelKey(desired.ProviderID, desired.UpstreamModel)
		if currentModel, ok := current[key]; ok {
			if reflect.DeepEqual(currentModel, desired) {
				record("managed_model", key, "skip", nil)
				continue
			}
			if _, err := client.UpdateManagedModel(ctx, desired.ProviderID, desired.UpstreamModel, desired); err != nil {
				record("managed_model", key, "error", fmt.Errorf("managed_model %q update: %w", key, err))
			} else {
				record("managed_model", key, "update", nil)
			}
			continue
		}
		if _, err := client.CreateManagedModel(ctx, desired); err != nil {
			record("managed_model", key, "error", fmt.Errorf("managed_model %q create: %w", key, err))
		} else {
			record("managed_model", key, "create", nil)
		}
	}
	return nil
}

func applyLLMRoutes(ctx context.Context, client *adminclient.Client, bundle *gatewaybundle.GatewayBundle, record func(kind, id, action string, err error)) error {
	items, err := client.ListLLMRoutes(ctx, adminclient.LLMRouteListOptions{})
	if err != nil {
		return err
	}
	current := map[string]adminclient.LLMRoute{}
	for _, item := range items {
		current[item.ID] = item
	}
	for _, desired := range bundle.LLMRoutes {
		desiredConfig, err := llmroutepkg.NewLLMRouteConfigFromConfig(desired)
		if err != nil {
			record("llm_route", desired.ID, "error", fmt.Errorf("llm route %q decode: %w", desired.ID, err))
			continue
		}
		item, ok := current[desired.ID]
		if !ok {
			if _, err := client.CreateLLMRoute(ctx, desiredConfig); err != nil {
				record("llm_route", desired.ID, "error", fmt.Errorf("llm route %q create: %w", desired.ID, err))
			} else {
				record("llm_route", desired.ID, "create", nil)
			}
			continue
		}
		currentLLMRoute, err := item.LLMRouteConfig()
		if err != nil {
			record("llm_route", desired.ID, "error", fmt.Errorf("llm route %q decode current config: %w", desired.ID, err))
			continue
		}
		currentLLMRoute = normalizeComparableLLMRoute(currentLLMRoute)
		desiredLLMRoute := normalizeComparableLLMRoute(desiredConfig)
		if reflect.DeepEqual(currentLLMRoute, desiredLLMRoute) {
			record("llm_route", desired.ID, "skip", nil)
			continue
		}
		if item.ReadOnly {
			record("llm_route", desired.ID, "error", fmt.Errorf("llm route %q is read-only", desired.ID))
			continue
		}
		if _, err := client.UpdateLLMRoute(ctx, desired.ID, desiredConfig); err != nil {
			record("llm_route", desired.ID, "error", fmt.Errorf("llm route %q update: %w", desired.ID, err))
		} else {
			record("llm_route", desired.ID, "update", nil)
		}
	}
	return nil
}

func applyCLIAuthAuthenticators(ctx context.Context, client *adminclient.Client, bundle *gatewaybundle.GatewayBundle, record func(kind, id, action string, err error)) error {
	items, err := client.ListCLIAuthAuthenticators(ctx)
	if err != nil {
		return err
	}
	current := map[string]adminclient.CLIAuthAuthenticator{}
	for _, item := range items {
		current[strings.ToLower(strings.TrimSpace(item.Name))] = item
	}
	for _, desired := range bundle.CLIAuthAuthenticators {
		name := strings.ToLower(strings.TrimSpace(desired.Name))
		item, ok := current[name]
		if ok && item.Enabled == desired.Enabled {
			if !desired.Enabled || reflect.DeepEqual(item.Config, desired.Config) {
				record("cliauth_authenticator", name, "skip", nil)
				continue
			}
		}

		req := adminclient.UpdateCLIAuthAuthenticatorRequest{
			Enabled: &desired.Enabled,
		}
		if desired.Enabled {
			cfg := desired.Config
			req.Config = &cfg
		}
		if _, err := client.UpdateCLIAuthAuthenticator(ctx, name, req); err != nil {
			record("cliauth_authenticator", name, "error", fmt.Errorf("cliauth authenticator %q update: %w", name, err))
			continue
		}
		if ok {
			record("cliauth_authenticator", name, "update", nil)
		} else {
			record("cliauth_authenticator", name, "create", nil)
		}
	}
	return nil
}

func applyVirtualKeys(ctx context.Context, client *adminclient.Client, bundle *gatewaybundle.GatewayBundle, record func(kind, id, action string, err error)) error {
	items, err := client.ListVirtualKeys(ctx, adminclient.VirtualKeyListOptions{})
	if err != nil {
		return err
	}
	current := map[string]adminclient.VirtualKey{}
	for _, item := range items {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			record("virtual_key", item.Key, "error", fmt.Errorf("virtual_key with key %q has empty id", item.Key))
			continue
		}
		if existing, exists := current[id]; exists && existing.Key != item.Key {
			record("virtual_key", id, "error", fmt.Errorf("virtual_key id %q is ambiguous across keys %q and %q", id, existing.Key, item.Key))
			continue
		}
		current[id] = item
	}
	for _, desired := range bundle.VirtualKeys {
		id := desired.ID
		item, ok := current[id]
		if !ok {
			req := bundleVirtualKeyConfig(desired)
			if _, err := client.CreateVirtualKey(ctx, req); err != nil {
				record("virtual_key", id, "error", fmt.Errorf("virtual_key %q create: %w", id, err))
			} else {
				record("virtual_key", id, "create", nil)
			}
			continue
		}
		currentKey := normalizeComparableVirtualKey(item.VirtualKey)
		desiredKey := normalizeComparableVirtualKey(desired.ToRuntimeVirtualKey(item.Key))
		if reflect.DeepEqual(currentKey, desiredKey) {
			record("virtual_key", id, "skip", nil)
			continue
		}
		if item.ReadOnly {
			record("virtual_key", id, "error", fmt.Errorf("virtual_key %q is read-only", id))
			continue
		}
		req := bundleVirtualKeyConfig(desired)
		if _, err := client.UpdateVirtualKey(ctx, item.ID, req); err != nil {
			record("virtual_key", id, "error", fmt.Errorf("virtual_key %q update: %w", id, err))
		} else {
			record("virtual_key", id, "update", nil)
		}
	}
	return nil
}

func providerConfigsEqual(a, b provider.ProviderConfig) bool {
	a = provider.NormalizeConfig(a, a.Id, a.ProviderType)
	b = provider.NormalizeConfig(b, b.Id, b.ProviderType)
	return reflect.DeepEqual(a, b)
}

func normalizeComparableLLMRoute(route llmroutepkg.LLMRouteConfig) llmroutepkg.LLMRouteConfig {
	route.Normalize()
	route.CreatedAt = time.Time{}
	route.UpdatedAt = time.Time{}
	return route
}

func normalizeComparableVirtualKey(key virtualkeypkg.VirtualKey) virtualkeypkg.VirtualKey {
	sort.Strings(key.AllowedRouteIDs)
	if len(key.AllowedRouteIDs) == 0 {
		key.AllowedRouteIDs = nil
	}
	key.CreatedAt = time.Time{}
	key.UpdatedAt = time.Time{}
	if key.ExpiresAt.IsZero() {
		key.ExpiresAt = time.Time{}
	}
	return key
}

func bundleVirtualKeyConfig(key gatewaybundle.BundleVirtualKey) adminclient.VirtualKeyConfig {
	return adminclient.VirtualKeyConfig{
		ID:              key.ID,
		Tag:             key.Tag,
		Description:     key.Description,
		Disabled:        key.Disabled,
		AllowedRouteIDs: append([]string(nil), key.AllowedRouteIDs...),
		RateLimits:      key.RateLimits,
		StatusMessage:   key.StatusMessage,
		ExpiresAt:       key.ExpiresAt,
	}
}

func applyMCPServices(ctx context.Context, client *adminclient.Client, bundle *gatewaybundle.GatewayBundle, record func(kind, id, action string, err error)) error {
	items, err := client.ListMCPServices(ctx)
	if err != nil {
		return err
	}
	current := map[string]adminclient.MCPServiceView{}
	for _, item := range items {
		current[item.ID] = item
	}
	for _, desired := range bundle.MCPServices {
		desired.Normalize()
		id := desired.ID
		item, ok := current[id]
		if !ok {
			if _, err := client.CreateMCPService(ctx, desired); err != nil {
				record("mcp_service", id, "error", fmt.Errorf("mcp_service %q create: %w", id, err))
			} else {
				record("mcp_service", id, "create", nil)
			}
			continue
		}
		if item.ReadOnly {
			record("mcp_service", id, "error", fmt.Errorf("mcp_service %q is read-only", id))
			continue
		}
		current := normalizeComparableMCPService(item.MCPServiceConfig)
		desiredNorm := normalizeComparableMCPService(desired)
		if reflect.DeepEqual(current, desiredNorm) {
			record("mcp_service", id, "skip", nil)
			continue
		}
		if _, err := client.UpdateMCPService(ctx, id, desired); err != nil {
			record("mcp_service", id, "error", fmt.Errorf("mcp_service %q update: %w", id, err))
		} else {
			record("mcp_service", id, "update", nil)
		}
	}
	return nil
}

func applyMCPRoutes(ctx context.Context, client *adminclient.Client, bundle *gatewaybundle.GatewayBundle, record func(kind, id, action string, err error)) error {
	items, err := client.ListMCPRoutes(ctx)
	if err != nil {
		return err
	}
	current := map[string]adminclient.MCPRouteView{}
	for _, item := range items {
		current[item.ID] = item
	}
	for _, desired := range bundle.MCPRoutes {
		desired.Normalize()
		id := desired.ID
		item, ok := current[id]
		if !ok {
			if _, err := client.CreateMCPRoute(ctx, desired); err != nil {
				record("mcp_route", id, "error", fmt.Errorf("mcp_route %q create: %w", id, err))
			} else {
				record("mcp_route", id, "create", nil)
			}
			continue
		}
		if item.ReadOnly {
			record("mcp_route", id, "error", fmt.Errorf("mcp_route %q is read-only", id))
			continue
		}
		currentNorm := normalizeComparableMCPRoute(item.MCPRouteConfig)
		desiredNorm := normalizeComparableMCPRoute(desired)
		if reflect.DeepEqual(currentNorm, desiredNorm) {
			record("mcp_route", id, "skip", nil)
			continue
		}
		if _, err := client.UpdateMCPRoute(ctx, id, desired); err != nil {
			record("mcp_route", id, "error", fmt.Errorf("mcp_route %q update: %w", id, err))
		} else {
			record("mcp_route", id, "update", nil)
		}
	}
	return nil
}

func applyACPServices(ctx context.Context, client *adminclient.Client, bundle *gatewaybundle.GatewayBundle, record func(kind, id, action string, err error)) error {
	items, err := client.ListACPServices(ctx)
	if err != nil {
		return err
	}
	current := map[string]adminclient.ACPServiceView{}
	for _, item := range items {
		current[item.ID] = item
	}
	for _, desired := range bundle.ACPServices {
		desired.Normalize()
		id := desired.ID
		item, ok := current[id]
		if !ok {
			if _, err := client.CreateACPService(ctx, desired); err != nil {
				record("acp_service", id, "error", fmt.Errorf("acp_service %q create: %w", id, err))
			} else {
				record("acp_service", id, "create", nil)
			}
			continue
		}
		if item.ReadOnly {
			record("acp_service", id, "error", fmt.Errorf("acp_service %q is read-only", id))
			continue
		}
		currentNorm := normalizeComparableACPService(item.ServiceConfig)
		desiredNorm := normalizeComparableACPService(desired)
		if reflect.DeepEqual(currentNorm, desiredNorm) {
			record("acp_service", id, "skip", nil)
			continue
		}
		if _, err := client.UpdateACPService(ctx, id, desired); err != nil {
			record("acp_service", id, "error", fmt.Errorf("acp_service %q update: %w", id, err))
		} else {
			record("acp_service", id, "update", nil)
		}
	}
	return nil
}

func applyACPRoutes(ctx context.Context, client *adminclient.Client, bundle *gatewaybundle.GatewayBundle, record func(kind, id, action string, err error)) error {
	items, err := client.ListACPRoutes(ctx)
	if err != nil {
		return err
	}
	current := map[string]adminclient.ACPRouteView{}
	for _, item := range items {
		current[item.ID] = item
	}
	for _, desired := range bundle.ACPRoutes {
		desired.Normalize()
		id := desired.ID
		item, ok := current[id]
		if !ok {
			if _, err := client.CreateACPRoute(ctx, desired); err != nil {
				record("acp_route", id, "error", fmt.Errorf("acp_route %q create: %w", id, err))
			} else {
				record("acp_route", id, "create", nil)
			}
			continue
		}
		if item.ReadOnly {
			record("acp_route", id, "error", fmt.Errorf("acp_route %q is read-only", id))
			continue
		}
		currentNorm := normalizeComparableACPRoute(item.ACPRouteConfig)
		desiredNorm := normalizeComparableACPRoute(desired)
		if reflect.DeepEqual(currentNorm, desiredNorm) {
			record("acp_route", id, "skip", nil)
			continue
		}
		if _, err := client.UpdateACPRoute(ctx, id, desired); err != nil {
			record("acp_route", id, "error", fmt.Errorf("acp_route %q update: %w", id, err))
		} else {
			record("acp_route", id, "update", nil)
		}
	}
	return nil
}

func applyBuiltinRoutes(ctx context.Context, client *adminclient.Client, bundle *gatewaybundle.GatewayBundle, record func(kind, id, action string, err error)) error {
	items, err := client.ListBuiltinRoutes(ctx)
	if err != nil {
		return err
	}
	current := map[string]adminclient.BuiltinRouteView{}
	for _, item := range items {
		current[item.ID] = item
	}
	for _, desired := range bundle.BuiltinRoutes {
		desired.Normalize()
		id := desired.ID
		item, ok := current[id]
		if !ok {
			if _, err := client.CreateBuiltinRoute(ctx, desired); err != nil {
				record("builtin_route", id, "error", fmt.Errorf("builtin_route %q create: %w", id, err))
			} else {
				record("builtin_route", id, "create", nil)
			}
			continue
		}
		if item.ReadOnly {
			record("builtin_route", id, "error", fmt.Errorf("builtin_route %q is read-only", id))
			continue
		}
		currentNorm := normalizeComparableBuiltinRoute(item.BuiltinRouteConfig)
		desiredNorm := normalizeComparableBuiltinRoute(desired)
		if reflect.DeepEqual(currentNorm, desiredNorm) {
			record("builtin_route", id, "skip", nil)
			continue
		}
		if _, err := client.UpdateBuiltinRoute(ctx, id, desired); err != nil {
			record("builtin_route", id, "error", fmt.Errorf("builtin_route %q update: %w", id, err))
		} else {
			record("builtin_route", id, "update", nil)
		}
	}
	return nil
}

func applyAgents(ctx context.Context, client *adminclient.Client, bundle *gatewaybundle.GatewayBundle, record func(kind, id, action string, err error)) error {
	if len(bundle.Agents) == 0 {
		return nil
	}
	items, err := client.ListAgents(ctx)
	if err != nil {
		return err
	}
	refs, err := loadAgentApplyReferences(ctx, client)
	if err != nil {
		return err
	}
	current := map[string]adminclient.AgentView{}
	for _, item := range items {
		current[item.ID] = item
	}
	for _, desired := range bundle.Agents {
		desired.Normalize()
		id := desired.ID
		if err := refs.validate(desired); err != nil {
			record("agent", id, "error", err)
			continue
		}
		item, ok := current[id]
		if !ok {
			if _, err := client.CreateAgent(ctx, desired); err != nil {
				record("agent", id, "error", fmt.Errorf("agent %q create: %w", id, err))
			} else {
				record("agent", id, "create", nil)
			}
			continue
		}
		currentNorm := normalizeComparableAgent(item.Agent)
		desiredNorm := normalizeComparableAgent(desired)
		if reflect.DeepEqual(currentNorm, desiredNorm) {
			record("agent", id, "skip", nil)
			continue
		}
		if _, err := client.UpdateAgent(ctx, id, desired); err != nil {
			record("agent", id, "error", fmt.Errorf("agent %q update: %w", id, err))
		} else {
			record("agent", id, "update", nil)
		}
	}
	return nil
}

type agentApplyReferences struct {
	providers     map[string]struct{}
	mcpServices   map[string]struct{}
	virtualKeys   map[string]struct{}
	llmRoutes     map[string]struct{}
	mcpRoutes     map[string]struct{}
	acpRoutes     map[string]struct{}
	builtinRoutes map[string]struct{}
	acpServices   map[string]struct{}
}

func loadAgentApplyReferences(ctx context.Context, client *adminclient.Client) (*agentApplyReferences, error) {
	providers, err := client.ListProviders(ctx, adminclient.ProviderListOptions{})
	if err != nil {
		return nil, err
	}
	mcpServices, err := client.ListMCPServices(ctx)
	if err != nil {
		return nil, err
	}
	virtualKeys, err := client.ListVirtualKeys(ctx, adminclient.VirtualKeyListOptions{})
	if err != nil {
		return nil, err
	}
	llmRoutes, err := client.ListLLMRoutes(ctx, adminclient.LLMRouteListOptions{})
	if err != nil {
		return nil, err
	}
	mcpRoutes, err := client.ListMCPRoutes(ctx)
	if err != nil {
		return nil, err
	}
	acpRoutes, err := client.ListACPRoutes(ctx)
	if err != nil {
		return nil, err
	}
	builtinRoutes, err := client.ListBuiltinRoutes(ctx)
	if err != nil {
		return nil, err
	}
	acpServices, err := client.ListACPServices(ctx)
	if err != nil {
		return nil, err
	}
	refs := &agentApplyReferences{
		providers:     map[string]struct{}{},
		mcpServices:   map[string]struct{}{},
		virtualKeys:   map[string]struct{}{},
		llmRoutes:     map[string]struct{}{},
		mcpRoutes:     map[string]struct{}{},
		acpRoutes:     map[string]struct{}{},
		builtinRoutes: map[string]struct{}{},
		acpServices:   map[string]struct{}{},
	}
	for _, item := range providers {
		refs.providers[item.Id] = struct{}{}
	}
	for _, item := range mcpServices {
		refs.mcpServices[item.ID] = struct{}{}
	}
	for _, item := range virtualKeys {
		refs.virtualKeys[item.ID] = struct{}{}
	}
	for _, item := range llmRoutes {
		refs.llmRoutes[item.ID] = struct{}{}
	}
	for _, item := range mcpRoutes {
		refs.mcpRoutes[item.ID] = struct{}{}
	}
	for _, item := range acpRoutes {
		refs.acpRoutes[item.ID] = struct{}{}
	}
	for _, item := range builtinRoutes {
		refs.builtinRoutes[item.ID] = struct{}{}
	}
	for _, item := range acpServices {
		refs.acpServices[item.ID] = struct{}{}
	}
	return refs, nil
}

func (r *agentApplyReferences) validate(a agentpkg.Agent) error {
	if r == nil {
		return nil
	}
	var missing []string
	check := func(kind string, ids []string, present map[string]struct{}) {
		for _, id := range ids {
			if strings.TrimSpace(id) == "" {
				continue
			}
			if _, ok := present[id]; !ok {
				missing = append(missing, fmt.Sprintf("%s %q", kind, id))
			}
		}
	}
	check("provider_id", a.Resources.ProviderIDs, r.providers)
	check("mcp_service_id", a.Resources.MCPServiceIDs, r.mcpServices)
	check("virtual_key_id", a.Resources.VirtualKeyIDs, r.virtualKeys)
	check("llm_route_id", a.Routes.LLMRouteIDs, r.llmRoutes)
	check("mcp_route_id", a.Routes.MCPRouteIDs, r.mcpRoutes)
	check("acp_route_id", a.Routes.ACPRouteIDs, r.acpRoutes)
	check("builtin_route_id", a.Routes.BuiltinRouteIDs, r.builtinRoutes)
	if serviceID := a.ACPServiceID(); serviceID != "" {
		check("runtime.acp.service_id", []string{serviceID}, r.acpServices)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("agent %q has dangling reference(s): %s", a.ID, strings.Join(missing, ", "))
	}
	return nil
}

func normalizeComparableAgent(a agentpkg.Agent) agentpkg.Agent {
	a.Normalize()
	a.CreatedAt = time.Time{}
	a.UpdatedAt = time.Time{}
	return a
}

func normalizeComparableACPService(cfg acpservice.ServiceConfig) acpservice.ServiceConfig {
	cfg.Normalize()
	cfg.CreatedAt = time.Time{}
	cfg.UpdatedAt = time.Time{}
	return cfg
}

func normalizeComparableACPRoute(cfg acproute.ACPRouteConfig) acproute.ACPRouteConfig {
	cfg.Normalize()
	cfg.CreatedAt = time.Time{}
	cfg.UpdatedAt = time.Time{}
	cfg.TargetPolicy = nil
	return cfg
}

func normalizeComparableBuiltinRoute(cfg builtinroute.BuiltinRouteConfig) builtinroute.BuiltinRouteConfig {
	cfg.Normalize()
	cfg.CreatedAt = time.Time{}
	cfg.UpdatedAt = time.Time{}
	cfg.TargetPolicy = nil
	return cfg
}

func normalizeComparableMCPService(cfg mcpservice.MCPServiceConfig) mcpservice.MCPServiceConfig {
	cfg.Normalize()
	cfg.CreatedAt = time.Time{}
	cfg.UpdatedAt = time.Time{}
	return cfg
}

func normalizeComparableMCPRoute(cfg mcproute.MCPRouteConfig) mcproute.MCPRouteConfig {
	cfg.Normalize()
	cfg.CreatedAt = time.Time{}
	cfg.UpdatedAt = time.Time{}
	cfg.TargetPolicy = nil
	return cfg
}

func managedModelKey(providerID, upstreamModel string) string {
	return strings.TrimSpace(providerID) + "/" + strings.TrimSpace(upstreamModel)
}

func printGatewayApplySummary(summary gatewayApplySummary) {
	fmt.Fprintf(rootCmd.OutOrStdout(), "gateway apply: %s\n", summary.File)
	for _, action := range summary.Actions {
		if action.Error != "" {
			fmt.Fprintf(rootCmd.OutOrStdout(), "  %s %s %s: %s\n", action.Action, action.Kind, action.ID, action.Error)
			continue
		}
		fmt.Fprintf(rootCmd.OutOrStdout(), "  %s %s %s\n", action.Action, action.Kind, action.ID)
	}
	fmt.Fprintf(rootCmd.OutOrStdout(), "summary: create=%d update=%d skip=%d error=%d\n",
		summary.Counts.Create,
		summary.Counts.Update,
		summary.Counts.Skip,
		summary.Counts.Error,
	)
}
