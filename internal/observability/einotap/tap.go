// Package einotap bridges eino component callbacks into the gateway's usage
// observability. It registers one process-global, stateless callbacks handler
// that folds chat-model call detail (token usage including cached and
// reasoning breakdowns) into the current interaction span.
//
// Design constraints (see docs/design/eino-reuse.md §4.1):
//
//   - Merge, never emit: the handler only calls SetExtension on the span
//     resolved from the request context. It never enqueues a usage event, so
//     it cannot double-count requests.
//   - Synchronous timings only: the handler declares (via TimingChecker) that
//     it needs OnEnd alone. Streaming token detail is captured by the
//     protocol handlers from ResponseMeta.Usage on the primary stream before
//     the span finishes, so the handler never takes a stream copy and the
//     late-usage race against span.Finish cannot occur.
//   - Stateless registration: the handler resolves the span from ctx, so it
//     captures no app instance. AppendGlobalHandlers is process-global,
//     not thread-safe, and has no unregister; the sync.Once makes Register
//     safe under Caddy config reloads re-running app provision.
package einotap

import (
	"context"
	"sync"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
)

var registerOnce sync.Once

// Register installs the global eino callbacks handler exactly once per
// process. Every bootstrap path (Caddy app provision, standalone server)
// calls it; repeat calls are no-ops.
func Register() {
	registerOnce.Do(func() {
		callbacks.AppendGlobalHandlers(Handler())
	})
}

// Handler returns the tap handler. Exposed for tests; production code uses
// Register.
func Handler() callbacks.Handler {
	return tapHandler{}
}

type tapHandler struct{}

var (
	_ callbacks.Handler       = tapHandler{}
	_ callbacks.TimingChecker = tapHandler{}
)

// Needed keeps the handler out of every timing except OnEnd. In particular it
// opts out of OnEndWithStreamOutput so the framework never copies streams for
// this handler.
func (tapHandler) Needed(_ context.Context, _ *callbacks.RunInfo, timing callbacks.CallbackTiming) bool {
	return timing == callbacks.TimingOnEnd
}

func (tapHandler) OnStart(ctx context.Context, _ *callbacks.RunInfo, _ callbacks.CallbackInput) context.Context {
	return ctx
}

func (tapHandler) OnEnd(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
	if info == nil || info.Component != components.ComponentOfChatModel {
		return ctx
	}
	out := model.ConvCallbackOutput(output)
	if out == nil || out.TokenUsage == nil {
		return ctx
	}
	tu := out.TokenUsage
	total := tu.TotalTokens
	if total == 0 {
		total = tu.PromptTokens + tu.CompletionTokens
	}
	ext := usage.LLMExtension{
		InputTokens:     usage.Int(tu.PromptTokens),
		OutputTokens:    usage.Int(tu.CompletionTokens),
		TotalTokens:     usage.Int(total),
		CachedTokens:    usage.Int(tu.PromptTokenDetails.CachedTokens),
		ReasoningTokens: usage.Int(tu.CompletionTokensDetails.ReasoningTokens),
	}
	if tu.PromptTokens > 0 || tu.CompletionTokens > 0 {
		ext.UsageFinalized = usage.Bool(true)
	}
	usage.SpanFromContext(ctx).SetExtension(ext)
	return ctx
}

func (tapHandler) OnError(ctx context.Context, _ *callbacks.RunInfo, _ error) context.Context {
	return ctx
}

func (tapHandler) OnStartWithStreamInput(ctx context.Context, _ *callbacks.RunInfo,
	input *schema.StreamReader[callbacks.CallbackInput]) context.Context {
	// Unreachable while Needed excludes this timing; close defensively so a
	// future timing change cannot leak the stream pipeline.
	input.Close()
	return ctx
}

func (tapHandler) OnEndWithStreamOutput(ctx context.Context, _ *callbacks.RunInfo,
	output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
	output.Close()
	return ctx
}
