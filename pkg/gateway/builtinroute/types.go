// Package builtinroute defines the config expansion and runtime route model
// for builtin-agent routes: route-dispatched turn ingress for agents hosted
// by the in-process ADK host (docs/design/agents-control-plane.md §5.7.5).
// The shape mirrors acproute; the target is an agent id instead of a service.
package builtinroute

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
)

type RouteKind = routecore.RouteKind

const (
	RouteKindBuiltin = routecore.RouteKindBuiltin
)

type RouteProtocol = routecore.RouteProtocol

const (
	RouteProtocolBuiltin = routecore.RouteProtocolBuiltin
)

type AgentRouteConfig = routecore.AgentRouteConfig
type RouteMatch = routecore.RouteMatchPolicy
type RouteAuthPolicy = routecore.RouteAuthPolicy
type RouteListOptions = routecore.RouteListOptions

type BuiltinRouteConfig struct {
	AgentRouteConfig
	AgentID string `json:"agent_id"`
}

type BuiltinRoute struct {
	AgentRouteConfig
	AgentID string `json:"agent_id"`
}

type routeTargetPolicy struct {
	Kind    routecore.RouteTargetPolicyKind `json:"kind,omitempty"`
	AgentID string                          `json:"agent_id"`
}

func normalizeConfigDefaults(cfg AgentRouteConfig) AgentRouteConfig {
	cfg.ID = strings.TrimSpace(cfg.ID)
	cfg.Kind = RouteKindBuiltin
	cfg.Protocol = RouteProtocolBuiltin
	cfg.Description = strings.TrimSpace(cfg.Description)
	cfg.MatchPolicy.Host = strings.TrimSpace(cfg.MatchPolicy.Host)
	cfg.MatchPolicy.PathPrefix = strings.TrimSpace(cfg.MatchPolicy.PathPrefix)
	for i := range cfg.MatchPolicy.Methods {
		cfg.MatchPolicy.Methods[i] = strings.TrimSpace(cfg.MatchPolicy.Methods[i])
	}
	return cfg
}

func (r *BuiltinRouteConfig) Normalize() {
	if r == nil {
		return
	}
	r.AgentRouteConfig = normalizeConfigDefaults(r.AgentRouteConfig)
	r.AgentID = strings.TrimSpace(r.AgentID)
	if r.ID == "" && r.AgentID != "" {
		r.ID = routecore.GenerateRouteID("builtin", r.AgentID, r.MatchPolicy.PathPrefix)
	}
}

func (r *BuiltinRouteConfig) NormalizeTimestamps(now time.Time) {
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

type builtinRouteJSON struct {
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

func (r BuiltinRouteConfig) MarshalJSON() ([]byte, error) {
	r.Normalize()
	cfg, err := r.ToConfig()
	if err != nil {
		return nil, err
	}
	return json.Marshal(builtinRouteJSON{
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

func (r *BuiltinRoute) UnmarshalJSON(data []byte) error {
	var cfg BuiltinRouteConfig
	if err := cfg.UnmarshalJSON(data); err != nil {
		return err
	}
	*r = NewRuntimeBuiltinRoute(cfg)
	return nil
}

func (r *BuiltinRouteConfig) UnmarshalJSON(data []byte) error {
	var raw builtinRouteJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.AgentRouteConfig = normalizeConfigDefaults(AgentRouteConfig{
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

func DecodeStoredBuiltinRoute(data []byte) (any, error) {
	var cfg AgentRouteConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode builtin route config: %w", err)
	}
	route, err := NewBuiltinRouteConfigFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	route.Normalize()
	return &route, nil
}

func NewBuiltinRouteConfigFromConfig(cfg AgentRouteConfig) (BuiltinRouteConfig, error) {
	cfg = normalizeConfigDefaults(cfg)
	agentID, err := decodeAgentID(cfg.TargetPolicy)
	if err != nil {
		return BuiltinRouteConfig{}, fmt.Errorf("route %q decode target policy: %w", cfg.ID, err)
	}
	route := BuiltinRouteConfig{
		AgentRouteConfig: cfg,
		AgentID:          agentID,
	}
	route.Normalize()
	return route, nil
}

func NewBuiltinRouteFromConfig(cfg AgentRouteConfig) (BuiltinRoute, error) {
	routeCfg, err := NewBuiltinRouteConfigFromConfig(cfg)
	if err != nil {
		return BuiltinRoute{}, err
	}
	return NewRuntimeBuiltinRoute(routeCfg), nil
}

func NewRuntimeBuiltinRoute(cfg BuiltinRouteConfig) BuiltinRoute {
	return BuiltinRoute{
		AgentRouteConfig: cfg.AgentRouteConfig,
		AgentID:          cfg.AgentID,
	}
}

func (r BuiltinRouteConfig) ToConfig() (AgentRouteConfig, error) {
	r.Normalize()
	targetPolicy, err := json.Marshal(routeTargetPolicy{
		Kind:    routecore.RouteTargetPolicyKindBuiltinAgent,
		AgentID: r.AgentID,
	})
	if err != nil {
		return AgentRouteConfig{}, fmt.Errorf("route %q encode target policy: %w", r.ID, err)
	}
	cfg := r.AgentRouteConfig
	cfg.ID = r.ID
	cfg.Kind = RouteKindBuiltin
	cfg.Protocol = RouteProtocolBuiltin
	cfg.Description = r.Description
	cfg.Disabled = r.Disabled
	cfg.AuthPolicy = r.AuthPolicy
	cfg.MatchPolicy = r.MatchPolicy
	cfg.TargetPolicy = targetPolicy
	cfg.CreatedAt = r.CreatedAt
	cfg.UpdatedAt = r.UpdatedAt
	return cfg, nil
}

func (r BuiltinRoute) Config() BuiltinRouteConfig {
	return BuiltinRouteConfig{
		AgentRouteConfig: r.AgentRouteConfig,
		AgentID:          r.AgentID,
	}
}

func (r BuiltinRoute) MarshalJSON() ([]byte, error) {
	return r.Config().MarshalJSON()
}

func (r BuiltinRoute) ToConfig() (AgentRouteConfig, error) {
	return r.Config().ToConfig()
}

// DecodeTargetAgentID extracts the agent id from a persisted builtin route
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
	return strings.TrimSpace(target.AgentID), nil
}
