package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/agent"
)

// Host defaults for the HITL permission policy (§5.7.7); definition values
// override them.
const (
	defaultPermissionTimeout = 600 * time.Second
	defaultMaxPending        = 32
)

// nowFunc is the permission clock; tests override it to drive TTL expiry.
var nowFunc = time.Now

// permissionInterruptInfo is the user-facing payload the approval gate
// attaches to its interrupt. It travels inside the checkpoint (gob), so the
// type is registered for serialization.
type permissionInterruptInfo struct {
	CallID       string
	MCPServiceID string
	ToolName     string
	Arguments    string
}

// permissionDecision is the per-call resume payload. It is delivered
// in-process through ResumeParams.Targets and never serialized.
type permissionDecision struct {
	Allow bool
}

func init() {
	schema.RegisterName[*permissionInterruptInfo]("agw_builtin_permission_interrupt")
}

// deniedToolResult is what the model sees for a refused call: a plain tool
// result, not an error, so the turn continues with the refusal on record.
func deniedToolResult(toolName string) string {
	return fmt.Sprintf("Tool %q execution was denied by the operator. Do not retry it; continue without its result.", toolName)
}

// gatedTool is the interactive-mode approval gate over one MCP tool. It wraps
// the observed tool (outermost), so an interrupted or denied call never opens
// an MCP child span — only approved executions reach the bridge.
type gatedTool struct {
	inner     tool.InvokableTool
	serviceID string
	toolName  string
}

// newGatedTool wraps invokable tools; anything else is returned unchanged
// (a non-invokable tool cannot execute, so there is nothing to gate).
func newGatedTool(inner tool.BaseTool, serviceID, toolName string) tool.BaseTool {
	invokable, ok := inner.(tool.InvokableTool)
	if !ok {
		return inner
	}
	return &gatedTool{inner: invokable, serviceID: serviceID, toolName: toolName}
}

func (t *gatedTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx)
}

func (t *gatedTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	isResumeFlow, hasData, decision := compose.GetResumeContext[*permissionDecision](ctx)
	if isResumeFlow {
		// Fail-closed: only an explicit allow executes; a missing or nil
		// decision payload is a deny.
		if hasData && decision != nil && decision.Allow {
			return t.inner.InvokableRun(ctx, argumentsInJSON, opts...)
		}
		return deniedToolResult(t.toolName), nil
	}
	// First run, or a resume that targets a different interrupt point: pause
	// (or re-pause, preserving this call's pending state) for a decision.
	return "", compose.Interrupt(ctx, &permissionInterruptInfo{
		CallID:       compose.GetToolCallID(ctx),
		MCPServiceID: t.serviceID,
		ToolName:     t.toolName,
		Arguments:    argumentsInJSON,
	})
}

// memCheckPointStore is the in-process ADK checkpoint store. Entries are
// transient interrupt state with the same restart-loss semantics as sessions;
// durable Runner persistence waits for eino v0.10 and this store is the swap
// seam.
type memCheckPointStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemCheckPointStore() *memCheckPointStore {
	return &memCheckPointStore{data: map[string][]byte{}}
}

func (s *memCheckPointStore) Get(_ context.Context, checkPointID string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.data[checkPointID]
	return data, ok, nil
}

func (s *memCheckPointStore) Set(_ context.Context, checkPointID string, checkPoint []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[checkPointID] = checkPoint
	return nil
}

func (s *memCheckPointStore) Delete(_ context.Context, checkPointID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, checkPointID)
	return nil
}

// pendingCall is one gated tool call awaiting a decision.
type pendingCall struct {
	// targetID is the InterruptCtx address id — the ResumeParams.Targets key.
	targetID     string
	CallID       string `json:"call_id"`
	MCPServiceID string `json:"mcp_service_id"`
	ToolName     string `json:"name"`
	Arguments    string `json:"arguments,omitempty"`
}

// pendingPermission is one suspended turn: the checkpoint id doubles as the
// request id, and the entry carries everything the resumed completion needs
// to commit the full exchange.
type pendingPermission struct {
	requestID   string
	agentID     string
	sessionID   string
	runID       string
	eventCursor ContinuationCursor
	// link* is the interaction span that produced this checkpoint. A resume
	// starts a new trace and links back to this asynchronous predecessor.
	linkTraceID   string
	linkSpanID    string
	routeID       string
	routeProtocol string
	virtualKeyID  string
	agentDepth    int
	// updatedAt guards materialization identity: a checkpoint must resume on
	// the graph that produced it, so a definition update invalidates the
	// pending permission.
	updatedAt  time.Time
	expiresAt  time.Time
	createdAt  time.Time
	calls      []pendingCall
	userMsg    *schema.Message
	transcript []*schema.Message
}

// permissionRegistry is the backend-owned opaque continuation store. Entries
// are one-shot on resume, but it never owns expiry: the common Agent permission
// broker claims expiry and calls Host.ExpirePermission for cleanup.
type permissionRegistry struct {
	mu        sync.Mutex
	byRequest map[string]*pendingPermission
}

func newPermissionRegistry() *permissionRegistry {
	return &permissionRegistry{byRequest: map[string]*pendingPermission{}}
}

func newPermissionRequestID() (string, error) {
	return newOpaqueID("perm-")
}

func newRunID() (string, error) {
	return newOpaqueID("run-")
}

func newOpaqueID(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate %s id: %w", strings.TrimSuffix(prefix, "-"), err)
	}
	return prefix + hex.EncodeToString(raw), nil
}

// register stores a pending permission after checking the per-agent cap.
// A true result reports a fail-closed rejection; only the common broker
// performs expiry cleanup.
func (r *permissionRegistry) register(p *pendingPermission, maxPending int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	live := 0
	for _, existing := range r.byRequest {
		if existing.agentID == p.agentID {
			live++
		}
	}
	// A re-interrupt re-registers under its original request id; replacing an
	// entry never counts against the cap.
	if _, replacing := r.byRequest[p.requestID]; !replacing && live >= maxPending {
		return true
	}
	r.byRequest[p.requestID] = p
	return false
}

// take removes and returns the pending permission (one-shot, fail-closed:
// whatever happens next, the entry cannot resolve twice).
func (r *permissionRegistry) take(requestID string) (*pendingPermission, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byRequest[requestID]
	if ok {
		delete(r.byRequest, requestID)
	}
	return p, ok
}

func (r *permissionRegistry) discard(agentID, requestID string) (*pendingPermission, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.byRequest[requestID]
	if p == nil || p.agentID != agentID {
		return nil, false
	}
	delete(r.byRequest, requestID)
	return p, true
}

func (r *permissionRegistry) storeCursor(agentID, requestID string, cursor ContinuationCursor) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.byRequest[requestID]
	if p == nil || p.agentID != agentID || p.runID != cursor.RunID {
		return false
	}
	p.eventCursor = cursor
	return true
}

func (r *permissionRegistry) loadCursor(agentID, requestID string) (ContinuationCursor, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.byRequest[requestID]
	if p == nil || p.agentID != agentID || p.eventCursor.RunID == "" {
		return ContinuationCursor{}, false
	}
	return p.eventCursor, true
}

// liveForSession returns the live pending request id bound to a session.
func (r *permissionRegistry) liveForSession(agentID, sessionID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.byRequest {
		if p.agentID == agentID && p.sessionID == sessionID {
			return id, true
		}
	}
	return "", false
}

// PendingPermissionView is the admin runtime view of one suspended turn.
type PendingPermissionView struct {
	RequestID string                  `json:"request_id"`
	AgentID   string                  `json:"agent_id"`
	SessionID string                  `json:"session_id"`
	RunID     string                  `json:"run_id"`
	CreatedAt time.Time               `json:"created_at"`
	ExpiresAt time.Time               `json:"expires_at"`
	Calls     []PendingPermissionCall `json:"calls"`
}

// PendingPermissionCall is the admin/SSE view of one gated tool call.
type PendingPermissionCall struct {
	CallID       string `json:"call_id"`
	MCPServiceID string `json:"mcp_service_id"`
	ToolName     string `json:"name"`
	Arguments    string `json:"arguments,omitempty"`
}

func (r *permissionRegistry) list() []PendingPermissionView {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]PendingPermissionView, 0, len(r.byRequest))
	for _, p := range r.byRequest {
		out = append(out, pendingView(p))
	}
	return out
}

func pendingView(p *pendingPermission) PendingPermissionView {
	view := PendingPermissionView{
		RequestID: p.requestID,
		AgentID:   p.agentID,
		SessionID: p.sessionID,
		RunID:     p.runID,
		CreatedAt: p.createdAt,
		ExpiresAt: p.expiresAt,
		Calls:     make([]PendingPermissionCall, 0, len(p.calls)),
	}
	for _, c := range p.calls {
		view.Calls = append(view.Calls, PendingPermissionCall{
			CallID:       c.CallID,
			MCPServiceID: c.MCPServiceID,
			ToolName:     c.ToolName,
			Arguments:    c.Arguments,
		})
	}
	return view
}

// permissionTimeout resolves the definition's pending-decision TTL.
func permissionTimeout(p *agent.BuiltinPermissions) time.Duration {
	if p != nil && p.TimeoutSeconds > 0 {
		return time.Duration(p.TimeoutSeconds) * time.Second
	}
	return defaultPermissionTimeout
}

// permissionMaxPending resolves the definition's pending cap.
func permissionMaxPending(p *agent.BuiltinPermissions) int {
	if p != nil && p.MaxPending > 0 {
		return p.MaxPending
	}
	return defaultMaxPending
}
