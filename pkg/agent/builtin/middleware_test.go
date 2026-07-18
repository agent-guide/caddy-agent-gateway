package builtin

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/agent"
	basemcp "github.com/agent-guide/agent-gateway/pkg/mcp"
)

func TestAgentsMDDocsBackendReadAndMiss(t *testing.T) {
	b := newAgentsMDDocs([]agent.BuiltinAgentsMDDoc{{Path: "AGENTS.md", Content: "hi"}})
	got, err := b.Read(t.Context(), &filesystem.ReadRequest{FilePath: "AGENTS.md", Offset: 1})
	if err != nil || got.Content != "hi" {
		t.Fatalf("Read() = %+v, %v, want the doc content", got, err)
	}
	if _, err := b.Read(t.Context(), &filesystem.ReadRequest{FilePath: "missing.md", Offset: 1}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("miss error = %v, want os.ErrNotExist so dangling @imports degrade to warnings", err)
	}
}

func TestServeTurnAgentsMDInjectsInlineDocsTransiently(t *testing.T) {
	model := replyModel("ok")
	a := testAgent("md")
	a.Runtime.Builtin.Middlewares = &agent.BuiltinMiddlewares{AgentsMD: &agent.BuiltinAgentsMD{
		Enabled: true,
		Docs: []agent.BuiltinAgentsMDDoc{
			// The dangling @missing/none.md must degrade to a load warning,
			// not fail the turn.
			{Path: "AGENTS.md", Content: "Root rules marker.\n@style/go.md\n@missing/none.md"},
			{Path: "style/go.md", Content: "Go style marker."},
		},
	}}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"md": a}},
		Models: &fakeModelResolver{model: model},
	})

	sink := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "md", TurnRequest{Input: "first"}, sink.sink); err != nil {
		t.Fatalf("first turn error = %v", err)
	}
	sessionID := sink.byName(EventSession)[0].SessionID
	if err := host.ServeTurn(t.Context(), "md", TurnRequest{SessionID: sessionID, Input: "second"}, sink.sink); err != nil {
		t.Fatalf("second turn error = %v", err)
	}

	model.mu.Lock()
	defer model.mu.Unlock()
	last := model.requests[len(model.requests)-1]
	injected := 0
	for _, msg := range last {
		if !strings.Contains(msg.Content, "Root rules marker.") {
			continue
		}
		injected++
		if msg.Role != schema.User {
			t.Fatalf("injected message role = %q, want user", msg.Role)
		}
		if !strings.Contains(msg.Content, "Go style marker.") {
			t.Fatalf("injected message misses the @imported doc: %q", msg.Content)
		}
	}
	// Exactly one injected section in the second turn proves both transiency
	// (the first turn's injection never entered the session history) and
	// per-run idempotence.
	if injected != 1 {
		t.Fatalf("injected sections in second-turn input = %d, want exactly 1", injected)
	}
}

func TestServeTurnReductionClearsOldToolOutputs(t *testing.T) {
	bigOutput := strings.Repeat("PAYLOAD ", 500) // ~4000 chars ≈ 1000 estimated tokens
	var mu sync.Mutex
	var requests [][]*schema.Message
	var calls int
	// Recording happens inside the script closure: with tools configured, ADK
	// calls WithTools and the bound copy does not share the root model's
	// request log.
	model := &fakeChatModel{script: func(input []*schema.Message) *schema.Message {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, input)
		if input[len(input)-1].Role == schema.Tool {
			return schema.AssistantMessage("done", nil)
		}
		calls++
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID:       fmt.Sprintf("call-%d", calls),
			Type:     "function",
			Function: schema.FunctionCall{Name: "fetch_doc", Arguments: `{"id":"x"}`},
		}})
	}}
	tools := &fakeToolCaller{
		tools: []basemcp.Tool{{Name: "fetch_doc", Description: "Fetch a document", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}},
		}}},
		result: &basemcp.ToolResult{Content: []any{map[string]any{"type": "text", "text": bigOutput}}},
	}
	a := testAgent("reducer")
	a.Runtime.Builtin.Tools = []agent.BuiltinToolSelection{{MCPServiceID: "docs", Tools: []string{"fetch_doc"}}}
	a.Resources.MCPServiceIDs = []string{"docs"}
	a.Runtime.Builtin.Middlewares = &agent.BuiltinMiddlewares{Reduction: &agent.BuiltinReduction{
		Enabled:           true,
		MaxTokensForClear: 200,
	}}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"reducer": a}},
		Models: &fakeModelResolver{model: model},
		Tools:  tools,
	})

	sink := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "reducer", TurnRequest{Input: "one"}, sink.sink); err != nil {
		t.Fatalf("first turn error = %v", err)
	}
	sessionID := sink.byName(EventSession)[0].SessionID
	if err := host.ServeTurn(t.Context(), "reducer", TurnRequest{SessionID: sessionID, Input: "two"}, sink.sink); err != nil {
		t.Fatalf("second turn error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	last := requests[len(requests)-1]
	var toolMsgs []*schema.Message
	for _, msg := range last {
		if msg.Role == schema.Tool {
			toolMsgs = append(toolMsgs, msg)
		}
	}
	if len(toolMsgs) != 2 {
		t.Fatalf("tool messages in final input = %d, want one per turn", len(toolMsgs))
	}
	if strings.Contains(toolMsgs[0].Content, "PAYLOAD") {
		t.Fatal("first-turn tool output was not cleared once the context exceeded max_tokens_for_clear")
	}
	if !strings.Contains(toolMsgs[1].Content, "PAYLOAD") {
		t.Fatal("retention must keep the most recent tool exchange uncleared")
	}
}

func TestServeTurnToolSearchGatesDynamicTools(t *testing.T) {
	var mu sync.Mutex
	var visibility [][]string
	var calls int
	model := &fakeChatModel{toolAwareScript: func(input []*schema.Message, visible []*schema.ToolInfo) *schema.Message {
		mu.Lock()
		defer mu.Unlock()
		names := make([]string, 0, len(visible))
		for _, ti := range visible {
			names = append(names, ti.Name)
		}
		visibility = append(visibility, names)
		calls++
		last := input[len(input)-1]
		switch {
		case last.Role == schema.Tool && strings.Contains(last.Content, `"matches"`):
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID:       fmt.Sprintf("call-%d", calls),
				Type:     "function",
				Function: schema.FunctionCall{Name: "fetch_doc", Arguments: `{"id":"x"}`},
			}})
		case last.Role == schema.Tool:
			return schema.AssistantMessage("done", nil)
		default:
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID:       fmt.Sprintf("call-%d", calls),
				Type:     "function",
				Function: schema.FunctionCall{Name: "tool_search", Arguments: `{"query":"select:fetch_doc"}`},
			}})
		}
	}}
	tools := &fakeToolCaller{
		tools: []basemcp.Tool{{Name: "fetch_doc", Description: "Fetch a document", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}},
		}}},
		result: &basemcp.ToolResult{Content: []any{map[string]any{"type": "text", "text": "DOC BODY"}}},
	}
	a := testAgent("searcher")
	a.Runtime.Builtin.Tools = []agent.BuiltinToolSelection{{MCPServiceID: "docs", Tools: []string{"fetch_doc"}}}
	a.Resources.MCPServiceIDs = []string{"docs"}
	a.Runtime.Builtin.Middlewares = &agent.BuiltinMiddlewares{ToolSearch: &agent.BuiltinToolSearch{Enabled: true}}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"searcher": a}},
		Models: &fakeModelResolver{model: model},
		Tools:  tools,
	})

	sink := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "searcher", TurnRequest{Input: "find the doc"}, sink.sink); err != nil {
		t.Fatalf("turn error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(visibility) < 3 {
		t.Fatalf("model calls = %d, want search -> load -> answer", len(visibility))
	}
	first, final := visibility[0], visibility[len(visibility)-1]
	if !slices.Contains(first, "tool_search") || slices.Contains(first, "fetch_doc") {
		t.Fatalf("first-call tools = %v, want tool_search visible and fetch_doc hidden", first)
	}
	if !slices.Contains(final, "fetch_doc") {
		t.Fatalf("final-call tools = %v, want fetch_doc loaded by the tool_search result", final)
	}
	if tools.calls != 1 {
		t.Fatalf("MCP tool executions = %d, want the dynamic tool to run exactly once", tools.calls)
	}
}

func TestServeTurnPlanTaskBoardIsSessionScoped(t *testing.T) {
	var mu sync.Mutex
	var calls int
	// Turn script: on "create" call TaskCreate, on "list" call TaskList, and
	// echo the task-list tool output back as the final answer so assertions
	// read it from the content events.
	model := &fakeChatModel{script: func(input []*schema.Message) *schema.Message {
		mu.Lock()
		defer mu.Unlock()
		calls++
		last := input[len(input)-1]
		if last.Role == schema.Tool {
			return schema.AssistantMessage("result: "+last.Content, nil)
		}
		switch last.Content {
		case "create":
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID:       fmt.Sprintf("call-%d", calls),
				Type:     "function",
				Function: schema.FunctionCall{Name: "TaskCreate", Arguments: `{"subject":"Ship the fix","description":"do it"}`},
			}})
		default:
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID:       fmt.Sprintf("call-%d", calls),
				Type:     "function",
				Function: schema.FunctionCall{Name: "TaskList", Arguments: `{}`},
			}})
		}
	}}
	a := testAgent("planner")
	a.Runtime.Builtin.Middlewares = &agent.BuiltinMiddlewares{PlanTask: &agent.BuiltinPlanTask{Enabled: true}}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"planner": a}},
		Models: &fakeModelResolver{model: model},
	})

	lastContent := func(sink *collectedEvents) string {
		events := sink.byName(EventContent)
		if len(events) == 0 {
			return ""
		}
		return events[len(events)-1].Text
	}

	sinkA := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "planner", TurnRequest{Input: "create"}, sinkA.sink); err != nil {
		t.Fatalf("create turn error = %v", err)
	}
	sessionA := sinkA.byName(EventSession)[0].SessionID
	if err := host.ServeTurn(t.Context(), "planner", TurnRequest{SessionID: sessionA, Input: "list"}, sinkA.sink); err != nil {
		t.Fatalf("list turn error = %v", err)
	}
	if got := lastContent(sinkA); !strings.Contains(got, "Ship the fix") {
		t.Fatalf("same-session TaskList output = %q, want the created task", got)
	}

	// A fresh session of the same agent must start with an empty board.
	sinkB := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "planner", TurnRequest{Input: "list"}, sinkB.sink); err != nil {
		t.Fatalf("other-session list turn error = %v", err)
	}
	if got := lastContent(sinkB); strings.Contains(got, "Ship the fix") {
		t.Fatalf("other-session TaskList output = %q, want no leaked tasks", got)
	}
}

func TestServeTurnSkillToolServesInlineSkills(t *testing.T) {
	var mu sync.Mutex
	var calls int
	var skillDesc string
	var skillResult string
	model := &fakeChatModel{toolAwareScript: func(input []*schema.Message, visible []*schema.ToolInfo) *schema.Message {
		mu.Lock()
		defer mu.Unlock()
		calls++
		for _, ti := range visible {
			if ti.Name == "skill" {
				skillDesc = ti.Desc
			}
		}
		last := input[len(input)-1]
		if last.Role == schema.Tool {
			skillResult = last.Content
			return schema.AssistantMessage("done", nil)
		}
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID:       fmt.Sprintf("call-%d", calls),
			Type:     "function",
			Function: schema.FunctionCall{Name: "skill", Arguments: `{"skill":"pdf-report"}`},
		}})
	}}
	a := testAgent("skilled")
	a.Runtime.Builtin.Middlewares = &agent.BuiltinMiddlewares{Skill: &agent.BuiltinSkill{
		Enabled: true,
		Skills: []agent.BuiltinSkillDoc{{
			Name:        "pdf-report",
			Description: "Generate the weekly PDF report",
			Content:     "REPORT RUNBOOK MARKER",
		}},
	}}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"skilled": a}},
		Models: &fakeModelResolver{model: model},
	})

	sink := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "skilled", TurnRequest{Input: "make the report"}, sink.sink); err != nil {
		t.Fatalf("turn error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(skillDesc, "pdf-report") || !strings.Contains(skillDesc, "Generate the weekly PDF report") {
		t.Fatalf("skill tool description = %q, want it to advertise the inline skill", skillDesc)
	}
	if !strings.Contains(skillResult, "REPORT RUNBOOK MARKER") {
		t.Fatalf("skill tool result = %q, want the inline skill content", skillResult)
	}
}

func TestServeTurnPatchToolCallsCompletesDanglingCalls(t *testing.T) {
	var mu sync.Mutex
	var requests [][]*schema.Message
	// The agent has no tools, so the first turn's tool call never executes
	// and the committed transcript ends with a dangling assistant tool call.
	model := &fakeChatModel{script: func(input []*schema.Message) *schema.Message {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, input)
		if len(requests) == 1 {
			return schema.AssistantMessage("let me check", []schema.ToolCall{{
				ID:       "orphan-1",
				Type:     "function",
				Function: schema.FunctionCall{Name: "fetch_doc", Arguments: `{"id":"x"}`},
			}})
		}
		return schema.AssistantMessage("done", nil)
	}}
	a := testAgent("patcher")
	a.Runtime.Builtin.Middlewares = &agent.BuiltinMiddlewares{PatchToolCalls: &agent.BuiltinPatchToolCalls{Enabled: true}}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"patcher": a}},
		Models: &fakeModelResolver{model: model},
	})

	sink := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "patcher", TurnRequest{Input: "first"}, sink.sink); err != nil {
		t.Fatalf("first turn error = %v", err)
	}
	sessionID := sink.byName(EventSession)[0].SessionID
	if err := host.ServeTurn(t.Context(), "patcher", TurnRequest{SessionID: sessionID, Input: "second"}, sink.sink); err != nil {
		t.Fatalf("second turn error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	last := requests[len(requests)-1]
	patched := false
	for i, msg := range last {
		if msg.Role != schema.Assistant || len(msg.ToolCalls) == 0 {
			continue
		}
		if i+1 < len(last) && last[i+1].Role == schema.Tool && last[i+1].ToolCallID == "orphan-1" {
			patched = true
		}
	}
	if !patched {
		t.Fatalf("second-turn input lacks a placeholder tool result for the dangling call: %v", messageRoles(last))
	}
}

func messageRoles(msgs []*schema.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, string(m.Role))
	}
	return out
}

func TestServeTurnFailsClosedOnInvalidAgentsMDConfig(t *testing.T) {
	// Enabled with no docs is rejected by definition validation, but a
	// definition persisted around it must still fail materialization, not
	// silently drop the middleware.
	a := testAgent("bad-md")
	a.Runtime.Builtin.Middlewares = &agent.BuiltinMiddlewares{AgentsMD: &agent.BuiltinAgentsMD{Enabled: true}}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"bad-md": a}},
		Models: &fakeModelResolver{model: replyModel("x")},
	})
	sink := &collectedEvents{}
	err := host.ServeTurn(t.Context(), "bad-md", TurnRequest{Input: "hi"}, sink.sink)
	if err == nil || !strings.Contains(err.Error(), "agentsmd") {
		t.Fatalf("ServeTurn() error = %v, want a materialization error naming agentsmd", err)
	}
}
