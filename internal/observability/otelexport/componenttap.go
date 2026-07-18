package otelexport

import (
	"context"
	"crypto/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
)

// The component tap exports one OTel span per eino chat-model component call
// (eino-reuse.md §4.4), parented under the gateway interaction span resolved
// from the request context — component-internal detail (call setup, SDK-level
// timing, per-call token usage) nests inside the same trace tree the
// usage-event exporter reconstructs. It complements einotap: that handler
// merges token detail into the usage event (metering), this one emits tracing
// spans and never touches metering.
//
// For streaming calls the span ends when the component returns the stream
// (setup latency); the full drain duration lives on the enclosing interaction
// span. The handler closes its stream copy immediately, so it never buffers
// or delays chunks.

var (
	tapOnce     sync.Once
	tapExporter atomic.Pointer[Exporter]
)

// EnableComponentTap points the process-global callbacks handler at e. The
// handler registers exactly once (AppendGlobalHandlers has no unregister and
// Caddy config reloads re-run provision); what a reload swaps is the exporter
// pointer.
func EnableComponentTap(e *Exporter) {
	tapOnce.Do(func() { callbacks.AppendGlobalHandlers(componentTap{}) })
	tapExporter.Store(e)
}

// disableComponentTap detaches e if it is still the active exporter; a newer
// exporter installed by a config reload is left in place.
func disableComponentTap(e *Exporter) {
	tapExporter.CompareAndSwap(e, nil)
}

type componentTap struct{}

var (
	_ callbacks.Handler       = componentTap{}
	_ callbacks.TimingChecker = componentTap{}
)

// tapSpanKey carries the per-call span state from OnStart to the terminal
// callback of the same component call (the aspect chain threads the ctx).
type tapSpanKey struct{}

type tapSpan struct {
	traceID      trace.TraceID
	parentSpanID trace.SpanID
	spanID       trace.SpanID
	start        time.Time
	model        string
}

// Needed opts into the start and terminal timings. Accepting
// OnEndWithStreamOutput costs one stream copy per call, which the handler
// closes immediately.
func (componentTap) Needed(_ context.Context, _ *callbacks.RunInfo, timing callbacks.CallbackTiming) bool {
	switch timing {
	case callbacks.TimingOnStart, callbacks.TimingOnEnd, callbacks.TimingOnError, callbacks.TimingOnEndWithStreamOutput:
		return true
	default:
		return false
	}
}

func (componentTap) OnStart(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
	if tapExporter.Load() == nil || info == nil || info.Component != components.ComponentOfChatModel {
		return ctx
	}
	// No interaction context, no span: an orphan component span could not be
	// correlated to anything and would pollute the trace store.
	dims, ok := usage.DimensionsFromContext(ctx)
	if !ok {
		return ctx
	}
	traceID, err := trace.TraceIDFromHex(dims.TraceID)
	if err != nil {
		return ctx
	}
	parentSpanID, err := trace.SpanIDFromHex(dims.SpanID)
	if err != nil {
		return ctx
	}
	spanID, err := randomSpanID()
	if err != nil {
		return ctx
	}
	span := &tapSpan{
		traceID:      traceID,
		parentSpanID: parentSpanID,
		spanID:       spanID,
		start:        time.Now(),
	}
	if in := model.ConvCallbackInput(input); in != nil && in.Config != nil {
		span.model = in.Config.Model
	}
	return context.WithValue(ctx, tapSpanKey{}, span)
}

func (componentTap) OnEnd(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
	finishComponentSpan(ctx, info, model.ConvCallbackOutput(output), nil, false)
	return ctx
}

func (componentTap) OnError(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
	finishComponentSpan(ctx, info, nil, err, false)
	return ctx
}

func (componentTap) OnStartWithStreamInput(ctx context.Context, _ *callbacks.RunInfo,
	input *schema.StreamReader[callbacks.CallbackInput]) context.Context {
	// Unreachable while Needed excludes this timing; close defensively so a
	// future timing change cannot leak the stream pipeline.
	input.Close()
	return ctx
}

func (componentTap) OnEndWithStreamOutput(ctx context.Context, info *callbacks.RunInfo,
	output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
	// Timing-only: the copy is closed unread so the tap never drains or
	// delays the primary stream.
	output.Close()
	finishComponentSpan(ctx, info, nil, nil, true)
	return ctx
}

func finishComponentSpan(ctx context.Context, info *callbacks.RunInfo, out *model.CallbackOutput, callErr error, streaming bool) {
	exporter := tapExporter.Load()
	if exporter == nil || info == nil || info.Component != components.ComponentOfChatModel {
		return
	}
	span, ok := ctx.Value(tapSpanKey{}).(*tapSpan)
	if !ok {
		return
	}
	name := "chatmodel generate"
	if streaming {
		name = "chatmodel stream"
	}
	attrs := make([]attribute.KeyValue, 0, 12)
	attrs = appendString(attrs, "agw.component", string(info.Component))
	attrs = appendString(attrs, "agw.component.type", info.Type)
	attrs = appendString(attrs, "agw.component.name", info.Name)
	attrs = appendString(attrs, "gen_ai.request.model", span.model)
	attrs = append(attrs, attribute.Bool("agw.llm.stream", streaming))
	if out != nil && out.TokenUsage != nil {
		tu := out.TokenUsage
		attrs = append(attrs,
			attribute.Int("gen_ai.usage.input_tokens", tu.PromptTokens),
			attribute.Int("gen_ai.usage.output_tokens", tu.CompletionTokens),
		)
		attrs = appendPositiveInt(attrs, "agw.llm.cached_tokens", tu.PromptTokenDetails.CachedTokens)
		attrs = appendPositiveInt(attrs, "agw.llm.reasoning_tokens", tu.CompletionTokensDetails.ReasoningTokens)
	}
	stub := tracetest.SpanStub{
		Name: name,
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    span.traceID,
			SpanID:     span.spanID,
			TraceFlags: trace.FlagsSampled,
		}),
		Parent: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    span.traceID,
			SpanID:     span.parentSpanID,
			TraceFlags: trace.FlagsSampled,
		}),
		SpanKind:   trace.SpanKindClient,
		StartTime:  span.start,
		EndTime:    time.Now(),
		Attributes: attrs,
	}
	if callErr != nil {
		stub.Status = sdktrace.Status{Code: codes.Error, Description: callErr.Error()}
	}
	exporter.exportComponentSpan(stub)
}

func randomSpanID() (trace.SpanID, error) {
	var id trace.SpanID
	for {
		if _, err := rand.Read(id[:]); err != nil {
			return trace.SpanID{}, err
		}
		if id.IsValid() {
			return id, nil
		}
	}
}
