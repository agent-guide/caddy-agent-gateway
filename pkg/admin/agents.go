package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	acpruntime "github.com/agent-guide/agent-gateway/pkg/acp/runtime"
	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	acproute "github.com/agent-guide/agent-gateway/pkg/gateway/acproute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
)

type AgentView struct {
	agentpkg.Agent
	Source string `json:"source"`
}

func (h *Handler) agentManagerOrError() (*agentpkg.Manager, error) {
	if h.agentManager == nil {
		return nil, fmt.Errorf("agent manager is not configured")
	}
	return h.agentManager, nil
}

func (h *Handler) handleListAgents(w http.ResponseWriter, r *http.Request) {
	manager, err := h.agentManagerOrError()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	items, err := manager.List(r.Context())
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	views := make([]AgentView, 0, len(items))
	for _, item := range items {
		views = append(views, AgentView{Agent: item, Source: "config_store"})
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"items": views})
}

func (h *Handler) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	manager, err := h.agentManagerOrError()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	var a agentpkg.Agent
	if err := httpjson.Decode(r, &a); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	a.Normalize()
	if err := manager.Create(r.Context(), a); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := manager.Get(r.Context(), a.ID)
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusCreated, AgentView{Agent: created, Source: "config_store"})
}

func (h *Handler) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	manager, err := h.agentManagerOrError()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	cfg, err := manager.Get(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		if errors.Is(err, agentpkg.ErrAgentNotConfigured) {
			_ = httpjson.Error(w, http.StatusNotFound, "agent not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, AgentView{Agent: cfg, Source: "config_store"})
}

func (h *Handler) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	manager, err := h.agentManagerOrError()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	var a agentpkg.Agent
	if err := httpjson.Decode(r, &a); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	if err := manager.Update(r.Context(), id, a); err != nil {
		if errors.Is(err, agentpkg.ErrAgentNotConfigured) || errors.Is(err, configstore.ErrNotFound) {
			_ = httpjson.Error(w, http.StatusNotFound, "agent not found")
			return
		}
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := manager.Get(r.Context(), id)
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, AgentView{Agent: updated, Source: "config_store"})
}

func (h *Handler) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	manager, err := h.agentManagerOrError()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	// Deleting an agent removes only the Agent record; it never cascade-deletes
	// the backing ACP service or routes, which may be shared or independently
	// operated. Report what was unbound so the caller knows the runtime backend
	// remains.
	current, getErr := manager.Get(r.Context(), id)
	if err := manager.Delete(r.Context(), id); err != nil {
		if errors.Is(err, agentpkg.ErrAgentNotConfigured) || errors.Is(err, configstore.ErrNotFound) {
			_ = httpjson.Error(w, http.StatusNotFound, "agent not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	unbound := map[string]any{}
	if getErr == nil {
		if svc := current.ACPServiceID(); svc != "" {
			unbound["acp_service_id"] = svc
		}
		if len(current.Routes.ACPRouteIDs) > 0 {
			unbound["acp_route_ids"] = current.Routes.ACPRouteIDs
		}
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"status": "deleted", "id": id, "unbound": unbound})
}

// AgentWorkspace is the summary/index read model for the agent detail page. It
// returns summaries, counts, runtime state, and references — never full session
// transcripts. The frontend drills into the linked ACP endpoints for content.
type AgentWorkspace struct {
	Agent       agentpkg.Agent       `json:"agent"`
	Runtime     string               `json:"runtime"`
	ACPService  *ACPServiceView      `json:"acp_service,omitempty"`
	ACPRoutes   []agentRouteRef      `json:"acp_routes,omitempty"`
	RuntimeView *agentRuntimeSummary `json:"runtime_view,omitempty"`
	Usage       *usage.ACPSummary    `json:"usage,omitempty"`
	Links       map[string]string    `json:"links,omitempty"`
}

type agentRouteRef struct {
	ID         string `json:"id"`
	PathPrefix string `json:"path_prefix,omitempty"`
	ServiceID  string `json:"service_id"`
}

type agentRuntimeSummary struct {
	PooledInstances    []acpruntime.PooledInstanceInfo    `json:"pooled_instances"`
	InFlightTurns      int                                `json:"in_flight_turns"`
	PendingPermissions []acpruntime.PendingPermissionInfo `json:"pending_permissions"`
}

func (h *Handler) handleGetAgentWorkspace(w http.ResponseWriter, r *http.Request) {
	manager, err := h.agentManagerOrError()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	a, err := manager.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, agentpkg.ErrAgentNotConfigured) {
			_ = httpjson.Error(w, http.StatusNotFound, "agent not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	ws := AgentWorkspace{Agent: a, Runtime: a.Runtime.Type}

	// The acp runtime view is the only one P0 assembles. An http-runtime agent
	// degrades to the runtime-agnostic parts.
	serviceID := a.ACPServiceID()
	if serviceID != "" {
		h.assembleACPWorkspace(r.Context(), &ws, serviceID)
	}
	_ = httpjson.Write(w, http.StatusOK, ws)
}

func (h *Handler) assembleACPWorkspace(ctx context.Context, ws *AgentWorkspace, serviceID string) {
	if h.sharedACPServiceManager != nil {
		if svc, err := h.sharedACPServiceManager.Get(ctx, serviceID); err == nil {
			view := ACPServiceView{ServiceConfig: svc, Source: "config_store"}
			ws.ACPService = &view
		}
	}
	ws.ACPRoutes = h.acpRoutesForService(ctx, serviceID)
	ws.RuntimeView = h.acpRuntimeSummaryForService(serviceID)
	if h.usageQuery != nil {
		if summary, err := h.usageQuery.ACPSummary(usage.BreakdownOptions{
			Filters: map[string]string{"service_id": serviceID},
		}); err == nil {
			ws.Usage = acpSummaryFromBreakdown(summary)
		}
	}
	ws.Links = map[string]string{}
	for _, route := range ws.ACPRoutes {
		prefix := strings.TrimRight(route.PathPrefix, "/")
		ws.Links["sessions"] = prefix + "/sessions"
		ws.Links["transcript"] = prefix + "/sessions/{session_id}/transcript"
		break
	}
	ws.Links["admin_sessions"] = "/admin/acp/services/" + serviceID + "/sessions"
	ws.Links["admin_runtime"] = "/admin/acp/runtime"
}

func (h *Handler) acpRoutesForService(ctx context.Context, serviceID string) []agentRouteRef {
	if h.sharedACPRouteResolver == nil {
		return nil
	}
	configs, err := h.sharedACPRouteResolver.ListConfigs(ctx, acproute.RouteListOptions{})
	if err != nil {
		return nil
	}
	var refs []agentRouteRef
	for _, cfg := range configs {
		if cfg.Kind != acproute.RouteKindACP {
			continue
		}
		route, err := acproute.NewACPRouteFromConfig(cfg)
		if err != nil {
			continue
		}
		if route.ServiceID != serviceID {
			continue
		}
		refs = append(refs, agentRouteRef{
			ID:         route.ID,
			PathPrefix: route.MatchPolicy.PathPrefix,
			ServiceID:  route.ServiceID,
		})
	}
	return refs
}

func (h *Handler) acpRuntimeSummaryForService(serviceID string) *agentRuntimeSummary {
	if h.acpRuntimeManager == nil {
		return nil
	}
	summary := &agentRuntimeSummary{}
	for _, inst := range h.acpRuntimeManager.ListInstances() {
		if acpruntime.ScopeServiceID(inst.Scope) == serviceID {
			summary.PooledInstances = append(summary.PooledInstances, inst)
		}
	}
	for _, turn := range h.acpRuntimeManager.ListInFlight() {
		if acpruntime.ScopeServiceID(turn.Scope) == serviceID {
			summary.InFlightTurns++
		}
	}
	for _, perm := range h.acpRuntimeManager.ListPendingPermissions() {
		if perm.ServiceID == serviceID {
			summary.PendingPermissions = append(summary.PendingPermissions, perm)
		}
	}
	return summary
}

// acpSummaryFromBreakdown collapses an ACP breakdown filtered by service into a
// single summary. The breakdown rows already aggregate the service, so the first
// row carries the totals.
func acpSummaryFromBreakdown(b usage.BreakdownResponse) *usage.ACPSummary {
	out := &usage.ACPSummary{}
	for _, row := range b.Items {
		out.RequestCount += intFromAny(row["request_count"])
		out.TurnCount += intFromAny(row["turn_count"])
		out.SuccessCount += intFromAny(row["success_count"])
		out.FailureCount += intFromAny(row["failure_count"])
	}
	return out
}

// getAgentOr404 loads an agent, writing the appropriate error response and
// returning ok=false when it is missing or the manager is unavailable.
func (h *Handler) getAgentOr404(w http.ResponseWriter, r *http.Request) (agentpkg.Agent, bool) {
	manager, err := h.agentManagerOrError()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return agentpkg.Agent{}, false
	}
	a, err := manager.Get(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		if errors.Is(err, agentpkg.ErrAgentNotConfigured) {
			_ = httpjson.Error(w, http.StatusNotFound, "agent not found")
			return agentpkg.Agent{}, false
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return agentpkg.Agent{}, false
	}
	return a, true
}

// agentAttributionFilter builds the metrics attribution selector for an agent:
// the durable agent_id tag OR any route/service the agent currently owns. This
// is what lets per-agent reads include untagged-but-mappable events (pre-P1
// history, or events written before a route/service was reassigned to this
// agent) instead of only events stamped at write time.
func agentAttributionFilter(a agentpkg.Agent) *usage.AttributionFilter {
	f := &usage.AttributionFilter{AgentID: a.ID}
	f.RouteIDs = append(f.RouteIDs, a.Routes.LLMRouteIDs...)
	f.RouteIDs = append(f.RouteIDs, a.Routes.MCPRouteIDs...)
	f.RouteIDs = append(f.RouteIDs, a.Routes.ACPRouteIDs...)
	// Only the ACP runtime service is a safe service-level fallback: it is bound
	// by at most one agent (P0 one-runtime-one-agent), so a service-keyed event
	// attributes unambiguously. MCP service resources have no such uniqueness
	// constraint — two agents may list the same mcp_service_id, so a service-level
	// fallback there would double-attribute untagged MCP usage. MCP events are
	// instead recovered through the agent's owned mcp_route_ids (route fallback).
	if svc := a.ACPServiceID(); svc != "" {
		f.ACPServiceIDs = append(f.ACPServiceIDs, svc)
	}
	return f
}

// agentAttributionFromRequest resolves an optional `agent_id` query filter into
// a metrics attribution selector (the durable agent_id tag OR the agent's owned
// routes/ACP service, matching the per-agent usage/interactions reads). When no
// agent_id is present it returns (nil, true) so callers apply no attribution.
// On an unresolvable agent id it writes the error response and returns ok=false.
func (h *Handler) agentAttributionFromRequest(w http.ResponseWriter, r *http.Request) (*usage.AttributionFilter, bool) {
	id := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	if id == "" {
		return nil, true
	}
	manager, err := h.agentManagerOrError()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return nil, false
	}
	a, err := manager.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, agentpkg.ErrAgentNotConfigured) {
			_ = httpjson.Error(w, http.StatusNotFound, "agent not found")
			return nil, false
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	return agentAttributionFilter(a), true
}

// handleGetAgentInteractions returns interaction events attributed to the agent.
// It prefers the durable agent_id tag and falls back to the agent's owned
// route/service mapping so untagged-but-mappable events still surface.
func (h *Handler) handleGetAgentInteractions(w http.ResponseWriter, r *http.Request) {
	a, ok := h.getAgentOr404(w, r)
	if !ok {
		return
	}
	opts, err := metricEventListOptions(r, []string{
		"route_kind", "route_protocol", "route_id", "virtual_key_id",
		"trace_id", "parent_span_id", "agent_depth", "service_id", "session_id",
	})
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	opts.Attribution = agentAttributionFilter(a)
	if h.usageQuery == nil {
		_ = httpjson.Write(w, http.StatusOK, usage.EventListResponse{Limit: opts.Limit})
		return
	}
	resp, err := h.usageQuery.ListInteractions(opts)
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, resp)
}

// handleGetAgentActivity assembles a recent activity feed: tagged interaction
// events plus the agent's pending ACP permissions.
func (h *Handler) handleGetAgentActivity(w http.ResponseWriter, r *http.Request) {
	a, ok := h.getAgentOr404(w, r)
	if !ok {
		return
	}
	out := map[string]any{}
	if h.usageQuery != nil {
		opts := usage.EventListOptions{Limit: 50, Attribution: agentAttributionFilter(a)}
		if resp, err := h.usageQuery.ListInteractions(opts); err == nil {
			out["interactions"] = resp.Items
		}
	}
	if serviceID := a.ACPServiceID(); serviceID != "" && h.acpRuntimeManager != nil {
		var pending []acpruntime.PendingPermissionInfo
		for _, perm := range h.acpRuntimeManager.ListPendingPermissions() {
			if perm.ServiceID == serviceID {
				pending = append(pending, perm)
			}
		}
		out["pending_permissions"] = pending
	}
	_ = httpjson.Write(w, http.StatusOK, out)
}

// handleGetAgentUsage returns per-protocol usage rollups filtered by the agent
// attribution tag.
func (h *Handler) handleGetAgentUsage(w http.ResponseWriter, r *http.Request) {
	a, ok := h.getAgentOr404(w, r)
	if !ok {
		return
	}
	out := map[string]any{"agent_id": a.ID}
	if h.usageQuery == nil {
		_ = httpjson.Write(w, http.StatusOK, out)
		return
	}
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	bucket := strings.TrimSpace(r.URL.Query().Get("bucket"))
	attribution := agentAttributionFilter(a)
	llm, err := h.usageQuery.LLMBreakdown(usage.BreakdownOptions{From: from, To: to, GroupBy: "upstream_model", Attribution: attribution})
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	out["llm"] = llm
	llmSeries, err := h.usageQuery.LLMTimeseries(usage.TimeseriesOptions{From: from, To: to, Bucket: bucket, GroupBy: "route_id", Attribution: attribution})
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	out["timeseries"] = map[string]any{"llm": llmSeries}
	mcp, err := h.usageQuery.MCPToolsSummary(usage.SummaryOptions{From: from, To: to, Attribution: attribution})
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	out["mcp"] = mcp
	acp, err := h.usageQuery.ACPSummary(usage.BreakdownOptions{From: from, To: to, GroupBy: "operation", Attribution: attribution})
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	out["acp"] = acp
	_ = httpjson.Write(w, http.StatusOK, out)
}

type agentResourcesView struct {
	Resources agentpkg.Resources      `json:"resources"`
	Routes    agentpkg.Routes         `json:"routes"`
	Resolved  *agentResolvedResources `json:"resolved,omitempty"`
}

// agentResolvedResources turns the agent's stored id lists into resolved object
// summaries so the resources endpoint is an agent console (P1: "show the LLM
// providers, routes, MCP services, and VirtualKeys the agent can use") rather
// than an echo of the raw id lists. exists=false flags a dangling reference.
type agentResolvedResources struct {
	Providers   []resourceRef `json:"providers"`
	MCPServices []resourceRef `json:"mcp_services"`
	VirtualKeys []resourceRef `json:"virtual_keys"`
	LLMRoutes   []resourceRef `json:"llm_routes"`
	MCPRoutes   []resourceRef `json:"mcp_routes"`
	ACPRoutes   []resourceRef `json:"acp_routes"`
}

type resourceRef struct {
	ID       string `json:"id"`
	Kind     string `json:"kind,omitempty"` // provider_type, transport, protocol, or tag
	Disabled bool   `json:"disabled,omitempty"`
	Detail   string `json:"detail,omitempty"` // route path prefix, vk description, etc.
	Exists   bool   `json:"exists"`
}

func (h *Handler) handleGetAgentResources(w http.ResponseWriter, r *http.Request) {
	a, ok := h.getAgentOr404(w, r)
	if !ok {
		return
	}
	_ = httpjson.Write(w, http.StatusOK, agentResourcesView{
		Resources: a.Resources,
		Routes:    a.Routes,
		Resolved:  h.resolveAgentResources(r.Context(), a),
	})
}

func (h *Handler) resolveAgentResources(ctx context.Context, a agentpkg.Agent) *agentResolvedResources {
	out := &agentResolvedResources{
		Providers:   []resourceRef{},
		MCPServices: []resourceRef{},
		VirtualKeys: []resourceRef{},
		LLMRoutes:   []resourceRef{},
		MCPRoutes:   []resourceRef{},
		ACPRoutes:   []resourceRef{},
	}
	for _, id := range a.Resources.ProviderIDs {
		ref := resourceRef{ID: id}
		if h.providerManager != nil {
			if cfg, err := h.providerManager.GetConfig(ctx, id); err == nil {
				ref.Exists = true
				ref.Kind = cfg.ProviderType
				ref.Disabled = cfg.Disabled
			}
		}
		out.Providers = append(out.Providers, ref)
	}
	for _, id := range a.Resources.MCPServiceIDs {
		ref := resourceRef{ID: id}
		if h.sharedMCPServiceManager != nil {
			if cfg, err := h.sharedMCPServiceManager.Get(ctx, id); err == nil {
				ref.Exists = true
				ref.Kind = string(cfg.Transport)
				ref.Disabled = cfg.Disabled
				ref.Detail = cfg.Name
			}
		}
		out.MCPServices = append(out.MCPServices, ref)
	}
	for _, id := range a.Resources.VirtualKeyIDs {
		ref := resourceRef{ID: id}
		if h.virtualKeyManager != nil {
			if vk, err := h.virtualKeyManager.GetByID(ctx, id); err == nil {
				ref.Exists = true
				ref.Kind = vk.Tag
				ref.Disabled = vk.Disabled
				ref.Detail = vk.Description
			}
		}
		out.VirtualKeys = append(out.VirtualKeys, ref)
	}
	for _, id := range a.Routes.LLMRouteIDs {
		ref := resourceRef{ID: id}
		if h.sharedLLMRouteResolver != nil {
			cfg, err := h.sharedLLMRouteResolver.GetConfig(ctx, id)
			ref = routeRefFromConfig(id, cfg, err)
		}
		out.LLMRoutes = append(out.LLMRoutes, ref)
	}
	for _, id := range a.Routes.MCPRouteIDs {
		ref := resourceRef{ID: id}
		if h.sharedMCPRouteResolver != nil {
			cfg, err := h.sharedMCPRouteResolver.GetConfig(ctx, id)
			ref = routeRefFromConfig(id, cfg, err)
		}
		out.MCPRoutes = append(out.MCPRoutes, ref)
	}
	for _, id := range a.Routes.ACPRouteIDs {
		ref := resourceRef{ID: id}
		if h.sharedACPRouteResolver != nil {
			cfg, err := h.sharedACPRouteResolver.GetConfig(ctx, id)
			ref = routeRefFromConfig(id, cfg, err)
		}
		out.ACPRoutes = append(out.ACPRoutes, ref)
	}
	return out
}

// routeRefFromConfig builds a resource ref from a route resolver GetConfig
// result. A non-nil error (typically not-found) leaves exists=false so a
// dangling route reference is visible.
func routeRefFromConfig(id string, cfg routecore.AgentRouteConfig, err error) resourceRef {
	ref := resourceRef{ID: id}
	if err != nil {
		return ref
	}
	ref.Exists = true
	ref.Kind = string(cfg.Protocol)
	ref.Disabled = cfg.Disabled
	ref.Detail = cfg.MatchPolicy.PathPrefix
	return ref
}

// handleUpdateAgentResources updates only the agent's resource/route management
// view, leaving runtime and policy untouched.
func (h *Handler) handleUpdateAgentResources(w http.ResponseWriter, r *http.Request) {
	manager, err := h.agentManagerOrError()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	a, ok := h.getAgentOr404(w, r)
	if !ok {
		return
	}
	var req agentResourcesView
	if err := httpjson.Decode(r, &req); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	a.Resources = req.Resources
	a.Routes = req.Routes
	if err := manager.Update(r.Context(), a.ID, a); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := manager.Get(r.Context(), a.ID)
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, agentResourcesView{
		Resources: updated.Resources,
		Routes:    updated.Routes,
		Resolved:  h.resolveAgentResources(r.Context(), updated),
	})
}

// handleGetAgentHealth returns a shallow health summary: disabled state, runtime
// counts, pending permissions, a recent error rate, and pipeline health.
func (h *Handler) handleGetAgentHealth(w http.ResponseWriter, r *http.Request) {
	a, ok := h.getAgentOr404(w, r)
	if !ok {
		return
	}
	health := map[string]any{
		"agent_id": a.ID,
		"disabled": a.Disabled,
		"runtime":  a.Runtime.Type,
	}
	if serviceID := a.ACPServiceID(); serviceID != "" {
		if summary := h.acpRuntimeSummaryForService(serviceID); summary != nil {
			health["pooled_instances"] = len(summary.PooledInstances)
			health["in_flight_turns"] = summary.InFlightTurns
			health["pending_permissions"] = len(summary.PendingPermissions)
		}
	}
	if h.usageQuery != nil {
		opts := usage.EventListOptions{Limit: 200, Attribution: agentAttributionFilter(a)}
		if resp, err := h.usageQuery.ListInteractions(opts); err == nil {
			total := len(resp.Items)
			failures := 0
			for _, item := range resp.Items {
				if !successFromAny(item["success"]) {
					failures++
				}
			}
			health["recent_window"] = total
			health["recent_failures"] = failures
		}
	}
	if h.usageStats != nil {
		health["pipeline"] = map[string]uint64{
			"dropped_events": h.usageStats.DroppedEvents(),
			"write_failures": h.usageStats.WriteFailures(),
		}
	}
	_ = httpjson.Write(w, http.StatusOK, health)
}

func intFromAny(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

func successFromAny(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case int:
		return x != 0
	case float64:
		return x != 0
	default:
		return false
	}
}

var _ = acpservice.ErrServiceNotConfigured
