package otelexport

import (
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
)

// spanStub converts one usage event into a SpanStub carrying the event's own
// W3C trace/span/parent ids, so the exported span slots into the exact tree
// position the gateway recorded. tracetest.SpanStub is the only supported way
// to construct a ReadOnlySpan outside the SDK (the interface has an unexported
// method), and its Snapshot form is what the OTLP exporter consumes.
func spanStub(ev any) (tracetest.SpanStub, error) {
	switch e := ev.(type) {
	case usage.LLMUsageEvent:
		return llmSpanStub(e)
	case usage.MCPUsageEvent:
		return mcpSpanStub(e)
	case usage.ACPUsageEvent:
		return acpSpanStub(e)
	case usage.BuiltinUsageEvent:
		return builtinSpanStub(e)
	default:
		return tracetest.SpanStub{}, fmt.Errorf("unsupported usage event type %T", ev)
	}
}

// builtinInternalProtocol marks LLM/MCP child events produced inside a
// builtin-agent turn (see pkg/agent/builtin). Those are host-internal calls,
// not served requests, so they export as internal spans.
const builtinInternalProtocol = "builtin"

func llmSpanStub(ev usage.LLMUsageEvent) (tracetest.SpanStub, error) {
	name := spanName("llm", ev.APIOperation)
	kind := trace.SpanKindServer
	if ev.RouteProtocol == builtinInternalProtocol {
		kind = trace.SpanKindInternal
	}
	attrs := commonAttributes(ev.InteractionEvent)
	attrs = appendString(attrs, "agw.llm.api", ev.LLMAPI)
	attrs = appendString(attrs, "agw.provider.id", ev.ProviderID)
	attrs = appendString(attrs, "agw.provider.type", ev.ProviderType)
	requestModel := ev.LogicalModel
	if requestModel == "" {
		requestModel = ev.UpstreamModel
	}
	attrs = appendString(attrs, "gen_ai.request.model", requestModel)
	attrs = appendString(attrs, "gen_ai.response.model", ev.UpstreamModel)
	attrs = append(attrs, attribute.Bool("agw.llm.stream", ev.Stream))
	if ev.UsageFinalized {
		attrs = append(attrs,
			attribute.Int("gen_ai.usage.input_tokens", ev.InputTokens),
			attribute.Int("gen_ai.usage.output_tokens", ev.OutputTokens),
			attribute.Int("agw.llm.total_tokens", ev.TotalTokens),
		)
		attrs = appendPositiveInt(attrs, "agw.llm.cached_tokens", ev.CachedTokens)
		attrs = appendPositiveInt(attrs, "agw.llm.reasoning_tokens", ev.ReasoningTokens)
	}
	attrs = appendString(attrs, "agw.llm.credential_source", ev.CredentialSource)
	attrs = appendString(attrs, "agw.llm.credential_id", ev.CredentialID)
	attrs = appendPositiveInt(attrs, "agw.llm.tool_call_count", ev.ToolCallCount)
	return baseSpanStub(name, kind, ev.InteractionEvent, attrs)
}

func mcpSpanStub(ev usage.MCPUsageEvent) (tracetest.SpanStub, error) {
	name := spanName("mcp", ev.Method)
	kind := trace.SpanKindServer
	if ev.RouteProtocol == builtinInternalProtocol {
		kind = trace.SpanKindInternal
	}
	attrs := commonAttributes(ev.InteractionEvent)
	attrs = appendString(attrs, "agw.mcp.service_id", ev.ServiceID)
	attrs = appendString(attrs, "agw.mcp.method", ev.Method)
	attrs = appendString(attrs, "agw.mcp.tool_name", ev.ToolName)
	attrs = appendString(attrs, "agw.mcp.result_status", ev.ResultStatus)
	if ev.Cancelled {
		attrs = append(attrs, attribute.Bool("agw.mcp.cancelled", true))
	}
	return baseSpanStub(name, kind, ev.InteractionEvent, attrs)
}

func acpSpanStub(ev usage.ACPUsageEvent) (tracetest.SpanStub, error) {
	name := spanName("acp", ev.Operation)
	attrs := commonAttributes(ev.InteractionEvent)
	attrs = appendString(attrs, "agw.acp.service_id", ev.ServiceID)
	attrs = appendString(attrs, "agw.acp.agent_type", ev.AgentType)
	attrs = appendString(attrs, "agw.acp.operation", ev.Operation)
	attrs = appendString(attrs, "agw.acp.session_id", ev.SessionID)
	attrs = appendString(attrs, "agw.acp.thread_id", ev.ThreadID)
	attrs = appendString(attrs, "agw.acp.result_status", ev.ResultStatus)
	return baseSpanStub(name, trace.SpanKindServer, ev.InteractionEvent, attrs)
}

func builtinSpanStub(ev usage.BuiltinUsageEvent) (tracetest.SpanStub, error) {
	name := spanName("builtin", ev.Operation)
	attrs := commonAttributes(ev.InteractionEvent)
	attrs = appendString(attrs, "agw.builtin.operation", ev.Operation)
	attrs = appendString(attrs, "agw.builtin.session_id", ev.SessionID)
	attrs = appendString(attrs, "agw.builtin.run_id", ev.RunID)
	attrs = appendString(attrs, "agw.builtin.permission_request_id", ev.PermissionRequestID)
	attrs = appendString(attrs, "agw.builtin.topology_kind", ev.TopologyKind)
	attrs = appendPositiveInt(attrs, "agw.builtin.model_steps", ev.ModelSteps)
	attrs = appendPositiveInt(attrs, "agw.builtin.tool_steps", ev.ToolSteps)
	attrs = appendString(attrs, "agw.builtin.result_status", ev.ResultStatus)
	stub, err := baseSpanStub(name, trace.SpanKindServer, ev.InteractionEvent, attrs)
	if err != nil {
		return tracetest.SpanStub{}, err
	}
	if linkTraceID, traceErr := trace.TraceIDFromHex(ev.LinkTraceID); traceErr == nil {
		if linkSpanID, spanErr := trace.SpanIDFromHex(ev.LinkSpanID); spanErr == nil {
			stub.Links = append(stub.Links, sdktrace.Link{SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: linkTraceID, SpanID: linkSpanID, TraceFlags: trace.FlagsSampled,
			})})
		}
	}
	return stub, nil
}

func baseSpanStub(name string, kind trace.SpanKind, ev usage.InteractionEvent, attrs []attribute.KeyValue) (tracetest.SpanStub, error) {
	traceID, err := trace.TraceIDFromHex(ev.TraceID)
	if err != nil {
		return tracetest.SpanStub{}, fmt.Errorf("event %s: invalid trace id %q: %w", ev.EventID, ev.TraceID, err)
	}
	spanID, err := trace.SpanIDFromHex(ev.SpanID)
	if err != nil {
		return tracetest.SpanStub{}, fmt.Errorf("event %s: invalid span id %q: %w", ev.EventID, ev.SpanID, err)
	}
	stub := tracetest.SpanStub{
		Name: name,
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     spanID,
			TraceFlags: trace.FlagsSampled,
		}),
		SpanKind:   kind,
		StartTime:  ev.StartedAt,
		EndTime:    endTime(ev),
		Attributes: attrs,
	}
	if parentSpanID, err := trace.SpanIDFromHex(ev.ParentSpanID); err == nil {
		stub.Parent = trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     parentSpanID,
			TraceFlags: trace.FlagsSampled,
		})
	}
	if !ev.Success {
		stub.Status = sdktrace.Status{Code: codes.Error, Description: ev.ErrorType}
	}
	return stub, nil
}

func endTime(ev usage.InteractionEvent) time.Time {
	if !ev.FinishedAt.IsZero() {
		return ev.FinishedAt
	}
	return ev.StartedAt.Add(time.Duration(ev.LatencyMS) * time.Millisecond)
}

func commonAttributes(ev usage.InteractionEvent) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 16)
	attrs = appendString(attrs, "agw.event.id", ev.EventID)
	attrs = appendString(attrs, "agw.route.id", ev.RouteID)
	attrs = appendString(attrs, "agw.route.kind", ev.RouteKind)
	attrs = appendString(attrs, "agw.route.protocol", ev.RouteProtocol)
	attrs = appendString(attrs, "agw.virtual_key.id", ev.VirtualKeyID)
	attrs = appendString(attrs, "agw.agent.id", ev.AgentID)
	attrs = appendPositiveInt(attrs, "agw.agent.depth", ev.AgentDepth)
	attrs = appendPositiveInt(attrs, "http.response.status_code", ev.StatusCode)
	attrs = appendString(attrs, "agw.error.type", ev.ErrorType)
	return attrs
}

func spanName(family, operation string) string {
	if operation == "" {
		return family
	}
	return family + " " + operation
}

func appendString(attrs []attribute.KeyValue, key, value string) []attribute.KeyValue {
	if value == "" {
		return attrs
	}
	return append(attrs, attribute.String(key, value))
}

func appendPositiveInt(attrs []attribute.KeyValue, key string, value int) []attribute.KeyValue {
	if value <= 0 {
		return attrs
	}
	return append(attrs, attribute.Int(key, value))
}
