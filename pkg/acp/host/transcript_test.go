package host

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/acp/agentspi"
	"github.com/agent-guide/agent-gateway/pkg/acp/hostconfig"
	acptransport "github.com/agent-guide/agent-gateway/pkg/acp/transport"
)

// transcriptFakeTransport replays a scripted update stream when session/load is
// requested, mirroring the real ACP ordering: all replayed updates are
// published before the session/load response returns.
type transcriptFakeTransport struct {
	loadCapability bool
	replay         []acptransport.Message

	mu       sync.Mutex
	subs     []chan acptransport.Message
	requests []recordedRequest
}

func (s *transcriptFakeTransport) Request(_ context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	s.requests = append(s.requests, recordedRequest{method: method, params: params})
	s.mu.Unlock()
	switch method {
	case "initialize":
		if s.loadCapability {
			return json.RawMessage(`{"agentCapabilities":{"loadSession":true}}`), nil
		}
		return json.RawMessage(`{"agentCapabilities":{"loadSession":false}}`), nil
	case "session/load":
		s.mu.Lock()
		subs := append([]chan acptransport.Message(nil), s.subs...)
		s.mu.Unlock()
		for _, msg := range s.replay {
			for _, sub := range subs {
				sub <- msg
			}
		}
		return json.RawMessage(`{}`), nil
	default:
		return json.RawMessage(`{}`), nil
	}
}

func (s *transcriptFakeTransport) Notify(string, any) error { return nil }

func (s *transcriptFakeTransport) Updates(buffer int) (<-chan acptransport.Message, func()) {
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan acptransport.Message, buffer)
	s.mu.Lock()
	s.subs = append(s.subs, ch)
	s.mu.Unlock()
	return ch, func() {}
}

func (s *transcriptFakeTransport) Alive() bool  { return true }
func (s *transcriptFakeTransport) Close() error { return nil }

type transcriptAgent struct {
	stubAgent
	transport *transcriptFakeTransport
}

func (a transcriptAgent) Name() string { return "transcript" }

func (a transcriptAgent) Open(context.Context, acptransport.Handlers) (acptransport.Transport, error) {
	return a.transport, nil
}

func (a transcriptAgent) SessionLoadParams(sessionID string) map[string]any {
	return map[string]any{"sessionId": sessionID, "mcpServers": []any{}}
}

// loaderOverrideAgent implements agentspi.TranscriptLoader and must bypass the
// generic session/load path entirely.
type loaderOverrideAgent struct{ stubAgent }

func (loaderOverrideAgent) Name() string { return "transcript-loader-override" }

func (loaderOverrideAgent) Open(context.Context, acptransport.Handlers) (acptransport.Transport, error) {
	panic("TranscriptLoader override must not open the generic transport")
}

func (loaderOverrideAgent) LoadSessionTranscript(_ context.Context, sessionID string) ([]agentspi.TranscriptEntry, error) {
	return []agentspi.TranscriptEntry{{Role: "user", Text: "from " + sessionID}}, nil
}

var transcriptTestTransport = &transcriptFakeTransport{}

func init() {
	agentspi.Register("transcript", func(agentspi.OpenRequest) (agentspi.Agent, error) {
		return transcriptAgent{transport: transcriptTestTransport}, nil
	})
	agentspi.Register("transcript-loader-override", func(agentspi.OpenRequest) (agentspi.Agent, error) {
		return loaderOverrideAgent{}, nil
	})
}

func TestLoadAgentTranscriptCollectsAndCoalesces(t *testing.T) {
	transcriptTestTransport.loadCapability = true
	transcriptTestTransport.replay = []acptransport.Message{
		chunkMessage("user_message_chunk", "fix "),
		chunkMessage("user_message_chunk", "the bug"),
		structuredMessage("tool_call", map[string]any{"toolCallId": "tc1"}),
		chunkMessage("agent_thought_chunk", "thinking"),
		chunkMessage("agent_message_chunk", "done"),
		chunkMessage("user_message_chunk", "thanks"),
	}
	transcriptTestTransport.subs = nil
	transcriptTestTransport.requests = nil

	result, err := loadAgentTranscript(context.Background(), hostconfig.Config{
		AgentType: "transcript",
		CWD:       "/tmp/project",
	}, TranscriptRequest{SessionID: "s1"})
	if err != nil {
		t.Fatalf("loadAgentTranscript: %v", err)
	}
	if result.SessionID != "s1" {
		t.Fatalf("session id = %q, want s1", result.SessionID)
	}
	want := []TranscriptMessage{
		{Role: "user", Text: "fix the bug"},
		{Role: "reasoning", Text: "thinking"},
		{Role: "assistant", Text: "done"},
		{Role: "user", Text: "thanks"},
	}
	if len(result.Messages) != len(want) {
		t.Fatalf("messages = %+v, want %+v", result.Messages, want)
	}
	for idx := range want {
		if result.Messages[idx] != want[idx] {
			t.Fatalf("message[%d] = %+v, want %+v", idx, result.Messages[idx], want[idx])
		}
	}
}

func TestLoadAgentTranscriptRequiresLoadCapability(t *testing.T) {
	transcriptTestTransport.loadCapability = false
	transcriptTestTransport.subs = nil
	transcriptTestTransport.requests = nil

	_, err := loadAgentTranscript(context.Background(), hostconfig.Config{
		AgentType: "transcript",
		CWD:       "/tmp/project",
	}, TranscriptRequest{SessionID: "s1"})
	if err == nil || !strings.Contains(err.Error(), "does not advertise session/load") {
		t.Fatalf("error = %v, want missing loadSession capability error", err)
	}
}

func TestLoadAgentTranscriptRequiresSessionID(t *testing.T) {
	_, err := loadAgentTranscript(context.Background(), hostconfig.Config{
		AgentType: "transcript",
		CWD:       "/tmp/project",
	}, TranscriptRequest{})
	if err == nil || !strings.Contains(err.Error(), "session_id is required") {
		t.Fatalf("error = %v, want session_id required error", err)
	}
}

func TestLoadAgentTranscriptUsesLoaderOverride(t *testing.T) {
	result, err := loadAgentTranscript(context.Background(), hostconfig.Config{
		AgentType: "transcript-loader-override",
		CWD:       "/tmp/project",
	}, TranscriptRequest{SessionID: "s9"})
	if err != nil {
		t.Fatalf("loadAgentTranscript: %v", err)
	}
	if len(result.Messages) != 1 || result.Messages[0].Text != "from s9" {
		t.Fatalf("override transcript = %+v, want the loader's entries", result.Messages)
	}
}

func TestTranscriptCollectorReturnsEmptySliceForNoMessages(t *testing.T) {
	var c transcriptCollector
	if got := c.finish(); got == nil || len(got) != 0 {
		t.Fatalf("finish() = %#v, want empty non-nil slice", got)
	}
}
