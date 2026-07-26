package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/mcp/einotool"
)

const (
	defaultMaxConcurrentTurns = 4
	defaultTurnTimeout        = 600 * time.Second
)

// Config wires the host's collaborator seams.
type Config struct {
	Agents   AgentSource
	Models   ChatModelResolver
	Tools    einotool.ToolCaller
	Observer usage.InteractionObserver
}

// Host is the generic ADK host. One instance serves every builtin agent:
// definitions are materialized lazily and cached keyed by agent id +
// updated_at, so a definition update re-materializes on the next turn while
// in-flight turns drain on the old graph.
type Host struct {
	agents   AgentSource
	models   ChatModelResolver
	tools    einotool.ToolCaller
	observer usage.InteractionObserver

	mu       sync.Mutex
	entries  map[string]*hostEntry
	sessions *sessionStore
	// checkpoints and permissions carry suspended interactive turns (§5.7.7).
	// Both are in-memory with the same restart-loss semantics as sessions.
	checkpoints *memCheckPointStore
	permissions *permissionRegistry
	// activity tracks in-flight ADK turns so an operator can force-cancel or
	// gracefully stop a running (or stuck) turn (agents-control-plane.md §10).
	activity *activityRegistry
}

type hostEntry struct {
	updatedAt    time.Time
	runner       *adk.Runner
	topologyKind string
	turnTimeout  time.Duration
	turnSem      chan struct{}

	mu      sync.Mutex
	turns   int
	buildAt time.Time
}

func NewHost(cfg Config) *Host {
	return &Host{
		agents:      cfg.Agents,
		models:      cfg.Models,
		tools:       cfg.Tools,
		observer:    cfg.Observer,
		entries:     map[string]*hostEntry{},
		sessions:    newSessionStore(),
		checkpoints: newMemCheckPointStore(),
		permissions: newPermissionRegistry(),
		activity:    newActivityRegistry(),
	}
}

// EntryState is the workspace-facing materialization view of one agent.
type EntryState struct {
	Materialized   bool      `json:"materialized"`
	MaterializedAt time.Time `json:"materialized_at,omitzero"`
	TopologyKind   string    `json:"topology_kind,omitempty"`
	InflightTurns  int       `json:"inflight_turns"`
	LiveSessions   int       `json:"live_sessions"`
}

// State reports the materialization/runtime state for the workspace view.
func (h *Host) State(agentID string) EntryState {
	if h == nil {
		return EntryState{}
	}
	h.mu.Lock()
	entry := h.entries[agentID]
	h.mu.Unlock()
	state := EntryState{LiveSessions: h.sessions.sessionCount(agentID)}
	if entry != nil {
		entry.mu.Lock()
		state.Materialized = true
		state.MaterializedAt = entry.buildAt
		state.TopologyKind = entry.topologyKind
		state.InflightTurns = entry.turns
		entry.mu.Unlock()
	}
	return state
}

// StoreContinuationCursor attaches common event-ordering state to a pending
// permission checkpoint. It fails closed if the checkpoint identity no longer
// exists or belongs to another Agent/run.
func (h *Host) StoreContinuationCursor(agentID, requestID string, cursor ContinuationCursor) bool {
	if h == nil || h.permissions == nil {
		return false
	}
	return h.permissions.storeCursor(agentID, requestID, cursor)
}

// LoadContinuationCursor reads, but does not consume, the cursor attached to
// a pending permission. The Host's one-shot checkpoint take owns deletion, so
// a request rejected before take may be corrected and retried.
func (h *Host) LoadContinuationCursor(agentID, requestID string) (ContinuationCursor, bool) {
	if h == nil || h.permissions == nil {
		return ContinuationCursor{}, false
	}
	return h.permissions.loadCursor(agentID, requestID)
}

// ServeTurn runs one turn of a builtin agent and streams events to emit.
// Client-correctable failures wrap ErrInvalidRequest; concurrency rejection
// wraps ErrTurnLimitExceeded; unknown agents wrap ErrAgentNotFound.
func (h *Host) ServeTurn(ctx context.Context, agentID string, req TurnRequest, emit EventSink) error {
	if h == nil {
		return fmt.Errorf("builtin host is not configured")
	}
	a, err := h.agents.Get(ctx, agentID)
	if err != nil {
		if errors.Is(err, agent.ErrAgentNotConfigured) {
			return fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
		}
		return err
	}
	if a.Runtime.Type != agent.RuntimeTypeBuiltin || a.Runtime.Builtin == nil {
		return fmt.Errorf("%w: agent %q is not a builtin-runtime agent", ErrInvalidRequest, agentID)
	}
	if a.Disabled {
		return fmt.Errorf("%w: agent %q is disabled", ErrInvalidRequest, agentID)
	}
	if req.Input != "" && req.Permission != nil {
		return fmt.Errorf("%w: input and permission are mutually exclusive", ErrInvalidRequest)
	}
	if req.Permission != nil {
		return h.servePermissionTurn(ctx, a, req, emit)
	}
	if req.Input == "" {
		return fmt.Errorf("%w: input is required", ErrInvalidRequest)
	}
	if dims, ok := usage.DimensionsFromContext(ctx); ok {
		if maxDepth := a.Policy.MaxAgentDepth; maxDepth > 0 && dims.AgentDepth >= maxDepth {
			return fmt.Errorf("%w: agent depth limit exceeded", ErrInvalidRequest)
		}
	}

	entry, err := h.entry(ctx, a)
	if err != nil {
		return err
	}

	// The turn timeout spans queueing and execution: it bounds the wait for
	// the session slot below as well as the ADK run.
	turnCtx, cancel := context.WithTimeout(ctx, entry.turnTimeout)
	defer cancel()

	// begin serializes turns on the same session: a concurrent turn for this
	// session id waits here until the in-flight one commits or releases, so
	// histories never interleave or lose an exchange. The wait is
	// context-aware and happens BEFORE the concurrency semaphore, so waiting
	// same-session turns never occupy max_concurrent_turns slots.
	handle, history, err := h.sessions.begin(turnCtx, agentID, req.SessionID)
	if err != nil {
		return err
	}
	sessionID := handle.sessionID()
	runID := req.RunID
	if runID == "" {
		// Direct Host callers remain supported until M2 moves every shipping
		// path behind runtimeapi. Route execution supplies this id at the common
		// boundary and the Host preserves it verbatim.
		runID, err = newRunID()
		if err != nil {
			handle.release()
			return err
		}
	}
	turnCtx = withBuiltinRunDimensions(turnCtx, agentID, runID)
	// A session suspended on a pending permission only moves forward through
	// an explicit resume or cancel; new input is rejected rather than
	// silently discarding the suspended work (§5.7.7).
	if pendingID, live := h.permissions.liveForSession(agentID, sessionID); true {
		if live {
			handle.release()
			return fmt.Errorf("%w: session has pending permission request %q; resume or cancel it first", ErrInvalidRequest, pendingID)
		}
	}
	if mw := a.Runtime.Builtin.Middlewares; mw != nil && mw.PlanTask != nil && mw.PlanTask.Enabled {
		// The plantask backend is stateless; the session's task board travels
		// on the turn context so task tools operate session-scoped storage.
		turnCtx = withTaskBoard(turnCtx, handle.board())
	}

	select {
	case entry.turnSem <- struct{}{}:
	default:
		handle.release()
		return ErrTurnLimitExceeded
	}
	entry.mu.Lock()
	entry.turns++
	entry.mu.Unlock()
	defer func() {
		entry.mu.Lock()
		entry.turns--
		entry.mu.Unlock()
		<-entry.turnSem
	}()
	span := usage.SpanFromContext(ctx)
	span.SetExtension(usage.CommonExtension{AgentID: agentID, RuntimeType: agent.RuntimeTypeBuiltin, RunID: runID})
	span.SetExtension(usage.BuiltinExtension{
		Operation:    "turn",
		SessionID:    sessionID,
		RunID:        runID,
		TopologyKind: entry.topologyKind,
	})

	userMsg := schema.UserMessage(req.Input)
	input := make([]*schema.Message, 0, len(history)+1)
	input = append(input, history...)
	input = append(input, userMsg)

	// Interactive mode runs under a preallocated checkpoint id so a
	// tool-permission interrupt can suspend the turn; the id doubles as the
	// permission request id. No checkpoint is written unless an interrupt
	// happens.
	var runOpts []adk.AgentRunOption
	requestID := ""
	if a.Runtime.Builtin.Permissions.Interactive() {
		requestID, err = newPermissionRequestID()
		if err != nil {
			handle.release()
			return err
		}
		runOpts = append(runOpts, adk.WithCheckPointID(requestID))
	}
	runEmit := correlatedSink(emit, runID, sessionID, "")
	if err := runEmit(TurnEvent{Event: EventSession}); err != nil {
		handle.release()
		return err
	}

	// WithCancel exposes an operator cancel handle for this run; register it so
	// force/graceful cancel can reach a running (or stuck) turn, and deregister
	// once the run returns.
	cancelOpt, cancelFn := adk.WithCancel()
	runOpts = append(runOpts, cancelOpt)
	activity := &inflightTurn{
		agentID:      agentID,
		sessionID:    sessionID,
		runID:        runID,
		requestID:    requestID,
		operation:    "turn",
		topologyKind: entry.topologyKind,
		startedAt:    time.Now().UTC(),
		cancel:       cancelFn,
	}
	h.activity.register(activity)
	defer h.activity.deregister(activity)

	result, runErr := h.runTurn(turnCtx, entry, input, runEmit, runOpts...)
	usage.SpanFromContext(ctx).SetExtension(usage.BuiltinExtension{
		ModelSteps:  usage.Int(result.modelSteps),
		ToolSteps:   usage.Int(result.toolSteps),
		EventCounts: result.eventCounts,
	})
	if runErr != nil {
		if requestID != "" {
			// An error (or cancel) after a saved interrupt would orphan the
			// checkpoint.
			_ = h.checkpoints.Delete(context.Background(), requestID)
		}
		handle.release()
		var cancelErr *adk.CancelError
		if errors.As(runErr, &cancelErr) {
			// Operator cancel: the turn was aborted, so the partial exchange is
			// discarded (released above, not committed) and the client gets a
			// clean cancelled terminal instead of an error.
			usage.SpanFromContext(ctx).SetExtension(usage.BuiltinExtension{ResultStatus: "cancelled"})
			return runEmit(TurnEvent{Event: EventDone, StopReason: StopReasonCancelled})
		}
		usage.SpanFromContext(ctx).SetExtension(usage.BuiltinExtension{ResultStatus: "error"})
		_ = runEmit(TurnEvent{Event: EventError, Message: runErr.Error()})
		return runErr
	}
	if result.interrupt != nil {
		return h.suspendTurn(ctx, a, handle, runID, requestID, userMsg, nil, result, runEmit)
	}
	handle.commit(append([]*schema.Message{userMsg}, result.transcript...))
	usage.SpanFromContext(ctx).SetExtension(usage.BuiltinExtension{ResultStatus: "success"})
	return runEmit(TurnEvent{Event: EventDone, StopReason: "end_turn"})
}

// suspendTurn parks an interrupted interactive turn: it registers the pending
// permission (fail-closed on capacity), releases the session, and tells the
// client what needs deciding. priorTranscript carries messages accumulated by
// earlier segments of the same suspended turn.
func (h *Host) suspendTurn(ctx context.Context, a agent.Agent, handle *sessionHandle, runID, requestID string, userMsg *schema.Message, priorTranscript []*schema.Message, result turnResult, emit EventSink) error {
	calls := pendingCallsFromInterrupt(result.interrupt)
	if requestID == "" || len(calls) == 0 {
		// Only the approval gate interrupts in a builtin graph; anything else
		// (or an interrupt without a checkpoint id) is unresumable.
		if requestID != "" {
			_ = h.checkpoints.Delete(context.Background(), requestID)
		}
		handle.release()
		usage.SpanFromContext(ctx).SetExtension(usage.BuiltinExtension{ResultStatus: "error"})
		err := fmt.Errorf("builtin agent produced an unsupported interrupt")
		_ = emit(TurnEvent{Event: EventError, Message: err.Error()})
		return err
	}
	perms := a.Runtime.Builtin.Permissions
	now := nowFunc()
	dims, _ := usage.DimensionsFromContext(ctx)
	pending := &pendingPermission{
		requestID:     requestID,
		agentID:       a.ID,
		sessionID:     handle.sessionID(),
		runID:         runID,
		linkTraceID:   dims.TraceID,
		linkSpanID:    dims.SpanID,
		routeID:       dims.RouteID,
		routeProtocol: dims.RouteProtocol,
		virtualKeyID:  dims.VirtualKeyID,
		agentDepth:    dims.AgentDepth,
		updatedAt:     a.UpdatedAt,
		createdAt:     now,
		expiresAt:     now.Add(permissionTimeout(perms)),
		calls:         calls,
		userMsg:       userMsg,
		transcript:    append(priorTranscript, result.transcript...),
	}
	capReached := h.permissions.register(pending, permissionMaxPending(perms))
	if capReached {
		_ = h.checkpoints.Delete(context.Background(), requestID)
		handle.release()
		usage.SpanFromContext(ctx).SetExtension(usage.BuiltinExtension{ResultStatus: "error"})
		_ = emit(TurnEvent{Event: EventError, Message: ErrPermissionCapacity.Error()})
		return ErrPermissionCapacity
	}
	h.sessions.setPendingPermission(a.ID, handle.sessionID(), true)
	handle.release()
	span := usage.SpanFromContext(ctx)
	span.AddAnnotation("permission_request_id", requestID)
	result.eventCounts[EventPermission]++
	span.SetExtension(usage.BuiltinExtension{
		RunID:               runID,
		PermissionRequestID: requestID,
		ResultStatus:        "interrupted",
		EventCounts:         result.eventCounts,
	})
	if err := emit(TurnEvent{Event: EventPermission, RequestID: requestID, Data: permissionEventData(pending)}); err != nil {
		return err
	}
	return emit(TurnEvent{Event: EventDone, RequestID: requestID, StopReason: StopReasonPermissionRequired})
}

// servePermissionTurn resolves a suspended turn: cancel discards it, and
// per-call decisions resume the checkpointed run on a fresh SSE stream.
// Pending entries are one-shot — taken before any execution — so a request
// resolves at most once no matter what happens next.
func (h *Host) servePermissionTurn(ctx context.Context, a agent.Agent, req TurnRequest, emit EventSink) error {
	perm := req.Permission
	requestID := strings.TrimSpace(perm.RequestID)
	if requestID == "" {
		return fmt.Errorf("%w: permission.request_id is required", ErrInvalidRequest)
	}
	if perm.Outcome != "" && perm.Outcome != "cancel" {
		return fmt.Errorf("%w: permission.outcome must be empty or %q", ErrInvalidRequest, "cancel")
	}
	decisions := map[string]bool{}
	for _, d := range perm.Decisions {
		switch d.Outcome {
		case "allow", "deny":
		default:
			return fmt.Errorf("%w: decision outcome for call %q must be %q or %q", ErrInvalidRequest, d.CallID, "allow", "deny")
		}
		if _, dup := decisions[d.CallID]; dup {
			return fmt.Errorf("%w: duplicate decision for call %q", ErrInvalidRequest, d.CallID)
		}
		decisions[d.CallID] = d.Outcome == "allow"
	}

	pending, ok := h.permissions.take(requestID)
	if !ok {
		return fmt.Errorf("%w: permission request %q not found or expired", ErrInvalidRequest, requestID)
	}
	dropPending := func() {
		_ = h.checkpoints.Delete(context.Background(), requestID)
		h.sessions.setPendingPermission(pending.agentID, pending.sessionID, false)
	}
	span := usage.SpanFromContext(ctx)
	span.AddAnnotation("permission_request_id", requestID)
	span.SetExtension(usage.BuiltinExtension{
		Operation:           "resume",
		SessionID:           pending.sessionID,
		RunID:               pending.runID,
		PermissionRequestID: requestID,
		LinkTraceID:         pending.linkTraceID,
		LinkSpanID:          pending.linkSpanID,
	})
	resumeEmit := correlatedSink(emit, pending.runID, pending.sessionID, requestID)
	if pending.agentID != a.ID {
		dropPending()
		return fmt.Errorf("%w: permission request %q does not belong to agent %q", ErrInvalidRequest, requestID, a.ID)
	}
	if req.SessionID != "" && req.SessionID != pending.sessionID {
		dropPending()
		return fmt.Errorf("%w: permission request %q belongs to a different session", ErrInvalidRequest, requestID)
	}
	if !a.UpdatedAt.Equal(pending.updatedAt) {
		// A checkpoint must resume on the graph that produced it.
		dropPending()
		return fmt.Errorf("%w: agent definition changed; permission request %q is invalidated", ErrInvalidRequest, requestID)
	}
	for callID := range decisions {
		if !slices.ContainsFunc(pending.calls, func(c pendingCall) bool { return c.CallID == callID }) {
			dropPending()
			return fmt.Errorf("%w: decision references unknown call %q", ErrInvalidRequest, callID)
		}
	}
	ctx = withBuiltinRunDimensions(ctx, a.ID, pending.runID)
	span.SetExtension(usage.CommonExtension{AgentID: a.ID, RuntimeType: agent.RuntimeTypeBuiltin, RunID: pending.runID})

	if perm.Outcome == "cancel" {
		dropPending()
		span.SetExtension(usage.BuiltinExtension{ResultStatus: "cancelled"})
		if err := resumeEmit(TurnEvent{Event: EventSession}); err != nil {
			return err
		}
		return resumeEmit(TurnEvent{Event: EventDone, StopReason: StopReasonCancelled})
	}

	entry, err := h.entry(ctx, a)
	if err != nil {
		dropPending()
		return err
	}
	turnCtx, cancel := context.WithTimeout(ctx, entry.turnTimeout)
	defer cancel()
	handle, _, err := h.sessions.begin(turnCtx, a.ID, pending.sessionID)
	if err != nil {
		dropPending()
		return err
	}
	if mw := a.Runtime.Builtin.Middlewares; mw != nil && mw.PlanTask != nil && mw.PlanTask.Enabled {
		turnCtx = withTaskBoard(turnCtx, handle.board())
	}
	select {
	case entry.turnSem <- struct{}{}:
	default:
		dropPending()
		handle.release()
		return ErrTurnLimitExceeded
	}
	entry.mu.Lock()
	entry.turns++
	entry.mu.Unlock()
	defer func() {
		entry.mu.Lock()
		entry.turns--
		entry.mu.Unlock()
		<-entry.turnSem
	}()
	span.SetExtension(usage.BuiltinExtension{
		Operation:    "resume",
		SessionID:    pending.sessionID,
		RunID:        pending.runID,
		TopologyKind: entry.topologyKind,
	})
	if err := resumeEmit(TurnEvent{Event: EventSession}); err != nil {
		dropPending()
		handle.release()
		return err
	}

	// A resumed turn is cancellable just like a fresh one.
	cancelOpt, cancelFn := adk.WithCancel()
	activity := &inflightTurn{
		agentID:      a.ID,
		sessionID:    pending.sessionID,
		runID:        pending.runID,
		requestID:    requestID,
		operation:    "resume",
		topologyKind: entry.topologyKind,
		startedAt:    time.Now().UTC(),
		cancel:       cancelFn,
	}
	h.activity.register(activity)
	defer h.activity.deregister(activity)

	// Unanswered calls are denied (fail-closed); every pending interrupt
	// point gets targeted so no gate is left to re-interrupt on its own.
	targets := make(map[string]any, len(pending.calls))
	for _, c := range pending.calls {
		targets[c.targetID] = &permissionDecision{Allow: decisions[c.CallID]}
	}
	result, runErr := h.resumeTurn(turnCtx, entry, requestID, targets, resumeEmit, cancelOpt)
	span.SetExtension(usage.BuiltinExtension{
		ModelSteps:  usage.Int(result.modelSteps),
		ToolSteps:   usage.Int(result.toolSteps),
		EventCounts: result.eventCounts,
	})
	if runErr != nil {
		dropPending()
		handle.release()
		var cancelErr *adk.CancelError
		if errors.As(runErr, &cancelErr) {
			span.SetExtension(usage.BuiltinExtension{ResultStatus: "cancelled"})
			return resumeEmit(TurnEvent{Event: EventDone, StopReason: StopReasonCancelled})
		}
		span.SetExtension(usage.BuiltinExtension{ResultStatus: "error"})
		_ = resumeEmit(TurnEvent{Event: EventError, Message: runErr.Error()})
		return runErr
	}
	if result.interrupt != nil {
		// The continued run hit another gated call: re-suspend under the same
		// request id (the runner rewrote the checkpoint in place).
		return h.suspendTurn(ctx, a, handle, pending.runID, requestID, pending.userMsg, pending.transcript, result, resumeEmit)
	}
	_ = h.checkpoints.Delete(context.Background(), requestID)
	transcript := make([]*schema.Message, 0, 1+len(pending.transcript)+len(result.transcript))
	transcript = append(transcript, pending.userMsg)
	transcript = append(transcript, pending.transcript...)
	transcript = append(transcript, result.transcript...)
	handle.commit(transcript)
	h.sessions.setPendingPermission(pending.agentID, pending.sessionID, false)
	span.SetExtension(usage.BuiltinExtension{ResultStatus: "success"})
	return resumeEmit(TurnEvent{Event: EventDone, StopReason: "end_turn"})
}

func (h *Host) recordPermissionExpiry(p *pendingPermission) {
	if h.observer == nil || p == nil {
		return
	}
	span, _ := h.observer.Begin(context.Background(), usage.InteractionDimensions{
		RouteID: p.routeID, RouteKind: "builtin", RouteProtocol: p.routeProtocol,
		VirtualKeyID: p.virtualKeyID, AgentDepth: p.agentDepth, AgentID: p.agentID,
		RuntimeType: agent.RuntimeTypeBuiltin, RunID: p.runID,
	})
	span.SetExtension(usage.BuiltinExtension{
		Operation: "permission_expire", SessionID: p.sessionID, RunID: p.runID,
		PermissionRequestID: p.requestID, LinkTraceID: p.linkTraceID,
		LinkSpanID: p.linkSpanID, ResultStatus: "expired",
	})
	span.Finish(usage.InteractionOutcome{Success: true, StatusCode: 200})
}

func withBuiltinRunDimensions(ctx context.Context, agentID, runID string) context.Context {
	dims, _ := usage.DimensionsFromContext(ctx)
	dims.AgentID = agentID
	dims.RuntimeType = agent.RuntimeTypeBuiltin
	dims.RunID = runID
	return usage.ContextWithDimensions(ctx, dims)
}

// pendingCallsFromInterrupt extracts the approval gate's root-cause interrupt
// points; other interrupt sources yield nothing and fail the turn upstream.
func pendingCallsFromInterrupt(info *adk.InterruptInfo) []pendingCall {
	var calls []pendingCall
	for _, ic := range info.InterruptContexts {
		if ic == nil || !ic.IsRootCause {
			continue
		}
		pi, ok := ic.Info.(*permissionInterruptInfo)
		if !ok {
			continue
		}
		calls = append(calls, pendingCall{
			targetID:     ic.ID,
			CallID:       pi.CallID,
			MCPServiceID: pi.MCPServiceID,
			ToolName:     pi.ToolName,
			Arguments:    pi.Arguments,
		})
	}
	return calls
}

// permissionEventData is the permission SSE event payload.
func permissionEventData(p *pendingPermission) json.RawMessage {
	view := pendingView(p)
	raw, err := json.Marshal(map[string]any{
		"expires_at": view.ExpiresAt,
		"calls":      view.Calls,
	})
	if err != nil {
		return nil
	}
	return raw
}

// RuntimeView is the /admin/builtin/runtime payload: per-agent
// materialization state plus the suspended interactive turns awaiting a
// decision.
type RuntimeView struct {
	Agents             map[string]EntryState   `json:"agents"`
	PendingPermissions []PendingPermissionView `json:"pending_permissions"`
	InFlight           []InFlightTurnView      `json:"in_flight"`
}

// Runtime reports the host-wide runtime view for the Admin API.
func (h *Host) Runtime() RuntimeView {
	view := RuntimeView{Agents: map[string]EntryState{}, PendingPermissions: []PendingPermissionView{}, InFlight: []InFlightTurnView{}}
	if h == nil {
		return view
	}
	view.InFlight = h.activity.list()
	h.mu.Lock()
	ids := make([]string, 0, len(h.entries))
	for id := range h.entries {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	for _, id := range ids {
		view.Agents[id] = h.State(id)
	}
	pending := h.permissions.list()
	slices.SortFunc(pending, func(a, b PendingPermissionView) int {
		return strings.Compare(a.RequestID, b.RequestID)
	})
	view.PendingPermissions = pending
	return view
}

// ListInFlight reports the running turns for the Admin API.
func (h *Host) ListInFlight() []InFlightTurnView {
	if h == nil {
		return []InFlightTurnView{}
	}
	return h.activity.list()
}

// CancelTurn requests cancellation of the running turn for (agentID,
// sessionID) and reports whether one was in flight. force aborts immediately;
// graceful stops after the current model/tool step, escalating to force after
// a grace period. The cancelled turn's own SSE stream emits a done event with
// stop_reason "cancelled"; a discarded (uncommitted) partial exchange leaves
// the session history untouched.
func (h *Host) CancelTurn(agentID, sessionID string, mode CancelMode) (bool, error) {
	if h == nil {
		return false, fmt.Errorf("builtin host is not configured")
	}
	return h.activity.cancel(agentID, sessionID, mode), nil
}

// CancelRun targets the exact logical run id used by the common Agent API.
func (h *Host) CancelRun(agentID, runID string, mode CancelMode) (bool, error) {
	if h == nil {
		return false, fmt.Errorf("builtin host is not configured")
	}
	return h.activity.cancelRun(agentID, runID, mode), nil
}

// ExpirePermission discards one opaque suspended checkpoint fail-closed.
func (h *Host) ExpirePermission(agentID, requestID string) bool {
	if h == nil || h.permissions == nil {
		return false
	}
	p, ok := h.permissions.discard(agentID, requestID)
	if !ok {
		return false
	}
	_ = h.checkpoints.Delete(context.Background(), requestID)
	h.sessions.setPendingPermission(p.agentID, p.sessionID, false)
	h.recordPermissionExpiry(p)
	return true
}

// entry returns the cached materialization for the agent, rebuilding when the
// definition changed (updated_at differs). In-flight turns keep the old entry.
func (h *Host) entry(ctx context.Context, a agent.Agent) (*hostEntry, error) {
	h.mu.Lock()
	existing := h.entries[a.ID]
	h.mu.Unlock()
	if existing != nil && existing.updatedAt.Equal(a.UpdatedAt) {
		return existing, nil
	}
	root, err := h.buildNode(ctx, a.ID, rootSpec(a), &a.Runtime.Builtin.Model, a.Runtime.Builtin, true)
	if err != nil {
		return nil, fmt.Errorf("materialize builtin agent %q: %w", a.ID, err)
	}
	limits := a.Runtime.Builtin.Limits
	maxTurns := defaultMaxConcurrentTurns
	timeout := defaultTurnTimeout
	if limits != nil {
		if limits.MaxConcurrentTurns > 0 {
			maxTurns = limits.MaxConcurrentTurns
		}
		if limits.TurnTimeoutSeconds > 0 {
			timeout = time.Duration(limits.TurnTimeoutSeconds) * time.Second
		}
	}
	topologyKind := a.Runtime.Builtin.Topology.Kind
	if topologyKind == "" {
		topologyKind = agent.TopologyKindSingle
	}
	entry := &hostEntry{
		updatedAt:    a.UpdatedAt,
		runner:       adk.NewRunner(ctx, adk.RunnerConfig{Agent: root, EnableStreaming: true, CheckPointStore: h.checkpoints}),
		topologyKind: topologyKind,
		turnTimeout:  timeout,
		turnSem:      make(chan struct{}, maxTurns),
		buildAt:      time.Now().UTC(),
	}
	h.mu.Lock()
	// Re-check under the lock; a concurrent turn may have materialized the
	// same definition already — prefer the existing entry so its semaphore
	// stays authoritative.
	if current := h.entries[a.ID]; current != nil && current.updatedAt.Equal(a.UpdatedAt) {
		h.mu.Unlock()
		return current, nil
	}
	h.entries[a.ID] = entry
	h.mu.Unlock()
	return entry, nil
}

type turnResult struct {
	modelSteps  int
	toolSteps   int
	eventCounts map[string]int
	transcript  []*schema.Message
	// interrupt is set when the run suspended on a checkpoint interrupt
	// instead of completing (§5.7.7).
	interrupt *adk.InterruptInfo
}

// runTurn drives one ADK run and maps agent events onto the turn event
// vocabulary. A panicking agent fails the turn, never the gateway process.
func (h *Host) runTurn(ctx context.Context, entry *hostEntry, input []*schema.Message, emit EventSink, opts ...adk.AgentRunOption) (result turnResult, err error) {
	result.eventCounts = map[string]int{}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("builtin agent panicked: %v", r)
		}
	}()
	iter := entry.runner.Run(ctx, input, opts...)
	err = h.driveIter(iter, emit, &result)
	return result, err
}

// resumeTurn continues a checkpointed run with per-interrupt-point resume
// targets.
func (h *Host) resumeTurn(ctx context.Context, entry *hostEntry, requestID string, targets map[string]any, emit EventSink, opts ...adk.AgentRunOption) (result turnResult, err error) {
	result.eventCounts = map[string]int{}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("builtin agent panicked: %v", r)
		}
	}()
	iter, err := entry.runner.ResumeWithParams(ctx, requestID, &adk.ResumeParams{Targets: targets}, opts...)
	if err != nil {
		return result, fmt.Errorf("resume builtin turn: %w", err)
	}
	err = h.driveIter(iter, emit, &result)
	return result, err
}

// driveIter consumes one run's event stream. Interrupt actions are recorded
// on the result rather than treated as errors — the runner has already saved
// the checkpoint by the time the event surfaces.
func (h *Host) driveIter(iter *adk.AsyncIterator[*adk.AgentEvent], emit EventSink, result *turnResult) error {
	countingEmit := func(ev TurnEvent) error {
		result.eventCounts[ev.Event]++
		return emit(ev)
	}
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event == nil {
			continue
		}
		if event.Err != nil {
			return event.Err
		}
		if event.Action != nil && event.Action.Interrupted != nil {
			result.interrupt = event.Action.Interrupted
			continue
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		msg, emitErr := h.emitMessageOutput(event.Output.MessageOutput, countingEmit, result)
		if emitErr != nil {
			return emitErr
		}
		if msg != nil {
			result.transcript = append(result.transcript, msg)
		}
	}
	return nil
}

// emitMessageOutput maps one agent message (streaming or complete) onto turn
// events and returns the complete message for the session transcript.
func (h *Host) emitMessageOutput(out *adk.MessageVariant, emit EventSink, result *turnResult) (*schema.Message, error) {
	msg := out.Message
	if out.IsStreaming {
		var chunks []*schema.Message
		stream := out.MessageStream
		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				stream.Close()
				return nil, err
			}
			chunks = append(chunks, chunk)
			if chunk != nil && chunk.Role == schema.Assistant && chunk.Content != "" {
				if err := emit(TurnEvent{Event: EventDelta, Text: chunk.Content}); err != nil {
					stream.Close()
					return nil, err
				}
			}
		}
		stream.Close()
		if len(chunks) == 0 {
			return nil, nil
		}
		merged, err := schema.ConcatMessages(chunks)
		if err != nil {
			return nil, fmt.Errorf("merge streamed message: %w", err)
		}
		msg = merged
	}
	if msg == nil {
		return nil, nil
	}
	switch msg.Role {
	case schema.Assistant:
		result.modelSteps++
		if msg.Content != "" {
			if err := emit(TurnEvent{Event: EventContent, Text: msg.Content}); err != nil {
				return nil, err
			}
		}
		if len(msg.ToolCalls) > 0 {
			if err := emit(TurnEvent{Event: EventToolCall, Data: toolCallsData(msg.ToolCalls)}); err != nil {
				return nil, err
			}
		}
		if usageData := usageEventData(msg); usageData != nil {
			if err := emit(TurnEvent{Event: EventUsage, Data: usageData}); err != nil {
				return nil, err
			}
		}
	case schema.Tool:
		result.toolSteps++
		if err := emit(TurnEvent{Event: EventToolCall, Data: toolResultData(out.ToolName, msg.Content)}); err != nil {
			return nil, err
		}
	}
	return msg, nil
}

func toolCallsData(calls []schema.ToolCall) json.RawMessage {
	type callView struct {
		ID        string `json:"id,omitempty"`
		Name      string `json:"name"`
		Arguments string `json:"arguments,omitempty"`
	}
	views := make([]callView, 0, len(calls))
	for _, c := range calls {
		views = append(views, callView{ID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments})
	}
	raw, err := json.Marshal(map[string]any{"calls": views})
	if err != nil {
		return nil
	}
	return raw
}

func toolResultData(toolName, output string) json.RawMessage {
	raw, err := json.Marshal(map[string]any{"name": toolName, "output": output})
	if err != nil {
		return nil
	}
	return raw
}

func usageEventData(msg *schema.Message) json.RawMessage {
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		return nil
	}
	u := msg.ResponseMeta.Usage
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 {
		return nil
	}
	raw, err := json.Marshal(map[string]int{
		"input_tokens":  u.PromptTokens,
		"output_tokens": u.CompletionTokens,
		"total_tokens":  u.TotalTokens,
	})
	if err != nil {
		return nil
	}
	return raw
}
