// Package openrouter implements the OpenRouter provider (OpenAI-compatible API).
package openrouter

import (
	"context"

	einoopenrouter "github.com/cloudwego/eino-ext/components/model/openrouter"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/internal/statuserr"
	"github.com/agent-guide/agent-gateway/pkg/httpclient"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/openaibase"
)

func init() {
	provider.RegisterProviderFactory("openrouter", New)
}

type Provider struct {
	*openaibase.Base
}

// New creates a new OpenRouter provider.
// OpenRouter requires HTTP-Referer and X-Title headers; set them via NetworkConfig.ExtraHeaders.
func New(config provider.ProviderConfig) (provider.Provider, error) {
	if config.BaseURL == "" {
		config.BaseURL = "https://openrouter.ai/api/v1"
	}
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
	p.ensureBase()
	return p.Base.DoCreateResponses(ctx, req)
}

func (p *Provider) StreamResponses(ctx context.Context, req *provider.ResponsesRequest) (*schema.StreamReader[*provider.ResponsesStreamEvent], error) {
	p.ensureBase()
	return p.Base.DoStreamResponses(ctx, req)
}

func (p *Provider) newChatModel(ctx context.Context, req *provider.ChatRequest) (einomodel.ToolCallingChatModel, []*schema.Message, []einomodel.Option, error) {
	state, err := provider.ResolveChatRequest(ctx, p.ProviderConfig, req)
	if err != nil {
		return nil, nil, nil, err
	}

	cfg := &einoopenrouter.Config{
		BaseURL:    p.ProviderConfig.BaseURL,
		Model:      state.ModelName,
		HTTPClient: httpclient.BuildHTTPClient(p.ProviderConfig.Network),
	}
	cfg.APIKey = provider.APIKeyFromContextOrConfig(ctx, p.ProviderConfig.APIKey)
	cfg.ExtraFields = provider.ChatCompletionsExtraFieldsFromOptions(provider.OpenRouterChatCompletionsFields, state.Options...)
	if p.CCCompat {
		provider.StripCCUnsupportedChatFields(cfg.ExtraFields)
	}

	chatModel, err := einoopenrouter.NewChatModel(ctx, cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	return chatModel, state.Messages, state.Options, nil
}

func (p *Provider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{
		Streaming: true,
		Tools:     true,
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
	_ provider.ResponsesProvider = (*Provider)(nil)
)
