// Package agent is the external control-plane layer that composes the gateway's
// LLM, MCP, ACP, and metrics surfaces around an operator-facing agent identity.
//
// It depends on the lower-level protocol managers and query services; the
// protocol packages must not depend on pkg/agent. See
// docs/design/agents-control-plane.md for the full direction.
package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	baseacp "github.com/agent-guide/agent-gateway/pkg/acp"
	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
)

var (
	ErrAgentNotConfigured = fmt.Errorf("agent is not configured")
	ErrAgentRouteTarget   = fmt.Errorf("agent is targeted by an agent route")
)

// Runtime backend types, split by who owns the agent's process lifecycle.
const (
	// RuntimeTypeACP: the gateway owns the lifecycle (process pool, sessions,
	// permission flow, transcript) through an ACP service.
	RuntimeTypeACP = "acp"
	// RuntimeTypeHTTP: the agent service owns its own lifecycle; the gateway is
	// only a client. P0 defines the shape but does not dispatch to it yet.
	RuntimeTypeHTTP = "http"
	// RuntimeTypeBuiltin: no separate process at all — the agent is a persisted
	// definition materialized by the in-process generic ADK host
	// (docs/design/agents-control-plane.md §5.7).
	RuntimeTypeBuiltin = "builtin"
)

// Agent is a first-class management object representing an operator-facing agent
// identity, not a protocol-specific service.
type Agent struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Runtime     Runtime   `json:"runtime"`
	Routes      Routes    `json:"routes"`
	Resources   Resources `json:"resources"`
	Policy      Policy    `json:"policy"`
	Disabled    bool      `json:"disabled"`
	// OwnsService is retained only for compiling the unreachable pre-M5
	// migration code and is never persisted or exposed.
	OwnsService bool      `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Runtime is authoritative for execution. It binds the agent to exactly one
// runtime backend instance, selected by Type.
type Runtime struct {
	Type    string          `json:"type"`
	ACP     *ACPRuntime     `json:"acp,omitempty"`
	HTTP    *HTTPRuntime    `json:"http,omitempty"`
	Builtin *BuiltinRuntime `json:"builtin,omitempty"`
}

// ACPRuntime is the Agent-owned ACP execution configuration. Agent identity,
// lifecycle metadata, and disabled state stay on Agent itself.
type ACPRuntime struct {
	// ServiceID is legacy migration input only; new Agent definitions reject it
	// during store/bundle preflight and it is never persisted.
	ServiceID       string                  `json:"-"`
	AgentType       string                  `json:"agent_type"`
	CWD             string                  `json:"cwd"`
	AllowedRoots    []string                `json:"allowed_roots,omitempty"`
	DefaultModel    string                  `json:"default_model,omitempty"`
	Env             map[string]string       `json:"env,omitempty"`
	ConfigOverrides map[string]string       `json:"config_overrides,omitempty"`
	IdleTTL         time.Duration           `json:"idle_ttl,omitempty"`
	MaxInstances    int                     `json:"max_instances,omitempty"`
	PermissionMode  string                  `json:"permission_mode,omitempty"`
	Codex           *acpservice.CodexConfig `json:"codex,omitempty"`
}

// HTTPRuntime carries the agent-level endpoint and callback auth for an agent
// that owns its own lifecycle.
type HTTPRuntime struct {
	Endpoint string `json:"endpoint"`
	AuthRef  string `json:"auth_ref,omitempty"`
}

// Routes are management/display references used to surface matching ingress
// routes and to drive attribution; they do not select the execution backend.
type Routes struct {
	LLMRouteIDs     []string `json:"llm_route_ids,omitempty"`
	MCPRouteIDs     []string `json:"mcp_route_ids,omitempty"`
	ACPRouteIDs     []string `json:"-"`
	BuiltinRouteIDs []string `json:"-"`
}

// ACPServiceID exists only for unreachable legacy adapter code retained until
// M7 source deletion. New persisted definitions can never populate ServiceID.
func (a Agent) ACPServiceID() string {
	if a.Runtime.Type == RuntimeTypeACP && a.Runtime.ACP != nil {
		return strings.TrimSpace(a.Runtime.ACP.ServiceID)
	}
	return ""
}

// Resources is a management view of what the agent is allowed to use. It is not
// enforced inline on the data-plane request path in P0/P1.
type Resources struct {
	ProviderIDs   []string `json:"provider_ids,omitempty"`
	MCPServiceIDs []string `json:"mcp_service_ids,omitempty"`
	VirtualKeyIDs []string `json:"virtual_key_ids,omitempty"`
}

// Policy holds runtime-agnostic governance only. Runtime-specific config
// belongs under runtime.<type> or on the backing service.
type Policy struct {
	MaxAgentDepth int     `json:"max_agent_depth,omitempty"`
	Budget        *Budget `json:"budget,omitempty"`
}

type Budget struct {
	MaxTurnsPerDay  int `json:"max_turns_per_day,omitempty"`
	MaxTokensPerDay int `json:"max_tokens_per_day,omitempty"`
}

func DecodeStoredAgentConfig(data []byte) (any, error) {
	var a Agent
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, err
	}
	a.Normalize()
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return &a, nil
}

func (a *Agent) Normalize() {
	if a == nil {
		return
	}
	a.ID = strings.TrimSpace(a.ID)
	a.Name = strings.TrimSpace(a.Name)
	a.Description = strings.TrimSpace(a.Description)
	a.Runtime.Type = strings.TrimSpace(a.Runtime.Type)
	switch a.Runtime.Type {
	case RuntimeTypeACP:
		if a.Runtime.ACP != nil {
			if strings.TrimSpace(a.Runtime.ACP.ServiceID) != "" && strings.TrimSpace(a.Runtime.ACP.AgentType) == "" {
				a.Runtime.ACP.AgentType, a.Runtime.ACP.CWD = baseacp.AgentTypeCodex, "/tmp"
				a.Runtime.ACP.AllowedRoots = []string{"/tmp"}
			}
			a.Runtime.ACP.normalize()
		}
		a.Runtime.HTTP = nil
		a.Runtime.Builtin = nil
	case RuntimeTypeHTTP:
		if a.Runtime.HTTP != nil {
			a.Runtime.HTTP.Endpoint = strings.TrimSpace(a.Runtime.HTTP.Endpoint)
			a.Runtime.HTTP.AuthRef = strings.TrimSpace(a.Runtime.HTTP.AuthRef)
		}
		a.Runtime.ACP = nil
		a.Runtime.Builtin = nil
	case RuntimeTypeBuiltin:
		a.Runtime.Builtin.normalize()
		a.Runtime.ACP = nil
		a.Runtime.HTTP = nil
	}
	a.Routes.LLMRouteIDs = normalizeIDs(a.Routes.LLMRouteIDs)
	a.Routes.MCPRouteIDs = normalizeIDs(a.Routes.MCPRouteIDs)
	a.Routes.ACPRouteIDs = normalizeIDs(a.Routes.ACPRouteIDs)
	a.Routes.BuiltinRouteIDs = normalizeIDs(a.Routes.BuiltinRouteIDs)
	a.Resources.ProviderIDs = normalizeIDs(a.Resources.ProviderIDs)
	a.Resources.MCPServiceIDs = normalizeIDs(a.Resources.MCPServiceIDs)
	a.Resources.VirtualKeyIDs = normalizeIDs(a.Resources.VirtualKeyIDs)
}

func (a Agent) Validate() error {
	if a.ID == "" {
		return fmt.Errorf("id is required")
	}
	if a.Name == "" {
		return fmt.Errorf("name is required")
	}
	switch a.Runtime.Type {
	case RuntimeTypeACP:
		if a.Runtime.ACP == nil {
			return fmt.Errorf("runtime.acp is required for acp runtime")
		}
		if a.Runtime.HTTP != nil {
			return fmt.Errorf("runtime.http must be empty for acp runtime")
		}
		if a.Runtime.Builtin != nil {
			return fmt.Errorf("runtime.builtin must be empty for acp runtime")
		}
		if err := a.Runtime.ACP.validate(a.ID); err != nil {
			return fmt.Errorf("runtime.acp: %w", err)
		}
	case RuntimeTypeHTTP:
		if a.Runtime.HTTP == nil || a.Runtime.HTTP.Endpoint == "" {
			return fmt.Errorf("runtime.http.endpoint is required for http runtime")
		}
		if a.Runtime.ACP != nil {
			return fmt.Errorf("runtime.acp must be empty for http runtime")
		}
		if a.Runtime.Builtin != nil {
			return fmt.Errorf("runtime.builtin must be empty for http runtime")
		}
	case RuntimeTypeBuiltin:
		if a.Runtime.ACP != nil {
			return fmt.Errorf("runtime.acp must be empty for builtin runtime")
		}
		if a.Runtime.HTTP != nil {
			return fmt.Errorf("runtime.http must be empty for builtin runtime")
		}
		if err := a.validateBuiltin(); err != nil {
			return err
		}
	case "":
		return fmt.Errorf("runtime.type is required")
	default:
		return fmt.Errorf("unsupported runtime.type %q", a.Runtime.Type)
	}
	if a.Runtime.Type != RuntimeTypeBuiltin && len(a.Routes.BuiltinRouteIDs) > 0 {
		return fmt.Errorf("routes.builtin_route_ids is only valid for builtin runtime agents")
	}
	if a.Policy.MaxAgentDepth < 0 {
		return fmt.Errorf("policy.max_agent_depth must be non-negative")
	}
	if a.Policy.Budget != nil {
		if a.Policy.Budget.MaxTurnsPerDay < 0 || a.Policy.Budget.MaxTokensPerDay < 0 {
			return fmt.Errorf("policy.budget values must be non-negative")
		}
	}
	return nil
}

func (c *ACPRuntime) normalize() {
	if c == nil {
		return
	}
	cfg := c.serviceConfig("agent")
	cfg.Normalize()
	c.AgentType, c.CWD, c.AllowedRoots = cfg.AgentType, cfg.CWD, cfg.AllowedRoots
	c.DefaultModel, c.Env, c.ConfigOverrides = cfg.DefaultModel, cfg.Env, cfg.ConfigOverrides
	c.IdleTTL, c.MaxInstances, c.PermissionMode, c.Codex = cfg.IdleTTL, cfg.MaxInstances, cfg.PermissionMode, cfg.Codex
}

func (c ACPRuntime) validate(agentID string) error {
	cfg := c.serviceConfig(agentID)
	if err := cfg.Validate(); err != nil {
		return err
	}
	return nil
}

func (c ACPRuntime) serviceConfig(agentID string) acpservice.ServiceConfig {
	name := strings.TrimSpace(agentID)
	if name == "" {
		name = "agent"
	}
	mode := strings.TrimSpace(c.PermissionMode)
	if mode == "" {
		mode = baseacp.PermissionModeDeny
	}
	return acpservice.ServiceConfig{
		ID: name, Name: name, AgentType: c.AgentType, CWD: c.CWD,
		AllowedRoots: append([]string(nil), c.AllowedRoots...), DefaultModel: c.DefaultModel,
		Env: cloneStringMap(c.Env), ConfigOverrides: cloneStringMap(c.ConfigOverrides),
		IdleTTL: c.IdleTTL, MaxInstances: c.MaxInstances, PermissionMode: mode,
		Codex: cloneACPCodexConfig(c.Codex),
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneACPCodexConfig(in *acpservice.CodexConfig) *acpservice.CodexConfig {
	if in == nil {
		return nil
	}
	out := *in
	out.AdapterArgs = append([]string(nil), in.AdapterArgs...)
	out.AppServerArgs = append([]string(nil), in.AppServerArgs...)
	return &out
}

func (a *Agent) NormalizeTimestamps(now time.Time) {
	if a == nil {
		return
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
}

func normalizeIDs(ids []string) []string {
	if len(ids) == 0 {
		return ids
	}
	out := make([]string, 0, len(ids))
	seen := map[string]struct{}{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
