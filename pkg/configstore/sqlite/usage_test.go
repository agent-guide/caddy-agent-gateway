package sqlite

import (
	"testing"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/metrics/usage"
)

func TestUsageAgentIDRoundTrip(t *testing.T) {
	backend, err := Open(t.Context(), Config{SQLitePath: t.TempDir() + "/usage.db"}, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	db := backend.UsageDB()
	if err := MigrateUsageTables(db); err != nil {
		t.Fatalf("MigrateUsageTables() error = %v", err)
	}
	now := time.Now().UTC()
	// One event tagged with an agent, one without.
	if err := InsertLLMUsageEvent(db, usage.LLMUsageEvent{
		InteractionEvent: usage.InteractionEvent{EventID: "ag-1", SpanID: "s1", StartedAt: now, FinishedAt: now, RouteID: "r1", RouteKind: "llm", Success: true, StatusCode: 200, AgentID: "coding-agent"},
		LLMAPI:           "openai", ProviderID: "p1",
	}); err != nil {
		t.Fatalf("insert tagged: %v", err)
	}
	if err := InsertLLMUsageEvent(db, usage.LLMUsageEvent{
		InteractionEvent: usage.InteractionEvent{EventID: "ag-2", SpanID: "s2", StartedAt: now, FinishedAt: now, RouteID: "r2", RouteKind: "llm", Success: true, StatusCode: 200},
		LLMAPI:           "openai", ProviderID: "p1",
	}); err != nil {
		t.Fatalf("insert untagged: %v", err)
	}
	q := NewUsageQueries(db)
	events, err := q.ListEvents("llm", usage.EventListOptions{Limit: 10, Filters: map[string]string{"agent_id": "coding-agent"}})
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events.Items) != 1 || events.Items[0]["event_id"] != "ag-1" {
		t.Fatalf("agent filter = %#v, want only ag-1", events.Items)
	}
	interactions, err := q.ListInteractions(usage.EventListOptions{Limit: 10, Filters: map[string]string{"agent_id": "coding-agent"}})
	if err != nil {
		t.Fatalf("ListInteractions() error = %v", err)
	}
	if len(interactions.Items) != 1 || interactions.Items[0]["agent_id"] != "coding-agent" {
		t.Fatalf("interactions agent filter = %#v, want only coding-agent", interactions.Items)
	}
}

func TestAttributionFilterFallback(t *testing.T) {
	backend, err := Open(t.Context(), Config{SQLitePath: t.TempDir() + "/usage.db"}, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	db := backend.UsageDB()
	if err := MigrateUsageTables(db); err != nil {
		t.Fatalf("MigrateUsageTables() error = %v", err)
	}
	now := time.Now().UTC()
	// Tagged event for the agent.
	if err := InsertLLMUsageEvent(db, usage.LLMUsageEvent{
		InteractionEvent: usage.InteractionEvent{EventID: "tagged", SpanID: "s1", StartedAt: now, FinishedAt: now, RouteID: "r-owned", RouteKind: "llm", Success: true, StatusCode: 200, AgentID: "coding-agent"},
		LLMAPI:           "openai", ProviderID: "p1",
	}); err != nil {
		t.Fatalf("insert tagged: %v", err)
	}
	// Untagged event on a route the agent owns (pre-P1 / reassignment fallback).
	if err := InsertLLMUsageEvent(db, usage.LLMUsageEvent{
		InteractionEvent: usage.InteractionEvent{EventID: "untagged-owned", SpanID: "s2", StartedAt: now, FinishedAt: now, RouteID: "r-owned", RouteKind: "llm", Success: true, StatusCode: 200},
		LLMAPI:           "openai", ProviderID: "p1",
	}); err != nil {
		t.Fatalf("insert untagged owned: %v", err)
	}
	// Unrelated event the agent must never see.
	if err := InsertLLMUsageEvent(db, usage.LLMUsageEvent{
		InteractionEvent: usage.InteractionEvent{EventID: "foreign", SpanID: "s3", StartedAt: now, FinishedAt: now, RouteID: "r-other", RouteKind: "llm", Success: true, StatusCode: 200},
		LLMAPI:           "openai", ProviderID: "p1",
	}); err != nil {
		t.Fatalf("insert foreign: %v", err)
	}

	q := NewUsageQueries(db)
	filter := &usage.AttributionFilter{AgentID: "coding-agent", RouteIDs: []string{"r-owned"}}

	events, err := q.ListEvents("llm", usage.EventListOptions{Limit: 10, Attribution: filter})
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	got := map[string]bool{}
	for _, item := range events.Items {
		got[item["event_id"].(string)] = true
	}
	if !got["tagged"] || !got["untagged-owned"] || got["foreign"] || len(got) != 2 {
		t.Fatalf("attribution fallback events = %#v, want {tagged, untagged-owned}", got)
	}

	// Aggregates must apply the same OR fallback (no double counting; the agent
	// owns both rows on r-owned and is tagged on one of them).
	breakdown, err := q.LLMBreakdown(usage.BreakdownOptions{GroupBy: "route_id", Attribution: filter, Limit: 10})
	if err != nil {
		t.Fatalf("LLMBreakdown() error = %v", err)
	}
	if len(breakdown.Items) != 1 || breakdown.Items[0]["group_value"] != "r-owned" {
		t.Fatalf("breakdown = %#v, want a single r-owned group", breakdown.Items)
	}
	if rc := breakdown.Items[0]["request_count"]; rc != int64(2) {
		t.Fatalf("breakdown request_count = %v, want 2", rc)
	}
	series, err := q.LLMTimeseries(usage.TimeseriesOptions{Bucket: "hour", GroupBy: "route_id", Attribution: filter})
	if err != nil {
		t.Fatalf("LLMTimeseries() error = %v", err)
	}
	if len(series.Items) != 1 || series.Items[0]["group_value"] != "r-owned" {
		t.Fatalf("timeseries = %#v, want a single r-owned group", series.Items)
	}
	if rc := series.Items[0]["request_count"]; rc != int64(2) {
		t.Fatalf("timeseries request_count = %v, want 2", rc)
	}

	// An empty filter must not silently widen to all rows.
	empty, err := q.ListEvents("llm", usage.EventListOptions{Limit: 10, Attribution: &usage.AttributionFilter{}})
	if err != nil {
		t.Fatalf("ListEvents(empty filter) error = %v", err)
	}
	if len(empty.Items) != 0 {
		t.Fatalf("empty attribution filter matched %d rows, want 0", len(empty.Items))
	}
}

func TestInteractionsSummaryServiceAndSessionFilters(t *testing.T) {
	backend, err := Open(t.Context(), Config{SQLitePath: t.TempDir() + "/usage.db"}, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	db := backend.UsageDB()
	if err := MigrateUsageTables(db); err != nil {
		t.Fatalf("MigrateUsageTables() error = %v", err)
	}
	now := time.Now().UTC()
	// Two ACP sessions on the same service plus an LLM event that carries neither
	// a service_id nor a session_id. The new filters must narrow to just the
	// matching ACP rows.
	if err := InsertACPUsageEvent(db, usage.ACPUsageEvent{
		InteractionEvent: usage.InteractionEvent{EventID: "acp-a", SpanID: "s1", StartedAt: now, FinishedAt: now, RouteID: "acp-route", RouteKind: "acp", Success: true, StatusCode: 200},
		ServiceID:        "codex-main", AgentType: "codex", Operation: "turn", SessionID: "sess-1",
	}); err != nil {
		t.Fatalf("insert acp-a: %v", err)
	}
	if err := InsertACPUsageEvent(db, usage.ACPUsageEvent{
		InteractionEvent: usage.InteractionEvent{EventID: "acp-b", SpanID: "s2", StartedAt: now, FinishedAt: now, RouteID: "acp-route", RouteKind: "acp", Success: true, StatusCode: 200},
		ServiceID:        "codex-main", AgentType: "codex", Operation: "turn", SessionID: "sess-2",
	}); err != nil {
		t.Fatalf("insert acp-b: %v", err)
	}
	if err := InsertLLMUsageEvent(db, usage.LLMUsageEvent{
		InteractionEvent: usage.InteractionEvent{EventID: "llm-1", SpanID: "s3", StartedAt: now, FinishedAt: now, RouteID: "llm-route", RouteKind: "llm", Success: true, StatusCode: 200},
		LLMAPI:           "openai", ProviderID: "p1",
	}); err != nil {
		t.Fatalf("insert llm-1: %v", err)
	}

	q := NewUsageQueries(db)

	// service_id narrows to both ACP rows (the LLM row projects NULL service_id).
	byService, err := q.InteractionsSummary(usage.BreakdownOptions{GroupBy: "route_kind", Limit: 10, Filters: map[string]string{"service_id": "codex-main"}})
	if err != nil {
		t.Fatalf("InteractionsSummary(service_id) error = %v", err)
	}
	if len(byService.Items) != 1 || byService.Items[0]["group_value"] != "acp" {
		t.Fatalf("service_id summary = %#v, want only acp group", byService.Items)
	}
	if rc := byService.Items[0]["request_count"]; rc != int64(2) {
		t.Fatalf("service_id summary request_count = %v, want 2", rc)
	}

	// session_id narrows to a single ACP session.
	bySession, err := q.InteractionsSummary(usage.BreakdownOptions{GroupBy: "route_kind", Limit: 10, Filters: map[string]string{"session_id": "sess-1"}})
	if err != nil {
		t.Fatalf("InteractionsSummary(session_id) error = %v", err)
	}
	if len(bySession.Items) != 1 || bySession.Items[0]["group_value"] != "acp" {
		t.Fatalf("session_id summary = %#v, want only acp group", bySession.Items)
	}
	if rc := bySession.Items[0]["request_count"]; rc != int64(1) {
		t.Fatalf("session_id summary request_count = %v, want 1", rc)
	}
}

func TestAttributionServiceFallbackInteractions(t *testing.T) {
	backend, err := Open(t.Context(), Config{SQLitePath: t.TempDir() + "/usage.db"}, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	db := backend.UsageDB()
	if err := MigrateUsageTables(db); err != nil {
		t.Fatalf("MigrateUsageTables() error = %v", err)
	}
	now := time.Now().UTC()
	// Untagged ACP event on the agent's bound service but on a route the agent did
	// not enumerate in acp_route_ids. Only the service-level fallback can recover
	// it through the interactions union.
	if err := InsertACPUsageEvent(db, usage.ACPUsageEvent{
		InteractionEvent: usage.InteractionEvent{EventID: "acp-svc-only", SpanID: "s1", StartedAt: now, FinishedAt: now, RouteID: "unlisted-route", RouteKind: "acp", Success: true, StatusCode: 200},
		ServiceID:        "codex-main", AgentType: "codex", Operation: "turn", SessionID: "sess-1",
	}); err != nil {
		t.Fatalf("insert acp: %v", err)
	}
	// Untagged MCP event whose service_id COLLIDES with the agent's ACP service id
	// (acp_services and mcp_services are separate stores with no global id
	// uniqueness). The ACP-scoped service arm must never credit it to the agent.
	if err := InsertMCPUsageEvent(db, usage.MCPUsageEvent{
		InteractionEvent: usage.InteractionEvent{EventID: "mcp-collision", SpanID: "s2", StartedAt: now, FinishedAt: now, RouteID: "mcp-route", RouteKind: "mcp", Success: true, StatusCode: 200},
		ServiceID:        "codex-main", Method: "tools/call", ToolName: "lookup",
	}); err != nil {
		t.Fatalf("insert mcp: %v", err)
	}

	q := NewUsageQueries(db)
	// Mirrors agentAttributionFilter for an agent that binds only the ACP service.
	filter := &usage.AttributionFilter{AgentID: "coding-agent", ACPServiceIDs: []string{"codex-main"}}
	interactions, err := q.ListInteractions(usage.EventListOptions{Limit: 10, Attribution: filter})
	if err != nil {
		t.Fatalf("ListInteractions() error = %v", err)
	}
	got := map[string]bool{}
	for _, item := range interactions.Items {
		got[item["event_id"].(string)] = true
	}
	if !got["acp-svc-only"] {
		t.Fatalf("service fallback missed the acp-service-only event: %#v", got)
	}
	if got["mcp-collision"] {
		t.Fatalf("acp service arm must not attribute the same-named mcp service event: %#v", got)
	}
	sessionFiltered, err := q.ListInteractions(usage.EventListOptions{
		Limit:       10,
		Attribution: filter,
		Filters:     map[string]string{"session_id": "sess-1"},
	})
	if err != nil {
		t.Fatalf("ListInteractions(session filter) error = %v", err)
	}
	if len(sessionFiltered.Items) != 1 || sessionFiltered.Items[0]["event_id"] != "acp-svc-only" || sessionFiltered.Items[0]["session_id"] != "sess-1" {
		t.Fatalf("session-filtered interactions = %#v, want only acp-svc-only with sess-1", sessionFiltered.Items)
	}

	// The MCP breakdown must likewise ignore the ACP service arm: the collision
	// event is untagged and on no owned route, so it must not appear.
	mcp, err := q.MCPToolsSummary(usage.SummaryOptions{Limit: 10, Attribution: filter})
	if err != nil {
		t.Fatalf("MCPToolsSummary() error = %v", err)
	}
	if len(mcp.Items) != 0 {
		t.Fatalf("mcp summary must not attribute the same-named service collision: %#v", mcp.Items)
	}

	summary, err := q.InteractionsSummary(usage.BreakdownOptions{GroupBy: "route_kind", Limit: 10, Attribution: filter})
	if err != nil {
		t.Fatalf("InteractionsSummary() error = %v", err)
	}
	if len(summary.Items) != 1 || summary.Items[0]["group_value"] != "acp" {
		t.Fatalf("interactions summary = %#v, want only acp service fallback", summary.Items)
	}
	if rc := summary.Items[0]["request_count"]; rc != int64(1) {
		t.Fatalf("interactions summary request_count = %v, want 1", rc)
	}
}

// fakeAttributor maps a fixed route id to an agent for the observer test.
type fakeAttributor struct{ routeID, agentID string }

func (f fakeAttributor) ResolveAgentID(routeID, _, _ string) (string, bool) {
	if routeID == f.routeID {
		return f.agentID, true
	}
	return "", false
}

func TestObserverStampsAgentIDFromAttribution(t *testing.T) {
	sink := &usage.InMemorySink{}
	attribution := usage.NewAgentAttribution()
	attribution.Set(fakeAttributor{routeID: "r1", agentID: "coding-agent"})
	obs := usage.NewObserverWithAttribution(sink, attribution)
	span, _ := obs.Begin(t.Context(), usage.InteractionDimensions{RouteID: "r1", RouteKind: "llm"})
	span.Finish(usage.InteractionOutcome{Success: true, StatusCode: 200})
	if len(sink.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(sink.Events))
	}
	ev, ok := sink.Events[0].(usage.LLMUsageEvent)
	if !ok {
		t.Fatalf("event type = %T, want LLMUsageEvent", sink.Events[0])
	}
	if ev.AgentID != "coding-agent" {
		t.Fatalf("AgentID = %q, want coding-agent", ev.AgentID)
	}
}

func TestUsageWriterQuerySummaryAndEvents(t *testing.T) {
	backend, err := Open(t.Context(), Config{SQLitePath: t.TempDir() + "/usage.db"}, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	db := backend.UsageDB()
	if db == nil {
		t.Fatal("UsageDB() = nil")
	}
	if err := MigrateUsageTables(db); err != nil {
		t.Fatalf("MigrateUsageTables() error = %v", err)
	}
	now := time.Now().UTC()
	if err := InsertLLMUsageEvent(db, usage.LLMUsageEvent{
		InteractionEvent: usage.InteractionEvent{
			EventID: "ev-1", SpanID: "span-1", StartedAt: now, FinishedAt: now.Add(10 * time.Millisecond),
			RouteID: "route-1", RouteKind: "llm", RouteProtocol: "openai", Success: true, StatusCode: 200, LatencyMS: 10,
		},
		LLMAPI: "openai", ProviderID: "provider-1", UpstreamModel: "gpt-4o", InputTokens: 3, OutputTokens: 4, TotalTokens: 7, UsageFinalized: true,
		RequestToolCount: 2, RequestToolNames: []string{"read_file", "write_file"}, ToolCallCount: 1, ToolNames: []string{"read_file"},
	}); err != nil {
		t.Fatalf("InsertLLMUsageEvent() error = %v", err)
	}
	q := NewUsageQueries(db)
	summary, err := q.Summary()
	if err != nil {
		t.Fatalf("Summary() error = %v", err)
	}
	if summary.LLM.RequestCount != 1 || summary.LLM.TotalTokens != 7 {
		t.Fatalf("summary.LLM = %+v, want one request and 7 tokens", summary.LLM)
	}
	events, err := q.ListEvents("llm", usage.EventListOptions{
		Limit:   10,
		Filters: map[string]string{"provider_id": "provider-1"},
	})
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events.Items) != 1 || events.Items[0]["event_id"] != "ev-1" {
		t.Fatalf("events = %#v, want ev-1", events.Items)
	}
	interactions, err := q.ListInteractions(usage.EventListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListInteractions() error = %v", err)
	}
	if len(interactions.Items) != 1 || interactions.Items[0]["route_kind"] != "llm" {
		t.Fatalf("interactions = %#v, want llm interaction", interactions.Items)
	}
	timeseries, err := q.LLMTimeseries(usage.TimeseriesOptions{Bucket: "hour", GroupBy: "provider_id"})
	if err != nil {
		t.Fatalf("LLMTimeseries() error = %v", err)
	}
	if len(timeseries.Items) != 1 || timeseries.Items[0]["group_value"] != "provider-1" {
		t.Fatalf("timeseries = %#v, want provider-1", timeseries.Items)
	}
	breakdown, err := q.LLMBreakdown(usage.BreakdownOptions{GroupBy: "provider_id", OrderBy: "total_tokens", Limit: 10})
	if err != nil {
		t.Fatalf("LLMBreakdown() error = %v", err)
	}
	if len(breakdown.Items) != 1 || breakdown.Items[0]["group_value"] != "provider-1" {
		t.Fatalf("breakdown = %#v, want provider-1", breakdown.Items)
	}
	if err := InsertMCPUsageEvent(db, usage.MCPUsageEvent{
		InteractionEvent: usage.InteractionEvent{EventID: "ev-2", SpanID: "span-2", StartedAt: now, FinishedAt: now, RouteID: "mcp-route", RouteKind: "mcp", RouteProtocol: "mcp", Success: true, StatusCode: 200},
		ServiceID:        "svc-1", Method: "tools/call", ToolName: "lookup",
	}); err != nil {
		t.Fatalf("InsertMCPUsageEvent() error = %v", err)
	}
	toolSummary, err := q.MCPToolsSummary(usage.SummaryOptions{Limit: 10})
	if err != nil {
		t.Fatalf("MCPToolsSummary() error = %v", err)
	}
	if len(toolSummary.Items) != 1 || toolSummary.Items[0]["group_value"] != "lookup" {
		t.Fatalf("tool summary = %#v, want lookup", toolSummary.Items)
	}
	mcpSeries, err := q.MCPTimeseries(usage.TimeseriesOptions{Bucket: "hour", GroupBy: "tool_name"})
	if err != nil {
		t.Fatalf("MCPTimeseries() error = %v", err)
	}
	if len(mcpSeries.Items) != 1 || mcpSeries.Items[0]["group_value"] != "lookup" || mcpSeries.Items[0]["tools_call_count"] != int64(1) {
		t.Fatalf("mcp timeseries = %#v, want lookup with tools_call_count 1", mcpSeries.Items)
	}
	mcpBreakdown, err := q.MCPBreakdown(usage.BreakdownOptions{GroupBy: "method", Limit: 10})
	if err != nil {
		t.Fatalf("MCPBreakdown() error = %v", err)
	}
	if len(mcpBreakdown.Items) != 1 || mcpBreakdown.Items[0]["group_value"] != "tools/call" {
		t.Fatalf("mcp breakdown = %#v, want tools/call", mcpBreakdown.Items)
	}
	if err := InsertACPUsageEvent(db, usage.ACPUsageEvent{
		InteractionEvent: usage.InteractionEvent{EventID: "ev-3", SpanID: "span-3", StartedAt: now, FinishedAt: now, RouteID: "acp-route", RouteKind: "acp", RouteProtocol: "acp", Success: true, StatusCode: 200},
		ServiceID:        "acp-svc", AgentType: "codex", Operation: "turn",
	}); err != nil {
		t.Fatalf("InsertACPUsageEvent() error = %v", err)
	}
	acpSummary, err := q.ACPSummary(usage.BreakdownOptions{GroupBy: "operation", Limit: 10})
	if err != nil {
		t.Fatalf("ACPSummary() error = %v", err)
	}
	if len(acpSummary.Items) != 1 || acpSummary.Items[0]["group_value"] != "turn" {
		t.Fatalf("acp summary = %#v, want turn", acpSummary.Items)
	}
	acpSeries, err := q.ACPTimeseries(usage.TimeseriesOptions{Bucket: "hour", GroupBy: "agent_type"})
	if err != nil {
		t.Fatalf("ACPTimeseries() error = %v", err)
	}
	if len(acpSeries.Items) != 1 || acpSeries.Items[0]["group_value"] != "codex" || acpSeries.Items[0]["turn_count"] != int64(1) {
		t.Fatalf("acp timeseries = %#v, want codex with turn_count 1", acpSeries.Items)
	}
	acpBreakdown, err := q.ACPBreakdown(usage.BreakdownOptions{GroupBy: "operation", Limit: 10})
	if err != nil {
		t.Fatalf("ACPBreakdown() error = %v", err)
	}
	if len(acpBreakdown.Items) != 1 || acpBreakdown.Items[0]["group_value"] != "turn" {
		t.Fatalf("acp breakdown = %#v, want turn", acpBreakdown.Items)
	}
	interactionSummary, err := q.InteractionsSummary(usage.BreakdownOptions{GroupBy: "route_kind", Limit: 10})
	if err != nil {
		t.Fatalf("InteractionsSummary() error = %v", err)
	}
	if len(interactionSummary.Items) != 3 {
		t.Fatalf("interaction summary count = %d, want 3 groups: %#v", len(interactionSummary.Items), interactionSummary.Items)
	}
	// The interactions projection must surface each protocol's labeling column so
	// the consumer can name a span by what it did rather than the route_id.
	mixed, err := q.ListInteractions(usage.EventListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListInteractions(mixed) error = %v", err)
	}
	byID := make(map[string]map[string]any, len(mixed.Items))
	for _, item := range mixed.Items {
		id, _ := item["event_id"].(string)
		byID[id] = item
	}
	if got := byID["ev-1"]["upstream_model"]; got != "gpt-4o" {
		t.Fatalf("ev-1 upstream_model = %#v, want gpt-4o", got)
	}
	if got := byID["ev-2"]["tool_name"]; got != "lookup" {
		t.Fatalf("ev-2 tool_name = %#v, want lookup", got)
	}
	if got := byID["ev-3"]["operation"]; got != "turn" {
		t.Fatalf("ev-3 operation = %#v, want turn", got)
	}
	if got := byID["ev-1"]["request_tool_count"]; got != int64(2) {
		t.Fatalf("ev-1 request_tool_count = %#v, want 2", got)
	}
	if got := byID["ev-1"]["request_tool_names"]; got != `["read_file","write_file"]` {
		t.Fatalf("ev-1 request_tool_names = %#v, want read/write tools", got)
	}
	if got := byID["ev-1"]["tool_call_count"]; got != int64(1) {
		t.Fatalf("ev-1 tool_call_count = %#v, want 1", got)
	}
	if got := byID["ev-1"]["tool_names"]; got != `["read_file"]` {
		t.Fatalf("ev-1 tool_names = %#v, want read_file", got)
	}
	if got := byID["ev-1"]["input_tokens"]; got != int64(3) {
		t.Fatalf("ev-1 input_tokens = %#v, want 3", got)
	}
	if got := byID["ev-1"]["output_tokens"]; got != int64(4) {
		t.Fatalf("ev-1 output_tokens = %#v, want 4", got)
	}
	if got := byID["ev-1"]["total_tokens"]; got != int64(7) {
		t.Fatalf("ev-1 total_tokens = %#v, want 7", got)
	}
	// Cross-protocol columns are NULL on branches that do not own them.
	if got := byID["ev-1"]["tool_name"]; got != nil {
		t.Fatalf("ev-1 tool_name = %#v, want nil", got)
	}
	if got := byID["ev-2"]["operation"]; got != nil {
		t.Fatalf("ev-2 operation = %#v, want nil", got)
	}
	if got := byID["ev-2"]["input_tokens"]; got != nil {
		t.Fatalf("ev-2 input_tokens = %#v, want nil", got)
	}
	if got := byID["ev-3"]["tool_call_count"]; got != nil {
		t.Fatalf("ev-3 tool_call_count = %#v, want nil", got)
	}
	old := now.Add(-40 * 24 * time.Hour)
	if err := InsertLLMUsageEvent(db, usage.LLMUsageEvent{
		InteractionEvent: usage.InteractionEvent{EventID: "ev-old", SpanID: "span-old", StartedAt: old, FinishedAt: old, RouteID: "old", RouteKind: "llm", RouteProtocol: "openai", Success: true, StatusCode: 200},
		LLMAPI:           "openai", ProviderID: "provider-old",
	}); err != nil {
		t.Fatalf("Insert old LLM event error = %v", err)
	}
	if err := CleanupUsageEvents(db, 30*24*time.Hour); err != nil {
		t.Fatalf("CleanupUsageEvents() error = %v", err)
	}
	var oldCount int64
	if err := db.Raw(`SELECT COUNT(*) FROM llm_usage_events WHERE event_id='ev-old'`).Row().Scan(&oldCount); err != nil {
		t.Fatalf("query old count: %v", err)
	}
	if oldCount != 0 {
		t.Fatalf("old event count = %d, want 0", oldCount)
	}
}

func TestResolveBucket(t *testing.T) {
	cases := []struct {
		bucket   string
		wantName string
		wantMS   int64
		wantErr  bool
	}{
		{"minute", "minute", int64(time.Minute / time.Millisecond), false},
		{"minutes", "minute", int64(time.Minute / time.Millisecond), false},
		{"min", "minute", int64(time.Minute / time.Millisecond), false},
		{"m", "minute", int64(time.Minute / time.Millisecond), false},
		{"hour", "hour", int64(time.Hour / time.Millisecond), false},
		{"hours", "hour", int64(time.Hour / time.Millisecond), false},
		{"", "hour", int64(time.Hour / time.Millisecond), false},
		{"DAY", "day", int64(24 * time.Hour / time.Millisecond), false},
		{"days", "day", int64(24 * time.Hour / time.Millisecond), false},
		{"3h", "3h", int64(3 * time.Hour / time.Millisecond), false},
		{"5m", "5m", int64(5 * time.Minute / time.Millisecond), false},
		{"30s", "30s", int64(30 * time.Second / time.Millisecond), false},
		{"90m", "90m", int64(90 * time.Minute / time.Millisecond), false},
		{"2d", "2d", int64(2 * 24 * time.Hour / time.Millisecond), false},
		{"0h", "", 0, true},
		{"-5m", "", 0, true},
		{"week", "", 0, true},
		{"abc", "", 0, true},
	}
	for _, tc := range cases {
		name, ms, err := resolveBucket(tc.bucket)
		if tc.wantErr {
			if err == nil {
				t.Errorf("resolveBucket(%q) error = nil, want error", tc.bucket)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveBucket(%q) error = %v", tc.bucket, err)
			continue
		}
		if name != tc.wantName || ms != tc.wantMS {
			t.Errorf("resolveBucket(%q) = (%q, %d), want (%q, %d)", tc.bucket, name, ms, tc.wantName, tc.wantMS)
		}
	}
}
