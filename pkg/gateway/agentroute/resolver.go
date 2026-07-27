package agentroute

import (
	"context"
	"fmt"
	"sync"

	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	"github.com/agent-guide/agent-gateway/pkg/gateway/runtimecore"
)

var (
	ErrRouteNotConfigured  = routecore.ErrRouteNotConfigured
	ErrStaticRouteReadOnly = routecore.ErrStaticRouteReadOnly
	ErrInvalidRouteID      = routecore.ErrInvalidRouteID
)

// AgentLookup reports whether a target Agent exists. Existence is a management
// validity check only: disabled Agents and Agents whose backend is not
// currently executable remain valid route targets (plan §6.2).
type AgentLookup interface {
	HasAgent(id string) bool
}

type AgentRouteResolver struct {
	configManager *routecore.AgentRouteConfigManager
	base          *runtimecore.Resolver[routecore.AgentRouteConfig, *AgentRoute, RouteListOptions]

	mu          sync.RWMutex
	agentLookup AgentLookup
}

func NewAgentRouteResolver(configManager *routecore.AgentRouteConfigManager) *AgentRouteResolver {
	return &AgentRouteResolver{
		configManager: configManager,
		base: runtimecore.NewResolver(
			runtimecore.FuncSource[routecore.AgentRouteConfig, RouteListOptions]{
				GetFunc: func(ctx context.Context, routeID string) (routecore.AgentRouteConfig, error) {
					if configManager == nil {
						return routecore.AgentRouteConfig{}, fmt.Errorf("route config manager is not configured")
					}
					return configManager.Get(ctx, routeID)
				},
				ListFunc: func(ctx context.Context, opts RouteListOptions) ([]routecore.AgentRouteConfig, error) {
					if configManager == nil {
						return nil, fmt.Errorf("route config manager is not configured")
					}
					return configManager.List(ctx, routecore.RouteListOptions(opts))
				},
			},
			func(cfg routecore.AgentRouteConfig) string {
				return cfg.ID
			},
			func(cfg routecore.AgentRouteConfig) (string, error) {
				return cfg.Fingerprint(), nil
			},
			func(cfg routecore.AgentRouteConfig) (*AgentRoute, error) {
				route, err := NewAgentRouteFromConfig(cfg)
				if err != nil {
					return nil, err
				}
				return &route, nil
			},
		),
	}
}

// SetAgentLookup wires the optional target-existence check used by
// CreateConfig/UpdateConfig. When nil, target validation is skipped.
func (r *AgentRouteResolver) SetAgentLookup(lookup AgentLookup) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.agentLookup = lookup
	r.mu.Unlock()
}

func (r *AgentRouteResolver) lookup() AgentLookup {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.agentLookup
}

// validateTarget enforces that a persisted AgentRoute names an existing Agent.
// Management validity is separate from execution availability: disabled or
// currently non-executable Agents are valid targets and fail at dispatch with
// their normalized runtime error instead.
func (r *AgentRouteResolver) validateTarget(route routecore.AgentRouteConfig) error {
	agentID, err := DecodeTargetAgentID(route.TargetPolicy)
	if err != nil {
		return fmt.Errorf("route %q decode target policy: %w", route.ID, err)
	}
	if agentID == "" {
		return fmt.Errorf("route %q requires target_policy.agent_id", route.ID)
	}
	if lookup := r.lookup(); lookup != nil && !lookup.HasAgent(agentID) {
		return fmt.Errorf("route %q targets unknown agent %q", route.ID, agentID)
	}
	return nil
}

func (r *AgentRouteResolver) ConfigManager() *routecore.AgentRouteConfigManager {
	if r == nil {
		return nil
	}
	return r.configManager
}

func (r *AgentRouteResolver) GetConfig(ctx context.Context, routeID string) (routecore.AgentRouteConfig, error) {
	manager := r.ConfigManager()
	if manager == nil {
		return routecore.AgentRouteConfig{}, fmt.Errorf("route config manager is not configured")
	}
	return manager.Get(ctx, routeID)
}

func (r *AgentRouteResolver) ListConfigs(ctx context.Context, opts RouteListOptions) ([]routecore.AgentRouteConfig, error) {
	manager := r.ConfigManager()
	if manager == nil {
		return nil, fmt.Errorf("route config manager is not configured")
	}
	return manager.List(ctx, routecore.RouteListOptions(opts))
}

func (r *AgentRouteResolver) CreateConfig(ctx context.Context, route routecore.AgentRouteConfig, tag string) error {
	if route.ID == "" {
		return fmt.Errorf("route id is required")
	}
	if err := routecore.ValidateRouteID(route.ID); err != nil {
		return err
	}
	if err := r.validateTarget(route); err != nil {
		return err
	}
	manager := r.ConfigManager()
	if manager == nil {
		return fmt.Errorf("route config manager is not configured")
	}
	if err := manager.Create(ctx, route, tag); err != nil {
		return err
	}
	r.base.Invalidate(route.ID)
	return nil
}

func (r *AgentRouteResolver) UpdateConfig(ctx context.Context, routeID string, route routecore.AgentRouteConfig) error {
	if routeID == "" {
		return fmt.Errorf("route id is required")
	}
	if err := routecore.ValidateRouteID(routeID); err != nil {
		return err
	}
	if err := r.validateTarget(route); err != nil {
		return err
	}
	manager := r.ConfigManager()
	if manager == nil {
		return fmt.Errorf("route config manager is not configured")
	}
	if err := manager.Update(ctx, routeID, route); err != nil {
		return err
	}
	r.base.Invalidate(routeID)
	return nil
}

func (r *AgentRouteResolver) DeleteConfig(ctx context.Context, routeID string) error {
	if routeID == "" {
		return fmt.Errorf("route id is required")
	}
	manager := r.ConfigManager()
	if manager == nil {
		return fmt.Errorf("route config manager is not configured")
	}
	if err := manager.Delete(ctx, routeID); err != nil {
		return err
	}
	r.base.Invalidate(routeID)
	return nil
}

func (r *AgentRouteResolver) Resolve(ctx context.Context, cfg routecore.AgentRouteConfig) (*AgentRoute, error) {
	if r == nil {
		return nil, fmt.Errorf("route config manager is not configured")
	}
	if cfg.ID == "" || cfg.Kind != routecore.RouteKindAgent {
		return nil, nil
	}
	route, err := r.base.ResolveConfig(cfg)
	if err != nil {
		return nil, err
	}
	if route == nil {
		return nil, fmt.Errorf("route %q is nil", cfg.ID)
	}
	return route, nil
}

func (r *AgentRouteResolver) ResolveByID(ctx context.Context, routeID string) (*AgentRoute, error) {
	cfg, err := r.GetConfig(ctx, routeID)
	if err != nil {
		return nil, err
	}
	return r.Resolve(ctx, cfg)
}
