package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/acp/agentspi"
	"github.com/agent-guide/agent-gateway/pkg/acp/host/acpupdate"
	"github.com/agent-guide/agent-gateway/pkg/acp/hostconfig"
	acptransport "github.com/agent-guide/agent-gateway/pkg/acp/transport"
)

// stubAgent is a minimal agentspi.Agent for driving instance.prompt directly.
// It deliberately does not implement TerminalUpdateDetector so the driver takes
// the buffered-drain path.
type stubAgent struct{}

func (stubAgent) Name() string { return "stub" }

func (stubAgent) Open(context.Context, acptransport.Handlers) (acptransport.Transport, error) {
	return nil, nil
}
func (stubAgent) InitializeParams() map[string]any        { return map[string]any{} }
func (stubAgent) SessionNewParams(string) map[string]any  { return map[string]any{} }
func (stubAgent) SessionLoadParams(string) map[string]any { return map[string]any{} }

func (stubAgent) PromptParams(sessionID, input string, _ string) map[string]any {
	return map[string]any{"sessionId": sessionID, "input": input}
}

func (stubAgent) Cancel(context.Context, acptransport.Transport, string) error { return nil }

type recordedRequest struct {
	method string
	params any
}

type scriptedTransport struct {
	promptResult json.RawMessage
	promptErr    error
	updates      chan acptransport.Message

	mu       sync.Mutex
	requests []recordedRequest
}

func (s *scriptedTransport) Request(_ context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	s.requests = append(s.requests, recordedRequest{method: method, params: params})
	s.mu.Unlock()
	switch method {
	case "session/prompt":
		return s.promptResult, s.promptErr
	case "session/new":
		return json.RawMessage(`{"sessionId":"s1"}`), nil
	default:
		return json.RawMessage(`{}`), nil
	}
}

func (s *scriptedTransport) recorded(method string) []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []recordedRequest
	for _, r := range s.requests {
		if r.method == method {
			out = append(out, r)
		}
	}
	return out
}

func (s *scriptedTransport) Notify(string, any) error { return nil }

func (s *scriptedTransport) Updates(int) (<-chan acptransport.Message, func()) {
	return s.updates, func() {}
}

func (s *scriptedTransport) Alive() bool  { return true }
func (s *scriptedTransport) Close() error { return nil }

func chunkMessage(sessionUpdate, text string) acptransport.Message {
	params, _ := json.Marshal(map[string]any{
		"update": map[string]any{
			"sessionUpdate": sessionUpdate,
			"content":       map[string]any{"type": "text", "text": text},
		},
	})
	return acptransport.Message{Method: "session/update", Params: params}
}

func deltaMessage(text string) acptransport.Message {
	return chunkMessage("agent_message_chunk", text)
}

// noReasoningAgent suppresses all reasoning updates via ReasoningUpdateFilter.
type noReasoningAgent struct{ stubAgent }

func (noReasoningAgent) AcceptReasoningUpdate(json.RawMessage) bool { return false }

func collectEvents(t *testing.T, agent agentspi.Agent, msgs ...acptransport.Message) []TurnEvent {
	t.Helper()
	updates := make(chan acptransport.Message, len(msgs)+1)
	for _, m := range msgs {
		updates <- m
	}
	tr := &scriptedTransport{promptResult: json.RawMessage(`{"stopReason":"end_turn"}`), updates: updates}
	inst := &instance{agent: agent, t: tr, sessionID: "s1"}
	var events []TurnEvent
	emit := func(ev TurnEvent) error {
		if ev.Event != "session" {
			events = append(events, ev)
		}
		return nil
	}
	if _, err := inst.prompt(context.Background(), TurnRequest{Input: "hi"}, emit); err != nil {
		t.Fatalf("prompt returned error: %v", err)
	}
	return events
}

func TestPromptParsesStopReasonAndDrainsBufferedUpdates(t *testing.T) {
	updates := make(chan acptransport.Message, 8)
	updates <- deltaMessage("hello ")
	updates <- deltaMessage("world")

	tr := &scriptedTransport{
		promptResult: json.RawMessage(`{"stopReason":"max_tokens"}`),
		updates:      updates,
	}
	inst := &instance{agent: stubAgent{}, t: tr, sessionID: "s1"}

	var deltas []string
	emit := func(ev TurnEvent) error {
		if ev.Event == "delta" {
			deltas = append(deltas, ev.Text)
		}
		return nil
	}

	stop, err := inst.prompt(context.Background(), TurnRequest{Input: "hi"}, emit)
	if err != nil {
		t.Fatalf("prompt returned error: %v", err)
	}
	if stop != "max_tokens" {
		t.Fatalf("stop reason = %q, want max_tokens", stop)
	}
	if got := strings.Join(deltas, ""); got != "hello world" {
		t.Fatalf("emitted deltas = %q, want %q (updates were dropped)", got, "hello world")
	}
}

// TestPromptDrainsUpdatesArrivingAfterResult reproduces the live opencode
// race: the session/prompt result is delivered before the final
// agent_message_chunk updates. The post-result idle-grace drain must still
// emit them.
func TestPromptDrainsUpdatesArrivingAfterResult(t *testing.T) {
	prevGrace := postResultIdleGrace
	postResultIdleGrace = 150 * time.Millisecond
	defer func() { postResultIdleGrace = prevGrace }()

	updates := make(chan acptransport.Message, 8)
	tr := &scriptedTransport{
		promptResult: json.RawMessage(`{"stopReason":"end_turn"}`),
		updates:      updates,
	}
	inst := &instance{agent: stubAgent{}, t: tr, sessionID: "s1"}

	go func() {
		// Arrive after the prompt result has already been returned.
		time.Sleep(40 * time.Millisecond)
		updates <- deltaMessage("late ")
		time.Sleep(40 * time.Millisecond)
		updates <- deltaMessage("tail")
	}()

	var deltas []string
	emit := func(ev TurnEvent) error {
		if ev.Event == "delta" {
			deltas = append(deltas, ev.Text)
		}
		return nil
	}
	stop, err := inst.prompt(context.Background(), TurnRequest{Input: "hi"}, emit)
	if err != nil {
		t.Fatalf("prompt returned error: %v", err)
	}
	if stop != "end_turn" {
		t.Fatalf("stop reason = %q, want end_turn", stop)
	}
	if got := strings.Join(deltas, ""); got != "late tail" {
		t.Fatalf("emitted deltas = %q, want %q (post-result updates were dropped)", got, "late tail")
	}
}

func TestPromptDefaultsStopReasonWhenAbsent(t *testing.T) {
	tr := &scriptedTransport{
		promptResult: json.RawMessage(`{}`),
		updates:      make(chan acptransport.Message, 1),
	}
	inst := &instance{agent: stubAgent{}, t: tr, sessionID: "s1"}

	stop, err := inst.prompt(context.Background(), TurnRequest{Input: "hi"}, func(TurnEvent) error { return nil })
	if err != nil {
		t.Fatalf("prompt returned error: %v", err)
	}
	if stop != "end_turn" {
		t.Fatalf("stop reason = %q, want end_turn", stop)
	}
}

func TestPromptEmitsReasoningSeparatelyFromText(t *testing.T) {
	events := collectEvents(t, stubAgent{},
		chunkMessage("agent_thought_chunk", "let me think"),
		chunkMessage("agent_message_chunk", "the answer"),
	)

	byEvent := map[string][]string{}
	for _, ev := range events {
		byEvent[ev.Event] = append(byEvent[ev.Event], ev.Text)
	}
	if got := strings.Join(byEvent["reasoning"], ""); got != "let me think" {
		t.Fatalf("reasoning event = %q, want %q", got, "let me think")
	}
	if got := strings.Join(byEvent["delta"], ""); got != "the answer" {
		t.Fatalf("delta event = %q, want %q", got, "the answer")
	}
}

func TestPromptDropsReasoningWhenFiltered(t *testing.T) {
	events := collectEvents(t, noReasoningAgent{},
		chunkMessage("agent_thought_chunk", "hidden"),
		chunkMessage("agent_message_chunk", "shown"),
	)
	for _, ev := range events {
		if ev.Event == "reasoning" {
			t.Fatalf("reasoning event should have been filtered, got %q", ev.Text)
		}
	}
	var deltas []string
	for _, ev := range events {
		if ev.Event == "delta" {
			deltas = append(deltas, ev.Text)
		}
	}
	if got := strings.Join(deltas, ""); got != "shown" {
		t.Fatalf("delta event = %q, want %q", got, "shown")
	}
}

func TestPromptEmitsStructuredToolCall(t *testing.T) {
	params, _ := json.Marshal(map[string]any{
		"update": map[string]any{"sessionUpdate": "tool_call", "toolCallId": "tc1", "title": "Read"},
	})
	events := collectEvents(t, stubAgent{}, acptransport.Message{Method: "session/update", Params: params})

	var found bool
	for _, ev := range events {
		if ev.Event == "tool_call" {
			found = true
			if len(ev.Data) == 0 {
				t.Fatal("tool_call event carried no structured Data")
			}
		}
	}
	if !found {
		t.Fatalf("no tool_call event emitted, got %+v", events)
	}
}

// modelSelectorAgent records SessionModelSelector invocations.
type modelSelectorAgent struct {
	stubAgent
	gotModelID string
	calls      int
}

func (a *modelSelectorAgent) SelectSessionModel(_ context.Context, _ acptransport.Transport, _, modelID string, _ []agentspi.ConfigOption) ([]agentspi.ConfigOption, error) {
	a.calls++
	a.gotModelID = modelID
	return nil, nil
}

func paramField(t *testing.T, params any, key string) string {
	t.Helper()
	m, ok := params.(map[string]any)
	if !ok {
		t.Fatalf("params is %T, want map", params)
	}
	v, _ := m[key].(string)
	return v
}

func TestInitializeAppliesRuntimeConfigOverrides(t *testing.T) {
	tr := &scriptedTransport{updates: make(chan acptransport.Message, 1)}
	inst := &instance{
		cfg:   hostconfig.Config{ConfigOverrides: map[string]string{"mode": "plan"}},
		agent: stubAgent{},
		t:     tr,
	}
	if err := inst.initialize(context.Background(), false); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	got := tr.recorded("session/set_config_option")
	if len(got) != 1 {
		t.Fatalf("set_config_option calls = %d, want 1", len(got))
	}
	if id := paramField(t, got[0].params, "configId"); id != "mode" {
		t.Fatalf("configId = %q, want mode", id)
	}
	if v := paramField(t, got[0].params, "value"); v != "plan" {
		t.Fatalf("value = %q, want plan", v)
	}
}

func TestInitializeInvokesModelSelector(t *testing.T) {
	tr := &scriptedTransport{updates: make(chan acptransport.Message, 1)}
	agent := &modelSelectorAgent{}
	inst := &instance{
		cfg:   hostconfig.Config{},
		model: "anthropic/claude-3-5-haiku-latest",
		agent: agent,
		t:     tr,
	}
	if err := inst.initialize(context.Background(), false); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if agent.calls != 1 {
		t.Fatalf("SelectSessionModel called %d times, want 1", agent.calls)
	}
	if agent.gotModelID != "anthropic/claude-3-5-haiku-latest" {
		t.Fatalf("model id = %q, want the configured model", agent.gotModelID)
	}
}

func TestPromptAppliesPerTurnConfigOverrides(t *testing.T) {
	tr := &scriptedTransport{
		promptResult: json.RawMessage(`{"stopReason":"end_turn"}`),
		updates:      make(chan acptransport.Message, 1),
	}
	inst := &instance{agent: stubAgent{}, t: tr, sessionID: "s1"}
	_, err := inst.prompt(context.Background(), TurnRequest{Input: "hi", ConfigOverrides: map[string]string{"mode": "build"}}, func(TurnEvent) error { return nil })
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	got := tr.recorded("session/set_config_option")
	if len(got) != 1 || paramField(t, got[0].params, "configId") != "mode" {
		t.Fatalf("expected one set_config_option for mode, got %+v", got)
	}
}

type sessionListAgent struct {
	stubAgent
	cwd string
}

func (a sessionListAgent) Name() string { return "session-list" }

func (a sessionListAgent) Open(context.Context, acptransport.Handlers) (acptransport.Transport, error) {
	tr := &sessionListTransport{requests: []recordedRequest{}}
	lastSessionListTransport = tr
	return tr, nil
}

func (a sessionListAgent) SessionListParams(cwd, cursor string) map[string]any {
	params := map[string]any{}
	if cwd != "" {
		params["cwd"] = cwd
	}
	if cursor != "" {
		params["cursor"] = cursor
	}
	return params
}

type sessionListTransport struct {
	mu       sync.Mutex
	requests []recordedRequest
}

var lastSessionListTransport *sessionListTransport

// sessionListPayload overrides the fake session/list response when non-empty.
var sessionListPayload string

func (s *sessionListTransport) Request(_ context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	s.requests = append(s.requests, recordedRequest{method: method, params: params})
	s.mu.Unlock()
	switch method {
	case "initialize":
		return json.RawMessage(`{"agentCapabilities":{"sessionCapabilities":{"list":{}}}}`), nil
	case "session/list":
		if sessionListPayload != "" {
			return json.RawMessage(sessionListPayload), nil
		}
		return json.RawMessage(`{"sessions":[{"sessionId":"s1","cwd":"/tmp/project","title":"Fix ACP","updatedAt":"2026-06-09T10:11:12Z","_meta":{"turns":3}}],"nextCursor":"next"}`), nil
	default:
		return json.RawMessage(`{}`), nil
	}
}

func (s *sessionListTransport) recorded(method string) []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []recordedRequest
	for _, r := range s.requests {
		if r.method == method {
			out = append(out, r)
		}
	}
	return out
}

func (s *sessionListTransport) Notify(string, any) error { return nil }
func (s *sessionListTransport) Updates(int) (<-chan acptransport.Message, func()) {
	return make(chan acptransport.Message), func() {}
}
func (s *sessionListTransport) Alive() bool  { return true }
func (s *sessionListTransport) Close() error { return nil }

func init() {
	agentspi.Register("session-list", func(req agentspi.OpenRequest) (agentspi.Agent, error) {
		return sessionListAgent{cwd: req.CWD}, nil
	})
	agentspi.Register("session-list-missing-cap", func(agentspi.OpenRequest) (agentspi.Agent, error) {
		return missingSessionListCapabilityAgent{}, nil
	})
	agentspi.Register("session-list-no-spi", func(agentspi.OpenRequest) (agentspi.Agent, error) {
		return stubAgent{}, nil
	})
}

type missingSessionListCapabilityAgent struct{ sessionListAgent }

func (missingSessionListCapabilityAgent) Name() string { return "session-list-missing-cap" }

func (missingSessionListCapabilityAgent) Open(context.Context, acptransport.Handlers) (acptransport.Transport, error) {
	return &missingSessionListCapabilityTransport{}, nil
}

type missingSessionListCapabilityTransport struct{ sessionListTransport }

func (s *missingSessionListCapabilityTransport) Request(_ context.Context, method string, params any) (json.RawMessage, error) {
	if method == "initialize" {
		return json.RawMessage(`{"agentCapabilities":{"sessionCapabilities":{}}}`), nil
	}
	return s.sessionListTransport.Request(context.Background(), method, params)
}

func TestListAgentSessionsCallsSessionList(t *testing.T) {
	result, err := listAgentSessions(context.Background(), hostconfig.Config{
		AgentType: "session-list",
		CWD:       "/tmp/project",
	}, ListSessionsRequest{CWD: "/tmp/project", Cursor: "cur"})
	if err != nil {
		t.Fatalf("listAgentSessions: %v", err)
	}
	if result.NextCursor != "next" {
		t.Fatalf("next cursor = %q, want next", result.NextCursor)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(result.Sessions))
	}
	got := result.Sessions[0]
	if got.SessionID != "s1" || got.CWD != "/tmp/project" || got.Title != "Fix ACP" {
		t.Fatalf("unexpected session info: %+v", got)
	}
	if got.UpdatedAt == nil || got.UpdatedAt.Format(time.RFC3339) != "2026-06-09T10:11:12Z" {
		t.Fatalf("updatedAt = %v, want parsed timestamp", got.UpdatedAt)
	}
}

func TestListAgentSessionsOmitsCWDFilterWhenAbsent(t *testing.T) {
	_, err := listAgentSessions(context.Background(), hostconfig.Config{
		AgentType: "session-list",
		CWD:       "/tmp/project",
	}, ListSessionsRequest{Cursor: "cur"})
	if err != nil {
		t.Fatalf("listAgentSessions: %v", err)
	}
	got := lastSessionListTransport.recorded("session/list")
	if len(got) != 1 {
		t.Fatalf("session/list calls = %d, want 1", len(got))
	}
	params, ok := got[0].params.(map[string]any)
	if !ok {
		t.Fatalf("session/list params are %T, want map", got[0].params)
	}
	if _, exists := params["cwd"]; exists {
		t.Fatalf("session/list unexpectedly included cwd filter: %+v", params)
	}
	if params["cursor"] != "cur" {
		t.Fatalf("cursor = %v, want cur", params["cursor"])
	}
}

func TestListAgentSessionsFiltersCWDGatewaySide(t *testing.T) {
	// Stored-cwd shapes differ per real agent: codex-acp stores the session/new
	// cwd verbatim while opencode stores the canonical symlink-resolved path.
	// The gateway-side filter must match both, and must never forward the cwd
	// to the agent.
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	canonicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("eval target: %v", err)
	}
	sessionListPayload = fmt.Sprintf(
		`{"sessions":[{"sessionId":"verbatim","cwd":%q},{"sessionId":"canonical","cwd":%q},{"sessionId":"other","cwd":"/somewhere/else"}]}`,
		link, canonicalTarget,
	)
	defer func() { sessionListPayload = "" }()

	result, err := listAgentSessions(context.Background(), hostconfig.Config{
		AgentType: "session-list",
		CWD:       link,
	}, ListSessionsRequest{CWD: link})
	if err != nil {
		t.Fatalf("listAgentSessions: %v", err)
	}
	if len(result.Sessions) != 2 {
		t.Fatalf("sessions = %+v, want the verbatim and canonical matches", result.Sessions)
	}
	if result.Sessions[0].SessionID != "verbatim" || result.Sessions[1].SessionID != "canonical" {
		t.Fatalf("unexpected filtered sessions: %+v", result.Sessions)
	}
	got := lastSessionListTransport.recorded("session/list")
	if len(got) != 1 {
		t.Fatalf("session/list calls = %d, want 1", len(got))
	}
	params, ok := got[0].params.(map[string]any)
	if !ok {
		t.Fatalf("session/list params are %T, want map", got[0].params)
	}
	if _, exists := params["cwd"]; exists {
		t.Fatalf("session/list unexpectedly forwarded cwd filter to the agent: %+v", params)
	}
}

func TestListAgentSessionsRequiresAdvertisedCapability(t *testing.T) {
	_, err := listAgentSessions(context.Background(), hostconfig.Config{
		AgentType: "session-list-missing-cap",
		CWD:       "/tmp/project",
	}, ListSessionsRequest{})
	if err == nil || !strings.Contains(err.Error(), "does not advertise session/list") {
		t.Fatalf("error = %v, want missing capability error", err)
	}
}

func TestListAgentSessionsRequiresSPI(t *testing.T) {
	_, err := listAgentSessions(context.Background(), hostconfig.Config{
		AgentType: "session-list-no-spi",
		CWD:       "/tmp/project",
	}, ListSessionsRequest{})
	if err == nil || !strings.Contains(err.Error(), "does not implement session/list") {
		t.Fatalf("error = %v, want missing SPI error", err)
	}
}

func structuredMessage(sessionUpdate string, fields map[string]any) acptransport.Message {
	update := map[string]any{"sessionUpdate": sessionUpdate}
	for k, v := range fields {
		update[k] = v
	}
	params, _ := json.Marshal(map[string]any{"update": update})
	return acptransport.Message{Method: "session/update", Params: params}
}

func TestSessionMetaCacheStoresAndReplaysLatestState(t *testing.T) {
	inst := &instance{agent: stubAgent{}, sessionID: "s1"}
	for _, msg := range []acptransport.Message{
		structuredMessage("available_commands_update", map[string]any{"availableCommands": []any{map[string]any{"name": "init"}}}),
		structuredMessage("usage_update", map[string]any{"used": float64(10)}),
		structuredMessage("usage_update", map[string]any{"used": float64(20)}),
		structuredMessage("session_info_update", map[string]any{"title": "Fix ACP"}),
	} {
		for _, ev := range acpupdate.Parse(msg.Params) {
			inst.meta.store(ev.Kind, ev.Data)
		}
	}

	snap := inst.meta.snapshot()
	if len(snap.AvailableCommands) == 0 || len(snap.SessionInfo) == 0 {
		t.Fatalf("metadata not cached: %+v", snap)
	}
	var usage struct {
		Used int `json:"used"`
	}
	if err := json.Unmarshal(snap.Usage, &usage); err != nil || usage.Used != 20 {
		t.Fatalf("usage snapshot = %s, want the latest update (used=20)", snap.Usage)
	}

	events := inst.meta.turnStartEvents()
	if len(events) != 3 {
		t.Fatalf("turn-start events = %d, want 3 (commands, session_info, usage)", len(events))
	}
	order := make([]string, 0, len(events))
	for _, ev := range events {
		if len(ev.Data) == 0 {
			t.Fatalf("turn-start event %q carried no data", ev.Event)
		}
		order = append(order, ev.Event)
	}
	want := []string{"available_commands", "session_info", "usage"}
	for idx := range want {
		if order[idx] != want[idx] {
			t.Fatalf("turn-start event order = %v, want %v", order, want)
		}
	}
}

func TestPromptReplaysCachedMetadataAtTurnStart(t *testing.T) {
	tr := &scriptedTransport{
		promptResult: json.RawMessage(`{"stopReason":"end_turn"}`),
		updates:      make(chan acptransport.Message, 1),
	}
	inst := &instance{agent: stubAgent{}, t: tr, sessionID: "s1"}
	msg := structuredMessage("available_commands_update", map[string]any{"availableCommands": []any{map[string]any{"name": "compact"}}})
	for _, ev := range acpupdate.Parse(msg.Params) {
		inst.meta.store(ev.Kind, ev.Data)
	}

	var events []string
	emit := func(ev TurnEvent) error {
		events = append(events, ev.Event)
		return nil
	}
	if _, err := inst.prompt(context.Background(), TurnRequest{Input: "hi"}, emit); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if len(events) < 2 || events[0] != "session" || events[1] != "available_commands" {
		t.Fatalf("events = %v, want session then cached available_commands", events)
	}
}

func TestCacheNewSessionConfigOptionsSynthesizesUpdateShape(t *testing.T) {
	inst := &instance{agent: stubAgent{}}
	inst.cacheNewSessionConfigOptions(json.RawMessage(`{"sessionId":"s1","configOptions":[{"id":"model","currentValue":"opencode/big-pickle"}]}`))

	snap := inst.meta.snapshot()
	if len(snap.ConfigOptions) == 0 {
		t.Fatal("config options from session/new were not cached")
	}
	var payload struct {
		SessionUpdate string `json:"sessionUpdate"`
		ConfigOptions []struct {
			ID           string `json:"id"`
			CurrentValue string `json:"currentValue"`
		} `json:"configOptions"`
	}
	if err := json.Unmarshal(snap.ConfigOptions, &payload); err != nil {
		t.Fatalf("decode cached config options: %v", err)
	}
	if payload.SessionUpdate != "config_option_update" {
		t.Fatalf("sessionUpdate = %q, want config_option_update", payload.SessionUpdate)
	}
	if len(payload.ConfigOptions) != 1 || payload.ConfigOptions[0].ID != "model" || payload.ConfigOptions[0].CurrentValue != "opencode/big-pickle" {
		t.Fatalf("unexpected cached config options: %+v", payload.ConfigOptions)
	}

	// A session/new result without config options must not synthesize an entry.
	empty := &instance{agent: stubAgent{}}
	empty.cacheNewSessionConfigOptions(json.RawMessage(`{"sessionId":"s2"}`))
	if snap := empty.meta.snapshot(); len(snap.ConfigOptions) != 0 {
		t.Fatalf("config options cached from a result without any: %s", snap.ConfigOptions)
	}
}

func TestDrainBufferedEmitsAllWithoutBlocking(t *testing.T) {
	updates := make(chan acptransport.Message, 4)
	updates <- deltaMessage("a")
	updates <- deltaMessage("b")
	updates <- acptransport.Message{Method: "notification/ignored"}

	var deltas []string
	err := drainBuffered(updates, func(msg acptransport.Message) error {
		if msg.Method != "session/update" {
			return nil
		}
		for _, ev := range acpupdate.Parse(msg.Params) {
			if ev.Kind == acpupdate.KindText {
				deltas = append(deltas, ev.Text)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("drainBuffered returned error: %v", err)
	}
	if got := strings.Join(deltas, ""); got != "ab" {
		t.Fatalf("drained deltas = %q, want %q", got, "ab")
	}
}
