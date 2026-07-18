package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	builtinroute "github.com/agent-guide/agent-gateway/pkg/gateway/builtinroute"
)

type BuiltinRouteView struct {
	builtinroute.BuiltinRouteConfig
	Source   string `json:"source"`
	ReadOnly bool   `json:"read_only"`
}

// MarshalJSON merges the view fields into the embedded config JSON. Without
// this the embedded BuiltinRouteConfig.MarshalJSON is promoted and silently
// drops source and read_only from admin responses.
func (v BuiltinRouteView) MarshalJSON() ([]byte, error) {
	return marshalRouteView(v.BuiltinRouteConfig, v.Source, v.ReadOnly)
}

func (v *BuiltinRouteView) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &v.BuiltinRouteConfig); err != nil {
		return err
	}
	return unmarshalRouteViewExtras(data, &v.Source, &v.ReadOnly)
}

// handleGetBuiltinRuntime reports the ADK host runtime view: per-agent
// materialization state plus pending interactive tool permissions (§5.7.7).
// The pending list is read-only here; decisions flow through the data-plane
// resume on POST /<builtin-route>/turn.
func (h *Handler) handleGetBuiltinRuntime(w http.ResponseWriter, _ *http.Request) {
	if h.builtinHost == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "builtin host is not configured")
		return
	}
	_ = httpjson.Write(w, http.StatusOK, h.builtinHost.Runtime())
}

func (h *Handler) handleListBuiltinRoutes(w http.ResponseWriter, r *http.Request) {
	resolver, err := h.builtinRouteResolver()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	items, err := resolver.ListConfigs(r.Context(), builtinroute.RouteListOptions{})
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	items = filterRouteConfigsByKind(items, builtinroute.RouteKindBuiltin)
	views := make([]BuiltinRouteView, 0, len(items))
	for _, item := range items {
		views = append(views, builtinRouteViewFromConfig(resolver, item))
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"items": views})
}

func (h *Handler) handleCreateBuiltinRoute(w http.ResponseWriter, r *http.Request) {
	resolver, err := h.builtinRouteResolver()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	var route builtinroute.BuiltinRouteConfig
	if err := httpjson.Decode(r, &route); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	if !route.CreatedAt.IsZero() || !route.UpdatedAt.IsZero() {
		_ = httpjson.Error(w, http.StatusBadRequest, "created_at and updated_at are managed by the server and must be omitted")
		return
	}
	route.Normalize()
	if route.AgentID == "" {
		_ = httpjson.Error(w, http.StatusBadRequest, "agent_id is required")
		return
	}
	cfg, err := route.ToConfig()
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := resolver.CreateConfig(r.Context(), cfg, ""); err != nil {
		if errors.Is(err, builtinroute.ErrInvalidRouteID) {
			_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	created, err := resolver.GetConfig(r.Context(), route.ID)
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusCreated, builtinRouteViewFromConfig(resolver, created))
}

func (h *Handler) handleGetBuiltinRoute(w http.ResponseWriter, r *http.Request) {
	resolver, err := h.builtinRouteResolver()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	item, err := resolver.GetConfig(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, builtinroute.ErrRouteNotConfigured) {
			_ = httpjson.Error(w, http.StatusNotFound, "builtin route not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if item.Kind != builtinroute.RouteKindBuiltin {
		_ = httpjson.Error(w, http.StatusNotFound, "builtin route not found")
		return
	}
	_ = httpjson.Write(w, http.StatusOK, builtinRouteViewFromConfig(resolver, item))
}

func (h *Handler) handleUpdateBuiltinRoute(w http.ResponseWriter, r *http.Request) {
	resolver, err := h.builtinRouteResolver()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	id := r.PathValue("id")
	current, err := resolver.GetConfig(r.Context(), id)
	if err != nil {
		if errors.Is(err, builtinroute.ErrRouteNotConfigured) {
			_ = httpjson.Error(w, http.StatusNotFound, "builtin route not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if current.Kind != builtinroute.RouteKindBuiltin {
		_ = httpjson.Error(w, http.StatusNotFound, "builtin route not found")
		return
	}
	var route builtinroute.BuiltinRouteConfig
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
	if route.AgentID == "" {
		_ = httpjson.Error(w, http.StatusBadRequest, "agent_id is required")
		return
	}
	cfg, err := route.ToConfig()
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := resolver.UpdateConfig(r.Context(), id, cfg); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			_ = httpjson.Error(w, http.StatusNotFound, "builtin route not found")
			return
		}
		if errors.Is(err, builtinroute.ErrInvalidRouteID) {
			_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	item, err := resolver.GetConfig(r.Context(), id)
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, builtinRouteViewFromConfig(resolver, item))
}

func (h *Handler) handleDeleteBuiltinRoute(w http.ResponseWriter, r *http.Request) {
	resolver, err := h.builtinRouteResolver()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	id := r.PathValue("id")
	item, err := resolver.GetConfig(r.Context(), id)
	if err != nil {
		if errors.Is(err, builtinroute.ErrRouteNotConfigured) {
			_ = httpjson.Error(w, http.StatusNotFound, "builtin route not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if item.Kind != builtinroute.RouteKindBuiltin {
		_ = httpjson.Error(w, http.StatusNotFound, "builtin route not found")
		return
	}
	if err := resolver.DeleteConfig(r.Context(), id); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			_ = httpjson.Error(w, http.StatusNotFound, "builtin route not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func builtinRouteViewFromConfig(resolver *builtinroute.BuiltinRouteResolver, cfg builtinroute.AgentRouteConfig) BuiltinRouteView {
	item, _ := builtinroute.NewBuiltinRouteConfigFromConfig(cfg)
	view := BuiltinRouteView{
		BuiltinRouteConfig: item,
		Source:             "store",
		ReadOnly:           false,
	}
	if resolver != nil {
		if configManager := resolver.ConfigManager(); configManager != nil && configManager.IsStatic(cfg.ID) {
			view.Source = "caddyfile"
			view.ReadOnly = true
		}
	}
	return view
}
