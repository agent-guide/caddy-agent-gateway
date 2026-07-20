package builtin

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
)

// CancelMode selects how an operator-requested cancel stops a running turn.
// It answers the "forced-cancel for stuck turns" question in
// docs/design/agents-control-plane.md §10 by adopting the ADK Runner cancel
// primitive (eino-reuse.md §5) for the builtin host.
type CancelMode string

const (
	// CancelModeForce aborts the turn immediately (adk.CancelImmediate): the
	// in-flight model or tool step is abandoned and the turn ends now. This is
	// the default and the answer for stuck turns.
	CancelModeForce CancelMode = "force"
	// CancelModeGraceful stops after the current model or tool step completes
	// (the adk safe-points CancelAfterChatModel|CancelAfterToolCalls),
	// propagating those safe-points through nested agents and escalating to
	// force if none is reached within cancelGracePeriod.
	CancelModeGraceful CancelMode = "graceful"
)

// cancelGracePeriod bounds a graceful cancel before it escalates to immediate,
// so a graceful cancel of a genuinely stuck turn still terminates.
const cancelGracePeriod = 30 * time.Second

// ParseCancelMode normalizes an operator-supplied mode; empty defaults to
// force. An unknown mode is a client-correctable error.
func ParseCancelMode(s string) (CancelMode, error) {
	switch CancelMode(strings.TrimSpace(s)) {
	case "", CancelModeForce:
		return CancelModeForce, nil
	case CancelModeGraceful:
		return CancelModeGraceful, nil
	default:
		return "", fmt.Errorf("%w: cancel mode must be %q or %q", ErrInvalidRequest, CancelModeForce, CancelModeGraceful)
	}
}

// options maps the mode onto the ADK cancel options.
func (m CancelMode) options() []adk.AgentCancelOption {
	if m == CancelModeGraceful {
		return []adk.AgentCancelOption{
			adk.WithAgentCancelMode(adk.CancelAfterChatModel | adk.CancelAfterToolCalls),
			adk.WithAgentCancelTimeout(cancelGracePeriod),
			adk.WithRecursive(),
		}
	}
	return []adk.AgentCancelOption{adk.WithAgentCancelMode(adk.CancelImmediate)}
}

// inflightTurn is one running ADK turn an operator can cancel. Turns on one
// session are serialized, so (agent_id, session_id) uniquely identifies the
// running turn.
type inflightTurn struct {
	agentID      string
	sessionID    string
	operation    string // "turn" or "resume"
	topologyKind string
	startedAt    time.Time
	cancel       adk.AgentCancelFunc
}

// InFlightTurnView is the admin view of one running turn.
type InFlightTurnView struct {
	AgentID      string    `json:"agent_id"`
	SessionID    string    `json:"session_id"`
	Operation    string    `json:"operation"`
	TopologyKind string    `json:"topology_kind,omitempty"`
	StartedAt    time.Time `json:"started_at"`
}

// activityRegistry tracks in-flight turns keyed by agent+session.
type activityRegistry struct {
	mu    sync.Mutex
	turns map[string]*inflightTurn
}

func newActivityRegistry() *activityRegistry {
	return &activityRegistry{turns: map[string]*inflightTurn{}}
}

func activityKey(agentID, sessionID string) string {
	return agentID + "\x00" + sessionID
}

func (r *activityRegistry) register(t *inflightTurn) {
	r.mu.Lock()
	r.turns[activityKey(t.agentID, t.sessionID)] = t
	r.mu.Unlock()
}

func (r *activityRegistry) deregister(t *inflightTurn) {
	r.mu.Lock()
	key := activityKey(t.agentID, t.sessionID)
	// A completed turn can release the session before its deferred cleanup
	// runs. Do not let that cleanup delete a newer turn which has already
	// registered under the same agent/session key.
	if r.turns[key] == t {
		delete(r.turns, key)
	}
	r.mu.Unlock()
}

func (r *activityRegistry) list() []InFlightTurnView {
	r.mu.Lock()
	out := make([]InFlightTurnView, 0, len(r.turns))
	for _, t := range r.turns {
		out = append(out, InFlightTurnView{
			AgentID:      t.agentID,
			SessionID:    t.sessionID,
			Operation:    t.operation,
			TopologyKind: t.topologyKind,
			StartedAt:    t.startedAt,
		})
	}
	r.mu.Unlock()
	slices.SortFunc(out, func(a, b InFlightTurnView) int {
		if c := strings.Compare(a.AgentID, b.AgentID); c != 0 {
			return c
		}
		return strings.Compare(a.SessionID, b.SessionID)
	})
	return out
}

// cancel requests cancellation of the turn for (agentID, sessionID) and
// reports whether a matching turn was running. The ADK cancel func is invoked
// outside the registry lock; it is designed for concurrent external calls and
// returns after committing the request without waiting for teardown.
func (r *activityRegistry) cancel(agentID, sessionID string, mode CancelMode) bool {
	r.mu.Lock()
	t := r.turns[activityKey(agentID, sessionID)]
	if t == nil {
		r.mu.Unlock()
		return false
	}
	// Keep the entry stable until the non-blocking ADK cancel request has been
	// committed. The returned bool distinguishes a cancellation that actually
	// contributed to this execution from one which arrived after it finished.
	_, contributed := t.cancel(mode.options()...)
	r.mu.Unlock()
	return contributed
}
