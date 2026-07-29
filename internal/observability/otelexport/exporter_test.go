package otelexport

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
)

const (
	testTraceID      = "0123456789abcdef0123456789abcdef"
	testSpanID       = "0123456789abcdef"
	testParentSpanID = "fedcba9876543210"
)

func testInteraction() usage.InteractionEvent {
	started := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	return usage.InteractionEvent{
		EventID:       "ev-1",
		TraceID:       testTraceID,
		SpanID:        testSpanID,
		ParentSpanID:  testParentSpanID,
		AgentDepth:    2,
		StartedAt:     started,
		FinishedAt:    started.Add(750 * time.Millisecond),
		RouteID:       "openai-chat",
		RouteKind:     "llm",
		RouteProtocol: "openai",
		VirtualKeyID:  "vk-1",
		Success:       true,
		StatusCode:    200,
		AgentID:       "agent-1",
	}
}

func attrValue(t *testing.T, attrs []attribute.KeyValue, key string) (attribute.Value, bool) {
	t.Helper()
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestSpanStubLLMEvent(t *testing.T) {
	ev := usage.LLMUsageEvent{
		InteractionEvent: testInteraction(),
		LLMAPI:           "openai",
		APIOperation:     "chat",
		ProviderID:       "openai-main",
		ProviderType:     "openai",
		LogicalModel:     "gpt-main",
		UpstreamModel:    "gpt-4.1",
		Stream:           true,
		InputTokens:      100,
		OutputTokens:     20,
		TotalTokens:      120,
		CachedTokens:     40,
		ReasoningTokens:  5,
		UsageFinalized:   true,
	}
	stub, err := spanStub(ev)
	if err != nil {
		t.Fatalf("spanStub() error = %v", err)
	}
	if stub.Name != "llm chat" {
		t.Fatalf("name = %q, want %q", stub.Name, "llm chat")
	}
	if stub.SpanKind != trace.SpanKindServer {
		t.Fatalf("kind = %v, want server", stub.SpanKind)
	}
	if got := stub.SpanContext.TraceID().String(); got != testTraceID {
		t.Fatalf("trace id = %q, want %q", got, testTraceID)
	}
	if got := stub.SpanContext.SpanID().String(); got != testSpanID {
		t.Fatalf("span id = %q, want %q", got, testSpanID)
	}
	if got := stub.Parent.SpanID().String(); got != testParentSpanID {
		t.Fatalf("parent span id = %q, want %q", got, testParentSpanID)
	}
	if got := stub.Parent.TraceID().String(); got != testTraceID {
		t.Fatalf("parent trace id = %q, want %q", got, testTraceID)
	}
	if !stub.SpanContext.IsSampled() {
		t.Fatal("span context must be sampled or the batch processor drops it")
	}
	if stub.Status.Code != codes.Unset {
		t.Fatalf("status = %v, want unset on success", stub.Status)
	}
	if stub.EndTime.Sub(stub.StartTime) != 750*time.Millisecond {
		t.Fatalf("duration = %v, want 750ms", stub.EndTime.Sub(stub.StartTime))
	}
	for key, want := range map[string]string{
		"agw.route.id":          "openai-chat",
		"agw.agent.id":          "agent-1",
		"gen_ai.request.model":  "gpt-main",
		"gen_ai.response.model": "gpt-4.1",
	} {
		v, ok := attrValue(t, stub.Attributes, key)
		if !ok || v.AsString() != want {
			t.Fatalf("attribute %s = %v (present=%v), want %q", key, v.Emit(), ok, want)
		}
	}
	for key, want := range map[string]int64{
		"gen_ai.usage.input_tokens":  100,
		"gen_ai.usage.output_tokens": 20,
		"agw.llm.cached_tokens":      40,
		"agw.llm.reasoning_tokens":   5,
		"agw.agent.depth":            2,
	} {
		v, ok := attrValue(t, stub.Attributes, key)
		if !ok || v.AsInt64() != want {
			t.Fatalf("attribute %s = %v (present=%v), want %d", key, v.Emit(), ok, want)
		}
	}
}

func TestSpanStubBuiltinInternalEventsAreInternalKind(t *testing.T) {
	llm := testInteraction()
	llm.RouteProtocol = "builtin"
	stubLLM, err := spanStub(usage.LLMUsageEvent{InteractionEvent: llm, APIOperation: "chat"})
	if err != nil {
		t.Fatalf("spanStub(llm) error = %v", err)
	}
	if stubLLM.SpanKind != trace.SpanKindInternal {
		t.Fatalf("builtin-internal llm span kind = %v, want internal", stubLLM.SpanKind)
	}
	mcp := testInteraction()
	mcp.RouteProtocol = "builtin"
	stubMCP, err := spanStub(usage.MCPUsageEvent{InteractionEvent: mcp, Method: "tools/call", ToolName: "fetch"})
	if err != nil {
		t.Fatalf("spanStub(mcp) error = %v", err)
	}
	if stubMCP.SpanKind != trace.SpanKindInternal {
		t.Fatalf("builtin-internal mcp span kind = %v, want internal", stubMCP.SpanKind)
	}
	if stubMCP.Name != "mcp tools/call" {
		t.Fatalf("mcp span name = %q, want %q", stubMCP.Name, "mcp tools/call")
	}
}

func TestSpanStubFailureStatusAndFallbackEndTime(t *testing.T) {
	ev := testInteraction()
	ev.Success = false
	ev.StatusCode = 502
	ev.ErrorType = "provider_request_failed"
	ev.FinishedAt = time.Time{}
	ev.LatencyMS = 1200
	stub, err := spanStub(usage.BuiltinUsageEvent{InteractionEvent: ev, Operation: "turn", TopologyKind: "single"})
	if err != nil {
		t.Fatalf("spanStub() error = %v", err)
	}
	if stub.Status.Code != codes.Error || stub.Status.Description != "provider_request_failed" {
		t.Fatalf("status = %+v, want error/provider_request_failed", stub.Status)
	}
	if stub.EndTime.Sub(stub.StartTime) != 1200*time.Millisecond {
		t.Fatalf("fallback duration = %v, want 1200ms", stub.EndTime.Sub(stub.StartTime))
	}
	if stub.Name != "builtin turn" {
		t.Fatalf("name = %q, want %q", stub.Name, "builtin turn")
	}
}

func TestBuiltinSpanStubCarriesRunCorrelationAndResumeLink(t *testing.T) {
	ev := testInteraction()
	linkedTraceID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	linkedSpanID := "bbbbbbbbbbbbbbbb"
	stub, err := spanStub(usage.BuiltinUsageEvent{
		InteractionEvent:    ev,
		Operation:           "resume",
		RunID:               "run-123",
		PermissionRequestID: "perm-123",
		LinkTraceID:         linkedTraceID,
		LinkSpanID:          linkedSpanID,
	})
	if err != nil {
		t.Fatalf("spanStub() error = %v", err)
	}
	if len(stub.Links) != 1 {
		t.Fatalf("links = %+v, want one resume link", stub.Links)
	}
	if got := stub.Links[0].SpanContext.TraceID().String(); got != linkedTraceID {
		t.Fatalf("link trace id = %q, want %q", got, linkedTraceID)
	}
	if got := stub.Links[0].SpanContext.SpanID().String(); got != linkedSpanID {
		t.Fatalf("link span id = %q, want %q", got, linkedSpanID)
	}
	attrs := map[attribute.Key]attribute.Value{}
	for _, attr := range stub.Attributes {
		attrs[attr.Key] = attr.Value
	}
	if got := attrs["agw.builtin.run_id"].AsString(); got != "run-123" {
		t.Fatalf("run_id attribute = %q", got)
	}
	if got := attrs["agw.agent.run_id"].AsString(); got != "run-123" {
		t.Fatalf("common run_id attribute = %q", got)
	}
	if got := attrs["agw.builtin.permission_request_id"].AsString(); got != "perm-123" {
		t.Fatalf("permission_request_id attribute = %q", got)
	}
}

func TestUnifiedAgentACPSpanHasCommonIdentityWithoutServiceIdentity(t *testing.T) {
	ev := testInteraction()
	ev.RouteID, ev.RouteKind, ev.RouteProtocol = "agent:reviewer:review", "agent", "agent"
	ev.AgentID, ev.RuntimeType, ev.RunID = "reviewer", "acp", "run-123"
	stub, err := spanStub(usage.ACPUsageEvent{InteractionEvent: ev, AgentType: "codex", Operation: "turn"})
	if err != nil {
		t.Fatalf("spanStub() error = %v", err)
	}
	for key, want := range map[string]string{
		"agw.route.kind": "agent", "agw.route.protocol": "agent", "agw.agent.id": "reviewer",
		"agw.agent.runtime_type": "acp", "agw.agent.run_id": "run-123",
	} {
		v, ok := attrValue(t, stub.Attributes, key)
		if !ok || v.AsString() != want {
			t.Fatalf("attribute %s = %v (present=%v), want %q", key, v.Emit(), ok, want)
		}
	}
	if _, ok := attrValue(t, stub.Attributes, "agw.acp.service_id"); ok {
		t.Fatal("unified ACP span exposed historical service identity")
	}
}

func TestSpanStubRejectsInvalidIDsAndUnknownTypes(t *testing.T) {
	ev := testInteraction()
	ev.TraceID = "not-hex"
	if _, err := spanStub(usage.ACPUsageEvent{InteractionEvent: ev}); err == nil {
		t.Fatal("spanStub() with invalid trace id must error")
	}
	if _, err := spanStub("not an event"); err == nil {
		t.Fatal("spanStub() with unknown type must error")
	}
}

func TestExporterEmitsSpansThroughProcessor(t *testing.T) {
	recorder := tracetest.NewInMemoryExporter()
	exp := newWithProcessor(sdktrace.NewSimpleSpanProcessor(recorder), usage.OTLPConfig{ServiceName: "test-gw"}.Normalized())
	if err := exp.ExportUsageEvent(usage.LLMUsageEvent{InteractionEvent: testInteraction(), APIOperation: "chat"}); err != nil {
		t.Fatalf("ExportUsageEvent() error = %v", err)
	}
	if err := exp.ExportUsageEvent("garbage"); err == nil {
		t.Fatal("ExportUsageEvent() with unknown type must error")
	}
	spans := recorder.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != "llm chat" {
		t.Fatalf("span name = %q, want %q", span.Name, "llm chat")
	}
	if span.Resource == nil {
		t.Fatal("span resource is nil")
	}
	v, ok := attrValue(t, span.Resource.Attributes(), "service.name")
	if !ok || v.AsString() != "test-gw" {
		t.Fatalf("service.name = %v (present=%v), want test-gw", v.Emit(), ok)
	}
	if span.InstrumentationScope.Name != scopeName {
		t.Fatalf("scope = %q, want %q", span.InstrumentationScope.Name, scopeName)
	}
	if err := exp.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestNewValidatesAndBuildsBothTransports(t *testing.T) {
	if _, err := New(context.Background(), usage.OTLPConfig{Endpoint: "127.0.0.1:4317", Protocol: "invalid"}); err == nil {
		t.Fatal("New() with invalid protocol must error")
	}
	for _, protocol := range []string{"grpc", "http"} {
		exp, err := New(context.Background(), usage.OTLPConfig{Endpoint: "127.0.0.1:0", Protocol: protocol, Insecure: true})
		if err != nil {
			t.Fatalf("New(%s) error = %v", protocol, err)
		}
		if err := exp.Close(); err != nil {
			t.Logf("Close(%s) after no export returned %v (transport never connected)", protocol, err)
		}
	}
}
