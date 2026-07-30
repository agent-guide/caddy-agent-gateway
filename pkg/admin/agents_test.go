package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
)

func TestAgentViewExposesNonExecutableRuntime(t *testing.T) {
	h := &Handler{}
	a := agentpkg.Agent{
		ID: "http-agent", Name: "HTTP Agent",
		Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeHTTP, HTTP: &agentpkg.HTTPRuntime{Endpoint: "https://example.com/agent"}},
	}

	view := h.agentView(t.Context(), a, "config_store")
	if view.RuntimeStatus == nil || view.RuntimeStatus.State != runtimeapi.RuntimeStateNotExecutable {
		t.Fatalf("runtime status = %#v, want not_executable", view.RuntimeStatus)
	}
	if view.Capabilities == nil || view.Capabilities.Executable {
		t.Fatalf("capabilities = %#v, want executable=false", view.Capabilities)
	}
}

type permissionResponseBackend struct{ resolved bool }

func (*permissionResponseBackend) RuntimeType() string { return agentpkg.RuntimeTypeBuiltin }
func (*permissionResponseBackend) Capabilities(context.Context, agentpkg.Agent) (runtimeapi.Capabilities, error) {
	return runtimeapi.Capabilities{Permissions: runtimeapi.PermissionCapabilities{Interactive: true, ResumeMode: runtimeapi.PermissionResumeNewStream}}, nil
}
func (*permissionResponseBackend) ServeTurn(context.Context, agentpkg.Agent, runtimeapi.TurnRequest, runtimeapi.EventSink) error {
	return nil
}
func (b *permissionResponseBackend) ResolvePermission(context.Context, agentpkg.Agent, runtimeapi.PermissionDecision) error {
	b.resolved = true
	return nil
}

type retryableCancelBackend struct{}

func (*retryableCancelBackend) RuntimeType() string { return agentpkg.RuntimeTypeBuiltin }
func (*retryableCancelBackend) Capabilities(context.Context, agentpkg.Agent) (runtimeapi.Capabilities, error) {
	return runtimeapi.Capabilities{Cancellation: runtimeapi.CancelCapabilities{Force: true}}, nil
}
func (*retryableCancelBackend) ServeTurn(context.Context, agentpkg.Agent, runtimeapi.TurnRequest, runtimeapi.EventSink) error {
	return nil
}
func (*retryableCancelBackend) CancelRun(context.Context, agentpkg.Agent, runtimeapi.CancelRequest) (runtimeapi.CancelResult, error) {
	return runtimeapi.CancelResult{}, runtimeapi.NewError(runtimeapi.ErrorBackendUnavailable, "run cancellation is not ready; retry")
}

type claimedAuditContinuation struct{}

func (claimedAuditContinuation) ValidateContinuationDecision(string, runtimeapi.PendingPermission, runtimeapi.PermissionDecision) error {
	return nil
}
func (claimedAuditContinuation) ResolveContinuation(context.Context, string, runtimeapi.PermissionDecision, time.Time) error {
	return nil
}
func (claimedAuditContinuation) ExpireContinuation(context.Context, string) error { return nil }

func TestResolveBuiltinAgentPermissionRequiresNewStreamResume(t *testing.T) {
	store := newAgentConfigStore()
	a := &agentpkg.Agent{ID: "a1", Name: "A1", Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeBuiltin, Builtin: &agentpkg.BuiltinRuntime{}}}
	if err := store.Create(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	backend := &permissionResponseBackend{}
	registry := runtimeapi.NewRegistry()
	if err := registry.Register(backend); err != nil {
		t.Fatal(err)
	}
	h := &Handler{agentManager: agentpkg.NewManager(store), runtimeRegistry: registry}
	req := httptest.NewRequest(http.MethodPost, "/admin/agents/a1/permissions/perm-1", strings.NewReader(`{"outcome":"allow"}`))
	req.SetPathValue("id", "a1")
	req.SetPathValue("request_id", "perm-1")
	rec := httptest.NewRecorder()
	h.handleResolveAgentPermission(rec, req)
	if rec.Code != http.StatusOK || !backend.resolved {
		t.Fatalf("status=%d resolved=%v body=%s", rec.Code, backend.resolved, rec.Body.String())
	}
	var response struct {
		Status         string `json:"status"`
		RequestID      string `json:"request_id"`
		ResumeRequired bool   `json:"resume_required"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.Status != "accepted" || response.RequestID != "perm-1" || !response.ResumeRequired {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}

func TestResolveAgentPermissionAuditRetainsClaimedRunCorrelation(t *testing.T) {
	store := newAgentConfigStore()
	a := &agentpkg.Agent{ID: "a1", Name: "A1", Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeBuiltin, Builtin: &agentpkg.BuiltinRuntime{}}}
	if err := store.Create(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	backend := &permissionResponseBackend{}
	registry := runtimeapi.NewRegistry()
	if err := registry.Register(backend); err != nil {
		t.Fatal(err)
	}
	broker := runtimeapi.NewPermissionBroker()
	t.Cleanup(func() { broker.Close(runtimeapi.WithPermissionSource(context.Background(), "test_cleanup")) })
	runID := "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := broker.Register(runtimeapi.PendingPermission{
		RequestID: "perm-claimed", AgentID: a.ID, RuntimeType: agentpkg.RuntimeTypeBuiltin,
		RunID: runID, SessionID: "session-1", ExpiresAt: time.Now().Add(time.Minute),
	}, "cont-claimed", claimedAuditContinuation{}); err != nil {
		t.Fatal(err)
	}
	// Simulate a concurrent entrypoint winning the atomic claim before this
	// Admin request begins; the bounded claimed metadata must still correlate
	// the audit span.
	if err := broker.Resolve(t.Context(), a.ID, runtimeapi.PermissionDecision{RequestID: "perm-claimed", Outcome: "allow"}); err != nil {
		t.Fatal(err)
	}
	sink := &adminCaptureSink{}
	h := &Handler{
		agentManager: agentpkg.NewManager(store), runtimeRegistry: registry,
		permissionBroker: broker, usageObserver: usage.NewObserver(sink),
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/agents/a1/permissions/perm-claimed", strings.NewReader(`{"outcome":"allow"}`))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("request_id", "perm-claimed")
	rec := httptest.NewRecorder()
	h.handleResolveAgentPermission(rec, req)

	if rec.Code != http.StatusOK || len(sink.events) != 1 {
		t.Fatalf("status/events = %d/%#v; body=%s", rec.Code, sink.events, rec.Body.String())
	}
	ev, ok := sink.events[0].(usage.BuiltinUsageEvent)
	if !ok || ev.AgentID != a.ID || ev.RuntimeType != agentpkg.RuntimeTypeBuiltin || ev.RunID != runID || ev.SessionID != "session-1" || ev.PermissionRequestID != "perm-claimed" || ev.Operation != "permission" || ev.ResultStatus != "success" {
		t.Fatalf("claimed permission audit = %#v", sink.events[0])
	}
}

func TestCancelAgentRunBackendUnavailableIsRetryable(t *testing.T) {
	store := newAgentConfigStore()
	a := &agentpkg.Agent{ID: "a1", Name: "A1", Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeBuiltin, Builtin: &agentpkg.BuiltinRuntime{}}}
	if err := store.Create(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	registry := runtimeapi.NewRegistry()
	if err := registry.Register(&retryableCancelBackend{}); err != nil {
		t.Fatal(err)
	}
	sink := &adminCaptureSink{}
	h := &Handler{agentManager: agentpkg.NewManager(store), runtimeRegistry: registry, usageObserver: usage.NewObserver(sink)}
	req := httptest.NewRequest(http.MethodDelete, "/admin/agents/a1/runs/run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
	req.SetPathValue("id", "a1")
	req.SetPathValue("run_id", "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	rec := httptest.NewRecorder()
	h.handleCancelAgentRun(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("status=%d retry-after=%q body=%s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if len(sink.events) != 1 {
		t.Fatalf("cancel audit events = %#v", sink.events)
	}
	ev, ok := sink.events[0].(usage.BuiltinUsageEvent)
	if !ok || ev.RouteKind != "agent" || ev.RouteProtocol != "admin" || ev.AgentID != "a1" || ev.RuntimeType != "builtin" || ev.RunID != "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || ev.Operation != "run_cancel" || ev.ResultStatus != "error" {
		t.Fatalf("cancel audit = %#v", sink.events[0])
	}
}

type agentConfigStore struct {
	mu    sync.RWMutex
	items map[string]*agentpkg.Agent
}

func newAgentConfigStore() *agentConfigStore {
	return &agentConfigStore{items: map[string]*agentpkg.Agent{}}
}

func (s *agentConfigStore) unwrap(obj any) (*agentpkg.Agent, error) {
	if u, ok := obj.(configstore.ObjectUnwrapper); ok {
		obj = u.ConfigStoreObject()
	}
	cfg, ok := obj.(*agentpkg.Agent)
	if !ok {
		return nil, fmt.Errorf("unexpected object type %T", obj)
	}
	return cfg, nil
}

func (s *agentConfigStore) Create(_ context.Context, obj any) error {
	cfg, err := s.unwrap(obj)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cloned := *cfg
	s.items[cloned.ID] = &cloned
	return nil
}

func (s *agentConfigStore) Update(_ context.Context, obj any) error {
	cfg, err := s.unwrap(obj)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cloned := *cfg
	s.items[cloned.ID] = &cloned
	return nil
}

func (s *agentConfigStore) Delete(_ context.Context, keyParts ...any) error {
	id := fmt.Sprint(keyParts[0])
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, id)
	return nil
}

func (s *agentConfigStore) Get(_ context.Context, keyParts ...any) (any, error) {
	id := fmt.Sprint(keyParts[0])
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg, ok := s.items[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	cloned := *cfg
	return &cloned, nil
}

func (s *agentConfigStore) List(_ context.Context) ([]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]any, 0, len(s.items))
	for _, cfg := range s.items {
		cloned := *cfg
		out = append(out, &cloned)
	}
	return out, nil
}

func (s *agentConfigStore) ListByTag(context.Context, string) ([]any, error) {
	return s.List(context.Background())
}

func (s *agentConfigStore) ListByTagPrefix(context.Context, string) ([]any, error) {
	return s.List(context.Background())
}

func (s *agentConfigStore) GetByIndex(context.Context, string, any) (any, error) {
	return nil, configstore.ErrNotFound
}

type recordingUsageQuery struct {
	opts          usage.EventListOptions
	seriesOpts    usage.TimeseriesOptions
	breakdownOpts usage.BreakdownOptions
	seriesErr     error
}

func (q *recordingUsageQuery) Summary() (usage.Summary, error) { return usage.Summary{}, nil }
func (q *recordingUsageQuery) ListEvents(string, usage.EventListOptions) (usage.EventListResponse, error) {
	return usage.EventListResponse{}, nil
}
func (q *recordingUsageQuery) ListInteractions(opts usage.EventListOptions) (usage.EventListResponse, error) {
	q.opts = opts
	return usage.EventListResponse{Limit: opts.Limit}, nil
}
func (q *recordingUsageQuery) LLMTimeseries(opts usage.TimeseriesOptions) (usage.SeriesResponse, error) {
	q.seriesOpts = opts
	if q.seriesErr != nil {
		return usage.SeriesResponse{}, q.seriesErr
	}
	return usage.SeriesResponse{Bucket: opts.Bucket, GroupBy: opts.GroupBy}, nil
}
func (q *recordingUsageQuery) LLMBreakdown(usage.BreakdownOptions) (usage.BreakdownResponse, error) {
	return usage.BreakdownResponse{}, nil
}
func (q *recordingUsageQuery) MCPTimeseries(usage.TimeseriesOptions) (usage.SeriesResponse, error) {
	return usage.SeriesResponse{}, nil
}
func (q *recordingUsageQuery) MCPBreakdown(usage.BreakdownOptions) (usage.BreakdownResponse, error) {
	return usage.BreakdownResponse{}, nil
}
func (q *recordingUsageQuery) MCPToolsSummary(usage.SummaryOptions) (usage.BreakdownResponse, error) {
	return usage.BreakdownResponse{}, nil
}
func (q *recordingUsageQuery) ACPTimeseries(usage.TimeseriesOptions) (usage.SeriesResponse, error) {
	return usage.SeriesResponse{}, nil
}
func (q *recordingUsageQuery) ACPBreakdown(usage.BreakdownOptions) (usage.BreakdownResponse, error) {
	return usage.BreakdownResponse{}, nil
}
func (q *recordingUsageQuery) ACPSummary(usage.BreakdownOptions) (usage.BreakdownResponse, error) {
	return usage.BreakdownResponse{}, nil
}
func (q *recordingUsageQuery) InteractionsSummary(opts usage.BreakdownOptions) (usage.BreakdownResponse, error) {
	q.breakdownOpts = opts
	return usage.BreakdownResponse{GroupBy: opts.GroupBy, Limit: opts.Limit}, nil
}

func TestAgentInteractionsAllowsDiagnosticFilters(t *testing.T) {
	store := newAgentConfigStore()
	manager := agentpkg.NewManager(store)
	if err := manager.Create(t.Context(), agentpkg.Agent{
		ID:   "coding-agent",
		Name: "Coding Agent",
		Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeACP, ACP: &agentpkg.ACPRuntime{
			AgentType: "codex", CWD: "/tmp", AllowedRoots: []string{"/tmp"},
		}},
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	query := &recordingUsageQuery{}
	h := &Handler{agentManager: manager, usageQuery: query}
	req := httptest.NewRequest(http.MethodGet, "/admin/agents/coding-agent/interactions?trace_id=t1&parent_span_id=p1&service_id=codex-main&session_id=sess-1&agent_depth=2", nil)
	req.SetPathValue("id", "coding-agent")
	rec := httptest.NewRecorder()

	h.handleGetAgentInteractions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for key, want := range map[string]string{
		"trace_id":       "t1",
		"parent_span_id": "p1",
		"service_id":     "codex-main",
		"session_id":     "sess-1",
		"agent_depth":    "2",
	} {
		if got := query.opts.Filters[key]; got != want {
			t.Fatalf("filter %s = %q, want %q; filters=%#v", key, got, want, query.opts.Filters)
		}
	}
}

func TestMetricInteractionsAllowsServiceAndSessionFilters(t *testing.T) {
	query := &recordingUsageQuery{}
	h := &Handler{usageQuery: query}
	req := httptest.NewRequest(http.MethodGet, "/admin/metrics/interactions?service_id=codex-main&session_id=sess-1", nil)
	rec := httptest.NewRecorder()

	h.handleListMetricInteractions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if query.opts.Filters["service_id"] != "codex-main" || query.opts.Filters["session_id"] != "sess-1" {
		t.Fatalf("interaction filters = %#v, want service_id and session_id preserved", query.opts.Filters)
	}
}

func TestMetricInteractionsResolvesAgentAttribution(t *testing.T) {
	store := newAgentConfigStore()
	manager := agentpkg.NewManager(store)
	if err := manager.Create(t.Context(), agentpkg.Agent{
		ID:   "coding-agent",
		Name: "Coding Agent",
		Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeACP, ACP: &agentpkg.ACPRuntime{
			AgentType: "codex", CWD: "/tmp", AllowedRoots: []string{"/tmp"},
		}},
		Routes: agentpkg.Routes{LLMRouteIDs: []string{"llm-route"}},
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	query := &recordingUsageQuery{}
	h := &Handler{agentManager: manager, usageQuery: query}
	// agent_id on the generic interactions endpoint must resolve to full attribution
	// (tag OR declared resource routes), not a literal agent_id filter — unified with
	// the per-agent interactions read and the metrics endpoints.
	req := httptest.NewRequest(http.MethodGet, "/admin/metrics/interactions?agent_id=coding-agent&route_kind=llm", nil)
	rec := httptest.NewRecorder()

	h.handleListMetricInteractions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	attr := query.opts.Attribution
	if attr == nil || attr.AgentID != "coding-agent" {
		t.Fatalf("attribution = %+v, want AgentID=coding-agent", attr)
	}
	if len(attr.RouteIDs) != 1 || attr.RouteIDs[0] != "llm-route" {
		t.Fatalf("route fallback = %#v, want llm-route", attr.RouteIDs)
	}
	// agent_id must not leak into the literal filter map; other filters still apply.
	if _, ok := query.opts.Filters["agent_id"]; ok {
		t.Fatalf("agent_id leaked into filters: %#v", query.opts.Filters)
	}
	if query.opts.Filters["route_kind"] != "llm" {
		t.Fatalf("route_kind filter = %q, want llm", query.opts.Filters["route_kind"])
	}
}

func TestMetricInteractionsUnknownAgentReturns404(t *testing.T) {
	store := newAgentConfigStore()
	manager := agentpkg.NewManager(store)
	h := &Handler{agentManager: manager, usageQuery: &recordingUsageQuery{}}
	req := httptest.NewRequest(http.MethodGet, "/admin/metrics/interactions?agent_id=ghost", nil)
	rec := httptest.NewRecorder()

	h.handleListMetricInteractions(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMetricInteractionsSummaryAllowsServiceAndSessionFilters(t *testing.T) {
	query := &recordingUsageQuery{}
	h := &Handler{usageQuery: query}
	req := httptest.NewRequest(http.MethodGet, "/admin/metrics/interactions/summary?group_by=route_kind&service_id=codex-main&session_id=sess-1", nil)
	rec := httptest.NewRecorder()

	h.handleMetricInteractionsSummary(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if query.breakdownOpts.Filters["service_id"] != "codex-main" || query.breakdownOpts.Filters["session_id"] != "sess-1" {
		t.Fatalf("summary filters = %#v, want service_id and session_id preserved", query.breakdownOpts.Filters)
	}
}

func TestSuccessFromAnyHandlesNormalizedBoolRows(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  bool
	}{
		{true, true},
		{false, false},
		{int64(1), true},
		{int64(0), false},
	} {
		if got := successFromAny(tc.value); got != tc.want {
			b, _ := json.Marshal(tc.value)
			t.Fatalf("successFromAny(%s) = %v, want %v", string(b), got, tc.want)
		}
	}
}

func TestAgentUsageIncludesAttributedLLMTimeseries(t *testing.T) {
	store := newAgentConfigStore()
	manager := agentpkg.NewManager(store)
	if err := manager.Create(t.Context(), agentpkg.Agent{
		ID:   "coding-agent",
		Name: "Coding Agent",
		Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeACP, ACP: &agentpkg.ACPRuntime{
			AgentType: "codex", CWD: "/tmp", AllowedRoots: []string{"/tmp"},
		}},
		Routes: agentpkg.Routes{LLMRouteIDs: []string{"llm-route"}},
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	query := &recordingUsageQuery{}
	h := &Handler{agentManager: manager, usageQuery: query}
	req := httptest.NewRequest(http.MethodGet, "/admin/agents/coding-agent/usage?bucket=day", nil)
	req.SetPathValue("id", "coding-agent")
	rec := httptest.NewRecorder()

	h.handleGetAgentUsage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if query.seriesOpts.Bucket != "day" || query.seriesOpts.GroupBy != "route_id" {
		t.Fatalf("series opts = %+v, want bucket=day group_by=route_id", query.seriesOpts)
	}
	if query.seriesOpts.Attribution == nil || query.seriesOpts.Attribution.AgentID != "coding-agent" {
		t.Fatalf("series attribution = %+v, want coding-agent", query.seriesOpts.Attribution)
	}
	if len(query.seriesOpts.Attribution.RouteIDs) != 1 || query.seriesOpts.Attribution.RouteIDs[0] != "llm-route" {
		t.Fatalf("series route fallback = %#v, want llm-route", query.seriesOpts.Attribution.RouteIDs)
	}
}

func TestMetricsTimeseriesResolvesAgentAttribution(t *testing.T) {
	store := newAgentConfigStore()
	manager := agentpkg.NewManager(store)
	if err := manager.Create(t.Context(), agentpkg.Agent{
		ID:   "coding-agent",
		Name: "Coding Agent",
		Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeACP, ACP: &agentpkg.ACPRuntime{
			AgentType: "codex", CWD: "/tmp", AllowedRoots: []string{"/tmp"},
		}},
		Routes: agentpkg.Routes{LLMRouteIDs: []string{"llm-route"}},
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	query := &recordingUsageQuery{}
	h := &Handler{agentManager: manager, usageQuery: query}
	// The generic metrics endpoints must resolve agent_id into the FULL attribution
	// (durable tag OR declared resource routes), not a literal agent_id filter — this
	// is what makes an agent-filtered read a superset of the per-agent usage rollup.
	req := httptest.NewRequest(http.MethodGet, "/admin/metrics/llm/timeseries?agent_id=coding-agent", nil)
	rec := httptest.NewRecorder()

	h.handleLLMMetricsTimeseries(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	attr := query.seriesOpts.Attribution
	if attr == nil || attr.AgentID != "coding-agent" {
		t.Fatalf("attribution = %+v, want AgentID=coding-agent", attr)
	}
	if len(attr.RouteIDs) != 1 || attr.RouteIDs[0] != "llm-route" {
		t.Fatalf("route fallback = %#v, want llm-route", attr.RouteIDs)
	}
	// agent_id must NOT leak into the literal filter map (attribution handles it).
	if _, ok := query.seriesOpts.Filters["agent_id"]; ok {
		t.Fatalf("agent_id leaked into filters: %#v", query.seriesOpts.Filters)
	}
}

func TestMetricsBreakdownUnknownAgentReturns404(t *testing.T) {
	store := newAgentConfigStore()
	manager := agentpkg.NewManager(store)
	h := &Handler{agentManager: manager, usageQuery: &recordingUsageQuery{}}
	req := httptest.NewRequest(http.MethodGet, "/admin/metrics/llm/breakdown?agent_id=ghost", nil)
	rec := httptest.NewRecorder()

	h.handleLLMMetricsBreakdown(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMetricsTimeseriesWithoutAgentHasNoAttribution(t *testing.T) {
	query := &recordingUsageQuery{}
	// No agentManager: a request without agent_id must never touch it.
	h := &Handler{usageQuery: query}
	req := httptest.NewRequest(http.MethodGet, "/admin/metrics/llm/timeseries?group_by=route_id", nil)
	rec := httptest.NewRecorder()

	h.handleLLMMetricsTimeseries(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if query.seriesOpts.GroupBy != "route_id" {
		t.Fatalf("series opts = %+v, want group_by=route_id (handler ran)", query.seriesOpts)
	}
	if query.seriesOpts.Attribution != nil {
		t.Fatalf("attribution = %+v, want nil when no agent_id", query.seriesOpts.Attribution)
	}
}

func TestAgentUsageReturnsBadRequestOnMetricQueryError(t *testing.T) {
	store := newAgentConfigStore()
	manager := agentpkg.NewManager(store)
	if err := manager.Create(t.Context(), agentpkg.Agent{
		ID:   "coding-agent",
		Name: "Coding Agent",
		Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeACP, ACP: &agentpkg.ACPRuntime{
			AgentType: "codex", CWD: "/tmp", AllowedRoots: []string{"/tmp"},
		}},
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	h := &Handler{agentManager: manager, usageQuery: &recordingUsageQuery{seriesErr: errors.New("bucket must be hour or day")}}
	req := httptest.NewRequest(http.MethodGet, "/admin/agents/coding-agent/usage?bucket=bad", nil)
	req.SetPathValue("id", "coding-agent")
	rec := httptest.NewRecorder()

	h.handleGetAgentUsage(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s; want 400", rec.Code, rec.Body.String())
	}
}
