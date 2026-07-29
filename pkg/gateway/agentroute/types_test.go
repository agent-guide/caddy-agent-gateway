package agentroute

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/configstore"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
)

func TestNormalizeGeneratesDeterministicID(t *testing.T) {
	cases := []struct {
		name       string
		agentID    string
		pathPrefix string
		wantID     string
	}{
		{"path prefix slugified", "reviewer", "/agents/reviewer", "agent:reviewer:agents-reviewer"},
		{"root path", "reviewer", "/", "agent:reviewer:root"},
		{"empty path", "reviewer", "", "agent:reviewer:root"},
		{"mixed case collapses", "worker", "/Agents/V1//run", "agent:worker:agents-v1-run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := AgentRouteConfig{
				AgentRouteBaseConfig: AgentRouteBaseConfig{MatchPolicy: RouteMatch{PathPrefix: tc.pathPrefix}},
				AgentID:              tc.agentID,
			}
			cfg.Normalize()
			if cfg.ID != tc.wantID {
				t.Fatalf("generated id = %q, want %q", cfg.ID, tc.wantID)
			}
			if cfg.Kind != routecore.RouteKindAgent || cfg.Protocol != routecore.RouteProtocolAgent {
				t.Fatalf("normalized kind/protocol = %q/%q", cfg.Kind, cfg.Protocol)
			}
		})
	}
}

func TestNormalizeKeepsExplicitIDAndTrims(t *testing.T) {
	cfg := AgentRouteConfig{
		AgentRouteBaseConfig: AgentRouteBaseConfig{
			ID:          "  my-route  ",
			Kind:        routecore.RouteKindLLM,
			Protocol:    routecore.RouteProtocolOpenAI,
			MatchPolicy: RouteMatch{PathPrefix: " /agents/x ", Host: " example.com "},
		},
		AgentID: "  worker  ",
	}
	cfg.Normalize()
	if cfg.ID != "my-route" || cfg.AgentID != "worker" {
		t.Fatalf("normalized id/agent = %q/%q", cfg.ID, cfg.AgentID)
	}
	// Foreign kind/protocol values are forced back to the agent family.
	if cfg.Kind != routecore.RouteKindAgent || cfg.Protocol != routecore.RouteProtocolAgent {
		t.Fatalf("normalized kind/protocol = %q/%q", cfg.Kind, cfg.Protocol)
	}
	if cfg.MatchPolicy.PathPrefix != "/agents/x" || cfg.MatchPolicy.Host != "example.com" {
		t.Fatalf("normalized match = %+v", cfg.MatchPolicy)
	}
}

func TestConfigRoundTripThroughPersistedShape(t *testing.T) {
	cfg := AgentRouteConfig{
		AgentRouteBaseConfig: AgentRouteBaseConfig{
			Description: "reviewer ingress",
			MatchPolicy: RouteMatch{PathPrefix: "/agents/reviewer", Methods: []string{"POST", "GET"}},
			AuthPolicy:  RouteAuthPolicy{RequireVirtualKey: true},
		},
		AgentID: "reviewer",
	}
	cfg.Normalize()
	stored, err := cfg.ToConfig()
	if err != nil {
		t.Fatalf("ToConfig: %v", err)
	}
	if stored.Kind != routecore.RouteKindAgent || stored.Protocol != routecore.RouteProtocolAgent {
		t.Fatalf("stored kind/protocol = %q/%q", stored.Kind, stored.Protocol)
	}
	var target struct {
		Kind    routecore.RouteTargetPolicyKind `json:"kind"`
		AgentID string                          `json:"agent_id"`
	}
	if err := json.Unmarshal(stored.TargetPolicy, &target); err != nil {
		t.Fatalf("decode target policy: %v", err)
	}
	if target.Kind != routecore.RouteTargetPolicyKindAgent || target.AgentID != "reviewer" {
		t.Fatalf("target policy = %+v", target)
	}

	decoded, err := NewAgentRouteConfigFromConfig(stored)
	if err != nil {
		t.Fatalf("NewAgentRouteConfigFromConfig: %v", err)
	}
	if decoded.ID != cfg.ID || decoded.AgentID != "reviewer" || decoded.Description != "reviewer ingress" {
		t.Fatalf("round trip = %+v, want %+v", decoded, cfg)
	}
	if !decoded.AuthPolicy.RequireVirtualKey {
		t.Fatal("round trip lost auth policy")
	}

	route, err := NewAgentRouteFromConfig(stored)
	if err != nil {
		t.Fatalf("NewAgentRouteFromConfig: %v", err)
	}
	if route.AgentID != "reviewer" || route.ID != cfg.ID {
		t.Fatalf("runtime route = %+v", route)
	}

	// JSON round trip through the expanded admin shape.
	raw, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fromJSON AgentRouteConfig
	if err := json.Unmarshal(raw, &fromJSON); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fromJSON.ID != cfg.ID || fromJSON.AgentID != "reviewer" {
		t.Fatalf("json round trip = %+v", fromJSON)
	}
}

func TestDecodeStoredAgentRoute(t *testing.T) {
	stored := AgentRouteBaseConfig{
		ID:           "agent:reviewer:root",
		Kind:         routecore.RouteKindAgent,
		Protocol:     routecore.RouteProtocolAgent,
		TargetPolicy: json.RawMessage(`{"kind":"agent","agent_id":"reviewer"}`),
	}
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded, err := DecodeStoredAgentRoute(data)
	if err != nil {
		t.Fatalf("DecodeStoredAgentRoute: %v", err)
	}
	cfg, ok := decoded.(*AgentRouteConfig)
	if !ok || cfg.AgentID != "reviewer" || cfg.ID != "agent:reviewer:root" {
		t.Fatalf("decoded = %#v", decoded)
	}
}

func TestDecodeTargetAgentIDRejectsForeignTargetKind(t *testing.T) {
	// A foreign target-policy kind on a kind=agent route fails closed instead
	// of silently lending its agent_id to attribution or target validation.
	if _, err := DecodeTargetAgentID(json.RawMessage(`{"kind":"mcp-service","agent_id":"reviewer"}`)); err == nil {
		t.Fatal("foreign target policy kind was accepted")
	}
	// The canonical shape and a kind-less policy still decode.
	for _, raw := range []string{
		`{"kind":"agent","agent_id":"reviewer"}`,
		`{"agent_id":"reviewer"}`,
	} {
		agentID, err := DecodeTargetAgentID(json.RawMessage(raw))
		if err != nil || agentID != "reviewer" {
			t.Fatalf("DecodeTargetAgentID(%s) = %q, %v", raw, agentID, err)
		}
	}
	stored := AgentRouteBaseConfig{
		ID:           "agent:reviewer:root",
		Kind:         routecore.RouteKindAgent,
		Protocol:     routecore.RouteProtocolAgent,
		TargetPolicy: json.RawMessage(`{"kind":"logical-model","agent_id":"reviewer"}`),
	}
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := DecodeStoredAgentRoute(data); err == nil {
		t.Fatal("stored route with a foreign target kind was decoded")
	}
}

// TestMatcherPriorityAcrossRouteFamilies proves the shared matcher treats
// kind=agent like every other family: longest path prefix wins regardless of
// kind, and same-prefix collisions surface deterministically by score.
func TestMatcherPriorityAcrossRouteFamilies(t *testing.T) {
	mk := func(id string, kind routecore.RouteKind, protocol routecore.RouteProtocol, prefix string) routecore.AgentRouteConfig {
		return routecore.AgentRouteConfig{ID: id, Kind: kind, Protocol: protocol, MatchPolicy: routecore.RouteMatchPolicy{PathPrefix: prefix}}
	}
	routes := []routecore.AgentRouteConfig{
		mk("llm", routecore.RouteKindLLM, routecore.RouteProtocolOpenAI, "/v1"),
		mk("mcp", routecore.RouteKindMCP, routecore.RouteProtocolMCP, "/mcp"),
		mk("agent", routecore.RouteKindAgent, routecore.RouteProtocolAgent, "/agents/reviewer"),
	}
	cases := []struct {
		path string
		want string
	}{
		{"/agents/reviewer/turn", "agent"},
		{"/v1/chat/completions", "llm"},
		{"/mcp/tools", "mcp"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("POST", tc.path, nil)
		matched, ok := routecore.MatchRouteConfigs(routes, r)
		if !ok || matched.ID != tc.want {
			t.Fatalf("match %q = %q (ok=%v), want %q", tc.path, matched.ID, ok, tc.want)
		}
	}
}

type staticAgentLookup map[string]bool

func (l staticAgentLookup) HasAgent(id string) bool { return l[id] }

// memoryRouteStore is a minimal in-memory route ConfigStore for resolver tests.
type memoryRouteStore struct {
	items map[string]routecore.AgentRouteConfig
}

func newMemoryRouteStore() *memoryRouteStore {
	return &memoryRouteStore{items: map[string]routecore.AgentRouteConfig{}}
}

func (s *memoryRouteStore) unwrap(obj any) (routecore.AgentRouteConfig, error) {
	if u, ok := obj.(configstore.ObjectUnwrapper); ok {
		obj = u.ConfigStoreObject()
	}
	switch cfg := obj.(type) {
	case *routecore.AgentRouteConfig:
		return *cfg, nil
	case routecore.AgentRouteConfig:
		return cfg, nil
	default:
		return routecore.AgentRouteConfig{}, fmt.Errorf("unexpected route object %T", obj)
	}
}

func (s *memoryRouteStore) Create(_ context.Context, obj any) error {
	cfg, err := s.unwrap(obj)
	if err != nil {
		return err
	}
	if _, exists := s.items[cfg.ID]; exists {
		return fmt.Errorf("route %q already exists", cfg.ID)
	}
	s.items[cfg.ID] = cfg
	return nil
}

func (s *memoryRouteStore) Update(_ context.Context, obj any) error {
	cfg, err := s.unwrap(obj)
	if err != nil {
		return err
	}
	s.items[cfg.ID] = cfg
	return nil
}

func (s *memoryRouteStore) Delete(_ context.Context, keyParts ...any) error {
	delete(s.items, fmt.Sprint(keyParts[0]))
	return nil
}

func (s *memoryRouteStore) Get(_ context.Context, keyParts ...any) (any, error) {
	cfg, ok := s.items[fmt.Sprint(keyParts[0])]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	out := cfg
	return &out, nil
}

func (s *memoryRouteStore) List(_ context.Context) ([]any, error) {
	out := make([]any, 0, len(s.items))
	for _, cfg := range s.items {
		cloned := cfg
		out = append(out, &cloned)
	}
	return out, nil
}

func (s *memoryRouteStore) ListByTag(ctx context.Context, _ string) ([]any, error) {
	return s.List(ctx)
}

func (s *memoryRouteStore) ListByTagPrefix(ctx context.Context, _ string) ([]any, error) {
	return s.List(ctx)
}

func (s *memoryRouteStore) GetByIndex(context.Context, string, any) (any, error) {
	return nil, configstore.ErrNotFound
}

func TestResolverValidatesTarget(t *testing.T) {
	ctx := context.Background()
	manager := routecore.NewAgentRouteConfigManager(newMemoryRouteStore())
	manager.InitStaticRoutes(nil)
	resolver := NewAgentRouteResolver(manager)
	resolver.SetAgentLookup(staticAgentLookup{"reviewer": true, "disabled-agent": true})

	build := func(agentID, prefix string) routecore.AgentRouteConfig {
		cfg := AgentRouteConfig{
			AgentRouteBaseConfig: AgentRouteBaseConfig{MatchPolicy: RouteMatch{PathPrefix: prefix}},
			AgentID:              agentID,
		}
		cfg.Normalize()
		stored, err := cfg.ToConfig()
		if err != nil {
			t.Fatalf("ToConfig: %v", err)
		}
		return stored
	}

	// Missing agent_id is rejected.
	empty := build("", "/agents/none")
	empty.ID = "agent:none:root"
	if err := resolver.CreateConfig(ctx, empty, "test"); err == nil {
		t.Fatal("CreateConfig accepted an empty agent_id target")
	}
	// Unknown target agent is rejected.
	if err := resolver.CreateConfig(ctx, build("ghost", "/agents/ghost"), "test"); err == nil {
		t.Fatal("CreateConfig accepted an unknown target agent")
	}
	// Existing (possibly disabled or non-executable) targets are valid:
	// management validity is separate from execution availability.
	if err := resolver.CreateConfig(ctx, build("disabled-agent", "/agents/disabled"), "test"); err != nil {
		t.Fatalf("CreateConfig(disabled target): %v", err)
	}
	if err := resolver.CreateConfig(ctx, build("reviewer", "/agents/reviewer"), "test"); err != nil {
		t.Fatalf("CreateConfig(reviewer): %v", err)
	}
	// Updates re-validate the target.
	if err := resolver.UpdateConfig(ctx, "agent:reviewer:agents-reviewer", build("ghost", "/agents/reviewer")); err == nil {
		t.Fatal("UpdateConfig accepted an unknown target agent")
	}

	route, err := resolver.ResolveByID(ctx, "agent:reviewer:agents-reviewer")
	if err != nil {
		t.Fatalf("ResolveByID: %v", err)
	}
	if route == nil || route.AgentID != "reviewer" {
		t.Fatalf("resolved route = %+v", route)
	}
}
