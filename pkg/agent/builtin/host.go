package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		agents:   cfg.Agents,
		models:   cfg.Models,
		tools:    cfg.Tools,
		observer: cfg.Observer,
		entries:  map[string]*hostEntry{},
		sessions: newSessionStore(),
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
	usage.SpanFromContext(ctx).SetExtension(usage.BuiltinExtension{
		Operation:    "turn",
		SessionID:    sessionID,
		TopologyKind: entry.topologyKind,
	})
	if err := emit(TurnEvent{Event: EventSession, SessionID: sessionID}); err != nil {
		handle.release()
		return err
	}

	userMsg := schema.UserMessage(req.Input)
	input := make([]*schema.Message, 0, len(history)+1)
	input = append(input, history...)
	input = append(input, userMsg)

	result, runErr := h.runTurn(turnCtx, entry, input, emit)
	usage.SpanFromContext(ctx).SetExtension(usage.BuiltinExtension{
		ModelSteps:  usage.Int(result.modelSteps),
		ToolSteps:   usage.Int(result.toolSteps),
		EventCounts: result.eventCounts,
	})
	if runErr != nil {
		handle.release()
		usage.SpanFromContext(ctx).SetExtension(usage.BuiltinExtension{ResultStatus: "error"})
		_ = emit(TurnEvent{Event: EventError, Message: runErr.Error()})
		return runErr
	}
	handle.commit(append([]*schema.Message{userMsg}, result.transcript...))
	usage.SpanFromContext(ctx).SetExtension(usage.BuiltinExtension{ResultStatus: "success"})
	return emit(TurnEvent{Event: EventDone, StopReason: "end_turn"})
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
		runner:       adk.NewRunner(ctx, adk.RunnerConfig{Agent: root, EnableStreaming: true}),
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
}

// runTurn drives the ADK runner and maps agent events onto the turn event
// vocabulary. A panicking agent fails the turn, never the gateway process.
func (h *Host) runTurn(ctx context.Context, entry *hostEntry, input []*schema.Message, emit EventSink) (result turnResult, err error) {
	result.eventCounts = map[string]int{}
	countingEmit := func(ev TurnEvent) error {
		result.eventCounts[ev.Event]++
		return emit(ev)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("builtin agent panicked: %v", r)
		}
	}()
	iter := entry.runner.Run(ctx, input)
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event == nil {
			continue
		}
		if event.Err != nil {
			return result, event.Err
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		msg, emitErr := h.emitMessageOutput(event.Output.MessageOutput, countingEmit, &result)
		if emitErr != nil {
			return result, emitErr
		}
		if msg != nil {
			result.transcript = append(result.transcript, msg)
		}
	}
	return result, nil
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
