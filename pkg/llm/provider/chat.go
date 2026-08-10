package provider

import (
	"context"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ChatOptions carries additional chat request options that extend the standard
// eino model options. Use WithTopK / GetChatOptions to set and read these.
type ChatOptions struct {
	TopK      int
	Responses *ResponsesRequestContext
	ChatExtra *ChatExtraFields
}

// ChatExtraFields carries chat-completions request fields that have no eino
// common-option equivalent but should still reach OpenAI-compatible providers.
// Values are already in chat wire shape so they pass through unchanged.
type ChatExtraFields struct {
	ResponseFormat    any
	Reasoning         map[string]any
	Thinking          map[string]any
	ReasoningEffort   string
	ToolStream        *bool
	StreamOptions     map[string]any
	User              string
	Metadata          map[string]any
	ParallelToolCalls *bool
	Store             *bool
}

// WithChatExtraFields stores extra chat-completions request fields inside
// ChatRequest.Options so they survive generic chat-provider compatibility.
func WithChatExtraFields(fields *ChatExtraFields) einomodel.Option {
	return einomodel.WrapImplSpecificOptFn(func(o *ChatOptions) {
		o.ChatExtra = cloneChatExtraFields(fields)
	})
}

// ChatExtraFieldsFromOptions extracts any stored chat-completions extra fields
// from a chat option list.
func ChatExtraFieldsFromOptions(opts ...einomodel.Option) *ChatExtraFields {
	chatOpts := GetChatOptions(opts...)
	if chatOpts == nil {
		return nil
	}
	return cloneChatExtraFields(chatOpts.ChatExtra)
}

// WithTopK adds a top-k sampling option. It is encoded as an impl-specific
// option so it travels inside ChatRequest.Options alongside standard options
// (temperature, max_tokens, etc.) and any provider can read it via GetChatOptions.
func WithTopK(topK int) einomodel.Option {
	return einomodel.WrapImplSpecificOptFn(func(o *ChatOptions) {
		o.TopK = topK
	})
}

// ResponsesRequestContext carries Responses API request fields that do not map
// directly onto the shared chat abstraction but still need to survive the
// compatibility path so any provider can inspect or reuse them.
type ResponsesRequestContext struct {
	PreviousResponseID string
	Store              *bool
	Text               map[string]any
	Metadata           map[string]any
	User               string
	Reasoning          map[string]any
	ParallelToolCalls  *bool
	Truncation         any
}

// WithResponsesRequestContext stores extra Responses API request fields inside
// ChatRequest.Options so they survive generic chat-provider compatibility.
func WithResponsesRequestContext(ctx *ResponsesRequestContext) einomodel.Option {
	return einomodel.WrapImplSpecificOptFn(func(o *ChatOptions) {
		o.Responses = cloneResponsesRequestContext(ctx)
	})
}

// GetChatOptions extracts ChatOptions from an option list.
func GetChatOptions(opts ...einomodel.Option) *ChatOptions {
	return einomodel.GetImplSpecificOptions[ChatOptions](nil, opts...)
}

// ResponsesRequestContextFromOptions extracts any stored Responses API request
// context from a chat option list.
func ResponsesRequestContextFromOptions(opts ...einomodel.Option) *ResponsesRequestContext {
	chatOpts := GetChatOptions(opts...)
	if chatOpts == nil {
		return nil
	}
	return cloneResponsesRequestContext(chatOpts.Responses)
}

// ChatRequest is the unified internal chat request format passed to providers.
type ChatRequest struct {
	Model    string
	Messages []*schema.Message
	Options  []einomodel.Option
}

// ChatResponse is the unified internal chat response format returned by providers.
type ChatResponse struct {
	Message *schema.Message
}

type ChatRequestState struct {
	ModelName     string
	Messages      []*schema.Message
	Options       []einomodel.Option
	CommonOptions *einomodel.Options
}

func ResolveChatRequest(_ context.Context, config ProviderConfig, req *ChatRequest) (*ChatRequestState, error) {
	modelName := req.Model
	if modelName == "" {
		modelName = config.DefaultModel
	}

	opts := append([]einomodel.Option(nil), req.Options...)

	return &ChatRequestState{
		ModelName:     modelName,
		Messages:      req.Messages,
		Options:       opts,
		CommonOptions: einomodel.GetCommonOptions(nil, opts...),
	}, nil
}

func ChatResponseFromEinoMessage(msg *schema.Message) *ChatResponse {
	if msg == nil {
		return &ChatResponse{}
	}

	return &ChatResponse{
		Message: msg,
	}
}

func FinishReason(msg *schema.Message) string {
	if msg == nil || msg.ResponseMeta == nil {
		return ""
	}
	return msg.ResponseMeta.FinishReason
}

func UsageFromMessage(msg *schema.Message) Usage {
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		return Usage{}
	}

	usage := msg.ResponseMeta.Usage
	total := usage.TotalTokens
	if total == 0 {
		total = usage.PromptTokens + usage.CompletionTokens
	}
	return Usage{
		InputTokens:     usage.PromptTokens,
		OutputTokens:    usage.CompletionTokens,
		TotalTokens:     total,
		CachedTokens:    usage.PromptTokenDetails.CachedTokens,
		ReasoningTokens: usage.CompletionTokensDetails.ReasoningTokens,
	}
}

func cloneResponsesRequestContext(ctx *ResponsesRequestContext) *ResponsesRequestContext {
	if ctx == nil {
		return nil
	}
	out := &ResponsesRequestContext{
		PreviousResponseID: ctx.PreviousResponseID,
		User:               ctx.User,
		Truncation:         ctx.Truncation,
	}
	if ctx.Store != nil {
		v := *ctx.Store
		out.Store = &v
	}
	if len(ctx.Text) > 0 {
		out.Text = cloneMap(ctx.Text)
	}
	if len(ctx.Metadata) > 0 {
		out.Metadata = cloneMap(ctx.Metadata)
	}
	if len(ctx.Reasoning) > 0 {
		out.Reasoning = cloneMap(ctx.Reasoning)
	}
	if ctx.ParallelToolCalls != nil {
		v := *ctx.ParallelToolCalls
		out.ParallelToolCalls = &v
	}
	return out
}

func cloneChatExtraFields(src *ChatExtraFields) *ChatExtraFields {
	if src == nil {
		return nil
	}
	if src.ResponseFormat == nil && len(src.Reasoning) == 0 && len(src.Thinking) == 0 && src.ReasoningEffort == "" &&
		src.ToolStream == nil && len(src.StreamOptions) == 0 &&
		src.User == "" && len(src.Metadata) == 0 && src.ParallelToolCalls == nil && src.Store == nil {
		return nil
	}
	out := &ChatExtraFields{
		ResponseFormat:  src.ResponseFormat,
		ReasoningEffort: src.ReasoningEffort,
		User:            src.User,
	}
	if len(src.Reasoning) > 0 {
		out.Reasoning = cloneMap(src.Reasoning)
	}
	if len(src.Thinking) > 0 {
		out.Thinking = cloneMap(src.Thinking)
	}
	if src.ToolStream != nil {
		v := *src.ToolStream
		out.ToolStream = &v
	}
	if len(src.StreamOptions) > 0 {
		out.StreamOptions = cloneMap(src.StreamOptions)
	}
	if len(src.Metadata) > 0 {
		out.Metadata = cloneMap(src.Metadata)
	}
	if src.ParallelToolCalls != nil {
		v := *src.ParallelToolCalls
		out.ParallelToolCalls = &v
	}
	if src.Store != nil {
		v := *src.Store
		out.Store = &v
	}
	return out
}

func cloneMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
