package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	"github.com/agent-guide/agent-gateway/pkg/gateway/agentroute"
)

type AgentRouteView struct {
	agentroute.AgentRouteConfig
	Source   string `json:"source"`
	ReadOnly bool   `json:"read_only"`
}

func (v AgentRouteView) MarshalJSON() ([]byte, error) {
	return marshalRouteView(v.AgentRouteConfig, v.Source, v.ReadOnly)
}
func (v *AgentRouteView) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &v.AgentRouteConfig); err != nil {
		return err
	}
	return unmarshalRouteViewExtras(data, &v.Source, &v.ReadOnly)
}

func (h *Handler) handleListAgentRoutes(w http.ResponseWriter, r *http.Request) {
	resolver, err := h.agentRouteResolver()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	items, err := resolver.ListConfigs(r.Context(), agentroute.RouteListOptions{})
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	items = filterRouteConfigsByKind(items, agentroute.RouteKindAgent)
	views := make([]AgentRouteView, 0, len(items))
	for _, item := range items {
		views = append(views, agentRouteViewFromConfig(resolver, item))
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"items": views})
}

func (h *Handler) handleCreateAgentRoute(w http.ResponseWriter, r *http.Request) {
	resolver, err := h.agentRouteResolver()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	var route agentroute.AgentRouteConfig
	if err := httpjson.Decode(r, &route); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	if !route.CreatedAt.IsZero() || !route.UpdatedAt.IsZero() {
		_ = httpjson.Error(w, http.StatusBadRequest, "created_at and updated_at are managed by the server and must be omitted")
		return
	}
	route.Normalize()
	cfg, err := route.ToConfig()
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := resolver.CreateConfig(r.Context(), cfg, ""); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := resolver.GetConfig(r.Context(), route.ID)
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusCreated, agentRouteViewFromConfig(resolver, created))
}

func (h *Handler) handleGetAgentRoute(w http.ResponseWriter, r *http.Request) {
	resolver, err := h.agentRouteResolver()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	item, err := resolver.GetConfig(r.Context(), r.PathValue("id"))
	if err != nil || item.Kind != agentroute.RouteKindAgent {
		_ = httpjson.Error(w, http.StatusNotFound, "agent route not found")
		return
	}
	_ = httpjson.Write(w, http.StatusOK, agentRouteViewFromConfig(resolver, item))
}

func (h *Handler) handleUpdateAgentRoute(w http.ResponseWriter, r *http.Request) {
	resolver, err := h.agentRouteResolver()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	id := r.PathValue("id")
	current, err := resolver.GetConfig(r.Context(), id)
	if err != nil || current.Kind != agentroute.RouteKindAgent {
		_ = httpjson.Error(w, http.StatusNotFound, "agent route not found")
		return
	}
	var route agentroute.AgentRouteConfig
	if err := httpjson.Decode(r, &route); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	if route.ID == "" {
		route.ID = id
	}
	if route.ID != id {
		_ = httpjson.Error(w, http.StatusBadRequest, "route id in body must match path")
		return
	}
	if !route.CreatedAt.IsZero() || !route.UpdatedAt.IsZero() {
		_ = httpjson.Error(w, http.StatusBadRequest, "created_at and updated_at are managed by the server and must be omitted")
		return
	}
	route.Normalize()
	cfg, err := route.ToConfig()
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := resolver.UpdateConfig(r.Context(), id, cfg); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			_ = httpjson.Error(w, http.StatusNotFound, "agent route not found")
			return
		}
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	item, _ := resolver.GetConfig(r.Context(), id)
	_ = httpjson.Write(w, http.StatusOK, agentRouteViewFromConfig(resolver, item))
}

func (h *Handler) handleDeleteAgentRoute(w http.ResponseWriter, r *http.Request) {
	resolver, err := h.agentRouteResolver()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	id := r.PathValue("id")
	item, err := resolver.GetConfig(r.Context(), id)
	if err != nil || item.Kind != agentroute.RouteKindAgent {
		_ = httpjson.Error(w, http.StatusNotFound, "agent route not found")
		return
	}
	if err := resolver.DeleteConfig(r.Context(), id); err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func agentRouteViewFromConfig(resolver *agentroute.AgentRouteResolver, cfg agentroute.AgentRouteBaseConfig) AgentRouteView {
	item, _ := agentroute.NewAgentRouteConfigFromConfig(cfg)
	view := AgentRouteView{AgentRouteConfig: item, Source: "store"}
	if resolver != nil && resolver.ConfigManager() != nil && resolver.ConfigManager().IsStatic(cfg.ID) {
		view.Source, view.ReadOnly = "caddyfile", true
	}
	return view
}
