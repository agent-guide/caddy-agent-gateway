// Package agentroute defines the unified Agent ingress route model
// (docs/plans/unified-agent-runtime.md §6). An AgentRoute targets a stable
// agent_id; the resolved Agent's runtime.type selects the execution backend,
// so changing an Agent's runtime never changes its route, URL, or VirtualKey
// allowlist. M5 exposes this model through Admin, CLI, bundles, Caddy, and the
// standalone dispatcher.
package agentroute

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
)

type RouteKind = routecore.RouteKind

const (
	RouteKindAgent = routecore.RouteKindAgent
)

type RouteProtocol = routecore.RouteProtocol

const (
	RouteProtocolAgent = routecore.RouteProtocolAgent
)

type AgentRouteBaseConfig = routecore.AgentRouteConfig
type RouteMatch = routecore.RouteMatchPolicy
type RouteAuthPolicy = routecore.RouteAuthPolicy
type RouteListOptions = routecore.RouteListOptions

// AgentRouteConfig is the expanded config form with direct agent_id access.
type AgentRouteConfig struct {
	AgentRouteBaseConfig
	AgentID string `json:"agent_id"`
}

// AgentRoute is the runtime route object used by dispatcher/runtime code.
type AgentRoute struct {
	AgentRouteBaseConfig
	AgentID string `json:"agent_id"`
}

type routeTargetPolicy struct {
	Kind    routecore.RouteTargetPolicyKind `json:"kind,omitempty"`
	AgentID string                          `json:"agent_id"`
}

func normalizeConfigDefaults(cfg AgentRouteBaseConfig) AgentRouteBaseConfig {
	cfg.ID = strings.TrimSpace(cfg.ID)
	cfg.Kind = RouteKindAgent
	cfg.Protocol = RouteProtocolAgent
	cfg.Description = strings.TrimSpace(cfg.Description)
	cfg.MatchPolicy.Host = strings.TrimSpace(cfg.MatchPolicy.Host)
	cfg.MatchPolicy.PathPrefix = strings.TrimSpace(cfg.MatchPolicy.PathPrefix)
	for i := range cfg.MatchPolicy.Methods {
		cfg.MatchPolicy.Methods[i] = strings.TrimSpace(cfg.MatchPolicy.Methods[i])
	}
	return cfg
}

func (r *AgentRouteConfig) Normalize() {
	if r == nil {
		return
	}
	r.AgentRouteBaseConfig = normalizeConfigDefaults(r.AgentRouteBaseConfig)
	r.AgentID = strings.TrimSpace(r.AgentID)
	if r.ID == "" && r.AgentID != "" {
		r.ID = routecore.GenerateRouteID("agent", r.AgentID, r.MatchPolicy.PathPrefix)
	}
}

func (r *AgentRouteConfig) NormalizeTimestamps(now time.Time) {
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

type agentRouteJSON struct {
	ID          string          `json:"id"`
	Kind        RouteKind       `json:"kind,omitempty"`
	Protocol    RouteProtocol   `json:"protocol,omitempty"`
	Description string          `json:"description,omitempty"`
	Disabled    bool            `json:"disabled"`
	MatchPolicy RouteMatch      `json:"match_policy"`
	AuthPolicy  RouteAuthPolicy `json:"auth_policy"`
	AgentID     string          `json:"agent_id"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

func (r AgentRouteConfig) MarshalJSON() ([]byte, error) {
	r.Normalize()
	cfg, err := r.ToConfig()
	if err != nil {
		return nil, err
	}
	return json.Marshal(agentRouteJSON{
		ID:          cfg.ID,
		Kind:        cfg.Kind,
		Protocol:    cfg.Protocol,
		Description: cfg.Description,
		Disabled:    cfg.Disabled,
		MatchPolicy: cfg.MatchPolicy,
		AuthPolicy:  cfg.AuthPolicy,
		AgentID:     r.AgentID,
		CreatedAt:   cfg.CreatedAt,
		UpdatedAt:   cfg.UpdatedAt,
	})
}

func (r *AgentRoute) UnmarshalJSON(data []byte) error {
	var cfg AgentRouteConfig
	if err := cfg.UnmarshalJSON(data); err != nil {
		return err
	}
	*r = NewRuntimeAgentRoute(cfg)
	return nil
}

func (r *AgentRouteConfig) UnmarshalJSON(data []byte) error {
	var raw agentRouteJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.AgentRouteBaseConfig = normalizeConfigDefaults(AgentRouteBaseConfig{
		ID:          raw.ID,
		Kind:        raw.Kind,
		Protocol:    raw.Protocol,
		Description: raw.Description,
		Disabled:    raw.Disabled,
		AuthPolicy:  raw.AuthPolicy,
		MatchPolicy: raw.MatchPolicy,
		CreatedAt:   raw.CreatedAt,
		UpdatedAt:   raw.UpdatedAt,
	})
	r.AgentID = raw.AgentID
	r.Normalize()
	return nil
}

func DecodeStoredAgentRoute(data []byte) (any, error) {
	var cfg AgentRouteBaseConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode agent route config: %w", err)
	}
	route, err := NewAgentRouteConfigFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	route.Normalize()
	return &route, nil
}

func NewAgentRouteConfigFromConfig(cfg AgentRouteBaseConfig) (AgentRouteConfig, error) {
	cfg = normalizeConfigDefaults(cfg)
	agentID, err := decodeAgentID(cfg.TargetPolicy)
	if err != nil {
		return AgentRouteConfig{}, fmt.Errorf("route %q decode target policy: %w", cfg.ID, err)
	}
	route := AgentRouteConfig{
		AgentRouteBaseConfig: cfg,
		AgentID:              agentID,
	}
	route.Normalize()
	return route, nil
}

func NewAgentRouteFromConfig(cfg AgentRouteBaseConfig) (AgentRoute, error) {
	routeCfg, err := NewAgentRouteConfigFromConfig(cfg)
	if err != nil {
		return AgentRoute{}, err
	}
	return NewRuntimeAgentRoute(routeCfg), nil
}

func NewRuntimeAgentRoute(cfg AgentRouteConfig) AgentRoute {
	return AgentRoute{
		AgentRouteBaseConfig: cfg.AgentRouteBaseConfig,
		AgentID:              cfg.AgentID,
	}
}

func (r AgentRouteConfig) ToConfig() (AgentRouteBaseConfig, error) {
	r.Normalize()
	targetPolicy, err := json.Marshal(routeTargetPolicy{
		Kind:    routecore.RouteTargetPolicyKindAgent,
		AgentID: r.AgentID,
	})
	if err != nil {
		return AgentRouteBaseConfig{}, fmt.Errorf("route %q encode target policy: %w", r.ID, err)
	}
	cfg := r.AgentRouteBaseConfig
	cfg.ID = r.ID
	cfg.Kind = RouteKindAgent
	cfg.Protocol = RouteProtocolAgent
	cfg.Description = r.Description
	cfg.Disabled = r.Disabled
	cfg.AuthPolicy = r.AuthPolicy
	cfg.MatchPolicy = r.MatchPolicy
	cfg.TargetPolicy = targetPolicy
	cfg.CreatedAt = r.CreatedAt
	cfg.UpdatedAt = r.UpdatedAt
	return cfg, nil
}

func (r AgentRoute) Config() AgentRouteConfig {
	return AgentRouteConfig{
		AgentRouteBaseConfig: r.AgentRouteBaseConfig,
		AgentID:              r.AgentID,
	}
}

func (r AgentRoute) MarshalJSON() ([]byte, error) {
	return r.Config().MarshalJSON()
}

func (r AgentRoute) ToConfig() (AgentRouteBaseConfig, error) {
	return r.Config().ToConfig()
}

// DecodeTargetAgentID extracts the agent id from a persisted agent route
// target policy. The dispatcher uses it to stamp explicit agent attribution
// on the turn span before dispatch.
func DecodeTargetAgentID(targetPolicy json.RawMessage) (string, error) {
	return decodeAgentID(targetPolicy)
}

func decodeAgentID(targetPolicy json.RawMessage) (string, error) {
	var target routeTargetPolicy
	if len(targetPolicy) == 0 {
		return "", nil
	}
	if err := json.Unmarshal(targetPolicy, &target); err != nil {
		return "", err
	}
	if target.Kind != "" && target.Kind != routecore.RouteTargetPolicyKindAgent {
		return "", fmt.Errorf("target policy kind %q is not %q", target.Kind, routecore.RouteTargetPolicyKindAgent)
	}
	return strings.TrimSpace(target.AgentID), nil
}
