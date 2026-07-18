package builtinroute

import (
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
)

func TestBuiltinRouteConfigRoundTrip(t *testing.T) {
	cfg := BuiltinRouteConfig{
		AgentRouteConfig: AgentRouteConfig{
			MatchPolicy: RouteMatch{PathPrefix: "/agents/triage"},
		},
		AgentID: "triage",
	}
	cfg.Normalize()
	if cfg.ID == "" {
		t.Fatal("Normalize() did not auto-generate the route id")
	}
	if cfg.Kind != routecore.RouteKindBuiltin || cfg.Protocol != routecore.RouteProtocolBuiltin {
		t.Fatalf("kind/protocol = %s/%s, want builtin/builtin forced", cfg.Kind, cfg.Protocol)
	}
	if err := routecore.ValidateRouteID(cfg.ID); err != nil {
		t.Fatalf("generated id %q is invalid: %v", cfg.ID, err)
	}

	stored, err := cfg.ToConfig()
	if err != nil {
		t.Fatalf("ToConfig() error = %v", err)
	}
	route, err := NewBuiltinRouteFromConfig(stored)
	if err != nil {
		t.Fatalf("NewBuiltinRouteFromConfig() error = %v", err)
	}
	if route.AgentID != "triage" {
		t.Fatalf("agent id = %q, want triage preserved through the target policy", route.AgentID)
	}
	if route.ID != cfg.ID {
		t.Fatalf("route id = %q, want %q", route.ID, cfg.ID)
	}

	agentID, err := DecodeTargetAgentID(stored.TargetPolicy)
	if err != nil || agentID != "triage" {
		t.Fatalf("DecodeTargetAgentID() = %q, %v; want triage", agentID, err)
	}
}

func TestGeneratedIDIsDeterministic(t *testing.T) {
	first := BuiltinRouteConfig{AgentRouteConfig: AgentRouteConfig{MatchPolicy: RouteMatch{PathPrefix: "/A/B"}}, AgentID: "bot"}
	second := BuiltinRouteConfig{AgentRouteConfig: AgentRouteConfig{MatchPolicy: RouteMatch{PathPrefix: "/A/B"}}, AgentID: "bot"}
	first.Normalize()
	second.Normalize()
	if first.ID != second.ID {
		t.Fatalf("ids differ: %q vs %q, want deterministic generation", first.ID, second.ID)
	}
}
