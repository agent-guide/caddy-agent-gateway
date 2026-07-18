// Package qwen implements the Alibaba Qwen provider (DashScope
// OpenAI-compatible mode) on top of the eino-ext qwen component.
package qwen

import (
	"context"
	"strconv"
	"strings"

	einoqwen "github.com/cloudwego/eino-ext/components/model/qwen"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/internal/statuserr"
	"github.com/agent-guide/agent-gateway/pkg/httpclient"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/openaibase"
)

func init() {
	provider.RegisterProviderFactory("qwen", New)
}

type Provider struct {
	*openaibase.Base
}

// New creates a new Qwen provider using DashScope's OpenAI-compatible API.
//
// Optional config.Options keys:
//   - "enable_thinking": bool or "true"/"false" → sets the request
//     `enable_thinking` field (plus `chat_template_kwargs` for Bailian).
//     When omitted the field is not sent and the model default applies.
//     Per-request reasoning fields override this option.
func New(config provider.ProviderConfig) (provider.Provider, error) {
	if config.BaseURL == "" {
		config.BaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	config.Network.Defaults()
	if _, err := provider.CompactModeFromOptions(config.Options); err != nil {
		return nil, err
	}

	return &Provider{Base: openaibase.NewBase(config)}, nil
}

func (p *Provider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	p.ensureBase()
	return provider.RetryProviderCall(p.ProviderConfig.Network, func() (*provider.ChatResponse, error) {
		chatModel, messages, opts, err := p.newChatModel(ctx, req)
		if err != nil {
			return nil, err
		}
		msg, err := chatModel.Generate(ctx, messages, opts...)
		if err != nil {
			return nil, statuserr.Wrap(openaibase.NormalizeError(err), 502)
		}
		return provider.ChatResponseFromEinoMessage(msg), nil
	})
}

func (p *Provider) StreamChat(ctx context.Context, req *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	p.ensureBase()
	chatModel, messages, opts, err := p.newChatModel(ctx, req)
	if err != nil {
		return nil, err
	}
	stream, err := chatModel.Stream(ctx, messages, opts...)
	if err != nil {
		return nil, statuserr.Wrap(openaibase.NormalizeError(err), 502)
	}
	return stream, nil
}

func (p *Provider) CreateResponses(ctx context.Context, req *provider.ResponsesRequest) (*provider.ResponsesResponse, error) {
	return provider.CreateResponsesViaChat(ctx, p, req)
}

func (p *Provider) StreamResponses(ctx context.Context, req *provider.ResponsesRequest) (*schema.StreamReader[*provider.ResponsesStreamEvent], error) {
	return provider.StreamResponsesViaChat(ctx, p, req)
}

func (p *Provider) newChatModel(ctx context.Context, req *provider.ChatRequest) (einomodel.ToolCallingChatModel, []*schema.Message, []einomodel.Option, error) {
	state, err := provider.ResolveChatRequest(ctx, p.ProviderConfig, req)
	if err != nil {
		return nil, nil, nil, err
	}

	cfg := &einoqwen.ChatModelConfig{
		BaseURL:    p.ProviderConfig.BaseURL,
		Model:      state.ModelName,
		HTTPClient: httpclient.BuildHTTPClient(p.ProviderConfig.Network),
	}
	cfg.APIKey = provider.APIKeyFromContextOrConfig(ctx, p.ProviderConfig.APIKey)

	chatModel, err := einoqwen.NewChatModel(ctx, cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	opts := append([]einomodel.Option(nil), state.Options...)
	extraFields := provider.ChatCompletionsExtraFieldsFromOptions(provider.ReasoningEffortField, state.Options...)
	if p.CCCompat {
		provider.StripCCUnsupportedChatFields(extraFields)
	}
	if enable := p.enableThinking(state); enable != nil {
		// Merged into the single extra-fields map rather than passed through
		// einoqwen.WithEnableThinking: the component's option appends its own
		// acl WithExtraFields, which replaces (not merges) any extra fields
		// set here and would drop them.
		extraFields = provider.MergeExtraFields(extraFields, map[string]any{
			"enable_thinking":      *enable,
			"chat_template_kwargs": map[string]bool{"enable_thinking": *enable},
		})
	}
	if len(extraFields) > 0 {
		opts = append(opts, einoqwen.WithExtraFields(extraFields))
	}

	return chatModel, state.Messages, opts, nil
}

// enableThinking resolves the request `enable_thinking` value. Per-request
// reasoning fields win over the provider-level "enable_thinking" option; nil
// means the field is omitted and the model default applies.
func (p *Provider) enableThinking(state *provider.ChatRequestState) *bool {
	if v := requestEnableThinking(state); v != nil {
		return v
	}
	return configEnableThinking(p.ProviderConfig.Options)
}

func requestEnableThinking(state *provider.ChatRequestState) *bool {
	if state == nil {
		return nil
	}
	if extra := provider.ChatExtraFieldsFromOptions(state.Options...); extra != nil {
		if v := enableFromReasoning(extra.Reasoning); v != nil {
			return v
		}
		if strings.TrimSpace(extra.ReasoningEffort) != "" {
			return boolPtr(true)
		}
	}
	if ctx := provider.ResponsesRequestContextFromOptions(state.Options...); ctx != nil {
		if v := enableFromReasoning(ctx.Reasoning); v != nil {
			return v
		}
	}
	return nil
}

func enableFromReasoning(reasoning map[string]any) *bool {
	if len(reasoning) == 0 {
		return nil
	}
	if typ, _ := reasoning["type"].(string); typ != "" {
		return boolPtr(!strings.EqualFold(strings.TrimSpace(typ), "disabled"))
	}
	if effort, _ := reasoning["effort"].(string); strings.TrimSpace(effort) != "" {
		return boolPtr(true)
	}
	for _, key := range []string{"budget_tokens", "max_tokens"} {
		switch n := reasoning[key].(type) {
		case int:
			if n > 0 {
				return boolPtr(true)
			}
		case int64:
			if n > 0 {
				return boolPtr(true)
			}
		case float64:
			if n > 0 {
				return boolPtr(true)
			}
		}
	}
	return nil
}

func configEnableThinking(opts map[string]any) *bool {
	v, ok := opts["enable_thinking"]
	if !ok {
		return nil
	}
	switch b := v.(type) {
	case bool:
		return boolPtr(b)
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(b))
		if err != nil {
			return nil
		}
		return boolPtr(parsed)
	default:
		return nil
	}
}

func boolPtr(v bool) *bool {
	return &v
}

func (p *Provider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{
		Streaming:       true,
		Tools:           true,
		Vision:          true,
		Embeddings:      true,
		ContextWindow:   131072,
		MaxOutputTokens: 8192,
	}
}

func (p *Provider) Config() provider.ProviderConfig {
	p.ensureBase()
	return p.ProviderConfig
}

func (p *Provider) ensureBase() {
	if p.Base == nil {
		p.Base = openaibase.NewBase(provider.ProviderConfig{})
	}
}

var (
	_ provider.Provider          = (*Provider)(nil)
	_ provider.EmbeddingProvider = (*Provider)(nil)
	_ provider.ResponsesProvider = (*Provider)(nil)
)
