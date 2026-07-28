package dispatcher

import (
	"strings"
	"testing"

	_ "github.com/agent-guide/agent-gateway/caddy/dispatcher/llmapi/anthropic"
	_ "github.com/agent-guide/agent-gateway/caddy/dispatcher/llmapi/cc"
	_ "github.com/agent-guide/agent-gateway/caddy/dispatcher/llmapi/openai"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	caddyfileadapter "github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
)

func TestParseAgentRouteDispatcher(t *testing.T) {
	d := caddyfile.NewTestDispenser(`
	agent_route_dispatcher {
		llm_api openai
		llm_api anthropic
		llm_api cc
		mcp
		agent
	}
	`)

	handler, err := parseAgentRouteDispatcher(httpcaddyfile.Helper{Dispenser: d})
	if err != nil {
		t.Fatalf("parseAgentRouteDispatcher() error = %v", err)
	}

	dispatcherHandler, ok := handler.(*AgentRouteDispatcher)
	if !ok {
		t.Fatalf("handler type = %T, want *AgentRouteDispatcher", handler)
	}
	if len(dispatcherHandler.APIHandlersRaw) != 3 {
		t.Fatalf("api handler count = %d, want 3", len(dispatcherHandler.APIHandlersRaw))
	}
	if _, ok := dispatcherHandler.APIHandlersRaw["openai"]; !ok {
		t.Fatal("missing openai api handler")
	}
	if _, ok := dispatcherHandler.APIHandlersRaw["anthropic"]; !ok {
		t.Fatal("missing anthropic api handler")
	}
	if _, ok := dispatcherHandler.APIHandlersRaw["cc"]; !ok {
		t.Fatal("missing cc api handler")
	}
	if !dispatcherHandler.EnableMCP {
		t.Fatal("expected mcp to be enabled")
	}
	if !dispatcherHandler.EnableAgent {
		t.Fatal("expected agent to be enabled")
	}
}

func TestAgentRouteDispatcherAdaptUsesHandlerType(t *testing.T) {
	input := []byte(`
		:8080 {
			agent_route_dispatcher {
				llm_api openai
				llm_api anthropic
				llm_api cc
				mcp
				agent
			}
		}
	`)

	adapter := caddyfileadapter.Adapter{ServerType: httpcaddyfile.ServerType{}}
	adapted, _, err := adapter.Adapt(input, nil)
	if err != nil {
		t.Fatalf("caddy.Adapt() error = %v", err)
	}

	json := string(adapted)
	if !strings.Contains(json, `"handler":"agent_route_dispatcher"`) {
		t.Fatalf("adapted config missing agent_route_dispatcher handler: %s", json)
	}
	if !strings.Contains(json, `"api_handlers":{"anthropic":{}`) || !strings.Contains(json, `"cc":{}`) || !strings.Contains(json, `"openai":{}`) {
		t.Fatalf("adapted config missing dispatcher api handlers: %s", json)
	}
	if !strings.Contains(json, `"mcp":true`) {
		t.Fatalf("adapted config missing mcp flag: %s", json)
	}
	if !strings.Contains(json, `"agent":true`) {
		t.Fatalf("adapted config missing agent flag: %s", json)
	}
}

func TestParseAgentRouteDispatcherAllowsMCPOnly(t *testing.T) {
	d := caddyfile.NewTestDispenser(`
	agent_route_dispatcher {
		mcp
	}
	`)

	handler, err := parseAgentRouteDispatcher(httpcaddyfile.Helper{Dispenser: d})
	if err != nil {
		t.Fatalf("parseAgentRouteDispatcher() error = %v", err)
	}

	dispatcherHandler, ok := handler.(*AgentRouteDispatcher)
	if !ok {
		t.Fatalf("handler type = %T, want *AgentRouteDispatcher", handler)
	}
	if !dispatcherHandler.EnableMCP {
		t.Fatal("expected mcp to be enabled")
	}
	if len(dispatcherHandler.APIHandlersRaw) != 0 {
		t.Fatalf("api handler count = %d, want 0", len(dispatcherHandler.APIHandlersRaw))
	}
}

func TestParseAgentRouteDispatcherAllowsAgentOnly(t *testing.T) {
	d := caddyfile.NewTestDispenser(`
	agent_route_dispatcher {
		agent
	}
	`)

	handler, err := parseAgentRouteDispatcher(httpcaddyfile.Helper{Dispenser: d})
	if err != nil {
		t.Fatalf("parseAgentRouteDispatcher() error = %v", err)
	}

	dispatcherHandler, ok := handler.(*AgentRouteDispatcher)
	if !ok {
		t.Fatalf("handler type = %T, want *AgentRouteDispatcher", handler)
	}
	if !dispatcherHandler.EnableAgent {
		t.Fatal("expected agent to be enabled")
	}
	if dispatcherHandler.EnableMCP || len(dispatcherHandler.APIHandlersRaw) != 0 {
		t.Fatalf("unexpected non-agent enablement: %#v", dispatcherHandler)
	}
}

func TestParseAgentRouteDispatcherRejectsLegacyRuntimeDirectives(t *testing.T) {
	for _, directive := range []string{"acp", "builtin"} {
		t.Run(directive, func(t *testing.T) {
			d := caddyfile.NewTestDispenser("agent_route_dispatcher {\n" + directive + "\n}")
			_, err := parseAgentRouteDispatcher(httpcaddyfile.Helper{Dispenser: d})
			if err == nil || !strings.Contains(err.Error(), "unknown subdirective: "+directive) {
				t.Fatalf("parseAgentRouteDispatcher() error = %v, want unknown legacy subdirective", err)
			}
		})
	}
}
