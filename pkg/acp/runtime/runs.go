package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var errNativeCancelSent = errors.New("native session cancel sent")

type activeRun struct {
	ownerID, runID, sessionID string
	startedAt                 time.Time
	cancel                    context.CancelCauseFunc
	instance                  *instance
	cancelRequested           bool
}

type activeRunRegistry struct {
	mu   sync.Mutex
	runs map[string]*activeRun
}

func newActiveRunRegistry() *activeRunRegistry {
	return &activeRunRegistry{runs: map[string]*activeRun{}}
}
func activeRunKey(ownerID, runID string) string { return ownerID + "\x00" + runID }

func (r *activeRunRegistry) begin(parent context.Context, ownerID, runID, sessionID string) (context.Context, *activeRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := activeRunKey(ownerID, runID)
	if r.runs[key] != nil {
		return parent, nil
	}
	ctx, cancel := context.WithCancelCause(parent)
	run := &activeRun{ownerID: ownerID, runID: runID, sessionID: sessionID, startedAt: time.Now().UTC(), cancel: cancel}
	r.runs[key] = run
	return ctx, run
}
func (r *activeRunRegistry) bind(run *activeRun, inst *instance) error {
	r.mu.Lock()
	if r.runs[activeRunKey(run.ownerID, run.runID)] == run {
		run.instance = inst
		if inst != nil && inst.sessionID != "" {
			run.sessionID = inst.sessionID
		}
	}
	cancelRequested := run.cancelRequested
	r.mu.Unlock()
	if !cancelRequested {
		return nil
	}
	if inst == nil || inst.protocolSessionID() == "" {
		return ErrRunNotReady
	}
	if err := inst.cancel(); err != nil {
		return fmt.Errorf("%w: session/cancel: %v", ErrRunNotReady, err)
	}
	run.cancel(errNativeCancelSent)
	return ErrTurnCancelled
}

// requestCancel marks a run fail-closed even when its native instance/session
// is not bound yet. bind observes the mark and sends session/cancel before the
// prompt loop is allowed to start.
func (r *activeRunRegistry) requestCancel(ownerID, runID string) error {
	r.mu.Lock()
	run := r.runs[activeRunKey(ownerID, runID)]
	if run == nil {
		r.mu.Unlock()
		return ErrRunNotFound
	}
	run.cancelRequested = true
	ready := run.instance != nil && run.instance.protocolSessionID() != ""
	r.mu.Unlock()
	if !ready {
		return nil
	}
	return r.cancel(ownerID, runID)
}
func (r *activeRunRegistry) finish(run *activeRun) {
	r.mu.Lock()
	if r.runs[activeRunKey(run.ownerID, run.runID)] == run {
		delete(r.runs, activeRunKey(run.ownerID, run.runID))
	}
	r.mu.Unlock()
	run.cancel(nil)
}
func (r *activeRunRegistry) cancel(ownerID, runID string) error {
	r.mu.Lock()
	run := r.runs[activeRunKey(ownerID, runID)]
	if run == nil {
		r.mu.Unlock()
		return ErrRunNotFound
	}
	inst, cancel := run.instance, run.cancel
	// A run is published before instance resolution so duplicate run ids are
	// rejected immediately. Force cancellation is deliberately retryable until
	// the live instance (and therefore its protocol session id) is bound: merely
	// cancelling the context here would skip the required session/cancel frame.
	if inst == nil || inst.protocolSessionID() == "" {
		r.mu.Unlock()
		return ErrRunNotReady
	}
	r.mu.Unlock()
	if err := inst.cancel(); err != nil {
		return fmt.Errorf("%w: session/cancel: %v", ErrRunNotReady, err)
	}
	cancel(errNativeCancelSent)
	return nil
}
func (r *activeRunRegistry) list(ownerID string) []ActiveRunInfo {
	r.mu.Lock()
	out := []ActiveRunInfo{}
	for _, run := range r.runs {
		if run.ownerID == ownerID {
			out = append(out, ActiveRunInfo{OwnerID: run.ownerID, RunID: run.runID, SessionID: run.sessionID, StartedAt: run.startedAt})
		}
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out
}
