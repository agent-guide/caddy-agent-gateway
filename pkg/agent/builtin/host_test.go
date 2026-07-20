package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/pkg/agent"
	basemcp "github.com/agent-guide/agent-gateway/pkg/mcp"
	mcpservice "github.com/agent-guide/agent-gateway/pkg/mcp/service"
)

// fakeChatModel scripts a sequence of assistant replies; when asked with tool
// results present it returns the final answer.
type fakeChatModel struct {
	mu       sync.Mutex
	requests [][]*schema.Message
	tools    []*schema.ToolInfo
	// script returns the next assistant message given the input.
	script func(input []*schema.Message) *schema.Message
	// toolAwareScript, when set, wins over script and also receives the tools
	// visible on this call: the per-call model.WithTools option when present
	// (how ADK applies middleware-filtered tool lists), else the bound tools.
	toolAwareScript func(input []*schema.Message, visible []*schema.ToolInfo) *schema.Message
	// scriptErr, when set, wins over both and may fail the call (retry tests).
	scriptErr func(input []*schema.Message) (*schema.Message, error)
	block     chan struct{} // when non-nil, Generate blocks until closed or ctx done
	started   chan struct{} // when non-nil, receives a signal as Generate is entered
}

func (m *fakeChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	if m.started != nil {
		select {
		case m.started <- struct{}{}:
		default:
		}
	}
	if m.block != nil {
		select {
		case <-m.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	m.mu.Lock()
	m.requests = append(m.requests, input)
	m.mu.Unlock()
	if m.scriptErr != nil {
		return m.scriptErr(input)
	}
	if m.toolAwareScript != nil {
		visible := m.tools
		if o := einomodel.GetCommonOptions(&einomodel.Options{}, opts...); o.Tools != nil {
			visible = o.Tools
		}
		return m.toolAwareScript(input, visible), nil
	}
	return m.script(input), nil
}

func (m *fakeChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(msg, nil)
	sw.Close()
	return sr, nil
}

func (m *fakeChatModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	bound := &fakeChatModel{script: m.script, toolAwareScript: m.toolAwareScript, scriptErr: m.scriptErr, block: m.block, started: m.started}
	bound.tools = tools
	// Share request recording with the root model so tests can inspect it.
	bound.mu = sync.Mutex{}
	return bound, nil
}

type fakeModelResolver struct {
	model einomodel.ToolCallingChatModel
	err   error

	mu               sync.Mutex
	requireToolsSeen []bool
}

func (r *fakeModelResolver) ResolveChatModel(_ context.Context, _, _ string, requireTools bool) (einomodel.ToolCallingChatModel, error) {
	r.mu.Lock()
	r.requireToolsSeen = append(r.requireToolsSeen, requireTools)
	r.mu.Unlock()
	return r.model, r.err
}

type fakeAgentSource struct {
	agents map[string]agent.Agent
}

func (s *fakeAgentSource) Get(_ context.Context, id string) (agent.Agent, error) {
	a, ok := s.agents[id]
	if !ok {
		return agent.Agent{}, agent.ErrAgentNotConfigured
	}
	return a, nil
}

type fakeToolCaller struct {
	tools  []basemcp.Tool
	result *basemcp.ToolResult
	calls  int
}

func (f *fakeToolCaller) ListTools(_ context.Context, _ string) ([]basemcp.Tool, error) {
	return f.tools, nil
}

func (f *fakeToolCaller) CallTool(_ context.Context, _, _ string, _ map[string]any, _ chan<- mcpservice.UpstreamProgress) (*basemcp.ToolResult, error) {
	f.calls++
	return f.result, nil
}

func testAgent(id string) agent.Agent {
	a := agent.Agent{
		ID:   id,
		Name: id,
		Runtime: agent.Runtime{
			Type: agent.RuntimeTypeBuiltin,
			Builtin: &agent.BuiltinRuntime{
				Model:        agent.BuiltinModel{LLMRouteID: "chat-main"},
				SystemPrompt: "You are a test agent.",
				Topology:     agent.BuiltinTopology{Kind: agent.TopologyKindSingle},
			},
		},
		Routes:    agent.Routes{LLMRouteIDs: []string{"chat-main"}},
		UpdatedAt: time.Now().UTC(),
	}
	return a
}

type collectedEvents struct {
	mu     sync.Mutex
	events []TurnEvent
}

func (c *collectedEvents) sink(ev TurnEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	return nil
}

func (c *collectedEvents) names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.events))
	for _, ev := range c.events {
		out = append(out, ev.Event)
	}
	return out
}

func (c *collectedEvents) byName(name string) []TurnEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []TurnEvent
	for _, ev := range c.events {
		if ev.Event == name {
			out = append(out, ev)
		}
	}
	return out
}

func replyModel(text string) *fakeChatModel {
	return &fakeChatModel{script: func(_ []*schema.Message) *schema.Message {
		msg := schema.AssistantMessage(text, nil)
		msg.ResponseMeta = &schema.ResponseMeta{
			FinishReason: "stop",
			Usage:        &schema.TokenUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
		}
		return msg
	}}
}

func TestServeTurnSingleAgentEmitsVocabulary(t *testing.T) {
	model := replyModel("hello from builtin")
	resolver := &fakeModelResolver{model: model}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"triage": testAgent("triage")}},
		Models: resolver,
	})

	sink := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "triage", TurnRequest{Input: "hi"}, sink.sink); err != nil {
		t.Fatalf("ServeTurn() error = %v", err)
	}
	names := sink.names()
	if names[0] != EventSession || names[len(names)-1] != EventDone {
		t.Fatalf("events = %v, want session first and done last", names)
	}
	contents := sink.byName(EventContent)
	if len(contents) == 0 || !strings.Contains(contents[0].Text, "hello from builtin") {
		t.Fatalf("content events = %+v, want the model reply", contents)
	}
	usages := sink.byName(EventUsage)
	if len(usages) == 0 {
		t.Fatalf("events = %v, want a usage event", names)
	}
	var tokens map[string]int
	if err := json.Unmarshal(usages[0].Data, &tokens); err != nil || tokens["total_tokens"] != 10 {
		t.Fatalf("usage data = %s, want total_tokens 10", usages[0].Data)
	}
	if got := resolver.requireToolsSeen; len(got) != 1 || got[0] {
		t.Fatalf("requireTools seen = %v, want [false] for a tool-less single node", got)
	}
}

func TestServeTurnSessionContinuity(t *testing.T) {
	model := replyModel("reply")
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"triage": testAgent("triage")}},
		Models: &fakeModelResolver{model: model},
	})

	sink := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "triage", TurnRequest{Input: "first"}, sink.sink); err != nil {
		t.Fatalf("first turn error = %v", err)
	}
	sessionID := sink.byName(EventSession)[0].SessionID
	if sessionID == "" {
		t.Fatal("first turn emitted no session id")
	}

	if err := host.ServeTurn(t.Context(), "triage", TurnRequest{SessionID: sessionID, Input: "second"}, sink.sink); err != nil {
		t.Fatalf("second turn error = %v", err)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	last := model.requests[len(model.requests)-1]
	joined := ""
	for _, msg := range last {
		joined += string(msg.Role) + ":" + msg.Content + "|"
	}
	if !strings.Contains(joined, "first") || !strings.Contains(joined, "second") {
		t.Fatalf("second-turn input = %q, want history including the first turn", joined)
	}
}

func TestServeTurnRejectsDisabledUnknownAndEmpty(t *testing.T) {
	disabled := testAgent("off")
	disabled.Disabled = true
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"off": disabled}},
		Models: &fakeModelResolver{model: replyModel("x")},
	})
	sink := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "off", TurnRequest{Input: "hi"}, sink.sink); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("disabled agent error = %v, want ErrInvalidRequest", err)
	}
	if err := host.ServeTurn(t.Context(), "missing", TurnRequest{Input: "hi"}, sink.sink); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("unknown agent error = %v, want ErrAgentNotFound", err)
	}
	ok := testAgent("ok")
	host2 := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"ok": ok}},
		Models: &fakeModelResolver{model: replyModel("x")},
	})
	if err := host2.ServeTurn(t.Context(), "ok", TurnRequest{}, sink.sink); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty input error = %v, want ErrInvalidRequest", err)
	}
}

func TestServeTurnConcurrencyLimitFailsClosed(t *testing.T) {
	blocked := make(chan struct{})
	model := replyModel("x")
	model.block = blocked
	a := testAgent("busy")
	a.Runtime.Builtin.Limits = &agent.BuiltinLimits{MaxConcurrentTurns: 1}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"busy": a}},
		Models: &fakeModelResolver{model: model},
	})

	firstDone := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		sink := &collectedEvents{}
		close(started)
		firstDone <- host.ServeTurn(context.Background(), "busy", TurnRequest{Input: "one"}, sink.sink)
	}()
	<-started
	// Wait for the first turn to occupy the semaphore.
	deadline := time.After(2 * time.Second)
	for {
		if host.State("busy").InflightTurns == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("first turn never became in-flight")
		case <-time.After(5 * time.Millisecond):
		}
	}
	sink := &collectedEvents{}
	if err := host.ServeTurn(context.Background(), "busy", TurnRequest{Input: "two"}, sink.sink); !errors.Is(err, ErrTurnLimitExceeded) {
		t.Fatalf("second concurrent turn error = %v, want ErrTurnLimitExceeded", err)
	}
	close(blocked)
	if err := <-firstDone; err != nil {
		t.Fatalf("first turn error = %v", err)
	}
}

func TestServeTurnToolLoopProducesChildSpans(t *testing.T) {
	// Script: first call returns a tool call, second returns the final answer.
	var callCount int
	var mu sync.Mutex
	model := &fakeChatModel{script: func(input []*schema.Message) *schema.Message {
		mu.Lock()
		defer mu.Unlock()
		callCount++
		if callCount == 1 {
			msg := schema.AssistantMessage("", []schema.ToolCall{{
				ID:   "call-1",
				Type: "function",
				Function: schema.FunctionCall{
					Name:      "read_file",
					Arguments: `{"path":"/tmp/x"}`,
				},
			}})
			return msg
		}
		return schema.AssistantMessage("file says hi", nil)
	}}
	tools := &fakeToolCaller{
		tools: []basemcp.Tool{{Name: "read_file", Description: "Read a file", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}},
		}}},
		result: &basemcp.ToolResult{Content: []any{map[string]any{"type": "text", "text": "hi"}}},
	}
	a := testAgent("tooler")
	a.Runtime.Builtin.Tools = []agent.BuiltinToolSelection{{MCPServiceID: "fs", Tools: []string{"read_file"}}}
	a.Resources.MCPServiceIDs = []string{"fs"}

	events := &usage.InMemorySink{}
	observer := usage.NewObserver(events)
	resolver := &fakeModelResolver{model: model}
	host := NewHost(Config{
		Agents:   &fakeAgentSource{agents: map[string]agent.Agent{"tooler": a}},
		Models:   resolver,
		Tools:    tools,
		Observer: observer,
	})

	span, ctx := observer.Begin(t.Context(), usage.InteractionDimensions{RouteKind: "builtin", RouteID: "builtin-route", AgentID: "tooler"})
	sink := &collectedEvents{}
	if err := host.ServeTurn(ctx, "tooler", TurnRequest{Input: "read it"}, sink.sink); err != nil {
		t.Fatalf("ServeTurn() error = %v", err)
	}
	span.Finish(usage.InteractionOutcome{Success: true, StatusCode: 200})

	if tools.calls != 1 {
		t.Fatalf("tool calls = %d, want 1", tools.calls)
	}
	if got := resolver.requireToolsSeen; len(got) != 1 || !got[0] {
		t.Fatalf("requireTools seen = %v, want [true] for a tool-carrying node", got)
	}
	toolEvents := sink.byName(EventToolCall)
	if len(toolEvents) < 2 {
		t.Fatalf("tool_call events = %d, want call and result", len(toolEvents))
	}

	var llmChildren, mcpChildren int
	var builtinEvents []usage.BuiltinUsageEvent
	for _, ev := range events.Events {
		switch typed := ev.(type) {
		case usage.LLMUsageEvent:
			llmChildren++
			if typed.AgentID != "tooler" {
				t.Fatalf("llm child span agent_id = %q, want tooler", typed.AgentID)
			}
		case usage.MCPUsageEvent:
			mcpChildren++
			if typed.ToolName != "read_file" || typed.ServiceID != "fs" {
				t.Fatalf("mcp child span = %+v, want read_file on fs", typed)
			}
		case usage.BuiltinUsageEvent:
			builtinEvents = append(builtinEvents, typed)
		}
	}
	if llmChildren < 2 {
		t.Fatalf("llm child events = %d, want one per model call (>=2)", llmChildren)
	}
	if mcpChildren != 1 {
		t.Fatalf("mcp child events = %d, want 1", mcpChildren)
	}
	if len(builtinEvents) != 1 {
		t.Fatalf("builtin events = %d, want exactly 1 turn event", len(builtinEvents))
	}
	turn := builtinEvents[0]
	if turn.Operation != "turn" || turn.ResultStatus != "success" || turn.SessionID == "" {
		t.Fatalf("turn event = %+v, want successful turn with session", turn)
	}
	if turn.ModelSteps < 2 || turn.ToolSteps != 1 {
		t.Fatalf("steps = model %d / tool %d, want >=2 and 1", turn.ModelSteps, turn.ToolSteps)
	}
}

func TestServeTurnSerializesConcurrentTurnsOnOneSession(t *testing.T) {
	// Each turn appends exactly two messages (user + assistant), and ADK
	// prepends the system instruction to every model input. With
	// session-level serialization the i-th turn must see 2*i prior history
	// messages, so the recorded input lengths are exactly {2, 4, 6, ...} —
	// each once, never duplicated or skipped.
	model := replyModel("ok")
	a := testAgent("serial")
	a.Runtime.Builtin.Limits = &agent.BuiltinLimits{MaxConcurrentTurns: 8}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"serial": a}},
		Models: &fakeModelResolver{model: model},
	})

	sink := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "serial", TurnRequest{Input: "turn-0"}, sink.sink); err != nil {
		t.Fatalf("seed turn error = %v", err)
	}
	sessionID := sink.byName(EventSession)[0].SessionID

	const concurrent = 4
	var wg sync.WaitGroup
	errs := make(chan error, concurrent)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := &collectedEvents{}
			errs <- host.ServeTurn(context.Background(), "serial", TurnRequest{SessionID: sessionID, Input: "concurrent"}, s.sink)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent turn error = %v", err)
		}
	}

	model.mu.Lock()
	defer model.mu.Unlock()
	lengths := map[int]int{}
	for _, req := range model.requests {
		lengths[len(req)]++
	}
	for i := 0; i <= concurrent; i++ {
		want := 2*i + 2
		if lengths[want] != 1 {
			t.Fatalf("input lengths = %v, want each of {2,4,...,%d} exactly once (turns serialized)", lengths, 2*concurrent+2)
		}
	}
}

func TestSessionWaitIsCancellableAndHoldsNoConcurrencySlot(t *testing.T) {
	blockSlow := make(chan struct{})
	model := &fakeChatModel{script: func(input []*schema.Message) *schema.Message {
		if strings.Contains(input[len(input)-1].Content, "slow") {
			<-blockSlow
		}
		return schema.AssistantMessage("ok", nil)
	}}
	a := testAgent("waiter")
	a.Runtime.Builtin.Limits = &agent.BuiltinLimits{MaxConcurrentTurns: 2, TurnTimeoutSeconds: 1}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"waiter": a}},
		Models: &fakeModelResolver{model: model},
	})

	// Seed a session, then occupy it with a slow turn.
	seed := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "waiter", TurnRequest{Input: "seed"}, seed.sink); err != nil {
		t.Fatalf("seed turn error = %v", err)
	}
	sessionID := seed.byName(EventSession)[0].SessionID

	slowDone := make(chan error, 1)
	go func() {
		s := &collectedEvents{}
		slowDone <- host.ServeTurn(context.Background(), "waiter", TurnRequest{SessionID: sessionID, Input: "slow"}, s.sink)
	}()
	deadline := time.After(2 * time.Second)
	for host.State("waiter").InflightTurns != 1 {
		select {
		case <-deadline:
			t.Fatal("slow turn never became in-flight")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// A second turn on the SAME session waits without holding a concurrency
	// slot and aborts with ErrSessionBusy when its turn timeout fires.
	waiterDone := make(chan error, 1)
	go func() {
		s := &collectedEvents{}
		waiterDone <- host.ServeTurn(context.Background(), "waiter", TurnRequest{SessionID: sessionID, Input: "queued"}, s.sink)
	}()

	// While the slow turn runs and the same-session turn waits, a DIFFERENT
	// session must still get the remaining concurrency slot.
	time.Sleep(50 * time.Millisecond)
	other := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "waiter", TurnRequest{Input: "fresh session"}, other.sink); err != nil {
		t.Fatalf("other-session turn error = %v, want success while a same-session turn is queued", err)
	}

	if err := <-waiterDone; !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("queued same-session turn error = %v, want ErrSessionBusy after turn timeout", err)
	}
	close(blockSlow)
	if err := <-slowDone; err != nil {
		t.Fatalf("slow turn error = %v", err)
	}
}

func TestSessionCapRejectsNewSessionsWhenAllBusy(t *testing.T) {
	block := make(chan struct{})
	model := replyModel("ok")
	model.block = block
	a := testAgent("packed")
	a.Runtime.Builtin.Limits = &agent.BuiltinLimits{MaxConcurrentTurns: maxSessionsPerAgent + 8, TurnTimeoutSeconds: 30}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"packed": a}},
		Models: &fakeModelResolver{model: model},
	})

	var wg sync.WaitGroup
	errs := make(chan error, maxSessionsPerAgent)
	for i := 0; i < maxSessionsPerAgent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := &collectedEvents{}
			errs <- host.ServeTurn(context.Background(), "packed", TurnRequest{Input: "hold"}, s.sink)
		}()
	}
	deadline := time.After(5 * time.Second)
	for host.State("packed").InflightTurns != maxSessionsPerAgent {
		select {
		case <-deadline:
			t.Fatalf("in-flight = %d, want %d busy sessions", host.State("packed").InflightTurns, maxSessionsPerAgent)
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Every session is busy and at the cap: a new session must be rejected,
	// not created.
	s := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "packed", TurnRequest{Input: "one too many"}, s.sink); !errors.Is(err, ErrSessionLimitExceeded) {
		t.Fatalf("over-cap turn error = %v, want ErrSessionLimitExceeded", err)
	}
	if got := host.State("packed").LiveSessions; got > maxSessionsPerAgent {
		t.Fatalf("live sessions = %d, want cap %d respected", got, maxSessionsPerAgent)
	}

	close(block)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("held turn error = %v", err)
		}
	}
}

func TestServeTurnRematerializesOnDefinitionUpdate(t *testing.T) {
	model := replyModel("v1")
	source := &fakeAgentSource{agents: map[string]agent.Agent{"a": testAgent("a")}}
	host := NewHost(Config{Agents: source, Models: &fakeModelResolver{model: model}})
	sink := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "a", TurnRequest{Input: "x"}, sink.sink); err != nil {
		t.Fatalf("turn error = %v", err)
	}
	first := host.State("a").MaterializedAt

	updated := source.agents["a"]
	updated.UpdatedAt = updated.UpdatedAt.Add(time.Second)
	source.agents["a"] = updated
	if err := host.ServeTurn(t.Context(), "a", TurnRequest{Input: "y"}, sink.sink); err != nil {
		t.Fatalf("turn after update error = %v", err)
	}
	if !host.State("a").MaterializedAt.After(first) {
		t.Fatal("definition update did not re-materialize the graph")
	}
}

func TestServeTurnDeepTopologyMaterializesAndReplies(t *testing.T) {
	model := replyModel("deep reply")
	resolver := &fakeModelResolver{model: model}
	a := testAgent("deep-agent")
	a.Runtime.Builtin.Topology = agent.BuiltinTopology{
		Kind:      agent.TopologyKindDeep,
		SubAgents: []agent.BuiltinSubAgent{{Name: "researcher", Description: "digs into details"}},
	}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"deep-agent": a}},
		Models: resolver,
	})

	sink := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "deep-agent", TurnRequest{Input: "hi"}, sink.sink); err != nil {
		t.Fatalf("ServeTurn() error = %v", err)
	}
	names := sink.names()
	if names[len(names)-1] != EventDone {
		t.Fatalf("events = %v, want done last", names)
	}
	contents := sink.byName(EventContent)
	if len(contents) == 0 || !strings.Contains(contents[0].Text, "deep reply") {
		t.Fatalf("content events = %+v, want the model reply", contents)
	}
	// The deep head always drives tools (write_todos plus the task tool) so
	// it requires a tool-capable candidate; the tool-less sub-agent is a
	// plain chat node and does not.
	if got := resolver.requireToolsSeen; len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("requireTools seen = %v, want [true false]", got)
	}
	if state := host.State("deep-agent"); state.TopologyKind != agent.TopologyKindDeep {
		t.Fatalf("topology kind = %q, want deep", state.TopologyKind)
	}
}

func TestServeTurnPlanExecuteTopologyRunsPlanExecuteReplan(t *testing.T) {
	// Script the three sequential role calls: the planner emits the plan via
	// a forced tool call, the executor answers the step in text, and the
	// replanner exits the loop through the respond tool.
	var mu sync.Mutex
	var call int
	model := &fakeChatModel{script: func(_ []*schema.Message) *schema.Message {
		mu.Lock()
		defer mu.Unlock()
		call++
		switch call {
		case 1:
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "plan-1", Type: "function",
				Function: schema.FunctionCall{Name: "plan", Arguments: `{"steps":["answer the question"]}`},
			}})
		case 2:
			return schema.AssistantMessage("step executed", nil)
		default:
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "respond-1", Type: "function",
				Function: schema.FunctionCall{Name: "respond", Arguments: `{"response":"final answer"}`},
			}})
		}
	}}
	resolver := &fakeModelResolver{model: model}
	a := testAgent("planner-agent")
	a.Runtime.Builtin.Topology = agent.BuiltinTopology{Kind: agent.TopologyKindPlanExecute}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"planner-agent": a}},
		Models: resolver,
	})

	sink := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "planner-agent", TurnRequest{Input: "solve it"}, sink.sink); err != nil {
		t.Fatalf("ServeTurn() error = %v", err)
	}
	names := sink.names()
	if names[len(names)-1] != EventDone {
		t.Fatalf("events = %v, want done last", names)
	}
	var all strings.Builder
	for _, ev := range sink.byName(EventContent) {
		all.WriteString(ev.Text)
	}
	for _, want := range []string{"answer the question", "step executed", "final answer"} {
		if !strings.Contains(all.String(), want) {
			t.Fatalf("contents = %q, want them to contain %q", all.String(), want)
		}
	}
	// Roles resolve as planner, executor, replanner: the planner and
	// replanner emit plans through forced tool calls so they require
	// tool-capable candidates; the tool-less executor does not.
	if got := resolver.requireToolsSeen; len(got) != 3 || !got[0] || got[1] || !got[2] {
		t.Fatalf("requireTools seen = %v, want [true false true]", got)
	}
}
