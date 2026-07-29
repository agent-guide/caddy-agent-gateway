package admin

import (
	"net/http"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
)

// handleGetBuiltinRuntime reports host/materialization diagnostics. Logical
// run control is exposed only through /admin/agents/{id}/runs/{run_id}.
func (h *Handler) handleGetBuiltinRuntime(w http.ResponseWriter, _ *http.Request) {
	if h.builtinHost == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "builtin host is not configured")
		return
	}
	_ = httpjson.Write(w, http.StatusOK, h.builtinHost.Runtime())
}

func (h *Handler) handleListBuiltinInFlight(w http.ResponseWriter, _ *http.Request) {
	if h.builtinHost == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "builtin host is not configured")
		return
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"items": h.builtinHost.ListInFlight()})
}
