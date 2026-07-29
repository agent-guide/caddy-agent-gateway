package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	acpruntime "github.com/agent-guide/agent-gateway/pkg/acp/runtime"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	builtinpkg "github.com/agent-guide/agent-gateway/pkg/agent/builtin"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	agentroute "github.com/agent-guide/agent-gateway/pkg/gateway/agentroute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	"go.uber.org/zap"
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
	previous, _ := manager.Get(r.Context(), id)
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
	if h.permissionBroker != nil {
		h.permissionBroker.DrainAgent(runtimeapi.WithPermissionSource(context.Background(), "definition_update"), id)
	}
	h.discardAgentContinuations(previous)
	if h.runRegistry != nil && previous.Runtime.Type != "" && previous.Runtime.Type != updated.Runtime.Type {
		if err := h.runRegistry.CancelAgent(context.Background(), id); err != nil && h.logger != nil {
			h.logger.Error("cancel Agent runs after runtime change", zap.String("agent_id", id), zap.Error(err))
		}
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
	current, getErr := manager.Get(r.Context(), id)
	if err := manager.Delete(r.Context(), id); err != nil {
		if errors.Is(err, agentpkg.ErrAgentNotConfigured) || errors.Is(err, configstore.ErrNotFound) {
			_ = httpjson.Error(w, http.StatusNotFound, "agent not found")
			return
		}
		if errors.Is(err, agentpkg.ErrAgentRouteTarget) {
			_ = httpjson.Error(w, http.StatusConflict, err.Error())
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if h.permissionBroker != nil {
		h.permissionBroker.DrainAgent(runtimeapi.WithPermissionSource(context.Background(), "agent_delete"), id)
	}
	if getErr == nil {
		h.discardAgentContinuations(current)
	}
	if h.runRegistry != nil {
		if err := h.runRegistry.CancelAgent(context.Background(), id); err != nil && h.logger != nil {
			h.logger.Error("cancel Agent runs after deletion", zap.String("agent_id", id), zap.Error(err))
		}
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"status": "deleted", "id": id})
}

func (h *Handler) discardAgentContinuations(a agentpkg.Agent) {
	if h == nil || h.runtimeRegistry == nil || strings.TrimSpace(a.Runtime.Type) == "" {
		return
	}
	backend, err := h.runtimeRegistry.Resolve(a.Runtime.Type)
	if err != nil {
		return
	}
	if discarder, ok := backend.(interface{ DiscardAgentContinuations(string) }); ok {
		discarder.DiscardAgentContinuations(a.ID)
	}
}

// AgentWorkspace is the summary/index read model for the agent detail page. It
// returns summaries, counts, runtime state, and references — never full session
// transcripts. The frontend drills into the linked ACP endpoints for content.
type AgentWorkspace struct {
	Agent          agentpkg.Agent             `json:"agent"`
	RuntimeType    string                     `json:"runtime_type"`
	Runtime        *runtimeapi.RuntimeSummary `json:"runtime,omitempty"`
	RuntimeDetails json.RawMessage            `json:"runtime_details,omitempty"`
	Capabilities   *runtimeapi.Capabilities   `json:"capabilities,omitempty"`
	AgentRoutes    []agentRouteRef            `json:"agent_routes,omitempty"`
	Builtin        *BuiltinWorkspaceView      `json:"builtin,omitempty"`
	RuntimeView    *agentRuntimeSummary       `json:"runtime_view,omitempty"`
	Links          map[string]string          `json:"links,omitempty"`
}

// BuiltinWorkspaceView is the builtin-runtime slice of the agent workspace: a
// condensed definition summary, the ADK host materialization state, and the
// agent's builtin routes.
type BuiltinWorkspaceView struct {
	Definition BuiltinDefinitionSummary `json:"definition"`
	HostState  builtinpkg.EntryState    `json:"host_state"`
}

// BuiltinDefinitionSummary condenses the persisted builtin definition. Limits
// report configured values only; zero means the host default applies.
type BuiltinDefinitionSummary struct {
	LLMRouteID           string   `json:"llm_route_id"`
	Model                string   `json:"model,omitempty"`
	TopologyKind         string   `json:"topology_kind"`
	ToolServiceIDs       []string `json:"tool_service_ids,omitempty"`
	MaxConcurrentTurns   int      `json:"max_concurrent_turns,omitempty"`
	TurnTimeoutSeconds   int      `json:"turn_timeout_seconds,omitempty"`
	SummarizationEnabled bool     `json:"summarization_enabled"`
}

type agentRouteRef struct {
	ID         string `json:"id"`
	PathPrefix string `json:"path_prefix,omitempty"`
	AgentID    string `json:"agent_id"`
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

	ws := AgentWorkspace{Agent: a, RuntimeType: a.Runtime.Type}
	if summary, caps, err := h.agentRuntimeRead(r.Context(), a); err == nil {
		ws.Runtime, ws.Capabilities = summary, caps
		if summary != nil {
			ws.RuntimeDetails = summary.Details
			summary.Details = nil
		}
	}

	ws.AgentRoutes = h.agentRoutesForAgent(r.Context(), a.ID)
	if len(ws.AgentRoutes) > 0 {
		prefix := strings.TrimRight(ws.AgentRoutes[0].PathPrefix, "/")
		ws.Links = map[string]string{
			"turn": prefix + "/turn", "sessions": prefix + "/sessions",
			"transcript": prefix + "/sessions/{session_id}/transcript",
		}
	}
	if a.Runtime.Type == agentpkg.RuntimeTypeACP {
		ws.RuntimeView = h.acpRuntimeSummaryForOwner(a.ID)
		if ws.Links == nil {
			ws.Links = map[string]string{}
		}
		ws.Links["admin_runtime"] = "/admin/acp/runtime"
	}
	if a.Runtime.Type == agentpkg.RuntimeTypeBuiltin {
		h.assembleBuiltinWorkspace(r.Context(), &ws, a)
	}
	_ = httpjson.Write(w, http.StatusOK, ws)
}

// assembleBuiltinWorkspace fills the builtin-runtime slice: the definition
// summary, the ADK host materialization state, and the agent's builtin routes.
func (h *Handler) assembleBuiltinWorkspace(ctx context.Context, ws *AgentWorkspace, a agentpkg.Agent) {
	view := &BuiltinWorkspaceView{HostState: h.builtinHost.State(a.ID)}
	if b := a.Runtime.Builtin; b != nil {
		kind := b.Topology.Kind
		if kind == "" {
			kind = agentpkg.TopologyKindSingle
		}
		view.Definition = BuiltinDefinitionSummary{
			LLMRouteID:   b.Model.LLMRouteID,
			Model:        b.Model.Model,
			TopologyKind: kind,
		}
		for _, sel := range b.Tools {
			view.Definition.ToolServiceIDs = append(view.Definition.ToolServiceIDs, sel.MCPServiceID)
		}
		if b.Limits != nil {
			view.Definition.MaxConcurrentTurns = b.Limits.MaxConcurrentTurns
			view.Definition.TurnTimeoutSeconds = b.Limits.TurnTimeoutSeconds
		}
		if b.Middlewares != nil && b.Middlewares.Summarization != nil {
			view.Definition.SummarizationEnabled = b.Middlewares.Summarization.Enabled
		}
	}
	ws.Builtin = view
}

func (h *Handler) agentRoutesForAgent(ctx context.Context, agentID string) []agentRouteRef {
	if h.sharedAgentRouteResolver == nil {
		return nil
	}
	configs, err := h.sharedAgentRouteResolver.ListConfigs(ctx, agentroute.RouteListOptions{})
	if err != nil {
		return nil
	}
	var refs []agentRouteRef
	for _, cfg := range configs {
		if cfg.Kind != agentroute.RouteKindAgent {
			continue
		}
		route, err := agentroute.NewAgentRouteFromConfig(cfg)
		if err == nil && route.AgentID == agentID {
			refs = append(refs, agentRouteRef{ID: route.ID, PathPrefix: route.MatchPolicy.PathPrefix, AgentID: agentID})
		}
	}
	return refs
}

func (h *Handler) acpRuntimeSummaryForOwner(agentID string) *agentRuntimeSummary {
	if h.acpRuntimeManager == nil {
		return nil
	}
	summary := &agentRuntimeSummary{}
	for _, inst := range h.acpRuntimeManager.ListInstances() {
		if acpruntime.ScopeOwnerID(inst.Scope) == agentID {
			summary.PooledInstances = append(summary.PooledInstances, inst)
		}
	}
	for _, turn := range h.acpRuntimeManager.ListInFlight() {
		if acpruntime.ScopeOwnerID(turn.Scope) == agentID {
			summary.InFlightTurns++
		}
	}
	for _, perm := range h.acpRuntimeManager.ListPendingPermissions() {
		if perm.OwnerID == agentID {
			summary.PendingPermissions = append(summary.PendingPermissions, perm)
		}
	}
	return summary
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
// the durable agent_id tag OR any resource route the agent currently owns. This
// is what lets per-agent reads include untagged-but-mappable events (pre-P1
// history, or events written before a resource route was reassigned to this
// agent) instead of only events stamped at write time.
func agentAttributionFilter(a agentpkg.Agent) *usage.AttributionFilter {
	f := &usage.AttributionFilter{AgentID: a.ID}
	f.RouteIDs = append(f.RouteIDs, a.Routes.LLMRouteIDs...)
	f.RouteIDs = append(f.RouteIDs, a.Routes.MCPRouteIDs...)
	return f
}

// agentAttributionFromRequest resolves an optional `agent_id` query filter into
// a metrics attribution selector (the durable agent_id tag OR the agent's owned
// resource routes, matching the per-agent usage/interactions reads). When no
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
// Agent ingress uses the durable agent_id tag; resource route ids recover
// historical and nested LLM/MCP rows without reviving ACP service identity.
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
	if h.permissionBroker != nil {
		out["pending_permissions"] = h.permissionBroker.List(a.ID)
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
		"agent_id":     a.ID,
		"disabled":     a.Disabled,
		"runtime_type": a.Runtime.Type,
	}
	if summary, caps, err := h.agentRuntimeRead(r.Context(), a); err == nil {
		health["runtime"] = summary
		if len(summary.Details) > 0 {
			health["runtime_details"] = summary.Details
			summary.Details = nil
		}
		health["capabilities"] = caps
	} else {
		health["runtime_error"] = runtimeapi.PublicError(err)
	}
	if !a.Disabled {
		if backend, err := h.agentBackend(a); err == nil {
			if checker, ok := backend.(runtimeapi.HealthChecker); ok {
				if runtimeHealth, checkErr := checker.Health(r.Context(), a); checkErr == nil {
					health["runtime_health"] = runtimeHealth
				} else {
					health["runtime_health_error"] = runtimeapi.PublicError(checkErr)
				}
			}
		}
	}
	if a.Runtime.Type == agentpkg.RuntimeTypeACP {
		if summary := h.acpRuntimeSummaryForOwner(a.ID); summary != nil {
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
