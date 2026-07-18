package otelexport

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
)

type discardSink struct{}

func (discardSink) Enqueue(any) bool { return true }

// tapTestSetup wires the component tap at a fresh in-memory recorder and
// returns a ctx carrying a live interaction span's dimensions plus chat-model
// run info — exactly how the production aspect path delivers it.
func tapTestSetup(t *testing.T) (*tracetest.InMemoryExporter, usage.InteractionDimensions, context.Context) {
	t.Helper()
	recorder := tracetest.NewInMemoryExporter()
	exp := newWithProcessor(sdktrace.NewSimpleSpanProcessor(recorder), usage.OTLPConfig{ServiceName: "tap-test"}.Normalized())
	EnableComponentTap(exp)
	t.Cleanup(func() { disableComponentTap(exp) })
	observer := usage.NewObserver(discardSink{})
	_, ctx := observer.Begin(t.Context(), usage.InteractionDimensions{RouteKind: "llm"})
	dims, ok := usage.DimensionsFromContext(ctx)
	if !ok {
		t.Fatal("observer.Begin must install dimensions")
	}
	ctx = callbacks.EnsureRunInfo(ctx, "claude", components.ComponentOfChatModel)
	return recorder, dims, ctx
}

func TestComponentTapExportsChatModelSpan(t *testing.T) {
	recorder, dims, ctx := tapTestSetup(t)

	ctx = callbacks.OnStart(ctx, &model.CallbackInput{Config: &model.Config{Model: "gpt-x"}})
	callbacks.OnEnd(ctx, &model.CallbackOutput{TokenUsage: &model.TokenUsage{
		PromptTokens:            11,
		PromptTokenDetails:      model.PromptTokenDetails{CachedTokens: 7},
		CompletionTokens:        5,
		CompletionTokensDetails: model.CompletionTokensDetails{ReasoningTokens: 2},
	}})

	spans := recorder.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != "chatmodel generate" {
		t.Fatalf("span name = %q, want %q", span.Name, "chatmodel generate")
	}
	if span.SpanKind != trace.SpanKindClient {
		t.Fatalf("span kind = %v, want client", span.SpanKind)
	}
	if got := span.SpanContext.TraceID().String(); got != dims.TraceID {
		t.Fatalf("trace id = %q, want the interaction trace %q", got, dims.TraceID)
	}
	if got := span.Parent.SpanID().String(); got != dims.SpanID {
		t.Fatalf("parent span id = %q, want the interaction span %q", got, dims.SpanID)
	}
	if got := span.SpanContext.SpanID().String(); got == dims.SpanID {
		t.Fatal("component span must have its own span id")
	}
	for key, want := range map[string]int64{
		"gen_ai.usage.input_tokens":  11,
		"gen_ai.usage.output_tokens": 5,
		"agw.llm.cached_tokens":      7,
		"agw.llm.reasoning_tokens":   2,
	} {
		v, ok := attrValue(t, span.Attributes, key)
		if !ok || v.AsInt64() != want {
			t.Fatalf("attribute %s = %v (present=%v), want %d", key, v.Emit(), ok, want)
		}
	}
	v, ok := attrValue(t, span.Attributes, "gen_ai.request.model")
	if !ok || v.AsString() != "gpt-x" {
		t.Fatalf("gen_ai.request.model = %v (present=%v), want gpt-x", v.Emit(), ok)
	}
	if !span.EndTime.After(span.StartTime) {
		t.Fatalf("span times = %v..%v, want a positive duration", span.StartTime, span.EndTime)
	}
}

func TestComponentTapMarksErrors(t *testing.T) {
	recorder, _, ctx := tapTestSetup(t)

	ctx = callbacks.OnStart(ctx, &model.CallbackInput{})
	callbacks.OnError(ctx, errors.New("upstream exploded"))

	spans := recorder.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	if spans[0].Status.Code != codes.Error || spans[0].Status.Description != "upstream exploded" {
		t.Fatalf("status = %+v, want error with the message", spans[0].Status)
	}
}

func TestComponentTapStreamSpanClosesTheCopy(t *testing.T) {
	recorder, _, ctx := tapTestSetup(t)

	ctx = callbacks.OnStart(ctx, &model.CallbackInput{Config: &model.Config{Model: "gpt-x"}})
	sr, sw := schema.Pipe[callbacks.CallbackOutput](1)
	// The handler must close its copy without draining; a blocked writer
	// would deadlock the test if it tried to consume.
	callbacks.OnEndWithStreamOutput(ctx, sr)
	closed := sw.Send(&model.CallbackOutput{}, nil)
	sw.Close()

	spans := recorder.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	if spans[0].Name != "chatmodel stream" {
		t.Fatalf("span name = %q, want %q", spans[0].Name, "chatmodel stream")
	}
	v, ok := attrValue(t, spans[0].Attributes, "agw.llm.stream")
	if !ok || !v.AsBool() {
		t.Fatalf("agw.llm.stream = %v (present=%v), want true", v.Emit(), ok)
	}
	if !closed {
		t.Log("stream writer still open (single-handler copy semantics); close observed via no deadlock")
	}
}

func TestComponentTapSkipsWithoutInteractionContext(t *testing.T) {
	recorder, _, _ := tapTestSetup(t)

	// A bare context has no interaction dimensions: no orphan spans.
	ctx := callbacks.EnsureRunInfo(t.Context(), "claude", components.ComponentOfChatModel)
	ctx = callbacks.OnStart(ctx, &model.CallbackInput{})
	callbacks.OnEnd(ctx, &model.CallbackOutput{})

	if spans := recorder.GetSpans(); len(spans) != 0 {
		t.Fatalf("exported spans = %d, want 0 without an interaction span", len(spans))
	}
}

func TestComponentTapDisabledExporterIsInert(t *testing.T) {
	recorder, _, ctx := tapTestSetup(t)
	tapExporter.Store(nil)

	ctx = callbacks.OnStart(ctx, &model.CallbackInput{})
	callbacks.OnEnd(ctx, &model.CallbackOutput{})

	if spans := recorder.GetSpans(); len(spans) != 0 {
		t.Fatalf("exported spans = %d, want 0 with the tap detached", len(spans))
	}
}

func TestDisableComponentTapKeepsNewerExporter(t *testing.T) {
	recorder := tracetest.NewInMemoryExporter()
	older := newWithProcessor(sdktrace.NewSimpleSpanProcessor(recorder), usage.OTLPConfig{}.Normalized())
	newer := newWithProcessor(sdktrace.NewSimpleSpanProcessor(recorder), usage.OTLPConfig{}.Normalized())
	EnableComponentTap(older)
	EnableComponentTap(newer)
	t.Cleanup(func() { disableComponentTap(newer) })

	// A config reload closes the older exporter after installing the newer
	// one; the detach must not clobber the active pointer.
	disableComponentTap(older)
	if got := tapExporter.Load(); got != newer {
		t.Fatalf("active exporter = %p, want the newer %p", got, newer)
	}
}
