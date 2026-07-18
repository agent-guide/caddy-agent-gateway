package provider

import (
	"context"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Callback aspect helpers for self-implemented chat providers. eino-ext
// components fire the callbacks aspect functions internally; self-implemented
// providers (codex, claudecode) call these helpers so registered callbacks
// handlers — the gateway's einotap and any vendor handler — observe every
// provider uniformly. One OnStart/OnEnd (or OnError) cycle per logical
// Chat/StreamChat call.

// OnChatStart ensures chat-model run info on the context and fires the
// OnStart timing. The returned context must be passed to the matching
// OnChatEnd / OnChatError / OnChatStreamEnd call.
func OnChatStart(ctx context.Context, providerType, modelName string, messages []*schema.Message) context.Context {
	ctx = callbacks.EnsureRunInfo(ctx, providerType, components.ComponentOfChatModel)
	return callbacks.OnStart(ctx, &einomodel.CallbackInput{
		Messages: messages,
		Config:   &einomodel.Config{Model: modelName},
	})
}

// OnChatEnd fires the OnEnd timing for a non-streaming chat result.
func OnChatEnd(ctx context.Context, modelName string, msg *schema.Message) {
	callbacks.OnEnd(ctx, chatCallbackOutput(modelName, msg))
}

// OnChatError fires the OnError timing.
func OnChatError(ctx context.Context, err error) {
	callbacks.OnError(ctx, err)
}

// OnChatStreamEnd fires the OnEndWithStreamOutput timing and returns the
// stream the provider must hand to its caller. The framework only copies the
// stream for handlers whose TimingChecker asks for the stream timing; with
// none registered this is a cheap lazy conversion.
func OnChatStreamEnd(ctx context.Context, modelName string, stream *schema.StreamReader[*schema.Message]) *schema.StreamReader[*schema.Message] {
	converted := schema.StreamReaderWithConvert(stream, func(msg *schema.Message) (callbacks.CallbackOutput, error) {
		return chatCallbackOutput(modelName, msg), nil
	})
	_, out := callbacks.OnEndWithStreamOutput(ctx, converted)
	return schema.StreamReaderWithConvert(out, func(v callbacks.CallbackOutput) (*schema.Message, error) {
		output, ok := v.(*einomodel.CallbackOutput)
		if !ok || output == nil {
			return nil, schema.ErrNoValue
		}
		return output.Message, nil
	})
}

func chatCallbackOutput(modelName string, msg *schema.Message) *einomodel.CallbackOutput {
	output := &einomodel.CallbackOutput{
		Message: msg,
		Config:  &einomodel.Config{Model: modelName},
	}
	if msg != nil && msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
		u := msg.ResponseMeta.Usage
		output.TokenUsage = &einomodel.TokenUsage{
			PromptTokens: u.PromptTokens,
			PromptTokenDetails: einomodel.PromptTokenDetails{
				CachedTokens: u.PromptTokenDetails.CachedTokens,
			},
			CompletionTokens: u.CompletionTokens,
			CompletionTokensDetails: einomodel.CompletionTokensDetails{
				ReasoningTokens: u.CompletionTokensDetails.ReasoningTokens,
			},
			TotalTokens: u.TotalTokens,
		}
	}
	return output
}
