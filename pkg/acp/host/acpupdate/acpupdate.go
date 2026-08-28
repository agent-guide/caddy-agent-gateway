// Package acpupdate parses ACP session/update notifications into a small set of
// neutral events the runtime driver forwards to the gateway north side. The
// sessionUpdate variant names and content shapes match the ACP v1 schema
// (agentclientprotocol/agent-client-protocol, schema/v1/schema.json).
package acpupdate

import (
	"encoding/json"
	"strings"
)

type Kind string

const (
	KindText          Kind = "delta"              // agent_message_chunk (text)
	KindReasoning     Kind = "reasoning"          // agent_thought_chunk
	KindContent       Kind = "content"            // agent_message_chunk (non-text)
	KindPlan          Kind = "plan"               // plan
	KindToolCall      Kind = "tool_call"          // tool_call / tool_call_update
	KindUsage         Kind = "usage"              // usage_update
	KindCommands      Kind = "available_commands" // available_commands_update
	KindSessionInfo   Kind = "session_info"       // session_info_update
	KindMode          Kind = "mode"               // current_mode_update
	KindConfigOptions Kind = "config_options"     // config_option_update
)

// Event is one neutral update. Text carries streamed text for KindText and
// KindReasoning; Data carries the raw ACP update object for structured kinds.
type Event struct {
	Kind Kind
	Text string
	Data json.RawMessage
}

type envelope struct {
	Update json.RawMessage `json:"update"`
}

type header struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       json.RawMessage `json:"content"`
}

// Parse converts one session/update params payload into zero or more events.
// user_message_chunk (replayed only on session/load) and unknown variants
// produce no events.
func Parse(params json.RawMessage) []Event {
	var env envelope
	if err := json.Unmarshal(params, &env); err != nil || len(env.Update) == 0 {
		return nil
	}
	var head header
	if err := json.Unmarshal(env.Update, &head); err != nil {
		return nil
	}
	switch strings.TrimSpace(head.SessionUpdate) {
	case "agent_message_chunk":
		if text, isText := contentText(head.Content); isText {
			if text == "" {
				return nil
			}
			return []Event{{Kind: KindText, Text: text}}
		}
		return []Event{{Kind: KindContent, Data: head.Content}}
	case "agent_thought_chunk":
		if text, isText := contentText(head.Content); isText {
			if text == "" {
				return nil
			}
			return []Event{{Kind: KindReasoning, Text: text}}
		}
		return []Event{{Kind: KindReasoning, Data: head.Content}}
	case "plan":
		return []Event{{Kind: KindPlan, Data: env.Update}}
	case "tool_call", "tool_call_update":
		return []Event{{Kind: KindToolCall, Data: env.Update}}
	case "usage_update":
		return []Event{{Kind: KindUsage, Data: env.Update}}
	case "available_commands_update":
		return []Event{{Kind: KindCommands, Data: env.Update}}
	case "session_info_update":
		return []Event{{Kind: KindSessionInfo, Data: env.Update}}
	case "current_mode_update":
		return []Event{{Kind: KindMode, Data: env.Update}}
	case "config_option_update":
		return []Event{{Kind: KindConfigOptions, Data: env.Update}}
	default:
		return nil
	}
}

// Transcript roles for replayed session/load message chunks.
const (
	TranscriptRoleUser      = "user"
	TranscriptRoleAssistant = "assistant"
	TranscriptRoleReasoning = "reasoning"
)

// ParseReplayChunk extracts one replayed message chunk from a session/update
// delivered during session/load. Unlike Parse, it includes user_message_chunk
// (replayed user turns) and maps each chunk to a transcript role. Non-text
// content blocks and non-message updates report ok=false.
func ParseReplayChunk(params json.RawMessage) (role, text string, ok bool) {
	var env envelope
	if err := json.Unmarshal(params, &env); err != nil || len(env.Update) == 0 {
		return "", "", false
	}
	var head header
	if err := json.Unmarshal(env.Update, &head); err != nil {
		return "", "", false
	}
	switch strings.TrimSpace(head.SessionUpdate) {
	case "user_message_chunk":
		role = TranscriptRoleUser
	case "agent_message_chunk":
		role = TranscriptRoleAssistant
	case "agent_thought_chunk":
		role = TranscriptRoleReasoning
	default:
		return "", "", false
	}
	text, isText := contentText(head.Content)
	if !isText {
		return "", "", false
	}
	return role, text, true
}

// contentText reports the text of an ACP ContentBlock. It returns isText=true
// for a bare string or a {"type":"text","text":...} block, and isText=false for
// non-text blocks (image/audio/resource_link/resource) so the caller surfaces
// them as structured content instead of dropping them.
func contentText(raw json.RawMessage) (text string, isText bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var block struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return "", false
	}
	if strings.TrimSpace(block.Type) == "text" || block.Text != "" {
		return block.Text, true
	}
	return "", false
}
