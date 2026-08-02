package gatewaybundle

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	agentroute "github.com/agent-guide/agent-gateway/pkg/gateway/agentroute"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	mcproute "github.com/agent-guide/agent-gateway/pkg/gateway/mcproute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/modelcatalog"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	virtualkeypkg "github.com/agent-guide/agent-gateway/pkg/gateway/virtualkey"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	mcpservice "github.com/agent-guide/agent-gateway/pkg/mcp/service"
	"gopkg.in/yaml.v3"
)

const (
	APIVersionV1Alpha1 = "gateway.agw/v1alpha1"
	KindGatewayBundle  = "GatewayBundle"
)

var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

type GatewayBundle struct {
	APIVersion    string                        `json:"apiVersion"`
	Kind          string                        `json:"kind"`
	Providers     []provider.ProviderConfig     `json:"providers,omitempty"`
	ManagedModels []modelcatalog.ManagedModel   `json:"managedModels,omitempty"`
	LLMRoutes     []routecore.AgentRouteConfig  `json:"llmRoutes,omitempty"`
	VirtualKeys   []BundleVirtualKey            `json:"virtualKeys,omitempty"`
	MCPServices   []mcpservice.MCPServiceConfig `json:"mcpServices,omitempty"`
	MCPRoutes     []mcproute.MCPRouteConfig     `json:"mcpRoutes,omitempty"`
	AgentRoutes   []agentroute.AgentRouteConfig `json:"agentRoutes,omitempty"`
	Agents        []agentpkg.Agent              `json:"agents,omitempty"`
}

type BundleVirtualKey struct {
	ID              string                              `json:"id,omitempty"`
	Tag             string                              `json:"tag,omitempty"`
	Description     string                              `json:"description,omitempty"`
	Disabled        bool                                `json:"disabled"`
	AllowedRouteIDs []string                            `json:"allowed_route_ids,omitempty"`
	RateLimits      *virtualkeypkg.VirtualKeyRateLimits `json:"rate_limits,omitempty"`
	StatusMessage   string                              `json:"status_message,omitempty"`
	ExpiresAt       time.Time                           `json:"expires_at,omitempty"`
}

type ValidationErrors struct {
	Errors []error
}

func LoadFile(path string) (*GatewayBundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read gateway bundle file %q: %w", path, err)
	}
	return DecodeYAML(data)
}

func DecodeYAML(data []byte) (*GatewayBundle, error) {
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode gateway bundle yaml: %w", err)
	}

	expanded, err := expandEnvValue(normalizeYAMLValue(raw))
	if err != nil {
		return nil, err
	}
	if root, ok := expanded.(map[string]any); ok {
		if _, exists := root["providerTypes"]; exists {
			return nil, fmt.Errorf("providerTypes is not supported in GatewayBundle; configure provider types at gateway startup")
		}
		if _, exists := root["cliAuthAuthenticators"]; exists {
			return nil, fmt.Errorf("cliAuthAuthenticators is not supported in GatewayBundle; use the external agw-auth tool")
		}
		legacy := []string{}
		for _, key := range []string{"acpServices", "acpRoutes", "builtinRoutes"} {
			if _, exists := root[key]; exists {
				legacy = append(legacy, key)
			}
		}
		if agents, ok := root["agents"].([]any); ok {
			for _, item := range agents {
				a, _ := item.(map[string]any)
				if _, ok := a["owns_service"]; ok {
					legacy = append(legacy, "Agent.owns_service")
					break
				}
				runtime, _ := a["runtime"].(map[string]any)
				acp, _ := runtime["acp"].(map[string]any)
				if _, ok := acp["service_id"]; ok {
					legacy = append(legacy, "Agent.runtime.acp.service_id")
					break
				}
			}
		}
		if len(legacy) > 0 {
			sort.Strings(legacy)
			return nil, fmt.Errorf("legacy_agent_runtime_config: legacy bundle fields detected (%s); run scripts/migrate-unified-agent-runtime", strings.Join(legacy, ", "))
		}
	}

	jsonBytes, err := json.Marshal(expanded)
	if err != nil {
		return nil, fmt.Errorf("encode gateway bundle intermediate json: %w", err)
	}

	var bundle GatewayBundle
	if err := json.Unmarshal(jsonBytes, &bundle); err != nil {
		return nil, fmt.Errorf("decode gateway bundle: %w", err)
	}
	return &bundle, nil
}

func EncodeYAML(bundle *GatewayBundle) ([]byte, error) {
	if bundle == nil {
		return nil, fmt.Errorf("gateway bundle is required")
	}
	jsonBytes, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("encode gateway bundle json: %w", err)
	}
	var raw any
	if err := json.Unmarshal(jsonBytes, &raw); err != nil {
		return nil, fmt.Errorf("decode gateway bundle json: %w", err)
	}
	removeManagedTimestamps(raw)
	yamlBytes, err := yaml.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("encode gateway bundle yaml: %w", err)
	}
	return yamlBytes, nil
}

func removeManagedTimestamps(value any) {
	root, ok := value.(map[string]any)
	if !ok {
		return
	}
	for _, family := range []string{
		"providers", "managedModels", "llmRoutes", "virtualKeys",
		"mcpServices", "mcpRoutes", "agents", "agentRoutes",
	} {
		items, _ := root[family].([]any)
		for _, item := range items {
			object, _ := item.(map[string]any)
			delete(object, "created_at")
			delete(object, "updated_at")
		}
	}
}

func (b *GatewayBundle) Validate() error {
	return b.validate(false)
}

func (b *GatewayBundle) ValidateForConfigStore() error {
	return b.validate(true)
}

func (b *GatewayBundle) ValidateForStaticConfig() error {
	if err := b.validate(false); err != nil {
		return err
	}
	if len(b.ManagedModels) > 0 {
		return fmt.Errorf("managedModels are not supported in static config; create managed models through the Admin API or agwctl gateway apply")
	}
	for i := range b.LLMRoutes {
		route, err := llmroutepkg.NewLLMRouteConfigFromConfig(b.LLMRoutes[i])
		if err != nil {
			return fmt.Errorf("llmRoutes[%q]: %w", strings.TrimSpace(b.LLMRoutes[i].ID), err)
		}
		if err := route.ValidateStaticDefinition(); err != nil {
			return fmt.Errorf("llmRoutes[%q]: %w", strings.TrimSpace(b.LLMRoutes[i].ID), err)
		}
	}
	if len(b.VirtualKeys) > 0 {
		return fmt.Errorf("virtualKeys are not supported in static config; create virtual keys through the Admin API after startup")
	}
	return nil
}

func (b *GatewayBundle) validate(_ bool) error {
	if b == nil {
		return fmt.Errorf("gateway bundle is required")
	}

	errs := &ValidationErrors{}
	if strings.TrimSpace(b.APIVersion) == "" {
		errs.Append(fmt.Errorf("apiVersion is required"))
	} else if b.APIVersion != APIVersionV1Alpha1 {
		errs.Append(fmt.Errorf("apiVersion must be %q", APIVersionV1Alpha1))
	}
	if strings.TrimSpace(b.Kind) == "" {
		errs.Append(fmt.Errorf("kind is required"))
	} else if b.Kind != KindGatewayBundle {
		errs.Append(fmt.Errorf("kind must be %q", KindGatewayBundle))
	}

	providerIDs := map[string]struct{}{}
	routeIDs := map[string]struct{}{}
	for i := range b.Providers {
		b.Providers[i] = provider.NormalizeConfig(b.Providers[i], b.Providers[i].Id, b.Providers[i].ProviderType)
		id := strings.TrimSpace(b.Providers[i].Id)
		if id == "" {
			errs.Append(fmt.Errorf("providers[%d].id is required", i))
			continue
		}
		if _, exists := providerIDs[id]; exists {
			errs.Append(fmt.Errorf("providers[%q]: duplicate id", id))
		} else {
			providerIDs[id] = struct{}{}
		}
		if strings.TrimSpace(b.Providers[i].ProviderType) == "" {
			errs.Append(fmt.Errorf("providers[%q].provider_type is required", id))
			continue
		}
		enabled, ok := provider.IsProviderTypeEnabled(b.Providers[i].ProviderType)
		if !ok {
			errs.Append(fmt.Errorf("providers[%q]: unknown provider_type %q", id, b.Providers[i].ProviderType))
			continue
		}
		if !enabled {
			errs.Append(fmt.Errorf("providers[%q]: provider_type %q is disabled by this gateway runtime", id, b.Providers[i].ProviderType))
		}
	}
	managedKeys := map[string]struct{}{}
	for i := range b.ManagedModels {
		b.ManagedModels[i].Normalize()
		providerID := strings.TrimSpace(b.ManagedModels[i].ProviderID)
		upstreamModel := strings.TrimSpace(b.ManagedModels[i].UpstreamModel)
		if providerID == "" || upstreamModel == "" {
			errs.Append(fmt.Errorf("managedModels[%d]: provider_id and upstream_model are required", i))
			continue
		}
		key := providerID + "/" + upstreamModel
		if _, exists := managedKeys[key]; exists {
			errs.Append(fmt.Errorf("managedModels[%q]: duplicate provider_id/upstream_model", key))
		} else {
			managedKeys[key] = struct{}{}
		}
	}
	for i := range b.LLMRoutes {
		routeID := strings.TrimSpace(b.LLMRoutes[i].ID)
		if routeID == "" {
			errs.Append(fmt.Errorf("llmRoutes[%d].id is required", i))
			continue
		}
		if _, exists := routeIDs[routeID]; exists {
			errs.Append(fmt.Errorf("llmRoutes[%q]: duplicate id", routeID))
		} else {
			routeIDs[routeID] = struct{}{}
		}
		route, err := llmroutepkg.NewLLMRouteConfigFromConfig(b.LLMRoutes[i])
		if err != nil {
			errs.Append(fmt.Errorf("llmRoutes[%q]: %v", routeID, err))
			continue
		}
		if err := route.ValidateDefinition(); err != nil {
			errs.Append(fmt.Errorf("llmRoutes[%q]: %w", routeID, err))
		}
	}
	virtualKeys := map[string]struct{}{}
	for i := range b.VirtualKeys {
		id := strings.TrimSpace(b.VirtualKeys[i].ID)
		if id == "" {
			errs.Append(fmt.Errorf("virtualKeys[%d].id is required", i))
			continue
		}
		if _, exists := virtualKeys[id]; exists {
			errs.Append(fmt.Errorf("virtualKeys[%q]: duplicate id", id))
		} else {
			virtualKeys[id] = struct{}{}
		}
		if err := b.VirtualKeys[i].ToRuntimeVirtualKey("").ValidateConfiguration(); err != nil {
			errs.Append(fmt.Errorf("virtualKeys[%q]: %w", id, err))
		}
		// allowed_route_ids are validated after every route family has been
		// collected — virtual keys may restrict to routes of any kind.
	}
	mcpServiceIDs := map[string]struct{}{}
	for i := range b.MCPServices {
		b.MCPServices[i].Normalize()
		id := b.MCPServices[i].ID
		if id == "" {
			errs.Append(fmt.Errorf("mcpServices[%d].id is required", i))
			continue
		}
		if _, exists := mcpServiceIDs[id]; exists {
			errs.Append(fmt.Errorf("mcpServices[%q]: duplicate id", id))
		} else {
			mcpServiceIDs[id] = struct{}{}
		}
		if err := b.MCPServices[i].Validate(); err != nil {
			errs.Append(fmt.Errorf("mcpServices[%q]: %w", id, err))
		}
	}
	mcpRouteIDs := map[string]struct{}{}
	for i := range b.MCPRoutes {
		b.MCPRoutes[i].Normalize()
		id := b.MCPRoutes[i].ID
		if id == "" {
			errs.Append(fmt.Errorf("mcpRoutes[%d].id is required", i))
			continue
		}
		if err := routecore.ValidateRouteID(id); err != nil {
			errs.Append(fmt.Errorf("mcpRoutes[%d]: %w", i, err))
		}
		if _, exists := mcpRouteIDs[id]; exists {
			errs.Append(fmt.Errorf("mcpRoutes[%q]: duplicate id", id))
		} else {
			mcpRouteIDs[id] = struct{}{}
		}
		if b.MCPRoutes[i].Kind != mcproute.RouteKindMCP {
			errs.Append(fmt.Errorf("mcpRoutes[%q]: kind must be %q", id, mcproute.RouteKindMCP))
		}
		if b.MCPRoutes[i].ServiceID == "" {
			errs.Append(fmt.Errorf("mcpRoutes[%q]: service_id is required", id))
		}
	}
	agentIngressRouteIDs := map[string]struct{}{}
	agentRouteTargetByID := map[string]string{}
	for i := range b.AgentRoutes {
		b.AgentRoutes[i].Normalize()
		id := b.AgentRoutes[i].ID
		if id == "" {
			errs.Append(fmt.Errorf("agentRoutes[%d].id is required", i))
			continue
		}
		if err := routecore.ValidateRouteID(id); err != nil {
			errs.Append(fmt.Errorf("agentRoutes[%d]: %w", i, err))
		}
		if _, exists := agentIngressRouteIDs[id]; exists {
			errs.Append(fmt.Errorf("agentRoutes[%q]: duplicate id", id))
		} else {
			agentIngressRouteIDs[id] = struct{}{}
		}
		if b.AgentRoutes[i].Kind != agentroute.RouteKindAgent {
			errs.Append(fmt.Errorf("agentRoutes[%q]: kind must be %q", id, agentroute.RouteKindAgent))
		}
		if b.AgentRoutes[i].AgentID == "" {
			errs.Append(fmt.Errorf("agentRoutes[%q]: agent_id is required", id))
		} else {
			agentRouteTargetByID[id] = b.AgentRoutes[i].AgentID
		}
	}
	// Routes of every kind share one persisted store, so route ids form one
	// global namespace: detect cross-family duplicates and validate virtual-key
	// allowed_route_ids against the union.
	allRouteIDs := map[string]string{}
	for family, ids := range map[string]map[string]struct{}{
		"llmRoutes":   routeIDs,
		"mcpRoutes":   mcpRouteIDs,
		"agentRoutes": agentIngressRouteIDs,
	} {
		for id := range ids {
			if other, exists := allRouteIDs[id]; exists {
				first, second := family, other
				if first > second {
					first, second = second, first
				}
				errs.Append(fmt.Errorf("route id %q is duplicated across %s and %s", id, first, second))
				continue
			}
			allRouteIDs[id] = family
		}
	}
	for i := range b.VirtualKeys {
		id := strings.TrimSpace(b.VirtualKeys[i].ID)
		if id == "" {
			continue
		}
		for _, routeID := range b.VirtualKeys[i].AllowedRouteIDs {
			trimmedRouteID := strings.TrimSpace(routeID)
			if trimmedRouteID == "" {
				errs.Append(fmt.Errorf("virtualKeys[%q]: allowed_route_ids entries must not be empty", id))
				continue
			}
			if len(allRouteIDs) > 0 {
				if _, ok := allRouteIDs[trimmedRouteID]; !ok {
					errs.Append(fmt.Errorf("virtualKeys[%q]: allowed_route_id %q does not exist in bundle routes", id, trimmedRouteID))
				}
			}
		}
	}
	agentIDs := map[string]struct{}{}
	agentRouteBindings := map[string]string{}
	for i := range b.Agents {
		b.Agents[i].Normalize()
		id := b.Agents[i].ID
		if id == "" {
			errs.Append(fmt.Errorf("agents[%d].id is required", i))
			continue
		}
		if _, exists := agentIDs[id]; exists {
			errs.Append(fmt.Errorf("agents[%q]: duplicate id", id))
		} else {
			agentIDs[id] = struct{}{}
		}
		if err := b.Agents[i].Validate(); err != nil {
			errs.Append(fmt.Errorf("agents[%q]: %w", id, err))
			continue
		}
		// Reject dangling references for every referenced object family. Each check
		// is guarded by "the referenced kind is present in the bundle" (mirrors the
		// virtualKeys allowed_route_ids rule): a partial bundle may legitimately
		// reference objects that already live in the config store and are resolved
		// at apply time, so the bundle-local validate only rejects references that
		// it can prove are dangling within the bundle.
		agent := &b.Agents[i]
		checkRefs := func(kind string, refs []string, present map[string]struct{}, store string) {
			if len(present) == 0 {
				return
			}
			for _, ref := range refs {
				if _, ok := present[ref]; !ok {
					errs.Append(fmt.Errorf("agents[%q]: %s %q does not exist in bundle %s", id, kind, ref, store))
				}
			}
		}
		checkRefs("provider_id", agent.Resources.ProviderIDs, providerIDs, "providers")
		checkRefs("mcp_service_id", agent.Resources.MCPServiceIDs, mcpServiceIDs, "mcpServices")
		checkRefs("virtual_key_id", agent.Resources.VirtualKeyIDs, virtualKeys, "virtualKeys")
		checkRefs("llm_route_id", agent.Routes.LLMRouteIDs, routeIDs, "llmRoutes")
		checkRefs("mcp_route_id", agent.Routes.MCPRouteIDs, mcpRouteIDs, "mcpRoutes")
		for _, routeID := range agentRouteIDs(*agent) {
			if owner, exists := agentRouteBindings[routeID]; exists {
				errs.Append(fmt.Errorf("agents[%q]: route %q is already bound by agent %q", id, routeID, owner))
			} else {
				agentRouteBindings[routeID] = id
			}
		}

	}
	for routeID, targetID := range agentRouteTargetByID {
		if len(agentIDs) > 0 {
			if _, ok := agentIDs[targetID]; !ok {
				errs.Append(fmt.Errorf("agentRoutes[%q]: agent_id %q does not exist in bundle agents", routeID, targetID))
			}
		}
	}

	if errs.HasErrors() {
		return errs
	}
	return nil
}

func agentRouteIDs(agent agentpkg.Agent) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, routeID := range agent.Routes.LLMRouteIDs {
		if routeID == "" {
			continue
		}
		if _, ok := seen[routeID]; ok {
			continue
		}
		seen[routeID] = struct{}{}
		out = append(out, routeID)
	}
	for _, routeID := range agent.Routes.MCPRouteIDs {
		if routeID == "" {
			continue
		}
		if _, ok := seen[routeID]; ok {
			continue
		}
		seen[routeID] = struct{}{}
		out = append(out, routeID)
	}
	return out
}

func (key BundleVirtualKey) ToRuntimeVirtualKey(generatedKey string) virtualkeypkg.VirtualKey {
	return virtualkeypkg.VirtualKey{
		ID:              key.ID,
		Key:             generatedKey,
		Tag:             key.Tag,
		Description:     key.Description,
		Disabled:        key.Disabled,
		AllowedRouteIDs: append([]string(nil), key.AllowedRouteIDs...),
		RateLimits:      cloneVirtualKeyRateLimits(key.RateLimits),
		StatusMessage:   key.StatusMessage,
		ExpiresAt:       key.ExpiresAt,
	}
}

func BundleVirtualKeyFromRuntime(key virtualkeypkg.VirtualKey) BundleVirtualKey {
	return BundleVirtualKey{
		ID:              key.ID,
		Tag:             key.Tag,
		Description:     key.Description,
		Disabled:        key.Disabled,
		AllowedRouteIDs: append([]string(nil), key.AllowedRouteIDs...),
		RateLimits:      cloneVirtualKeyRateLimits(key.RateLimits),
		StatusMessage:   key.StatusMessage,
		ExpiresAt:       key.ExpiresAt,
	}
}

func cloneVirtualKeyRateLimits(limits *virtualkeypkg.VirtualKeyRateLimits) *virtualkeypkg.VirtualKeyRateLimits {
	if limits == nil {
		return nil
	}
	cloned := *limits
	if limits.LLM != nil {
		value := *limits.LLM
		cloned.LLM = &value
	}
	if limits.MCP != nil {
		value := *limits.MCP
		cloned.MCP = &value
	}
	if limits.Agent != nil {
		value := *limits.Agent
		cloned.Agent = &value
	}
	return &cloned
}

func (e *ValidationErrors) Append(err error) {
	if e == nil || err == nil {
		return
	}
	e.Errors = append(e.Errors, err)
}

func (e *ValidationErrors) HasErrors() bool {
	return e != nil && len(e.Errors) > 0
}

func (e *ValidationErrors) Error() string {
	if e == nil || len(e.Errors) == 0 {
		return ""
	}
	if len(e.Errors) == 1 {
		return e.Errors[0].Error()
	}
	parts := make([]string, 0, len(e.Errors))
	for _, err := range e.Errors {
		parts = append(parts, err.Error())
	}
	sort.Strings(parts)
	return fmt.Sprintf("gateway bundle validation failed (%d errors): %s", len(parts), strings.Join(parts, "; "))
}

func normalizeYAMLValue(v any) any {
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, child := range value {
			out[key] = normalizeYAMLValue(child)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(value))
		for key, child := range value {
			out[fmt.Sprint(key)] = normalizeYAMLValue(child)
		}
		return out
	case []any:
		out := make([]any, 0, len(value))
		for _, child := range value {
			out = append(out, normalizeYAMLValue(child))
		}
		return out
	default:
		return value
	}
}

func expandEnvValue(v any) (any, error) {
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, child := range value {
			expanded, err := expandEnvValue(child)
			if err != nil {
				return nil, err
			}
			out[key] = expanded
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(value))
		for _, child := range value {
			expanded, err := expandEnvValue(child)
			if err != nil {
				return nil, err
			}
			out = append(out, expanded)
		}
		return out, nil
	case string:
		return expandEnvString(value)
	default:
		return value, nil
	}
}

func expandEnvString(s string) (string, error) {
	matches := envVarPattern.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return s, nil
	}

	values := map[string]string{}
	for _, match := range matches {
		name := match[1]
		if _, ok := values[name]; ok {
			continue
		}
		value, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("expand env in gateway bundle: environment variable %q is not set", name)
		}
		values[name] = value
	}

	return envVarPattern.ReplaceAllStringFunc(s, func(token string) string {
		match := envVarPattern.FindStringSubmatch(token)
		if len(match) != 2 {
			return token
		}
		return values[match[1]]
	}), nil
}
