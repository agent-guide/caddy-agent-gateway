package host

import (
	"encoding/json"
	"time"
)

type TurnRequest struct {
	RunID           string            `json:"run_id,omitempty"`
	ThreadID        string            `json:"thread_id"`
	SessionID       string            `json:"session_id,omitempty"`
	Input           string            `json:"input"`
	CWD             string            `json:"cwd,omitempty"`
	Model           string            `json:"model,omitempty"`
	FreshSession    bool              `json:"fresh_session,omitempty"`
	ConfigOverrides map[string]string `json:"config_overrides,omitempty"`
}

// ActiveRunInfo is the exact logical-run view used by the common Agent
// control plane. It deliberately does not expose the native cancel handle.
type ActiveRunInfo struct {
	OwnerID   string    `json:"owner_id"`
	RunID     string    `json:"run_id"`
	SessionID string    `json:"session_id,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

type TurnEvent struct {
	Event               string          `json:"-"`
	SessionID           string          `json:"session_id,omitempty"`
	RequestID           string          `json:"request_id,omitempty"`
	PermissionExpiresAt time.Time       `json:"-"`
	Text                string          `json:"text,omitempty"`
	StopReason          string          `json:"stop_reason,omitempty"`
	Message             string          `json:"message,omitempty"`
	Data                json.RawMessage `json:"data,omitempty"`
}

type EventSink func(TurnEvent) error

// SessionMetadata is the cached structured state of one pooled session. Every
// field carries the raw ACP update object of its kind (config_option_update,
// available_commands_update, session_info_update, current_mode_update,
// usage_update).
type SessionMetadata struct {
	ConfigOptions     json.RawMessage `json:"config_options,omitempty"`
	AvailableCommands json.RawMessage `json:"available_commands,omitempty"`
	SessionInfo       json.RawMessage `json:"session_info,omitempty"`
	Mode              json.RawMessage `json:"mode,omitempty"`
	Usage             json.RawMessage `json:"usage,omitempty"`
}

// PooledInstanceInfo is the operator-facing view of one pooled agent instance.
type PooledInstanceInfo struct {
	Scope     string          `json:"scope"`
	SessionID string          `json:"session_id,omitempty"`
	Alive     bool            `json:"alive"`
	Active    bool            `json:"active"`
	LastUsed  time.Time       `json:"last_used"`
	IdleTTL   time.Duration   `json:"idle_ttl,omitempty"`
	Metadata  SessionMetadata `json:"metadata"`
}

type ListSessionsRequest struct {
	CWD    string `json:"cwd,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type SessionInfo struct {
	SessionID string          `json:"session_id"`
	CWD       string          `json:"cwd"`
	Title     string          `json:"title,omitempty"`
	UpdatedAt *time.Time      `json:"updated_at,omitempty"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

type ListSessionsResponse struct {
	Sessions   []SessionInfo `json:"sessions"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// PendingPermissionInfo describes one in-flight interactive permission
// request. Data carries the raw ACP session/request_permission params (tool
// call context plus the agent's permission options and their exact ids).
type PendingPermissionInfo struct {
	RequestID string          `json:"request_id"`
	OwnerID   string          `json:"owner_id"`
	SessionID string          `json:"session_id,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// PermissionDecision is the north-side answer to a pending interactive
// permission request. Outcome follows the ACP RequestPermissionOutcome
// discriminators: "selected" (with the chosen option id) or "cancelled".
type PermissionDecision struct {
	RequestID string `json:"request_id"`
	Outcome   string `json:"outcome"`
	OptionID  string `json:"option_id,omitempty"`
}

type TranscriptRequest struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd,omitempty"`
}

// TranscriptMessage is one coalesced replayed message. Role is one of user,
// assistant, or reasoning.
type TranscriptMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type TranscriptResponse struct {
	SessionID string              `json:"session_id"`
	Messages  []TranscriptMessage `json:"messages"`
}
