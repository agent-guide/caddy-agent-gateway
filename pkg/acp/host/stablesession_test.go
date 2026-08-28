package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	acptransport "github.com/agent-guide/agent-gateway/pkg/acp/transport"
)

// stableAgent implements StableSessionResolver. With deferred=true the first
// resolution attempt (during setup) reports "not resolvable yet".
type stableAgent struct {
	stubAgent
	bound      string
	deferred   bool
	resolveErr error

	calls  int
	gotRaw []string
}

func (a *stableAgent) ResolveBoundSessionID(_ context.Context, _ acptransport.Transport, _, raw string) (string, error) {
	a.calls++
	a.gotRaw = append(a.gotRaw, raw)
	if a.resolveErr != nil {
		return "", a.resolveErr
	}
	if a.deferred && a.calls == 1 {
		return "", nil
	}
	return a.bound, nil
}

// loadResolverAgent implements SessionLoadResolver, mapping the host-visible
// id to a distinct backend load id and host-bound id.
type loadResolverAgent struct {
	stubAgent
	gotRequested string
}

func (a *loadResolverAgent) ResolveLoadSessionID(_ context.Context, _ acptransport.Transport, _, requested string) (string, string, error) {
	a.gotRequested = requested
	return "load-1", "host-1b", nil
}

func (a *loadResolverAgent) SessionLoadParams(sessionID string) map[string]any {
	return map[string]any{"sessionId": sessionID}
}

func TestInitializeResolvesStableSessionIDImmediately(t *testing.T) {
	tr := &scriptedTransport{promptResult: json.RawMessage(`{"stopReason":"end_turn"}`), updates: make(chan acptransport.Message, 1)}
	agent := &stableAgent{bound: "stable-1"}
	inst := &instance{agent: agent, t: tr}
	if err := inst.initialize(context.Background(), false); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if inst.sessionID != "stable-1" || inst.rawSessionID != "s1" || inst.stablePending {
		t.Fatalf("ids = bound %q raw %q pending %v, want stable-1/s1/false", inst.sessionID, inst.rawSessionID, inst.stablePending)
	}
	if len(agent.gotRaw) != 1 || agent.gotRaw[0] != "s1" {
		t.Fatalf("resolver received raw ids %v, want [s1]", agent.gotRaw)
	}

	// The wire keeps using the raw protocol id; the session event carries the
	// stable id.
	var sessionEvents []string
	if _, err := inst.prompt(context.Background(), TurnRequest{Input: "hi"}, func(ev TurnEvent) error {
		if ev.Event == "session" {
			sessionEvents = append(sessionEvents, ev.SessionID)
		}
		return nil
	}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	prompts := tr.recorded("session/prompt")
	if len(prompts) != 1 || paramField(t, prompts[0].params, "sessionId") != "s1" {
		t.Fatalf("session/prompt params = %+v, want raw id s1", prompts)
	}
	if len(sessionEvents) != 1 || sessionEvents[0] != "stable-1" {
		t.Fatalf("session events = %v, want [stable-1]", sessionEvents)
	}
}

func TestPromptEmitsLateSessionEventForDeferredStableID(t *testing.T) {
	tr := &scriptedTransport{promptResult: json.RawMessage(`{"stopReason":"end_turn"}`), updates: make(chan acptransport.Message, 1)}
	agent := &stableAgent{bound: "stable-9", deferred: true}
	inst := &instance{agent: agent, t: tr}
	if err := inst.initialize(context.Background(), false); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if !inst.stablePending || inst.sessionID != "s1" {
		t.Fatalf("after setup: pending %v session %q, want pending raw id", inst.stablePending, inst.sessionID)
	}

	var sessionEvents []string
	if _, err := inst.prompt(context.Background(), TurnRequest{Input: "hi"}, func(ev TurnEvent) error {
		if ev.Event == "session" {
			sessionEvents = append(sessionEvents, ev.SessionID)
		}
		return nil
	}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if len(sessionEvents) != 2 || sessionEvents[0] != "s1" || sessionEvents[1] != "stable-9" {
		t.Fatalf("session events = %v, want [s1 stable-9] (initial then late stable id)", sessionEvents)
	}
	if inst.sessionID != "stable-9" || inst.stablePending {
		t.Fatalf("after prompt: session %q pending %v, want stable-9/false", inst.sessionID, inst.stablePending)
	}
	if got := strings.Join(agent.gotRaw, ","); got != "s1,s1" {
		t.Fatalf("resolver raw ids = %q, want s1,s1", got)
	}
}

func TestInitializeFailsOnStableResolverError(t *testing.T) {
	tr := &scriptedTransport{updates: make(chan acptransport.Message, 1)}
	inst := &instance{agent: &stableAgent{resolveErr: fmt.Errorf("boom")}, t: tr}
	if err := inst.initialize(context.Background(), false); err == nil || !strings.Contains(err.Error(), "resolve bound session id") {
		t.Fatalf("error = %v, want resolve bound session id failure", err)
	}
}

func TestInitializeUsesSessionLoadResolver(t *testing.T) {
	tr := &scriptedTransport{updates: make(chan acptransport.Message, 1)}
	agent := &loadResolverAgent{}
	inst := &instance{agent: agent, t: tr, sessionID: "host-1"}
	if err := inst.initialize(context.Background(), false); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if agent.gotRequested != "host-1" {
		t.Fatalf("resolver received %q, want host-1", agent.gotRequested)
	}
	loads := tr.recorded("session/load")
	if len(loads) != 1 || paramField(t, loads[0].params, "sessionId") != "load-1" {
		t.Fatalf("session/load params = %+v, want backend load id load-1", loads)
	}
	if inst.rawSessionID != "load-1" || inst.sessionID != "host-1b" {
		t.Fatalf("ids = raw %q bound %q, want load-1/host-1b", inst.rawSessionID, inst.sessionID)
	}
}
