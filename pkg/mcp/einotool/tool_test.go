package einotool

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"

	basemcp "github.com/agent-guide/agent-gateway/pkg/mcp"
	mcpservice "github.com/agent-guide/agent-gateway/pkg/mcp/service"
)

type fakeCaller struct {
	tools      []basemcp.Tool
	listErr    error
	callErr    error
	result     *basemcp.ToolResult
	calledID   string
	calledName string
	calledArgs map[string]any
}

func (f *fakeCaller) ListTools(_ context.Context, id string) ([]basemcp.Tool, error) {
	f.calledID = id
	return f.tools, f.listErr
}

func (f *fakeCaller) CallTool(_ context.Context, id string, name string, args map[string]any, _ chan<- mcpservice.UpstreamProgress) (*basemcp.ToolResult, error) {
	f.calledID = id
	f.calledName = name
	f.calledArgs = args
	return f.result, f.callErr
}

func testTools() []basemcp.Tool {
	return []basemcp.Tool{
		{
			Name:        "read_file",
			Description: "Read a file",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []any{"path"},
			},
		},
		{Name: "list_dir", Description: "List a directory"},
	}
}

func TestToolsWrapsAndFiltersByName(t *testing.T) {
	caller := &fakeCaller{tools: testTools()}
	tools, err := Tools(t.Context(), caller, "svc-1", "read_file")
	if err != nil {
		t.Fatalf("Tools() error = %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	info, err := tools[0].Info(t.Context())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "read_file" || info.Desc != "Read a file" {
		t.Fatalf("info = %+v, want read_file", info)
	}
	js, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatalf("ToJSONSchema() error = %v", err)
	}
	if js == nil || js.Properties == nil {
		t.Fatalf("params schema = %+v, want object schema with properties", js)
	}
	if _, ok := js.Properties.Get("path"); !ok {
		t.Fatalf("schema properties = %+v, want path", js.Properties)
	}
}

func TestToolsMissingAllowlistedNameFails(t *testing.T) {
	caller := &fakeCaller{tools: testTools()}
	if _, err := Tools(t.Context(), caller, "svc-1", "renamed_tool"); err == nil {
		t.Fatal("Tools() error = nil, want missing-tool error")
	}
}

func TestInvokableRunPassesArgsAndFlattensText(t *testing.T) {
	caller := &fakeCaller{
		tools: testTools(),
		result: &basemcp.ToolResult{
			Content: []any{
				map[string]any{"type": "text", "text": "line one"},
				map[string]any{"type": "text", "text": "line two"},
			},
		},
	}
	tools, err := Tools(t.Context(), caller, "svc-1", "read_file")
	if err != nil {
		t.Fatalf("Tools() error = %v", err)
	}
	invokable, ok := tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatalf("tool type = %T, want tool.InvokableTool", tools[0])
	}
	out, err := invokable.InvokableRun(t.Context(), `{"path":"/tmp/a.txt"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if out != "line one\nline two" {
		t.Fatalf("output = %q, want flattened text blocks", out)
	}
	if caller.calledID != "svc-1" || caller.calledName != "read_file" {
		t.Fatalf("call target = %s/%s, want svc-1/read_file", caller.calledID, caller.calledName)
	}
	if caller.calledArgs["path"] != "/tmp/a.txt" {
		t.Fatalf("args = %#v, want path passed through", caller.calledArgs)
	}
}

func TestInvokableRunToolErrorFailsClosed(t *testing.T) {
	caller := &fakeCaller{
		tools: testTools(),
		result: &basemcp.ToolResult{
			IsError: true,
			Content: []any{map[string]any{"type": "text", "text": "permission denied"}},
		},
	}
	tools, err := Tools(t.Context(), caller, "svc-1", "read_file")
	if err != nil {
		t.Fatalf("Tools() error = %v", err)
	}
	_, err = tools[0].(tool.InvokableTool).InvokableRun(t.Context(), `{}`)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("InvokableRun() error = %v, want tool error carrying the result text", err)
	}
}

func TestInvokableRunUpstreamErrorPropagates(t *testing.T) {
	caller := &fakeCaller{tools: testTools(), callErr: errors.New("upstream down")}
	tools, err := Tools(t.Context(), caller, "svc-1", "read_file")
	if err != nil {
		t.Fatalf("Tools() error = %v", err)
	}
	if _, err := tools[0].(tool.InvokableTool).InvokableRun(t.Context(), `{}`); err == nil {
		t.Fatal("InvokableRun() error = nil, want upstream error")
	}
}
