package dispatcher

import (
	"fmt"
	"net/http"

	caddygateway "github.com/agent-guide/agent-gateway/caddy/gateway"
	dispatcherpkg "github.com/agent-guide/agent-gateway/pkg/dispatcher"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	caddy.RegisterModule(AgentRouteDispatcher{})
}

// AgentRouteDispatcher is the Caddy middleware adapter for the runtime dispatcher.
type AgentRouteDispatcher struct {
	APIHandlersRaw caddy.ModuleMap `json:"api_handlers,omitempty" caddy:"namespace=agent_route_dispatcher.llm_apis"`
	EnableMCP      bool            `json:"mcp,omitempty"`
	EnableAgent    bool            `json:"agent,omitempty"`

	handler *dispatcherpkg.Handler
}

func (AgentRouteDispatcher) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.agent_route_dispatcher",
		New: func() caddy.Module { return new(AgentRouteDispatcher) },
	}
}

func (h *AgentRouteDispatcher) Provision(ctx caddy.Context) error {
	app, err := caddygateway.GetApp(ctx)
	if err != nil {
		return fmt.Errorf("agent_route_dispatcher: get agent_gateway app: %w", err)
	}

	modules, err := ctx.LoadModule(h, "APIHandlersRaw")
	if err != nil {
		return fmt.Errorf("agent_route_dispatcher: load api handlers: %w", err)
	}
	loaded, ok := modules.(map[string]any)
	if !ok {
		return fmt.Errorf("agent_route_dispatcher: unexpected api handler module type %T", modules)
	}

	apiHandlers := make(map[string]dispatcherpkg.LLMApiHandler, len(loaded))
	for name, mod := range loaded {
		apiHandler, ok := mod.(dispatcherpkg.LLMApiHandler)
		if !ok {
			return fmt.Errorf("agent_route_dispatcher: api handler %q does not implement dispatcher.LLMApiHandler: %T", name, mod)
		}
		apiHandlers[name] = apiHandler
	}

	h.handler = dispatcherpkg.NewHandler(app.AgentGateway(), apiHandlers, ctx.Logger(h), dispatcherpkg.HandlerOptions{EnableMCP: h.EnableMCP, EnableAgent: h.EnableAgent})
	return nil
}

func (h *AgentRouteDispatcher) Validate() error {
	if h.handler == nil {
		return fmt.Errorf("agent_route_dispatcher is not provisioned")
	}
	return h.handler.Validate()
}

func (h AgentRouteDispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if h.handler == nil {
		return fmt.Errorf("agent gateway is not configured")
	}
	return h.handler.Dispatch(w, r, next)
}

var (
	_ caddy.Module                = (*AgentRouteDispatcher)(nil)
	_ caddy.Provisioner           = (*AgentRouteDispatcher)(nil)
	_ caddy.Validator             = (*AgentRouteDispatcher)(nil)
	_ caddyhttp.MiddlewareHandler = (*AgentRouteDispatcher)(nil)
	_ caddyfile.Unmarshaler       = (*AgentRouteDispatcher)(nil)
)
