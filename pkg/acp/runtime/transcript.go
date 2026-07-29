package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agent-guide/agent-gateway/pkg/acp/agentspi"
	"github.com/agent-guide/agent-gateway/pkg/acp/runtime/acpupdate"
	"github.com/agent-guide/agent-gateway/pkg/acp/runtimeconfig"
	acptransport "github.com/agent-guide/agent-gateway/pkg/acp/transport"
)

// loadAgentTranscript replays one session through a transient connection and
// returns the coalesced message transcript. Per ACP, session/load replays the
// session history as session/update notifications that all precede the
// session/load response, so the collector consumes updates concurrently with
// the request and drains anything still buffered once the response arrives.
// An agent implementing agentspi.TranscriptLoader overrides this generic path.
func loadAgentTranscript(ctx context.Context, cfg runtimeconfig.Config, req TranscriptRequest) (TranscriptResponse, error) {
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		return TranscriptResponse{}, fmt.Errorf("%w: session_id is required", ErrInvalidRequest)
	}
	openCWD := strings.TrimSpace(req.CWD)
	if openCWD == "" {
		openCWD = cfg.CWD
	}
	agent, err := agentspi.New(cfg.AgentType, agentspi.OpenRequest{Config: cfg, CWD: openCWD})
	if err != nil {
		return TranscriptResponse{}, err
	}
	if loader, ok := agent.(agentspi.TranscriptLoader); ok {
		entries, err := loader.LoadSessionTranscript(ctx, sessionID)
		if err != nil {
			return TranscriptResponse{}, err
		}
		messages := make([]TranscriptMessage, 0, len(entries))
		for _, entry := range entries {
			messages = append(messages, TranscriptMessage{Role: entry.Role, Text: entry.Text})
		}
		return TranscriptResponse{SessionID: sessionID, Messages: messages}, nil
	}

	t, err := agent.Open(ctx, acptransport.Handlers{Permission: permissionHandler(cfg.PermissionMode)})
	if err != nil {
		return TranscriptResponse{}, err
	}
	defer func() { _ = t.Close() }()

	setupCtx, cancel := context.WithTimeout(ctx, initializeTimeout)
	defer cancel()
	initResult, err := t.Request(setupCtx, "initialize", agent.InitializeParams())
	if err != nil {
		return TranscriptResponse{}, fmt.Errorf("initialize: %w", err)
	}
	if !supportsSessionLoad(initResult) {
		return TranscriptResponse{}, fmt.Errorf("acp agent %q does not advertise session/load", agent.Name())
	}

	// Map the host-visible session id to the backend load id for agents that
	// keep the two distinct (SessionLoadResolver).
	loadID := sessionID
	if resolver, ok := agent.(agentspi.SessionLoadResolver); ok {
		resolvedLoadID, _, err := resolver.ResolveLoadSessionID(ctx, t, openCWD, sessionID)
		if err != nil {
			return TranscriptResponse{}, fmt.Errorf("resolve load session id: %w", err)
		}
		if strings.TrimSpace(resolvedLoadID) != "" {
			loadID = strings.TrimSpace(resolvedLoadID)
		}
	}

	// Subscribe before sending session/load so no replayed update is missed.
	updates, unsubscribe := t.Updates(256)
	defer unsubscribe()

	done := make(chan error, 1)
	go func() {
		_, err := t.Request(ctx, "session/load", agent.SessionLoadParams(loadID))
		done <- err
	}()

	var collector transcriptCollector
	collect := func(msg acptransport.Message) error {
		if msg.Method != "session/update" {
			return nil
		}
		if role, text, ok := acpupdate.ParseReplayChunk(msg.Params); ok {
			collector.add(role, text)
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return TranscriptResponse{}, ctx.Err()
		case err := <-done:
			if err != nil {
				return TranscriptResponse{}, fmt.Errorf("session/load: %w", err)
			}
			if err := drainBuffered(updates, collect); err != nil {
				return TranscriptResponse{}, err
			}
			return TranscriptResponse{SessionID: sessionID, Messages: collector.finish()}, nil
		case msg, ok := <-updates:
			if !ok {
				return TranscriptResponse{}, fmt.Errorf("acp transport closed during session/load")
			}
			_ = collect(msg)
		}
	}
}

// supportsSessionLoad checks the agent-advertised loadSession capability
// (verified against the real `opencode acp` initialize response).
func supportsSessionLoad(raw json.RawMessage) bool {
	var payload struct {
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false
	}
	return payload.AgentCapabilities.LoadSession
}

// transcriptCollector coalesces consecutive replayed chunks of the same role
// into one message, preserving order.
type transcriptCollector struct {
	messages []TranscriptMessage
}

func (c *transcriptCollector) add(role, text string) {
	if text == "" {
		return
	}
	if n := len(c.messages); n > 0 && c.messages[n-1].Role == role {
		c.messages[n-1].Text += text
		return
	}
	c.messages = append(c.messages, TranscriptMessage{Role: role, Text: text})
}

func (c *transcriptCollector) finish() []TranscriptMessage {
	if c.messages == nil {
		return []TranscriptMessage{}
	}
	return c.messages
}
