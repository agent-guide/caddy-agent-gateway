package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/agent-guide/agent-gateway/pkg/agent"
)

// EventCursor is the process-lifetime sequencing state stored beside a
// suspended backend continuation. NextSequence starts at 1 for a new run.
type EventCursor struct {
	RunID        string `json:"run_id"`
	NextSequence uint64 `json:"next_sequence"`
	NextSegment  uint32 `json:"next_segment"`
}

// RunSequencer owns ordering across every stream segment of one logical run.
// Its emission lock deliberately covers the downstream sink call so concurrent
// backend producers cannot reorder events after sequence allocation.
type RunSequencer struct {
	mu           sync.Mutex
	emitMu       sync.Mutex
	agentID      string
	runtimeType  string
	runID        string
	nextSequence uint64
	nextSegment  uint32
	segmentOpen  bool
}

func NewRunSequencer(agentID, runtimeType string) (*RunSequencer, error) {
	runID, err := NewRunID()
	if err != nil {
		return nil, err
	}
	return RestoreRunSequencer(agentID, runtimeType, EventCursor{RunID: runID, NextSequence: 1})
}

func RestoreRunSequencer(agentID, runtimeType string, cursor EventCursor) (*RunSequencer, error) {
	agentID = strings.TrimSpace(agentID)
	runtimeType = strings.TrimSpace(runtimeType)
	if agentID == "" || runtimeType == "" || !ValidRunID(cursor.RunID) {
		return nil, NewError(ErrorInvalidRequest, "invalid run sequencing identity")
	}
	if cursor.NextSequence == 0 {
		cursor.NextSequence = 1
	}
	return &RunSequencer{
		agentID: agentID, runtimeType: runtimeType, runID: cursor.RunID,
		nextSequence: cursor.NextSequence, nextSegment: cursor.NextSegment,
	}, nil
}

func (r *RunSequencer) RunID() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runID
}

func (r *RunSequencer) Cursor() EventCursor {
	if r == nil {
		return EventCursor{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return EventCursor{RunID: r.runID, NextSequence: r.nextSequence, NextSegment: r.nextSegment}
}

// SegmentResult tells transports whether event emission was attempted and
// whether terminal emission was attempted. A caller maps Err to a
// pre-stream HTTP response only when Started is false; once Started is true,
// the common sequencer owns terminal SSE behavior.
type SegmentResult struct {
	Started  bool
	Terminal bool
}

// NewTurnSequencer creates a fresh run sequencer or restores the cursor owned
// by a permission continuation. Resume requests fail closed when the selected
// backend does not own the referenced continuation.
func NewTurnSequencer(ctx context.Context, backend Backend, a agent.Agent, req TurnRequest) (*RunSequencer, error) {
	if backend == nil {
		return nil, NewError(ErrorRuntimeNotExecutable, "agent runtime is not executable")
	}
	if req.Permission == nil {
		return NewRunSequencer(a.ID, backend.RuntimeType())
	}
	requestID := strings.TrimSpace(req.Permission.RequestID)
	if requestID == "" {
		return nil, NewError(ErrorInvalidRequest, "permission.request_id is required")
	}
	owner, ok := backend.(ContinuationCursorBackend)
	if !ok {
		return nil, NewError(ErrorCapabilityNotSupported, "runtime does not support turn continuation")
	}
	cursor, err := owner.LoadContinuationCursor(ctx, a, requestID)
	if err != nil {
		return nil, NormalizeError(err)
	}
	return RestoreRunSequencer(a.ID, backend.RuntimeType(), cursor)
}

// ServeSegment invokes one backend stream. Backend validation failures before
// the first event are returned without starting a stream. Once a stream has
// started, this method guarantees exactly one terminal event.
func (r *RunSequencer) ServeSegment(ctx context.Context, backend Backend, a agent.Agent, req TurnRequest, sink EventSink) (SegmentResult, error) {
	if r == nil || backend == nil || sink == nil {
		return SegmentResult{}, NewError(ErrorInvalidRequest, "runtime segment is not configured")
	}
	r.mu.Lock()
	if req.RunID != "" && req.RunID != r.runID {
		r.mu.Unlock()
		return SegmentResult{}, NewError(ErrorInvalidRequest, "turn request run_id does not match the run sequencer")
	}
	if r.segmentOpen {
		r.mu.Unlock()
		return SegmentResult{}, NewError(ErrorSessionBusy, "another run segment is active")
	}
	segment := r.nextSegment
	r.segmentOpen = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.segmentOpen = false
		r.mu.Unlock()
	}()

	req.RunID = r.runID
	requestID := ""
	if req.Permission != nil {
		requestID = req.Permission.RequestID
	}
	ctx = MergeIdentities(ctx, Identities{AgentID: r.agentID, RuntimeType: r.runtimeType, RunID: r.runID, SessionID: req.SessionID, RequestID: requestID, SegmentIndex: segment})
	ss := &segmentSink{run: r, segment: segment, sink: sink}
	if owner, ok := backend.(ContinuationCursorBackend); ok {
		ss.persistContinuation = func(requestID string, cursor EventCursor) error {
			return owner.StoreContinuationCursor(ctx, a, requestID, cursor)
		}
	}
	err := NormalizeError(backend.ServeTurn(ctx, a, req, ss.emit))
	if ss.terminalSeen() {
		return ss.result(), err
	}
	if err == nil {
		err = NormalizeError(ss.emit(TurnEvent{Event: EventDone}))
		return ss.result(), err
	}
	if !ss.started() {
		return ss.result(), err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, ErrTurnCancelled) {
		payload, _ := json.Marshal(map[string]string{"stop_reason": StopReasonCancelled})
		_ = ss.emit(TurnEvent{Event: EventDone, Data: payload})
		return ss.result(), err
	}
	public := PublicError(err)
	payload, _ := json.Marshal(public)
	_ = ss.emit(TurnEvent{Event: EventError, Data: payload})
	return ss.result(), err
}

type segmentSink struct {
	run                 *RunSequencer
	segment             uint32
	sink                EventSink
	emitted             bool
	terminal            bool
	allocated           bool
	permissionRequestID string
	persistContinuation func(string, EventCursor) error
}

func (s *segmentSink) emit(ev TurnEvent) error {
	s.run.emitMu.Lock()
	defer s.run.emitMu.Unlock()
	s.run.mu.Lock()
	if s.terminal {
		s.run.mu.Unlock()
		return nil
	}
	if ev.AgentID != "" && ev.AgentID != s.run.agentID {
		s.run.mu.Unlock()
		return fmt.Errorf("event agent_id %q does not match run agent_id %q", ev.AgentID, s.run.agentID)
	}
	if ev.RunID != "" && ev.RunID != s.run.runID {
		s.run.mu.Unlock()
		return fmt.Errorf("event run_id %q does not match run_id %q", ev.RunID, s.run.runID)
	}
	if !s.allocated {
		s.run.nextSegment++
		s.allocated = true
	}
	ev.AgentID = s.run.agentID
	ev.RunID = s.run.runID
	ev.SegmentIndex = s.segment
	ev.Sequence = s.run.nextSequence
	s.run.nextSequence++
	if ev.Event == EventPermission && ev.RequestID != "" {
		s.permissionRequestID = ev.RequestID
	}
	continuation := false
	if ev.Event == EventDone && s.permissionRequestID != "" {
		var terminal struct {
			StopReason string `json:"stop_reason"`
		}
		if json.Unmarshal(ev.Data, &terminal) == nil && terminal.StopReason == StopReasonPermissionRequired {
			continuation = true
		}
	}
	if isTerminalEvent(ev.Event) && !continuation {
		s.terminal = true
	}
	s.emitted = true
	cursor := EventCursor{RunID: s.run.runID, NextSequence: s.run.nextSequence, NextSegment: s.run.nextSegment}
	s.run.mu.Unlock()
	if continuation {
		if s.persistContinuation == nil {
			return NewError(ErrorCapabilityNotSupported, "runtime does not persist permission continuations")
		}
		if err := s.persistContinuation(s.permissionRequestID, cursor); err != nil {
			return err
		}
		s.run.mu.Lock()
		s.terminal = true
		s.run.mu.Unlock()
	}
	return s.sink(ev)
}

func (s *segmentSink) started() bool {
	s.run.mu.Lock()
	defer s.run.mu.Unlock()
	return s.emitted
}

func (s *segmentSink) terminalSeen() bool {
	s.run.mu.Lock()
	defer s.run.mu.Unlock()
	return s.terminal
}

func (s *segmentSink) result() SegmentResult {
	s.run.mu.Lock()
	defer s.run.mu.Unlock()
	return SegmentResult{Started: s.emitted, Terminal: s.terminal}
}

func isTerminalEvent(name string) bool {
	return name == EventDone || name == EventError
}
