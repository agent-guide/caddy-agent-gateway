package llmroute

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
)

type RouteKind = routecore.RouteKind

const (
	RouteKindLLM = routecore.RouteKindLLM
	RouteKindMCP = routecore.RouteKindMCP
)

type RouteProtocol = routecore.RouteProtocol

const (
	RouteProtocolOpenAI    = routecore.RouteProtocolOpenAI
	RouteProtocolAnthropic = routecore.RouteProtocolAnthropic
	RouteProtocolCC        = routecore.RouteProtocolCC
	RouteProtocolMCP       = routecore.RouteProtocolMCP
)

type AgentRouteConfig = routecore.AgentRouteConfig
type RouteMatchPolicy = routecore.RouteMatchPolicy
type RouteAuthPolicy = routecore.RouteAuthPolicy

type RouteTargetPolicyKind string

const (
	RouteTargetPolicyKindDirectProvider RouteTargetPolicyKind = "direct-provider"
	RouteTargetPolicyKindLogicalModel   RouteTargetPolicyKind = "logical-model"
)

// RouteSelectionStrategy controls how a route prefers model candidates.
type RouteSelectionStrategy string

const (
	RouteSelectionStrategyAuto     RouteSelectionStrategy = "auto"
	RouteSelectionStrategyWeighted RouteSelectionStrategy = "weighted"
	RouteSelectionStrategyPriority RouteSelectionStrategy = "priority"
)

type RouteCredentialSelectStrategy string

const (
	RouteCredentialSelectRoundRobin RouteCredentialSelectStrategy = "round_robin"
	RouteCredentialSelectFillFirst  RouteCredentialSelectStrategy = "fill_first"
)

type RouteCredentialScope string

const (
	RouteCredentialScopeModelCustom RouteCredentialScope = "model_custom"
	RouteCredentialScopeProviderID  RouteCredentialScope = "provider_id"
)

type RouteCredentialType string

const (
	RouteCredentialTypeAPIKey     RouteCredentialType = "api_key"
	RouteCredentialTypeOAuthToken RouteCredentialType = "oauth_token"
)

type RouteTargetPolicy interface {
	Normalize()
	PolicyKind() RouteTargetPolicyKind
	ValidateDefinition(routeID string) error
	ResolveTarget(ctx context.Context, routeID string, catalog ModelCatalogResolver, providers ProviderConfigResolver, req RequestRequirements) (*ResolvedTarget, error)
	ProviderIDs() []string
	CredentialSelector() RouteCredentialSelectStrategy
	CredentialScopeOrder() []RouteCredentialScope
	CredentialTypeOrder() []RouteCredentialType
	FallbackPolicy() RouteFallbackPolicy
}

type RouteTargetPolicyCommon struct {
	CredentialSelectorValue   RouteCredentialSelectStrategy `json:"credential_selector,omitempty"`
	CredentialScopeOrderValue []RouteCredentialScope        `json:"credential_scope_order,omitempty"`
	CredentialTypeOrderValue  []RouteCredentialType         `json:"credential_type_order,omitempty"`
}

type RouteLogicalModelTargetPolicy struct {
	RouteTargetPolicyCommon
	DefaultModel          string                 `json:"default_model,omitempty"`
	ModelSelectorStrategy RouteSelectionStrategy `json:"model_selector_strategy,omitempty"`
	Fallback              RouteFallbackPolicy    `json:"fallback,omitempty"`
	ModelTargets          []RouteModelTarget     `json:"model_targets,omitempty"`
}

type RouteDirectProviderPolicy struct {
	RouteTargetPolicyCommon
	ProviderID     string               `json:"provider_id,omitempty"`
	ProviderTarget DirectProviderTarget `json:"provider_target,omitempty"`
}

// DecodeStoredRoute decodes a persisted route config and converts it to a runtime route.
func DecodeStoredRoute(data []byte) (any, error) {
	var cfg AgentRouteConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode route config: %w", err)
	}
	route, err := NewLLMRouteConfigFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	route.Normalize()
	route.NormalizeTimestamps(time.Now().UTC())
	return &route, nil
}

// LLMRouteState holds runtime-only state for a resolved route.
type LLMRouteState struct{}

// LLMRouteConfig is the expanded configuration form of an LLM route.
type LLMRouteConfig struct {
	AgentRouteConfig
	TargetPolicy RouteTargetPolicy
}

// LLMRoute is the runtime gateway route instance used by dispatch and resolution.
type LLMRoute struct {
	AgentRouteConfig
	TargetPolicy RouteTargetPolicy
	State        LLMRouteState
}

type RouteModelTarget struct {
	Name             string                 `json:"name"`
	Strategy         RouteSelectionStrategy `json:"strategy,omitempty"`
	DefaultCandidate string                 `json:"default_candidate,omitempty"`
	Candidates       []RouteModelCandidate  `json:"candidates,omitempty"`
}

type RouteModelCandidate struct {
	ProviderID    string `json:"provider_id"`
	UpstreamModel string `json:"upstream_model"`
	Weight        int    `json:"weight,omitempty"`
	Priority      int    `json:"priority,omitempty"`
	Default       bool   `json:"default,omitempty"`
}

type DirectProviderTarget struct {
	ProviderID string `json:"provider_id"`
}

type RouteFallbackPolicy struct {
	Enabled bool `json:"enabled,omitempty"`
	MaxNum  int  `json:"max_num,omitempty"`
}

func (r *LLMRouteConfig) NormalizeTimestamps(now time.Time) {
	if r == nil {
		return
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = now
	}
}

func (r LLMRouteConfig) MarshalJSON() ([]byte, error) {
	type llmRouteJSON struct {
		ID           string            `json:"id"`
		Kind         RouteKind         `json:"kind,omitempty"`
		Protocol     RouteProtocol     `json:"protocol,omitempty"`
		Description  string            `json:"description,omitempty"`
		Disabled     bool              `json:"disabled"`
		MatchPolicy  RouteMatchPolicy  `json:"match_policy"`
		TargetPolicy RouteTargetPolicy `json:"target_policy"`
		AuthPolicy   RouteAuthPolicy   `json:"auth_policy"`
		CreatedAt    time.Time         `json:"created_at"`
		UpdatedAt    time.Time         `json:"updated_at"`
	}
	r.Normalize()
	cfg, err := r.ToConfig()
	if err != nil {
		return nil, err
	}
	return json.Marshal(llmRouteJSON{
		ID:           cfg.ID,
		Kind:         cfg.Kind,
		Protocol:     cfg.Protocol,
		Description:  cfg.Description,
		Disabled:     cfg.Disabled,
		MatchPolicy:  cfg.MatchPolicy,
		TargetPolicy: r.TargetPolicy,
		AuthPolicy:   cfg.AuthPolicy,
		CreatedAt:    cfg.CreatedAt,
		UpdatedAt:    cfg.UpdatedAt,
	})
}

func (r *LLMRoute) UnmarshalJSON(data []byte) error {
	var cfg LLMRouteConfig
	if err := cfg.UnmarshalJSON(data); err != nil {
		return err
	}
	r.AgentRouteConfig = cfg.AgentRouteConfig
	r.TargetPolicy = cfg.TargetPolicy
	r.State = LLMRouteState{}
	return nil
}

func (r *LLMRouteConfig) UnmarshalJSON(data []byte) error {
	type llmRouteJSON struct {
		ID           string           `json:"id"`
		Kind         RouteKind        `json:"kind,omitempty"`
		Protocol     RouteProtocol    `json:"protocol,omitempty"`
		Description  string           `json:"description,omitempty"`
		Disabled     bool             `json:"disabled"`
		MatchPolicy  RouteMatchPolicy `json:"match_policy"`
		TargetPolicy json.RawMessage  `json:"target_policy"`
		AuthPolicy   RouteAuthPolicy  `json:"auth_policy"`
		CreatedAt    time.Time        `json:"created_at"`
		UpdatedAt    time.Time        `json:"updated_at"`
	}
	var raw llmRouteJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	policy, err := unmarshalRouteTargetPolicy(raw.TargetPolicy)
	if err != nil {
		return err
	}
	r.AgentRouteConfig = AgentRouteConfig{
		ID:          raw.ID,
		Kind:        raw.Kind,
		Protocol:    raw.Protocol,
		Description: raw.Description,
		Disabled:    raw.Disabled,
		AuthPolicy:  raw.AuthPolicy,
		MatchPolicy: raw.MatchPolicy,
		CreatedAt:   raw.CreatedAt,
		UpdatedAt:   raw.UpdatedAt,
	}
	r.TargetPolicy = policy
	return nil
}

func NewLLMRouteConfigFromConfig(cfg AgentRouteConfig) (LLMRouteConfig, error) {
	cfg = normalizeConfigDefaults(cfg)
	targetPolicy, err := unmarshalRouteTargetPolicy(cfg.TargetPolicy)
	if err != nil {
		return LLMRouteConfig{}, fmt.Errorf("route %q decode target policy: %w", cfg.ID, err)
	}
	return LLMRouteConfig{
		AgentRouteConfig: cfg,
		TargetPolicy:     targetPolicy,
	}, nil
}

func NewLLMRouteFromConfig(cfg AgentRouteConfig) (LLMRoute, error) {
	routeCfg, err := NewLLMRouteConfigFromConfig(cfg)
	if err != nil {
		return LLMRoute{}, err
	}
	return NewRuntimeLLMRoute(routeCfg), nil
}

func NewRuntimeLLMRoute(cfg LLMRouteConfig) LLMRoute {
	return LLMRoute{
		AgentRouteConfig: cfg.AgentRouteConfig,
		TargetPolicy:     cfg.TargetPolicy,
		State:            LLMRouteState{},
	}
}

func (r LLMRouteConfig) ToConfig() (AgentRouteConfig, error) {
	r.Normalize()
	targetPolicy, err := json.Marshal(r.TargetPolicy)
	if err != nil {
		return AgentRouteConfig{}, fmt.Errorf("route %q encode target policy: %w", r.ID, err)
	}
	cfg := r.AgentRouteConfig
	cfg.ID = strings.TrimSpace(r.ID)
	cfg.Kind = RouteKindLLM
	cfg.Protocol = normalizeRouteProtocol(cfg.Protocol)
	cfg.Description = strings.TrimSpace(r.Description)
	cfg.Disabled = r.Disabled
	cfg.AuthPolicy = r.AuthPolicy
	cfg.MatchPolicy = r.MatchPolicy
	cfg.TargetPolicy = targetPolicy
	cfg.CreatedAt = r.CreatedAt
	cfg.UpdatedAt = r.UpdatedAt
	return cfg, nil
}

func (r LLMRoute) ToConfig() (AgentRouteConfig, error) {
	return r.Config().ToConfig()
}

func normalizeConfigDefaults(cfg AgentRouteConfig) AgentRouteConfig {
	cfg.ID = strings.TrimSpace(cfg.ID)
	cfg.Protocol = normalizeRouteProtocol(cfg.Protocol)
	cfg.Description = strings.TrimSpace(cfg.Description)
	if cfg.Kind == "" {
		cfg.Kind = RouteKindLLM
	}
	return cfg
}

func normalizeRouteProtocol(protocol RouteProtocol) RouteProtocol {
	return RouteProtocol(strings.TrimSpace(string(protocol)))
}

func (p RouteLogicalModelTargetPolicy) MarshalJSON() ([]byte, error) {
	p.Normalize()
	type logicalJSON struct {
		Type                  RouteTargetPolicyKind         `json:"type,omitempty"`
		DefaultModel          string                        `json:"default_model,omitempty"`
		ModelSelectorStrategy RouteSelectionStrategy        `json:"model_selector_strategy,omitempty"`
		CredentialSelector    RouteCredentialSelectStrategy `json:"credential_selector,omitempty"`
		CredentialScopeOrder  []RouteCredentialScope        `json:"credential_scope_order,omitempty"`
		CredentialTypeOrder   []RouteCredentialType         `json:"credential_type_order,omitempty"`
		Fallback              RouteFallbackPolicy           `json:"fallback,omitempty"`
		ModelTargets          []RouteModelTarget            `json:"model_targets,omitempty"`
	}
	return json.Marshal(logicalJSON{
		Type:                  p.PolicyKind(),
		DefaultModel:          p.DefaultModel,
		ModelSelectorStrategy: p.ModelSelectorStrategy,
		CredentialSelector:    p.CredentialSelector(),
		CredentialScopeOrder:  p.CredentialScopeOrder(),
		CredentialTypeOrder:   p.CredentialTypeOrder(),
		Fallback:              p.Fallback,
		ModelTargets:          p.ModelTargets,
	})
}

func (p RouteDirectProviderPolicy) MarshalJSON() ([]byte, error) {
	p.Normalize()
	type directJSON struct {
		Type                 RouteTargetPolicyKind         `json:"type,omitempty"`
		ProviderID           string                        `json:"provider_id,omitempty"`
		CredentialSelector   RouteCredentialSelectStrategy `json:"credential_selector,omitempty"`
		CredentialScopeOrder []RouteCredentialScope        `json:"credential_scope_order,omitempty"`
		CredentialTypeOrder  []RouteCredentialType         `json:"credential_type_order,omitempty"`
		ProviderTarget       DirectProviderTarget          `json:"provider_target,omitempty"`
	}
	return json.Marshal(directJSON{
		Type:                 p.PolicyKind(),
		ProviderID:           p.ProviderID,
		CredentialSelector:   p.CredentialSelector(),
		CredentialScopeOrder: p.CredentialScopeOrder(),
		CredentialTypeOrder:  p.CredentialTypeOrder(),
		ProviderTarget:       p.ProviderTarget,
	})
}

func unmarshalRouteTargetPolicy(data []byte) (RouteTargetPolicy, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var probe struct {
		Type           RouteTargetPolicyKind `json:"type,omitempty"`
		ProviderID     string                `json:"provider_id,omitempty"`
		ProviderTarget DirectProviderTarget  `json:"provider_target,omitempty"`
		ModelTargets   []RouteModelTarget    `json:"model_targets,omitempty"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	kind := probe.Type
	if kind == "" {
		switch {
		case strings.TrimSpace(probe.ProviderID) != "" || strings.TrimSpace(probe.ProviderTarget.ProviderID) != "":
			kind = RouteTargetPolicyKindDirectProvider
		case len(probe.ModelTargets) > 0:
			kind = RouteTargetPolicyKindLogicalModel
		}
	}
	switch kind {
	case RouteTargetPolicyKindDirectProvider:
		var policy RouteDirectProviderPolicy
		if err := json.Unmarshal(data, &policy); err != nil {
			return nil, err
		}
		policy.Normalize()
		return &policy, nil
	case RouteTargetPolicyKindLogicalModel:
		var policy RouteLogicalModelTargetPolicy
		if err := json.Unmarshal(data, &policy); err != nil {
			return nil, err
		}
		policy.Normalize()
		return &policy, nil
	case "":
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown route target policy kind %q", kind)
	}
}

// Normalize fills defaults on a route config value before it is used.
func (r *LLMRouteConfig) Normalize() {
	if r == nil {
		return
	}
	r.AgentRouteConfig = normalizeConfigDefaults(r.AgentRouteConfig)
	if r.TargetPolicy != nil {
		r.TargetPolicy.Normalize()
	}
}

func (r LLMRouteConfig) UsesLogicalModel() bool {
	return r.TargetPolicy != nil && r.TargetPolicy.PolicyKind() == RouteTargetPolicyKindLogicalModel
}

func (r LLMRoute) Config() LLMRouteConfig {
	return LLMRouteConfig{
		AgentRouteConfig: r.AgentRouteConfig,
		TargetPolicy:     r.TargetPolicy,
	}
}

func (r *LLMRoute) Normalize() {
	if r == nil {
		return
	}
	cfg := r.Config()
	cfg.Normalize()
	r.AgentRouteConfig = cfg.AgentRouteConfig
	r.TargetPolicy = cfg.TargetPolicy
}

func (r *LLMRoute) NormalizeTimestamps(now time.Time) {
	if r == nil {
		return
	}
	cfg := r.Config()
	cfg.NormalizeTimestamps(now)
	r.AgentRouteConfig = cfg.AgentRouteConfig
}

func (r LLMRoute) MarshalJSON() ([]byte, error) {
	return r.Config().MarshalJSON()
}

func (r LLMRoute) UsesLogicalModel() bool {
	return r.Config().UsesLogicalModel()
}
