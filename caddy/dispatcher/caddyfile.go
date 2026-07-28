package dispatcher

import (
	"encoding/json"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	httpcaddyfile.RegisterHandlerDirective("agent_route_dispatcher", parseAgentRouteDispatcher)
	httpcaddyfile.RegisterDirectiveOrder("agent_route_dispatcher", httpcaddyfile.Before, "reverse_proxy")
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (h *AgentRouteDispatcher) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	if !d.Next() {
		return d.Err("expected directive name")
	}
	if d.NextArg() {
		return d.ArgErr()
	}

	for d.NextBlock(0) {
		switch d.Val() {
		case "llm_api":
			if !d.NextArg() {
				return d.ArgErr()
			}
			apiName := strings.Trim(d.Val(), "\"`")
			if d.NextArg() {
				return d.ArgErr()
			}
			moduleID := "agent_route_dispatcher.llm_apis." + apiName
			mod, err := caddy.GetModule(moduleID)
			if err != nil {
				return d.Errf("getting module named '%s': %v", moduleID, err)
			}
			if h.APIHandlersRaw == nil {
				h.APIHandlersRaw = make(map[string]json.RawMessage)
			}
			h.APIHandlersRaw[apiName] = caddyconfig.JSON(mod.New(), nil)
		case "mcp":
			if d.NextArg() {
				return d.ArgErr()
			}
			h.EnableMCP = true
		case "agent":
			if d.NextArg() {
				return d.ArgErr()
			}
			h.EnableAgent = true
		default:
			return d.Errf("unknown subdirective: %s", d.Val())
		}
	}
	if len(h.APIHandlersRaw) == 0 && !h.EnableMCP && !h.EnableAgent {
		return d.Err("agent_route_dispatcher requires at least one llm_api, mcp, or agent")
	}
	return nil
}

func parseAgentRouteDispatcher(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	handler := &AgentRouteDispatcher{}
	if err := handler.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return handler, nil
}
