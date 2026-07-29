package routecore

import (
	"encoding/json"
	"strconv"
	"time"
)

type RouteKind string

const (
	RouteKindLLM RouteKind = "llm"
	RouteKindMCP RouteKind = "mcp"
	// RouteKindAgent is the unified Agent ingress route kind. It targets an
	// agent_id and stays runtime-neutral: the resolved Agent's runtime.type
	// selects the execution backend (docs/plans/unified-agent-runtime.md §6).
	RouteKindAgent RouteKind = "agent"
)

type RouteProtocol string

const (
	RouteProtocolOpenAI    RouteProtocol = "openai"
	RouteProtocolAnthropic RouteProtocol = "anthropic"
	RouteProtocolCC        RouteProtocol = "cc"
	RouteProtocolMCP       RouteProtocol = "mcp"
	RouteProtocolAgent     RouteProtocol = "agent"
)

type RouteTargetPolicyKind string

const (
	RouteTargetPolicyKindDirectProvider RouteTargetPolicyKind = "direct-provider"
	RouteTargetPolicyKindLogicalModel   RouteTargetPolicyKind = "logical-model"
	RouteTargetPolicyKindMCPService     RouteTargetPolicyKind = "mcp-service"
	RouteTargetPolicyKindAgent          RouteTargetPolicyKind = "agent"
)

type AgentRouteConfig struct {
	ID           string           `json:"id"`
	Kind         RouteKind        `json:"kind"`
	Protocol     RouteProtocol    `json:"protocol"`
	Description  string           `json:"description,omitempty"`
	Disabled     bool             `json:"disabled"`
	AuthPolicy   RouteAuthPolicy  `json:"auth_policy"`
	MatchPolicy  RouteMatchPolicy `json:"match_policy"`
	TargetPolicy json.RawMessage  `json:"target_policy"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
}

// Fingerprint returns a cheap version string for the route config used as the
// runtime resolver materializer key. UpdatedAt is bumped on every Create/Update,
// so it changes whenever the stored config changes. This is a secondary cache
// signal only: the route resolvers also explicitly Invalidate on every mutation,
// so the fingerprint exists to self-heal any resolver that missed invalidation.
// Avoid marshaling the whole config here; this runs on every matched request.
func (c AgentRouteConfig) Fingerprint() string {
	return strconv.FormatInt(c.UpdatedAt.UnixNano(), 10)
}

// RouteMatchPolicy contains transport-facing match fields for binding requests to a route.
type RouteMatchPolicy struct {
	Host       string   `json:"host,omitempty"`
	PathPrefix string   `json:"path_prefix,omitempty"`
	Methods    []string `json:"methods,omitempty"`
}

type RouteAuthPolicy struct {
	RequireVirtualKey bool `json:"require_virtual_key"`
}
