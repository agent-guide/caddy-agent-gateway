package runtimeapi

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultRunTombstoneTTL = 10 * time.Minute
	defaultRunTombstoneCap = 1024
)

// RunInfo is the process-local operator view of one Agent run.
type RunInfo struct {
	AgentID     string    `json:"agent_id"`
	RuntimeType string    `json:"runtime_type"`
	RunID       string    `json:"run_id"`
	SessionID   string    `json:"session_id,omitempty"`
	State       RunState  `json:"state"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
	StopReason  string    `json:"stop_reason,omitempty"`
}

type runCancelFunc func(context.Context, CancelMode) error

type runEntry struct {
	info   RunInfo
	cancel runCancelFunc
}

// RunRegistry owns exact-run cancellation and bounded terminal tombstones.
// It is process-local until durable Workflow runs replace its history source.
type RunRegistry struct {
	mu       sync.Mutex
	active   map[string]map[string]*runEntry
	terminal map[string]map[string]*runEntry
	ttl      time.Duration
	cap      int
	now      func() time.Time
}

func NewRunRegistry() *RunRegistry {
	return newRunRegistry(defaultRunTombstoneTTL, defaultRunTombstoneCap, time.Now)
}

func newRunRegistry(ttl time.Duration, cap int, now func() time.Time) *RunRegistry {
	return &RunRegistry{active: map[string]map[string]*runEntry{}, terminal: map[string]map[string]*runEntry{}, ttl: ttl, cap: cap, now: now}
}

// Begin publishes one active run and its exact backend cancellation binding.
// A duplicate active or retained run id is rejected fail-closed. Callers that
// retry one logical execution must allocate a distinct attempt run_id while a
// prior attempt's tombstone is retained; a durable logical execution key must
// not be reused as this process-local cancellation identity.
func (r *RunRegistry) Begin(agentID, runtimeType, runID, sessionID string, cancel func(context.Context, CancelMode) error) error {
	agentID, runtimeType, runID = strings.TrimSpace(agentID), strings.TrimSpace(runtimeType), strings.TrimSpace(runID)
	if r == nil || agentID == "" || runtimeType == "" || !ValidRunID(runID) || cancel == nil {
		return NewError(ErrorInvalidRequest, "invalid run registration")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked()
	if existing := r.active[agentID][runID]; existing != nil {
		return NewError(ErrorInvalidRequest, "run_id is already active")
	}
	if r.terminal[agentID] != nil && r.terminal[agentID][runID] != nil {
		return NewError(ErrorInvalidRequest, "run_id is already registered")
	}
	if r.active[agentID] == nil {
		r.active[agentID] = map[string]*runEntry{}
	}
	r.active[agentID][runID] = &runEntry{info: RunInfo{AgentID: agentID, RuntimeType: runtimeType, RunID: runID, SessionID: strings.TrimSpace(sessionID), State: RunStateRunning, StartedAt: r.now().UTC()}, cancel: cancel}
	return nil
}

// Rebind replaces the native cancel handle when a suspended logical run starts
// its next transport segment. It never creates a missing or terminal run.
func (r *RunRegistry) Rebind(agentID, runtimeType, runID, sessionID string, cancel func(context.Context, CancelMode) error) error {
	if r == nil || cancel == nil {
		return NewError(ErrorRunNotFound, "run not found")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked()
	e := r.active[strings.TrimSpace(agentID)][strings.TrimSpace(runID)]
	if e == nil {
		return NewError(ErrorRunNotFound, "run not found")
	}
	if e.info.RuntimeType != strings.TrimSpace(runtimeType) {
		return NewError(ErrorRuntimeNotExecutable, "run runtime changed")
	}
	e.cancel = cancel
	if strings.TrimSpace(sessionID) != "" {
		e.info.SessionID = strings.TrimSpace(sessionID)
	}
	return nil
}

// SetSession records a backend-assigned session id without changing run state.
func (r *RunRegistry) SetSession(agentID, runID, sessionID string) {
	if r == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	r.mu.Lock()
	if e := r.active[strings.TrimSpace(agentID)][strings.TrimSpace(runID)]; e != nil {
		e.info.SessionID = strings.TrimSpace(sessionID)
	}
	r.mu.Unlock()
}

// Complete atomically retires an active cancel binding into a terminal tombstone.
func (r *RunRegistry) Complete(agentID, runID string, state RunState, stopReason string) {
	if r == nil {
		return
	}
	if state != RunStateCompleted && state != RunStateCancelled && state != RunStateFailed {
		state = RunStateFailed
	}
	agentID, runID = strings.TrimSpace(agentID), strings.TrimSpace(runID)
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.active[agentID][runID]
	if e == nil {
		return
	}
	delete(r.active[agentID], runID)
	e.cancel = nil
	e.info.State, e.info.StopReason, e.info.FinishedAt = state, strings.TrimSpace(stopReason), r.now().UTC()
	if r.terminal[agentID] == nil {
		r.terminal[agentID] = map[string]*runEntry{}
	}
	r.terminal[agentID][runID] = e
	r.sweepLocked()
}

func (r *RunRegistry) List(agentID string) []RunInfo {
	if r == nil {
		return []RunInfo{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked()
	var out []RunInfo
	for _, e := range r.active[strings.TrimSpace(agentID)] {
		out = append(out, e.info)
	}
	for _, e := range r.terminal[strings.TrimSpace(agentID)] {
		out = append(out, e.info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].RunID < out[j].RunID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	if out == nil {
		return []RunInfo{}
	}
	return out
}

// Cancel invokes only the exact active run binding. Retained terminal runs are
// returned unchanged, making repeated cancellation idempotent.
func (r *RunRegistry) Cancel(ctx context.Context, agentID string, req CancelRequest) (CancelResult, error) {
	if r == nil {
		return CancelResult{}, NewError(ErrorRunNotFound, "run not found")
	}
	agentID, req.RunID = strings.TrimSpace(agentID), strings.TrimSpace(req.RunID)
	r.mu.Lock()
	r.sweepLocked()
	if e := r.terminal[agentID][req.RunID]; e != nil {
		result := cancelResult(e.info)
		r.mu.Unlock()
		return result, nil
	}
	e := r.active[agentID][req.RunID]
	if e == nil {
		r.mu.Unlock()
		return CancelResult{}, NewError(ErrorRunNotFound, "run not found")
	}
	cancel := e.cancel
	r.mu.Unlock()
	if err := cancel(ctx, req.Mode); err != nil {
		r.mu.Lock()
		if terminal := r.terminal[agentID][req.RunID]; terminal != nil {
			result := cancelResult(terminal.info)
			r.mu.Unlock()
			return result, nil
		}
		r.mu.Unlock()
		return CancelResult{}, err
	}
	// The registry owns the terminal transition after a backend accepts exact
	// cancellation. Backend callbacks must not recursively Complete the same
	// run; a concurrently finishing turn simply makes this Complete a no-op.
	r.Complete(agentID, req.RunID, RunStateCancelled, StopReasonCancelled)
	r.mu.Lock()
	terminal := r.terminal[agentID][req.RunID]
	if terminal != nil {
		result := cancelResult(terminal.info)
		r.mu.Unlock()
		return result, nil
	}
	r.mu.Unlock()
	return CancelResult{RunID: req.RunID, State: RunStateCancelled, StopReason: StopReasonCancelled}, nil
}

func (r *RunRegistry) CancelAgent(ctx context.Context, agentID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	ids := make([]string, 0, len(r.active[agentID]))
	for id := range r.active[agentID] {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		_, _ = r.Cancel(ctx, agentID, CancelRequest{RunID: id, Mode: CancelModeForce})
	}
}

func cancelResult(info RunInfo) CancelResult {
	return CancelResult{RunID: info.RunID, State: info.State, StopReason: info.StopReason, FinishedAt: info.FinishedAt}
}

func (r *RunRegistry) sweepLocked() {
	now := r.now().UTC()
	for agentID, entries := range r.terminal {
		for id, e := range entries {
			if now.Sub(e.info.FinishedAt) >= r.ttl {
				delete(entries, id)
			}
		}
		if r.cap > 0 && len(entries) > r.cap {
			ordered := make([]*runEntry, 0, len(entries))
			for _, e := range entries {
				ordered = append(ordered, e)
			}
			sort.Slice(ordered, func(i, j int) bool { return ordered[i].info.FinishedAt.Before(ordered[j].info.FinishedAt) })
			for _, e := range ordered[:len(ordered)-r.cap] {
				delete(entries, e.info.RunID)
			}
		}
		if len(entries) == 0 {
			delete(r.terminal, agentID)
		}
	}
}
