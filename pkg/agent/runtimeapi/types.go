package runtimeapi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/agent"
)

const TurnOptionsVersionV1 = "v1"

// Backend is the required turn-first execution contract implemented by every
// executable Agent runtime.
type Backend interface {
	RuntimeType() string
	Capabilities(context.Context, agent.Agent) (Capabilities, error)
	ServeTurn(context.Context, agent.Agent, TurnRequest, EventSink) error
}

// TurnRequest contains only runtime-neutral turn semantics. Runtime-specific
// northbound options live in Options.Runtime and are decoded strictly by the
// selected backend.
type TurnRequest struct {
	RunID      string              `json:"run_id,omitempty"`
	Input      string              `json:"input,omitempty"`
	SessionID  string              `json:"session_id,omitempty"`
	Permission *PermissionDecision `json:"permission,omitempty"`
	Options    TurnOptions         `json:"options,omitempty"`
}

// TurnOptions is the versioned options envelope. Execution is trusted,
// gateway-only metadata and is never decoded from northbound JSON.
type TurnOptions struct {
	Version   string           `json:"version,omitempty"`
	Runtime   json.RawMessage  `json:"runtime,omitempty"`
	Execution ExecutionOptions `json:"-"`
}

// ExecutionOptions carries trusted caller metadata. The logical execution key
// is reserved for Workflow-owned idempotent execution.
type ExecutionOptions struct {
	LogicalExecutionKey string
}

// TurnEvent is the common event envelope. The common event sequencer added in
// M1 owns Sequence and SegmentIndex; backends must not allocate them.
type TurnEvent struct {
	Event        string          `json:"-"`
	AgentID      string          `json:"agent_id"`
	RunID        string          `json:"run_id"`
	SessionID    string          `json:"session_id,omitempty"`
	RequestID    string          `json:"request_id,omitempty"`
	Sequence     uint64          `json:"sequence"`
	SegmentIndex uint32          `json:"segment_index"`
	Text         string          `json:"text,omitempty"`
	Data         json.RawMessage `json:"data,omitempty"`
}

// EventSink receives turn events in emission order.
type EventSink func(TurnEvent) error

const (
	EventSession           = "session"
	EventDelta             = "delta"
	EventReasoning         = "reasoning"
	EventContent           = "content"
	EventPlan              = "plan"
	EventToolCall          = "tool_call"
	EventUsage             = "usage"
	EventPermission        = "permission"
	EventDone              = "done"
	EventError             = "error"
	EventAvailableCommands = "available_commands"
	EventSessionInfo       = "session_info"
	EventMode              = "mode"
	EventConfigOptions     = "config_options"
)

// Capabilities is the authoritative description of one backend for one Agent
// definition version.
type Capabilities struct {
	Executable   bool                   `json:"executable"`
	Turn         TurnCapabilities       `json:"turn"`
	Sessions     SessionCapabilities    `json:"sessions"`
	Permissions  PermissionCapabilities `json:"permissions"`
	Cancellation CancelCapabilities     `json:"cancellation"`
	Events       []string               `json:"events,omitempty"`
}

type TurnCapabilities struct {
	Streaming bool `json:"streaming"`
}

type SessionCapabilities struct {
	Resume     bool `json:"resume"`
	List       bool `json:"list"`
	Transcript bool `json:"transcript"`
	Durable    bool `json:"durable"`
}

type PermissionCapabilities struct {
	Interactive bool                 `json:"interactive"`
	ResumeMode  PermissionResumeMode `json:"resume_mode,omitempty"`
}

type PermissionResumeMode string

const (
	PermissionResumeActiveStream PermissionResumeMode = "active_stream"
	PermissionResumeNewStream    PermissionResumeMode = "new_stream"
)

type CancelCapabilities struct {
	Force    bool `json:"force"`
	Graceful bool `json:"graceful"`
}

// SessionLister is implemented only by backends with an explicit, bounded
// session-list contract.
type SessionLister interface {
	ListSessions(context.Context, agent.Agent, ListSessionsRequest) (ListSessionsResponse, error)
}

type ListSessionsRequest struct {
	CWD    string `json:"cwd,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type ListSessionsResponse struct {
	Sessions   []Session `json:"sessions"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

type Session struct {
	SessionID string          `json:"session_id"`
	Title     string          `json:"title,omitempty"`
	UpdatedAt *time.Time      `json:"updated_at,omitempty"`
	Details   json.RawMessage `json:"details,omitempty"`
}

// TranscriptLoader is implemented only when a backend exposes transcript
// replay with explicit visibility and bounded response semantics.
type TranscriptLoader interface {
	LoadTranscript(context.Context, agent.Agent, TranscriptRequest) (TranscriptResponse, error)
}

type TranscriptRequest struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd,omitempty"`
}

type TranscriptResponse struct {
	SessionID string              `json:"session_id"`
	Messages  []TranscriptMessage `json:"messages"`
}

type TranscriptMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// PermissionResolver resolves a permission through the common one-shot broker
// introduced in M3. Native continuation state never crosses this interface.
type PermissionResolver interface {
	ResolvePermission(context.Context, agent.Agent, PermissionDecision) error
}

type PermissionDecision struct {
	RequestID string                     `json:"request_id"`
	Outcome   string                     `json:"outcome,omitempty"`
	OptionID  string                     `json:"option_id,omitempty"`
	Decisions []PermissionActionDecision `json:"decisions,omitempty"`
}

type PermissionActionDecision struct {
	ActionID string `json:"action_id"`
	Outcome  string `json:"outcome"`
}

// RunCanceller performs exact-run cancellation. Unsupported modes must fail
// with capability_not_supported and must never be silently converted.
type RunCanceller interface {
	CancelRun(context.Context, agent.Agent, CancelRequest) (CancelResult, error)
}

type CancelMode string

const (
	CancelModeForce    CancelMode = "force"
	CancelModeGraceful CancelMode = "graceful"
)

type CancelRequest struct {
	RunID string     `json:"run_id"`
	Mode  CancelMode `json:"mode"`
}

type CancelResult struct {
	RunID      string    `json:"run_id"`
	State      RunState  `json:"state"`
	StopReason string    `json:"stop_reason,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

type RunState string

const (
	RunStateRunning   RunState = "running"
	RunStateCompleted RunState = "completed"
	RunStateCancelled RunState = "cancelled"
	RunStateFailed    RunState = "failed"
)

type RuntimeInspector interface {
	RuntimeSummary(context.Context, agent.Agent) (RuntimeSummary, error)
}

type RuntimeState string

const (
	RuntimeStateUnknown       RuntimeState = "unknown"
	RuntimeStateDisabled      RuntimeState = "disabled"
	RuntimeStateNotExecutable RuntimeState = "not_executable"
	RuntimeStateStarting      RuntimeState = "starting"
	RuntimeStateReady         RuntimeState = "ready"
	RuntimeStateDegraded      RuntimeState = "degraded"
	RuntimeStateUnhealthy     RuntimeState = "unhealthy"
)

type RuntimeSummary struct {
	Type               string          `json:"type"`
	Executable         bool            `json:"executable"`
	Healthy            bool            `json:"healthy"`
	State              RuntimeState    `json:"state"`
	ActiveRuns         int             `json:"active_runs"`
	PendingPermissions int             `json:"pending_permissions"`
	SessionCount       int             `json:"session_count"`
	LastActivityAt     *time.Time      `json:"last_activity_at,omitempty"`
	Details            json.RawMessage `json:"details,omitempty"`
}

// HealthChecker reports bounded, side-effect-free health. Implementations must
// not start a process, materialize a graph, create a session, or execute a turn.
type HealthChecker interface {
	Health(context.Context, agent.Agent) (Health, error)
}

type Health struct {
	Healthy   bool            `json:"healthy"`
	State     RuntimeState    `json:"state"`
	CheckedAt time.Time       `json:"checked_at"`
	Message   string          `json:"message,omitempty"`
	Details   json.RawMessage `json:"details,omitempty"`
}

// OptionalCapabilities reports which narrow optional interfaces a backend
// actually implements. It describes Go-level support, not the per-Agent
// capability values returned by Backend.Capabilities.
type OptionalCapabilities struct {
	SessionList       bool
	Transcript        bool
	PermissionResolve bool
	RunCancel         bool
	RuntimeInspect    bool
	HealthCheck       bool
}

func DetectOptionalCapabilities(backend Backend) OptionalCapabilities {
	if backend == nil {
		return OptionalCapabilities{}
	}
	_, sessions := backend.(SessionLister)
	_, transcript := backend.(TranscriptLoader)
	_, permissions := backend.(PermissionResolver)
	_, cancellation := backend.(RunCanceller)
	_, inspection := backend.(RuntimeInspector)
	_, health := backend.(HealthChecker)
	return OptionalCapabilities{
		SessionList:       sessions,
		Transcript:        transcript,
		PermissionResolve: permissions,
		RunCancel:         cancellation,
		RuntimeInspect:    inspection,
		HealthCheck:       health,
	}
}
