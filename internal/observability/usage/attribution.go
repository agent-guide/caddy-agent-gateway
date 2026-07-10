package usage

import "sync/atomic"

// AgentAttributor maps an originating route/service/session back to a single
// agent id for write-time usage attribution. It is implemented by the agent
// control-plane layer and consumed here so the lower layers never depend on
// pkg/agent (the dependency arrow stays pkg/agent -> runtime). It returns
// ok=false when the mapping is empty or ambiguous, in which case the agent id is
// left empty.
type AgentAttributor interface {
	ResolveAgentID(routeID, serviceID, sessionID string) (string, bool)
}

// AgentAttribution is a settable holder for an AgentAttributor. The metrics
// pipeline (and therefore the observer) is constructed before the agent manager
// exists, so the concrete attributor is injected later via Set without rebuilding
// the observer.
type AgentAttribution struct {
	v atomic.Value // holds attributorBox
}

type attributorBox struct {
	attr AgentAttributor
}

func NewAgentAttribution() *AgentAttribution {
	return &AgentAttribution{}
}

// Set installs (or replaces) the active attributor. A nil attributor is ignored.
func (a *AgentAttribution) Set(attr AgentAttributor) {
	if a == nil || attr == nil {
		return
	}
	a.v.Store(attributorBox{attr: attr})
}

// ResolveAgentID delegates to the installed attributor, returning ok=false when
// none is installed.
func (a *AgentAttribution) ResolveAgentID(routeID, serviceID, sessionID string) (string, bool) {
	if a == nil {
		return "", false
	}
	box, ok := a.v.Load().(attributorBox)
	if !ok || box.attr == nil {
		return "", false
	}
	return box.attr.ResolveAgentID(routeID, serviceID, sessionID)
}

// AttributionFilter selects events that belong to one agent. Write-time
// agent_id stamping is the primary signal, but it cannot cover events written
// before the agent existed, before the agent_id column existed, or while the
// route/service was bound to a different agent. The filter therefore matches the
// durable agent_id tag OR a route/service the agent currently owns, so per-agent
// reads (interactions, usage, activity, health) fall back to the route/service
// mapping for untagged-but-mappable events. An empty filter matches nothing.
//
// ACPServiceIDs is intentionally ACP-only: the ACP runtime service is bound by
// at most one agent (P0 one-runtime-one-agent), so it is the only unambiguous
// service-level fallback. MCP service resources carry no uniqueness constraint
// and are recovered through RouteIDs instead. The query layer keeps this arm
// ACP-scoped so a same-named MCP service is never matched.
type AttributionFilter struct {
	AgentID       string
	RouteIDs      []string
	ACPServiceIDs []string
}

// IsEmpty reports whether the filter carries no selector at all.
func (f *AttributionFilter) IsEmpty() bool {
	return f == nil || (f.AgentID == "" && len(f.RouteIDs) == 0 && len(f.ACPServiceIDs) == 0)
}
