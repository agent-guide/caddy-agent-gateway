// Package builtin is the generic ADK host for builtin-runtime agents: the
// gateway ships this one host compiled into agw/agwd, and a builtin agent is a
// persisted definition (agent.BuiltinRuntime) that the host materializes into
// an eino ADK object graph on demand. See
// docs/design/agents-control-plane.md §5.7.
package builtin

import (
	"context"
	"encoding/json"
	"errors"

	einomodel "github.com/cloudwego/eino/components/model"

	"github.com/agent-guide/agent-gateway/pkg/agent"
)

// ErrInvalidRequest marks client-correctable turn failures (unknown agent
// runtime, disabled agent, empty input, depth exceeded). The dispatcher maps
// it to HTTP 400, mirroring the ACP error contract.
var ErrInvalidRequest = errors.New("invalid builtin turn request")

// ErrTurnLimitExceeded is returned when limits.max_concurrent_turns is
// reached. Fail-closed: the turn is rejected, never queued.
var ErrTurnLimitExceeded = errors.New("builtin agent concurrent turn limit exceeded")

// ErrPermissionCapacity is returned when permissions.max_pending is reached:
// the interrupting turn fails instead of storing another checkpoint
// (fail-closed, §5.7.7).
var ErrPermissionCapacity = errors.New("builtin agent pending permission capacity exceeded")

// ErrAgentNotFound is returned when the agent id does not resolve.
var ErrAgentNotFound = errors.New("builtin agent not found")

// TurnRequest is the data-plane turn input. Sessions are in-memory
// conversation histories keyed by session_id; state does not survive a
// gateway restart (documented PB1 semantics — durable checkpoints wait for
// eino v0.10). Exactly one of Input and Permission must be set: Input starts
// a turn, Permission resumes one suspended on a tool-permission interrupt
// (§5.7.7) and streams the continuation on this request's SSE response.
type TurnRequest struct {
	SessionID  string          `json:"session_id,omitempty"`
	Input      string          `json:"input,omitempty"`
	Permission *TurnPermission `json:"permission,omitempty"`
}

// TurnPermission answers a pending tool-permission request. Outcome is empty
// to deliver per-call decisions, or "cancel" to discard the suspended turn.
// A pending call absent from Decisions is denied (fail-closed).
type TurnPermission struct {
	RequestID string                   `json:"request_id"`
	Outcome   string                   `json:"outcome,omitempty"`
	Decisions []TurnPermissionDecision `json:"decisions,omitempty"`
}

// TurnPermissionDecision resolves one gated tool call: outcome "allow" or
// "deny".
type TurnPermissionDecision struct {
	CallID  string `json:"call_id"`
	Outcome string `json:"outcome"`
}

// TurnEvent is one SSE event of a builtin turn. The vocabulary is a marked
// subset of the ACP turn vocabulary: session, delta, content, tool_call,
// usage, permission, done, error.
type TurnEvent struct {
	Event      string          `json:"-"`
	SessionID  string          `json:"session_id,omitempty"`
	RunID      string          `json:"run_id,omitempty"`
	RequestID  string          `json:"request_id,omitempty"`
	Text       string          `json:"text,omitempty"`
	StopReason string          `json:"stop_reason,omitempty"`
	Message    string          `json:"message,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// Turn event names.
const (
	EventSession    = "session"
	EventDelta      = "delta"
	EventContent    = "content"
	EventToolCall   = "tool_call"
	EventUsage      = "usage"
	EventPermission = "permission"
	EventDone       = "done"
	EventError      = "error"
)

// Done stop reasons beyond "end_turn".
const (
	StopReasonPermissionRequired = "permission_required"
	StopReasonCancelled          = "cancelled"
)

// EventSink receives turn events in emission order.
type EventSink func(TurnEvent) error

// correlatedSink stamps every event in one streamed run segment with the
// stable logical run id and the segment's session/permission identifiers.
func correlatedSink(next EventSink, runID, sessionID, requestID string) EventSink {
	return func(event TurnEvent) error {
		if event.RunID == "" {
			event.RunID = runID
		}
		if event.SessionID == "" {
			event.SessionID = sessionID
		}
		if event.RequestID == "" {
			event.RequestID = requestID
		}
		return next(event)
	}
}

// AgentSource resolves agent definitions; *agent.Manager satisfies it.
type AgentSource interface {
	Get(ctx context.Context, id string) (agent.Agent, error)
}

// ChatModelResolver resolves a gateway LLM route to an eino chat model.
// The gateway-side implementation wraps a RoutedProvider through the
// einomodel bridge, so credential scheduling, candidate fallback, and LLM
// usage recording apply unchanged. requireTools narrows logical-model
// candidates to tool-capable bindings so a node that carries tools never
// routes to a model that cannot call them.
type ChatModelResolver interface {
	ResolveChatModel(ctx context.Context, llmRouteID, model string, requireTools bool) (einomodel.ToolCallingChatModel, error)
}
