// Package openai implements the OpenAI provider.
package openai

import (
	"context"
	"strings"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/internal/statuserr"
	"github.com/agent-guide/agent-gateway/pkg/httpclient"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/openaibase"
)

func init() {
	provider.RegisterProviderFactory("openai", New)
}

type Provider struct {
	*openaibase.Base
}

// New creates a new OpenAI provider.
//
// Optional config.Options keys:
//   - "organization": string → sent as OpenAI-Organization header.
//   - "project":      string → sent as OpenAI-Project header.
func New(config provider.ProviderConfig) (provider.Provider, error) {
	if config.BaseURL == "" {
		config.BaseURL = "https://api.openai.com/v1"
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	config.Network.Defaults()
	if _, err := provider.CompactModeFromOptions(config.Options); err != nil {
		return nil, err
	}

	// Inject OpenAI-specific headers via ExtraHeaders so Base.setHeaders picks them up.
	if config.Network.ExtraHeaders == nil {
		config.Network.ExtraHeaders = make(map[string]string)
	}
	if v, ok := config.Options["organization"]; ok {
		if s, ok := v.(string); ok && s != "" {
			config.Network.ExtraHeaders["OpenAI-Organization"] = s
		}
	}
	if v, ok := config.Options["project"]; ok {
		if s, ok := v.(string); ok && s != "" {
			config.Network.ExtraHeaders["OpenAI-Project"] = s
		}
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

	cfg := &einoopenai.ChatModelConfig{
		BaseURL:    p.ProviderConfig.BaseURL,
		Model:      state.ModelName,
		HTTPClient: httpclient.BuildHTTPClient(p.ProviderConfig.Network),
	}
	cfg.APIKey = provider.APIKeyFromContextOrConfig(ctx, p.ProviderConfig.APIKey)

	chatModel, err := einoopenai.NewChatModel(ctx, cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	opts := append([]einomodel.Option(nil), state.Options...)
	extraFields := provider.ChatCompletionsExtraFieldsFromOptions(provider.OpenAIChatCompletionsFields, state.Options...)
	if p.CCCompat {
		provider.StripCCUnsupportedChatFields(extraFields)
	}
	if len(extraFields) > 0 {
		opts = append(opts, einoopenai.WithExtraFields(extraFields))
	}
	return chatModel, state.Messages, opts, nil
}

func (p *Provider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{
		Streaming:       true,
		Tools:           true,
		Vision:          true,
		Embeddings:      true,
		ContextWindow:   128000,
		MaxOutputTokens: 16384,
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

// Interface guards.
var (
	_ provider.Provider          = (*Provider)(nil)
	_ provider.EmbeddingProvider = (*Provider)(nil)
	_ provider.ResponsesProvider = (*Provider)(nil)
)
