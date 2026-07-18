package builtin

import (
	"context"
	"io"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
)

// Inner LLM and MCP calls of a builtin turn get their own child interaction
// spans so they produce llm_usage_events / mcp_usage_events exactly like
// route-dispatched traffic, parented under the turn span and stamped with the
// agent id (§5.7.6). routeProtocol marks them as host-internal traffic.
const builtinInternalProtocol = "builtin"

func childDims(ctx context.Context, kind, routeID, agentID string) usage.InteractionDimensions {
	dims := usage.InteractionDimensions{
		RouteID:       routeID,
		RouteKind:     kind,
		RouteProtocol: builtinInternalProtocol,
		AgentID:       agentID,
	}
	if parent, ok := usage.DimensionsFromContext(ctx); ok {
		dims.TraceID = parent.TraceID
		dims.ParentSpanID = parent.SpanID
		dims.AgentDepth = parent.AgentDepth
		dims.VirtualKeyID = parent.VirtualKeyID
	}
	return dims
}

// observedModel begins a child "llm" span around every model call. The
// RoutedProvider underneath folds provider, credential, and token detail into
// that span through the usual SetExtension path.
type observedModel struct {
	inner    einomodel.ToolCallingChatModel
	observer usage.InteractionObserver
	routeID  string
	agentID  string
}

func newObservedModel(inner einomodel.ToolCallingChatModel, observer usage.InteractionObserver, routeID, agentID string) einomodel.ToolCallingChatModel {
	if observer == nil {
		return inner
	}
	return &observedModel{inner: inner, observer: observer, routeID: routeID, agentID: agentID}
}

func (m *observedModel) Generate(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	span, cctx := m.observer.Begin(ctx, childDims(ctx, "llm", m.routeID, m.agentID))
	out, err := m.inner.Generate(cctx, input, opts...)
	if err != nil {
		span.AddAnnotation("error_type", "provider_request_failed")
		span.Finish(usage.InteractionOutcome{Success: false, StatusCode: 502})
		return nil, err
	}
	span.Finish(usage.InteractionOutcome{Success: true, StatusCode: 200})
	return out, nil
}

func (m *observedModel) Stream(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	span, cctx := m.observer.Begin(ctx, childDims(ctx, "llm", m.routeID, m.agentID))
	stream, err := m.inner.Stream(cctx, input, opts...)
	if err != nil {
		span.AddAnnotation("error_type", "provider_stream_failed")
		span.Finish(usage.InteractionOutcome{Success: false, StatusCode: 502})
		return nil, err
	}
	// Streaming usage arrives on the final chunk's ResponseMeta; relay chunks
	// through a pipe so the span can capture it and finish when the stream
	// drains. The pipe goroutine exits when either side closes.
	sr, sw := schema.Pipe[*schema.Message](8)
	go func() {
		defer stream.Close()
		defer sw.Close()
		var tokens usage.LLMExtension
		success := true
		for {
			msg, recvErr := stream.Recv()
			if recvErr == io.EOF {
				break
			}
			if recvErr != nil {
				success = false
				sw.Send(nil, recvErr)
				break
			}
			if msg != nil && msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
				u := msg.ResponseMeta.Usage
				total := u.TotalTokens
				if total == 0 {
					total = u.PromptTokens + u.CompletionTokens
				}
				tokens = usage.LLMExtension{
					InputTokens:     usage.Int(u.PromptTokens),
					OutputTokens:    usage.Int(u.CompletionTokens),
					TotalTokens:     usage.Int(total),
					CachedTokens:    usage.Int(u.PromptTokenDetails.CachedTokens),
					ReasoningTokens: usage.Int(u.CompletionTokensDetails.ReasoningTokens),
					UsageFinalized:  usage.Bool(u.PromptTokens > 0 || u.CompletionTokens > 0),
				}
			}
			if closed := sw.Send(msg, nil); closed {
				break
			}
		}
		span.SetExtension(tokens)
		if success {
			span.Finish(usage.InteractionOutcome{Success: true, StatusCode: 200})
		} else {
			span.AddAnnotation("error_type", "provider_stream_failed")
			span.Finish(usage.InteractionOutcome{Success: false, StatusCode: 502})
		}
	}()
	return sr, nil
}

func (m *observedModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	bound, err := m.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &observedModel{inner: bound, observer: m.observer, routeID: m.routeID, agentID: m.agentID}, nil
}

// observedTool begins a child "mcp" span around every tool execution.
type observedTool struct {
	inner     tool.InvokableTool
	observer  usage.InteractionObserver
	serviceID string
	toolName  string
	agentID   string
}

func newObservedTool(inner tool.BaseTool, observer usage.InteractionObserver, serviceID, toolName, agentID string) tool.BaseTool {
	invokable, ok := inner.(tool.InvokableTool)
	if !ok || observer == nil {
		return inner
	}
	return &observedTool{inner: invokable, observer: observer, serviceID: serviceID, toolName: toolName, agentID: agentID}
}

func (t *observedTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx)
}

func (t *observedTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	span, cctx := t.observer.Begin(ctx, childDims(ctx, "mcp", "", t.agentID))
	span.SetExtension(usage.MCPExtension{
		ServiceID: t.serviceID,
		Method:    "tools/call",
		ToolName:  t.toolName,
	})
	out, err := t.inner.InvokableRun(cctx, argumentsInJSON, opts...)
	if err != nil {
		span.SetExtension(usage.MCPExtension{ResultStatus: "error"})
		span.AddAnnotation("error_type", "tool_call_failed")
		span.Finish(usage.InteractionOutcome{Success: false, StatusCode: 502})
		return "", err
	}
	span.SetExtension(usage.MCPExtension{ResultStatus: "success"})
	span.Finish(usage.InteractionOutcome{Success: true, StatusCode: 200})
	return out, nil
}
