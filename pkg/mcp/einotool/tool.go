// Package einotool adapts gateway-managed MCP services (pkg/mcp/service) to
// eino tools, so gateway-governed MCP tools are directly consumable by
// in-process eino agents and graphs without an HTTP loopback. Resource
// governance (service config, auth, allowlisting by the caller) stays in the
// gateway; execution stays in eino.
//
// The adapter is a pure bridge: it does not record MCP usage events. In-process
// callers own their observability span; the builtin agent runtime (see
// docs/design/agents-control-plane.md §5.7.6) is the layer that attributes
// tool calls.
package einotool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

	basemcp "github.com/agent-guide/agent-gateway/pkg/mcp"
	mcpservice "github.com/agent-guide/agent-gateway/pkg/mcp/service"
)

// ToolCaller is the slice of *service.Manager the adapter needs. It is an
// interface so tests can stub the MCP service layer.
type ToolCaller interface {
	ListTools(ctx context.Context, id string) ([]basemcp.Tool, error)
	CallTool(ctx context.Context, id string, name string, args map[string]any, progressCh chan<- mcpservice.UpstreamProgress) (*basemcp.ToolResult, error)
}

var _ ToolCaller = (*mcpservice.Manager)(nil)

// Tools lists the MCP service's tools and wraps each as an eino
// InvokableTool. When names is non-empty it acts as an allowlist and every
// listed name must exist — a missing tool is an error, not a silent skip, so
// an agent definition referencing a renamed tool fails loudly at
// materialization time.
func Tools(ctx context.Context, caller ToolCaller, serviceID string, names ...string) ([]tool.BaseTool, error) {
	if caller == nil {
		return nil, fmt.Errorf("einotool: tool caller is nil")
	}
	serviceID = strings.TrimSpace(serviceID)
	if serviceID == "" {
		return nil, fmt.Errorf("einotool: service id is empty")
	}
	listed, err := caller.ListTools(ctx, serviceID)
	if err != nil {
		return nil, fmt.Errorf("einotool: list tools of service %q: %w", serviceID, err)
	}
	byName := make(map[string]basemcp.Tool, len(listed))
	for _, t := range listed {
		byName[t.Name] = t
	}
	selected := make([]basemcp.Tool, 0, len(listed))
	if len(names) == 0 {
		selected = listed
	} else {
		for _, name := range names {
			t, ok := byName[strings.TrimSpace(name)]
			if !ok {
				return nil, fmt.Errorf("einotool: tool %q not found in service %q", name, serviceID)
			}
			selected = append(selected, t)
		}
	}
	out := make([]tool.BaseTool, 0, len(selected))
	for _, t := range selected {
		info, err := toolInfo(t)
		if err != nil {
			return nil, fmt.Errorf("einotool: tool %q of service %q: %w", t.Name, serviceID, err)
		}
		out = append(out, &invokableTool{caller: caller, serviceID: serviceID, name: t.Name, info: info})
	}
	return out, nil
}

type invokableTool struct {
	caller    ToolCaller
	serviceID string
	name      string
	info      *schema.ToolInfo
}

var _ tool.InvokableTool = (*invokableTool)(nil)

func (t *invokableTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

func (t *invokableTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var args map[string]any
	if trimmed := strings.TrimSpace(argumentsInJSON); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
			return "", fmt.Errorf("einotool: tool %q arguments are not a JSON object: %w", t.name, err)
		}
	}
	result, err := t.caller.CallTool(ctx, t.serviceID, t.name, args, nil)
	if err != nil {
		return "", fmt.Errorf("einotool: call tool %q of service %q: %w", t.name, t.serviceID, err)
	}
	rendered := renderToolResult(result)
	if result != nil && result.IsError {
		return "", fmt.Errorf("einotool: tool %q reported an error: %s", t.name, rendered)
	}
	return rendered, nil
}

func toolInfo(t basemcp.Tool) (*schema.ToolInfo, error) {
	info := &schema.ToolInfo{
		Name: t.Name,
		Desc: t.Description,
	}
	if len(t.InputSchema) > 0 {
		raw, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("encode input schema: %w", err)
		}
		js := &jsonschema.Schema{}
		if err := json.Unmarshal(raw, js); err != nil {
			return nil, fmt.Errorf("decode input schema: %w", err)
		}
		info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(js)
	}
	return info, nil
}

// renderToolResult flattens an MCP tool result into the string an eino tool
// returns. Text-only content collapses to plain text; anything else (images,
// structured content, mixed blocks) is returned as compact JSON.
func renderToolResult(result *basemcp.ToolResult) string {
	if result == nil {
		return ""
	}
	if result.StructuredContent != nil {
		if raw, err := json.Marshal(result.StructuredContent); err == nil {
			return string(raw)
		}
	}
	if texts, ok := textBlocks(result.Content); ok {
		return strings.Join(texts, "\n")
	}
	if result.Content == nil {
		return ""
	}
	raw, err := json.Marshal(result.Content)
	if err != nil {
		return fmt.Sprintf("%v", result.Content)
	}
	return string(raw)
}

func textBlocks(content any) ([]string, bool) {
	blocks, ok := content.([]any)
	if !ok {
		return nil, false
	}
	texts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			return nil, false
		}
		if blockType, _ := block["type"].(string); blockType != "text" {
			return nil, false
		}
		text, ok := block["text"].(string)
		if !ok {
			return nil, false
		}
		texts = append(texts, text)
	}
	return texts, true
}
