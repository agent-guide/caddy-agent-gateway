// Package deepseek implements the DeepSeek provider.
package deepseek

import (
	"context"
	"strconv"
	"strings"

	einodeepseek "github.com/cloudwego/eino-ext/components/model/deepseek"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/internal/statuserr"
	"github.com/agent-guide/agent-gateway/pkg/httpclient"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/openaibase"
)

func init() {
	provider.RegisterProviderFactory("deepseek", New)
}

type Provider struct {
	*openaibase.Base
}

// New creates a new DeepSeek provider using the eino-ext DeepSeek component.
func New(config provider.ProviderConfig) (provider.Provider, error) {
	if config.BaseURL == "" {
		config.BaseURL = "https://api.deepseek.com"
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

	cfg := &einodeepseek.ChatModelConfig{
		BaseURL:    p.ProviderConfig.BaseURL,
		Model:      state.ModelName,
		Timeout:    p.ProviderConfig.Network.RequestTimeout(),
		HTTPClient: httpclient.BuildHTTPClient(p.ProviderConfig.Network),
	}
	cfg.APIKey = provider.APIKeyFromContextOrConfig(ctx, p.ProviderConfig.APIKey)
	applyOptions(cfg, p.ProviderConfig.Options)
	requestExtra := provider.ChatCompletionsExtraFieldsFromOptions(provider.ReasoningEffortField, state.Options...)
	if p.CCCompat {
		provider.StripCCUnsupportedChatFields(requestExtra)
	}

	chatModel, err := einodeepseek.NewChatModel(ctx, cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	opts := append([]einomodel.Option(nil), state.Options...)
	extraFields := requestExtra
	if thinkingType := p.thinkingType(); thinkingType != "" {
		extraFields = provider.MergeExtraFields(extraFields, map[string]any{
			"thinking": map[string]any{
				"type": thinkingType,
			},
		})
	}
	if len(extraFields) > 0 {
		opts = append(opts, einodeepseek.WithExtraFields(extraFields))
	}
	return chatModel, normalizeMessagesForDeepSeek(state.Messages), opts, nil
}

func normalizeMessagesForDeepSeek(messages []*schema.Message) []*schema.Message {
	if len(messages) == 0 {
		return messages
	}
	normalized := make([]*schema.Message, 0, len(messages))
	for _, msg := range messages {
		if msg == nil {
			normalized = append(normalized, msg)
			continue
		}
		if msg.Role != schema.RoleType("developer") {
			normalized = append(normalized, msg)
			continue
		}
		clone := *msg
		clone.Role = schema.System
		normalized = append(normalized, &clone)
	}
	return normalized
}

// thinkingType resolves the deepseek `thinking.type` request field.
//
// DeepSeek v4 models run in thinking mode by default and then require the
// `reasoning_content` of every tool-calling assistant turn to be replayed on
// the next request. The cc/anthropic protocol does not round-trip reasoning
// content, so the default is "disabled" to keep Claude Code CLI tool loops
// working. Set `option thinking_type none` to omit the field entirely.
func (p *Provider) thinkingType() string {
	v, ok := p.ProviderConfig.Options["thinking_type"]
	if !ok {
		return "disabled"
	}
	s, ok := v.(string)
	if !ok {
		return "disabled"
	}
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return "disabled"
	}
	if s == "none" {
		return ""
	}
	return s
}

func (p *Provider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{
		Streaming:       true,
		Tools:           true,
		ContextWindow:   128000,
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

func applyOptions(cfg *einodeepseek.ChatModelConfig, opts map[string]any) {
	if len(opts) == 0 {
		return
	}
	if v := stringOption(opts, "path"); v != "" {
		cfg.Path = v
	}
	if v := stringOption(opts, "response_format_type"); v != "" {
		cfg.ResponseFormatType = deepSeekResponseFormat(v)
	}
	if v, ok := intOption(opts, "max_tokens"); ok {
		cfg.MaxTokens = v
	}
	if v, ok := float32Option(opts, "temperature"); ok {
		cfg.Temperature = v
	}
	if v, ok := float32Option(opts, "top_p"); ok {
		cfg.TopP = v
	}
	if v, ok := float32Option(opts, "presence_penalty"); ok {
		cfg.PresencePenalty = v
	}
	if v, ok := float32Option(opts, "frequency_penalty"); ok {
		cfg.FrequencyPenalty = v
	}
	if v, ok := boolOption(opts, "log_probs"); ok {
		cfg.LogProbs = v
	}
	if v, ok := intOption(opts, "top_log_probs"); ok {
		cfg.TopLogProbs = v
	}
}

func deepSeekResponseFormat(formatType string) einodeepseek.ResponseFormatType {
	switch strings.TrimSpace(formatType) {
	case string(einodeepseek.ResponseFormatTypeText):
		return einodeepseek.ResponseFormatTypeText
	case string(einodeepseek.ResponseFormatTypeJSONObject):
		return einodeepseek.ResponseFormatTypeJSONObject
	default:
		return ""
	}
}

func stringOption(opts map[string]any, key string) string {
	v, ok := opts[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

func intOption(opts map[string]any, key string) (int, bool) {
	switch v := opts[key].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(v))
		return i, err == nil
	default:
		return 0, false
	}
}

func float32Option(opts map[string]any, key string) (float32, bool) {
	switch v := opts[key].(type) {
	case float32:
		return v, true
	case float64:
		return float32(v), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 32)
		return float32(f), err == nil
	default:
		return 0, false
	}
}

func boolOption(opts map[string]any, key string) (bool, bool) {
	switch v := opts[key].(type) {
	case bool:
		return v, true
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		return b, err == nil
	default:
		return false, false
	}
}

var (
	_ provider.Provider          = (*Provider)(nil)
	_ provider.ResponsesProvider = (*Provider)(nil)
)
