package agentspi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/agent-guide/agent-gateway/pkg/acp/hostconfig"
	"github.com/agent-guide/agent-gateway/pkg/acp/transport"
)

type OpenRequest struct {
	Config hostconfig.Config
	CWD    string
}

type Factory func(OpenRequest) (Agent, error)

type Agent interface {
	Name() string
	Open(ctx context.Context, h transport.Handlers) (transport.Transport, error)
	InitializeParams() map[string]any
	SessionNewParams(modelID string) map[string]any
	SessionLoadParams(sessionID string) map[string]any
	PromptParams(sessionID, input, modelID string) map[string]any
	Cancel(ctx context.Context, t transport.Transport, sessionID string) error
}

type TerminalUpdateDetector interface {
	IsTerminalUpdate(params json.RawMessage) bool
}

type ReasoningUpdateFilter interface {
	AcceptReasoningUpdate(params json.RawMessage) bool
}

type SessionModelSelector interface {
	SelectSessionModel(ctx context.Context, t transport.Transport, sessionID, modelID string, opts []ConfigOption) ([]ConfigOption, error)
}

type SessionLister interface {
	SessionListParams(cwd, cursor string) map[string]any
}

// StableSessionResolver maps the raw session/new protocol id to the agent's
// stable, host-visible session id (e.g. a thick codex bridge resolving the
// persisted thread id). Returning "" without an error means the stable id is
// not resolvable yet (typically before the first prompt persists the
// session); the driver then re-resolves after each prompt and emits a late
// session event once the stable id settles. The raw protocol id keeps being
// used on the wire either way. Verified live: the thin codex-acp adapter does
// NOT need this — its raw session ids are already stable (listable and
// loadable from a fresh process after the first turn).
type StableSessionResolver interface {
	ResolveBoundSessionID(ctx context.Context, t transport.Transport, cwd, rawSessionID string) (string, error)
}

// SessionLoadResolver maps a host-visible session id to the backend id that
// session/load accepts, plus the host-bound id the loaded session should be
// known by (empty means the requested id).
type SessionLoadResolver interface {
	ResolveLoadSessionID(ctx context.Context, t transport.Transport, cwd, requestedSessionID string) (loadSessionID, boundSessionID string, err error)
}

// TranscriptEntry is one coalesced transcript message returned by a
// TranscriptLoader.
type TranscriptEntry struct {
	Role string `json:"role"` // user | assistant | reasoning
	Text string `json:"text"`
}

// TranscriptLoader replaces the runtime's generic session/load transcript
// replay with an agent-specific implementation (e.g. a thick codex bridge
// reading its own backend). Agents without it get the generic path: a
// transient connection that replays the session via session/load and collects
// the message chunks.
type TranscriptLoader interface {
	LoadSessionTranscript(ctx context.Context, sessionID string) ([]TranscriptEntry, error)
}

type ConfigOption struct {
	ID    string `json:"id"`
	Value string `json:"value,omitempty"`
}

var registry = struct {
	sync.RWMutex
	factories map[string]Factory
}{factories: map[string]Factory{}}

func Register(name string, factory Factory) {
	name = strings.TrimSpace(name)
	if name == "" {
		panic("acp agent name is required")
	}
	if factory == nil {
		panic("acp agent factory is nil")
	}
	registry.Lock()
	defer registry.Unlock()
	if _, exists := registry.factories[name]; exists {
		panic("acp agent factory already registered: " + name)
	}
	registry.factories[name] = factory
}

func New(name string, req OpenRequest) (Agent, error) {
	name = strings.TrimSpace(name)
	registry.RLock()
	factory := registry.factories[name]
	registry.RUnlock()
	if factory == nil {
		return nil, fmt.Errorf("unsupported acp agent_type %q", name)
	}
	return factory(req)
}
