package acpupdate

import (
	"encoding/json"
	"testing"
)

func sessionUpdate(t *testing.T, update map[string]any) json.RawMessage {
	t.Helper()
	params, err := json.Marshal(map[string]any{"sessionId": "s1", "update": update})
	if err != nil {
		t.Fatalf("marshal update: %v", err)
	}
	return params
}

func TestParseVariants(t *testing.T) {
	cases := []struct {
		name     string
		update   map[string]any
		wantKind Kind
		wantText string
		wantData bool
		wantNone bool
	}{
		{
			name:     "agent message text block",
			update:   map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "hello"}},
			wantKind: KindText, wantText: "hello",
		},
		{
			name:     "agent message bare string",
			update:   map[string]any{"sessionUpdate": "agent_message_chunk", "content": "world"},
			wantKind: KindText, wantText: "world",
		},
		{
			name:     "agent message non-text is structured content",
			update:   map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "image", "mimeType": "image/png", "data": "AAAA"}},
			wantKind: KindContent, wantData: true,
		},
		{
			name:     "agent thought maps to reasoning",
			update:   map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "thinking"}},
			wantKind: KindReasoning, wantText: "thinking",
		},
		{
			name:     "tool call",
			update:   map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "Read"},
			wantKind: KindToolCall, wantData: true,
		},
		{
			name:     "tool call update",
			update:   map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": "completed"},
			wantKind: KindToolCall, wantData: true,
		},
		{
			name:     "plan",
			update:   map[string]any{"sessionUpdate": "plan", "entries": []any{}},
			wantKind: KindPlan, wantData: true,
		},
		{
			name:     "usage",
			update:   map[string]any{"sessionUpdate": "usage_update", "used": 10, "size": 100},
			wantKind: KindUsage, wantData: true,
		},
		{
			name:     "available commands",
			update:   map[string]any{"sessionUpdate": "available_commands_update", "availableCommands": []any{}},
			wantKind: KindCommands, wantData: true,
		},
		{
			name:     "session info",
			update:   map[string]any{"sessionUpdate": "session_info_update", "title": "My session"},
			wantKind: KindSessionInfo, wantData: true,
		},
		{
			name:     "current mode",
			update:   map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": "code"},
			wantKind: KindMode, wantData: true,
		},
		{
			name:     "config option",
			update:   map[string]any{"sessionUpdate": "config_option_update", "configOptions": []any{}},
			wantKind: KindConfigOptions, wantData: true,
		},
		{
			name:     "user message chunk is ignored",
			update:   map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "hi"}},
			wantNone: true,
		},
		{
			name:     "unknown variant is ignored",
			update:   map[string]any{"sessionUpdate": "future_variant"},
			wantNone: true,
		},
		{
			name:     "empty agent text is ignored",
			update:   map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": ""}},
			wantNone: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := Parse(sessionUpdate(t, tc.update))
			if tc.wantNone {
				if len(events) != 0 {
					t.Fatalf("expected no events, got %+v", events)
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
			}
			ev := events[0]
			if ev.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", ev.Kind, tc.wantKind)
			}
			if ev.Text != tc.wantText {
				t.Fatalf("text = %q, want %q", ev.Text, tc.wantText)
			}
			if tc.wantData && len(ev.Data) == 0 {
				t.Fatal("expected structured Data, got none")
			}
			if !tc.wantData && len(ev.Data) != 0 {
				t.Fatalf("unexpected Data: %s", ev.Data)
			}
		})
	}
}

func TestParseReplayChunk(t *testing.T) {
	cases := []struct {
		name     string
		update   map[string]any
		wantRole string
		wantText string
		wantOK   bool
	}{
		{"user chunk", map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "hi"}}, TranscriptRoleUser, "hi", true},
		{"agent chunk", map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "done"}}, TranscriptRoleAssistant, "done", true},
		{"thought chunk", map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "hmm"}}, TranscriptRoleReasoning, "hmm", true},
		{"non-message update", map[string]any{"sessionUpdate": "plan", "entries": []any{}}, "", "", false},
		{"non-text content", map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "image", "data": "AAAA"}}, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			role, text, ok := ParseReplayChunk(sessionUpdate(t, tc.update))
			if ok != tc.wantOK || role != tc.wantRole || text != tc.wantText {
				t.Fatalf("ParseReplayChunk = (%q, %q, %v), want (%q, %q, %v)", role, text, ok, tc.wantRole, tc.wantText, tc.wantOK)
			}
		})
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	if events := Parse(json.RawMessage(`not json`)); events != nil {
		t.Fatalf("expected nil for malformed params, got %+v", events)
	}
	if events := Parse(json.RawMessage(`{"sessionId":"s1"}`)); events != nil {
		t.Fatalf("expected nil when update is absent, got %+v", events)
	}
}
