package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	acpruntime "github.com/agent-guide/agent-gateway/pkg/acp/runtime"
	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	configschema "github.com/agent-guide/agent-gateway/pkg/configstore/schema"
	configstoresqlite "github.com/agent-guide/agent-gateway/pkg/configstore/sqlite"
	"github.com/agent-guide/agent-gateway/pkg/gateway"
	"github.com/agent-guide/agent-gateway/pkg/gateway/acproute"
)

type acpControlRuntime struct {
	permission     acpruntime.PermissionDecision
	permissionErr  error
	serviceID      string
	listReq        acpruntime.ListSessionsRequest
	listResp       acpruntime.ListSessionsResponse
	listErr        error
	transcriptReq  acpruntime.TranscriptRequest
	transcriptResp acpruntime.TranscriptResponse
	transcriptErr  error
}

type fixedAgentAttributor string

func (a fixedAgentAttributor) ResolveAgentID(string, string, string) (string, bool) {
	return string(a), a != ""
}

func (f *acpControlRuntime) ResolvePermission(decision acpruntime.PermissionDecision) error {
	f.permission = decision
	return f.permissionErr
}

func (f *acpControlRuntime) ListSessions(_ context.Context, serviceID string, req acpruntime.ListSessionsRequest) (acpruntime.ListSessionsResponse, error) {
	f.serviceID, f.listReq = serviceID, req
	return f.listResp, f.listErr
}

func (f *acpControlRuntime) LoadTranscript(_ context.Context, serviceID string, req acpruntime.TranscriptRequest) (acpruntime.TranscriptResponse, error) {
	f.serviceID, f.transcriptReq = serviceID, req
	return f.transcriptResp, f.transcriptErr
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (r *flushRecorder) Flush() {
	r.flushes++
	r.ResponseRecorder.Flush()
}

// wrappedResponseWriter exposes Flush only through Unwrap, reproducing the
// Caddy v2.7+ ResponseWriter that no longer satisfies a direct http.Flusher
// assertion.
type wrappedResponseWriter struct {
	inner *flushRecorder
}

type headerCountingWriter struct {
	*httptest.ResponseRecorder
	writeHeaders int
}

func (w *headerCountingWriter) WriteHeader(status int) {
	w.writeHeaders++
	w.ResponseRecorder.WriteHeader(status)
}

func (w *headerCountingWriter) Flush() { w.ResponseRecorder.Flush() }

func (w *wrappedResponseWriter) Header() http.Header         { return w.inner.Header() }
func (w *wrappedResponseWriter) Write(p []byte) (int, error) { return w.inner.Write(p) }
func (w *wrappedResponseWriter) WriteHeader(status int)      { w.inner.WriteHeader(status) }
func (w *wrappedResponseWriter) Unwrap() http.ResponseWriter { return w.inner }

func TestACPSSESinkFlushesThroughUnwrapChain(t *testing.T) {
	t.Parallel()

	inner := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := &wrappedResponseWriter{inner: inner}
	if _, ok := any(w).(http.Flusher); ok {
		t.Fatal("wrapped response writer must not directly implement http.Flusher")
	}

	emit := newACPSSESink(w)
	if err := emit(acpruntime.TurnEvent{Event: "content", Text: "hello"}); err != nil {
		t.Fatalf("emit returned error: %v", err)
	}

	if inner.flushes == 0 {
		t.Fatal("expected SSE frame to flush through wrapped response writer Unwrap chain")
	}
	body := inner.Body.String()
	if !strings.HasPrefix(body, "event: content\n") {
		t.Fatalf("unexpected SSE event line: %q", body)
	}
	if !strings.Contains(body, `"text":"hello"`) {
		t.Fatalf("expected event payload in SSE frame, got %q", body)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("expected SSE frame to end with a blank line, got %q", body)
	}
}

func TestACPSSESinkDefaultsEventNameToDelta(t *testing.T) {
	t.Parallel()

	inner := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := &wrappedResponseWriter{inner: inner}

	emit := newACPSSESink(w)
	if err := emit(acpruntime.TurnEvent{Text: "chunk"}); err != nil {
		t.Fatalf("emit returned error: %v", err)
	}

	if !strings.HasPrefix(inner.Body.String(), "event: delta\n") {
		t.Fatalf("expected empty event name to default to delta, got %q", inner.Body.String())
	}
}

func TestACPRequestErrorStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "service not configured", err: acpservice.ErrServiceNotConfigured, want: http.StatusNotFound},
		{name: "store not found", err: configstore.ErrNotFound, want: http.StatusNotFound},
		{name: "wrapped service not configured", err: fmt.Errorf("get service: %w", acpservice.ErrServiceNotConfigured), want: http.StatusNotFound},
		{name: "invalid request", err: acpruntime.ErrInvalidRequest, want: http.StatusBadRequest},
		{name: "wrapped invalid request", err: fmt.Errorf("%w: cwd is outside allowed_roots", acpruntime.ErrInvalidRequest), want: http.StatusBadRequest},
		{name: "upstream failure", err: errors.New("initialize: transport closed"), want: http.StatusBadGateway},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := acpRequestErrorStatus(tt.err); got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMatchACPRouteEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		path        string
		wantMatch   bool
		wantKind    string
		wantSession string
	}{
		{name: "turn", path: "/turn", wantMatch: true, wantKind: "turn"},
		{name: "permission", path: "/permission", wantMatch: true, wantKind: "permission"},
		{name: "sessions", path: "/sessions", wantMatch: true, wantKind: "sessions"},
		{name: "transcript", path: "/sessions/sess-1/transcript", wantMatch: true, wantKind: "transcript", wantSession: "sess-1"},
		{name: "escaped transcript", path: "/sessions/sess%201/transcript", wantMatch: true, wantKind: "transcript", wantSession: "sess 1"},
		{name: "unknown", path: "/health", wantMatch: false},
		{name: "missing session id", path: "/sessions//transcript", wantMatch: false},
		{name: "nested session id", path: "/sessions/a/b/transcript", wantMatch: false},
		{name: "sessions child", path: "/sessions/sess-1", wantMatch: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotKind, gotSession, gotMatch := matchACPRouteEndpoint(tt.path)
			if gotMatch != tt.wantMatch {
				t.Fatalf("match = %v, want %v", gotMatch, tt.wantMatch)
			}
			if gotKind != tt.wantKind {
				t.Fatalf("kind = %q, want %q", gotKind, tt.wantKind)
			}
			if gotSession != tt.wantSession {
				t.Fatalf("session = %q, want %q", gotSession, tt.wantSession)
			}
		})
	}
}

func TestACPControlSubrouteContractInventory(t *testing.T) {
	h := &Handler{}
	t.Run("permission nested outcome and one-shot status", func(t *testing.T) {
		fake := &acpControlRuntime{}
		req := httptest.NewRequest(http.MethodPost, "/permission", strings.NewReader(`{"request_id":"perm-1","outcome":"selected","option_id":"allow_once"}`))
		rec := httptest.NewRecorder()
		if err := h.dispatchACPPermission(rec, req, fake); err != nil {
			t.Fatalf("dispatch permission: %v", err)
		}
		if rec.Code != http.StatusOK || fake.permission.RequestID != "perm-1" || fake.permission.Outcome != "selected" || fake.permission.OptionID != "allow_once" {
			t.Fatalf("permission response=%d body=%s decision=%+v", rec.Code, rec.Body.String(), fake.permission)
		}
		fake.permissionErr = acpruntime.ErrPermissionNotFound
		rec = httptest.NewRecorder()
		if err := h.dispatchACPPermission(rec, httptest.NewRequest(http.MethodPost, "/permission", strings.NewReader(`{"request_id":"perm-1","outcome":"cancelled"}`)), fake); err != nil {
			t.Fatalf("dispatch missing permission: %v", err)
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("missing permission status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("sessions forwards bounded filters", func(t *testing.T) {
		fake := &acpControlRuntime{listResp: acpruntime.ListSessionsResponse{Sessions: []acpruntime.SessionInfo{{SessionID: "s1", Title: "One"}}, NextCursor: "next"}}
		req := httptest.NewRequest(http.MethodGet, "/sessions?cwd=%2Fworkspace&cursor=cur", nil)
		rec := httptest.NewRecorder()
		if err := h.dispatchACPSessions(rec, req, fake, "svc-1"); err != nil {
			t.Fatalf("dispatch sessions: %v", err)
		}
		if rec.Code != http.StatusOK || fake.serviceID != "svc-1" || fake.listReq.CWD != "/workspace" || fake.listReq.Cursor != "cur" {
			t.Fatalf("sessions status=%d req=%+v", rec.Code, fake.listReq)
		}
		var body acpruntime.ListSessionsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Sessions) != 1 || body.NextCursor != "next" {
			t.Fatalf("sessions body=%s err=%v", rec.Body.String(), err)
		}
	})

	t.Run("transcript preserves service session and cwd", func(t *testing.T) {
		fake := &acpControlRuntime{transcriptResp: acpruntime.TranscriptResponse{SessionID: "s1", Messages: []acpruntime.TranscriptMessage{{Role: "assistant", Text: "hello"}}}}
		req := httptest.NewRequest(http.MethodGet, "/sessions/s1/transcript?cwd=%2Fworkspace", nil)
		rec := httptest.NewRecorder()
		if err := h.dispatchACPTranscript(rec, req, fake, "svc-1", "s1"); err != nil {
			t.Fatalf("dispatch transcript: %v", err)
		}
		if rec.Code != http.StatusOK || fake.serviceID != "svc-1" || fake.transcriptReq.SessionID != "s1" || fake.transcriptReq.CWD != "/workspace" {
			t.Fatalf("transcript status=%d req=%+v body=%s", rec.Code, fake.transcriptReq, rec.Body.String())
		}
	})
}

func TestDispatchACPBoundAndUnboundMigrationPaths(t *testing.T) {
	t.Skip("legacy ACP ingress removed by unified Agent runtime M5")
	ctx := t.Context()
	backend, err := configstore.OpenBackend(ctx, "sqlite", configstoresqlite.Config{SQLitePath: t.TempDir() + "/config.db"}, nil)
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	if err := configschema.RegisterDefaultStores(backend); err != nil {
		t.Fatalf("register stores: %v", err)
	}
	attribution := usage.NewAgentAttribution()
	usageSink := &usage.InMemorySink{}
	gw := gateway.NewAgentGateway()
	if err := gw.Bootstrap(ctx, gateway.BootstrapOptions{
		ConfigStoreBackend: backend,
		UsageObserver:      usage.NewObserverWithAttribution(usageSink, attribution),
	}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	attribution.Set(gw.AgentManager())

	cwd := t.TempDir()
	service := acpservice.ServiceConfig{
		ID: "disabled-service", Name: "Disabled", AgentType: "opencode",
		CWD: cwd, AllowedRoots: []string{cwd}, Disabled: true,
	}
	if err := gw.ACPServiceManager().Create(ctx, service); err != nil {
		t.Fatalf("create service: %v", err)
	}
	route := acproute.ACPRouteConfig{
		AgentRouteConfig: acproute.AgentRouteConfig{MatchPolicy: acproute.RouteMatch{PathPrefix: "/acp-test"}},
		ServiceID:        service.ID,
	}
	route.Normalize()
	routeConfig, err := route.ToConfig()
	if err != nil {
		t.Fatalf("route config: %v", err)
	}
	if err := gw.ACPRouteResolver().CreateConfig(ctx, routeConfig, "test"); err != nil {
		t.Fatalf("create route: %v", err)
	}
	handler := NewHandler(gw, nil, zap.NewNop(), HandlerOptions{})
	body := `{"thread_id":"thread-1","input":"hello"}`

	// Before an Agent owns the route, the sole M2 exception remains native:
	// service failure is an SSE error on HTTP 200, with exactly one header write.
	unbound := &headerCountingWriter{ResponseRecorder: httptest.NewRecorder()}
	if err := handler.Dispatch(unbound, httptest.NewRequest(http.MethodPost, "/acp-test/turn", strings.NewReader(body)), nil); err != nil {
		t.Fatalf("dispatch unbound: %v", err)
	}
	if unbound.Code != http.StatusOK || unbound.writeHeaders != 1 || unbound.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(unbound.Body.String(), "event: error") {
		t.Fatalf("unbound response status=%d headers=%d content-type=%q body=%q", unbound.Code, unbound.writeHeaders, unbound.Header().Get("Content-Type"), unbound.Body.String())
	}

	if err := gw.AgentManager().Create(ctx, agentpkg.Agent{
		ID: "bound-agent", Name: "Bound", Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeACP, ACP: &agentpkg.ACPRuntime{ServiceID: service.ID}},
		Routes: agentpkg.Routes{ACPRouteIDs: []string{route.ID}},
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	bound := &headerCountingWriter{ResponseRecorder: httptest.NewRecorder()}
	if err := handler.Dispatch(bound, httptest.NewRequest(http.MethodPost, "/acp-test/turn", strings.NewReader(body)), nil); err != nil {
		t.Fatalf("dispatch bound: %v", err)
	}
	if bound.Code != http.StatusBadRequest || bound.writeHeaders != 1 || bound.Header().Get("Content-Type") != "application/json" || !strings.Contains(bound.Body.String(), "agent is disabled") {
		t.Fatalf("bound response status=%d headers=%d content-type=%q body=%q", bound.Code, bound.writeHeaders, bound.Header().Get("Content-Type"), bound.Body.String())
	}
	acpEvents := eventsOfType[usage.ACPUsageEvent](usageSink.Events)
	if len(acpEvents) == 0 {
		t.Fatal("bound pre-stream failure emitted no ACP usage event")
	}
	boundEvent := acpEvents[len(acpEvents)-1]
	if boundEvent.Success || boundEvent.ResultStatus != "error" || boundEvent.StatusCode != http.StatusBadRequest {
		t.Fatalf("bound pre-stream usage event = %+v, want failed/error/400", boundEvent)
	}

	// Updating a legacy route does not revalidate the Agent that owns it. The
	// dispatcher must fail closed instead of executing the Agent's stale service
	// while attributing the turn to the route's new service.
	replacement := service
	replacement.ID = "replacement-service"
	replacement.Name = "Replacement"
	if err := gw.ACPServiceManager().Create(ctx, replacement); err != nil {
		t.Fatalf("create replacement service: %v", err)
	}
	mismatchedRoute := route
	mismatchedRoute.ServiceID = replacement.ID
	mismatchedConfig, err := mismatchedRoute.ToConfig()
	if err != nil {
		t.Fatalf("mismatched route config: %v", err)
	}
	if err := gw.ACPRouteResolver().UpdateConfig(ctx, route.ID, mismatchedConfig); err != nil {
		t.Fatalf("update route service: %v", err)
	}
	mismatched := &headerCountingWriter{ResponseRecorder: httptest.NewRecorder()}
	if err := handler.Dispatch(mismatched, httptest.NewRequest(http.MethodPost, "/acp-test/turn", strings.NewReader(body)), nil); err != nil {
		t.Fatalf("dispatch mismatched route: %v", err)
	}
	if mismatched.Code != http.StatusNotImplemented || mismatched.writeHeaders != 1 || mismatched.Header().Get("Content-Type") != "application/json" || !strings.Contains(mismatched.Body.String(), "agent runtime is not executable") {
		t.Fatalf("mismatched response status=%d headers=%d content-type=%q body=%q", mismatched.Code, mismatched.writeHeaders, mismatched.Header().Get("Content-Type"), mismatched.Body.String())
	}

	// A stale attribution pointing at a deleted Agent is a normalized 404,
	// distinct from an operational Agent-store failure.
	attribution.Set(fixedAgentAttributor("deleted-agent"))
	missing := &headerCountingWriter{ResponseRecorder: httptest.NewRecorder()}
	if err := handler.Dispatch(missing, httptest.NewRequest(http.MethodPost, "/acp-test/turn", strings.NewReader(body)), nil); err != nil {
		t.Fatalf("dispatch missing agent: %v", err)
	}
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "agent not found") {
		t.Fatalf("missing-agent response status=%d body=%q", missing.Code, missing.Body.String())
	}
}
