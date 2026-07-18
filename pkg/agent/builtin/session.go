package builtin

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

// ErrSessionBusy is returned when waiting for an in-flight turn on the same
// session is cancelled (client disconnect or turn timeout). Fail-closed and
// client-correctable: retry once the running turn finishes.
var ErrSessionBusy = errors.New("builtin session has a turn in flight")

// ErrSessionLimitExceeded is returned when an agent is at its session cap and
// no idle session can be evicted. Fail-closed: the new session is rejected,
// never queued.
var ErrSessionLimitExceeded = errors.New("builtin agent session limit exceeded")

// Sessions are in-memory conversation histories. eino v0.9.x has no durable
// Runner session persistence, so PB1 ships documented restart-loss semantics:
// a gateway restart (or eviction pressure) drops histories.
//
// Turns on the SAME session are serialized: each session carries a serial
// slot held for the whole turn, so a later turn always sees the earlier
// turn's exchange and appends in execution order. The wait is context-aware
// and happens BEFORE the agent concurrency semaphore is taken, so waiting
// same-session turns never occupy max_concurrent_turns slots.
const (
	sessionIdleTTL      = time.Hour
	maxSessionsPerAgent = 128
	// maxSessionMessages bounds one session's history; the oldest messages are
	// dropped beyond it.
	maxSessionMessages = 200
)

type session struct {
	id string
	// serial is a capacity-1 slot held for the full duration of a turn on
	// this session; acquisition selects against the caller's context.
	serial chan struct{}
	// busy counts turns holding or waiting on serial; the evictor skips busy
	// sessions. Guarded by the store mutex.
	busy       int
	messages   []*schema.Message
	lastAccess time.Time
	// taskBoard is the session's plantask storage, created lazily when the
	// agent enables the plantask middleware. Evicted with the session.
	taskBoard *planTaskBoard
}

// sessionStore keys sessions by agent id then session id. It survives
// definition updates (histories are graph-independent) but not restarts.
type sessionStore struct {
	mu     sync.Mutex
	agents map[string]map[string]*session
}

func newSessionStore() *sessionStore {
	return &sessionStore{agents: map[string]map[string]*session{}}
}

// sessionHandle is one turn's exclusive hold on a session. Exactly one of
// commit or release must be called.
type sessionHandle struct {
	store   *sessionStore
	agentID string
	sess    *session
}

// waitCtx is the subset of context.Context begin needs; a parameter type so
// the wait is explicitly cancellation-driven.
type waitCtx interface {
	Done() <-chan struct{}
	Err() error
}

// begin resolves (or creates) the session and waits for its serial slot. The
// wait aborts when ctx is cancelled (client disconnect, turn timeout) and
// releases the busy marker, returning ErrSessionBusy. Creating a session
// beyond the per-agent cap fails with ErrSessionLimitExceeded when nothing is
// evictable.
func (s *sessionStore) begin(ctx waitCtx, agentID, sessionID string) (*sessionHandle, []*schema.Message, error) {
	s.mu.Lock()
	byID := s.agents[agentID]
	if byID == nil {
		byID = map[string]*session{}
		s.agents[agentID] = byID
	}
	sess, ok := byID[sessionID]
	if ok {
		// Mark the reused session busy before any eviction sweep so the
		// TTL/cap evictors can never drop the session this turn is about to
		// continue (evictors skip busy sessions).
		sess.busy++
	}
	s.evictExpiredLocked(byID)
	if !ok {
		s.evictForCapLocked(byID)
		if len(byID) >= maxSessionsPerAgent {
			s.mu.Unlock()
			return nil, nil, ErrSessionLimitExceeded
		}
		if sessionID == "" {
			sessionID = uuid.NewString()
		}
		sess = &session{id: sessionID, serial: make(chan struct{}, 1), lastAccess: time.Now()}
		byID[sessionID] = sess
		sess.busy++
	}
	s.mu.Unlock()

	select {
	case sess.serial <- struct{}{}:
	case <-ctx.Done():
		s.mu.Lock()
		sess.busy--
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("%w: %v", ErrSessionBusy, ctx.Err())
	}
	s.mu.Lock()
	sess.lastAccess = time.Now()
	history := make([]*schema.Message, len(sess.messages))
	copy(history, sess.messages)
	s.mu.Unlock()
	return &sessionHandle{store: s, agentID: agentID, sess: sess}, history, nil
}

// commit appends the turn's messages and releases the session.
func (h *sessionHandle) commit(msgs []*schema.Message) {
	h.store.mu.Lock()
	h.sess.messages = append(h.sess.messages, msgs...)
	if over := len(h.sess.messages) - maxSessionMessages; over > 0 {
		h.sess.messages = append([]*schema.Message(nil), h.sess.messages[over:]...)
	}
	h.sess.lastAccess = time.Now()
	h.sess.busy--
	h.store.mu.Unlock()
	<-h.sess.serial
}

// release ends the turn without appending (failed turns leave the history
// untouched).
func (h *sessionHandle) release() {
	h.store.mu.Lock()
	h.sess.lastAccess = time.Now()
	h.sess.busy--
	h.store.mu.Unlock()
	<-h.sess.serial
}

func (h *sessionHandle) sessionID() string {
	return h.sess.id
}

// board returns the session's plantask storage, creating it on first use.
func (h *sessionHandle) board() *planTaskBoard {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	if h.sess.taskBoard == nil {
		h.sess.taskBoard = newPlanTaskBoard()
	}
	return h.sess.taskBoard
}

// evictExpiredLocked drops sessions idle past the TTL. Sessions with turns in
// flight or waiting are never evicted. Caller holds s.mu.
func (s *sessionStore) evictExpiredLocked(byID map[string]*session) {
	now := time.Now()
	for id, sess := range byID {
		if sess.busy == 0 && now.Sub(sess.lastAccess) > sessionIdleTTL {
			delete(byID, id)
		}
	}
}

// evictForCapLocked makes room for one new session by dropping the oldest
// idle sessions. It runs only on the create path so reusing an existing
// session never evicts a neighbor (or, before the busy marker existed, the
// reused session itself). Caller holds s.mu.
func (s *sessionStore) evictForCapLocked(byID map[string]*session) {
	for len(byID) >= maxSessionsPerAgent {
		oldestID := ""
		var oldest time.Time
		for id, sess := range byID {
			if sess.busy > 0 {
				continue
			}
			if oldestID == "" || sess.lastAccess.Before(oldest) {
				oldestID = id
				oldest = sess.lastAccess
			}
		}
		if oldestID == "" {
			return
		}
		delete(byID, oldestID)
	}
}

// sessionCount reports live sessions for one agent (workspace view).
func (s *sessionStore) sessionCount(agentID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.agents[agentID])
}
