// Package zhipu implements the Zhipu BigModel provider.
package zhipu

import (
	"context"
	"fmt"
	"strings"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/agent-guide/agent-gateway/internal/statuserr"
	"github.com/agent-guide/agent-gateway/pkg/httpclient"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/openaibase"
)

func init() {
	provider.RegisterProviderFactory("zhipu", New)
}

type Provider struct {
	*openaibase.Base
	apiProfile   apiProfile
	capabilities provider.ProviderCapabilities
}

type apiProfile string

const (
	apiProfileStandard   apiProfile = "standard"
	apiProfileCodingPlan apiProfile = "coding_plan"
)

// New creates a new Zhipu provider using BigModel's OpenAI-compatible API.
func New(config provider.ProviderConfig) (provider.Provider, error) {
	if config.BaseURL == "" {
		config.BaseURL = "https://open.bigmodel.cn/api/paas/v4"
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	config.Network.Defaults()
	if _, err := provider.CompactModeFromOptions(config.Options); err != nil {
		return nil, err
	}
	profile, err := apiProfileFromOptions(config.Options, config.BaseURL)
	if err != nil {
		return nil, err
	}
	capabilities, err := capabilitiesFromOptions(config.Options, profile)
	if err != nil {
		return nil, err
	}

	return &Provider{
		Base:         openaibase.NewBase(config),
		apiProfile:   profile,
		capabilities: capabilities,
	}, nil
}

func (p *Provider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	p.ensureBase()
	zap.L().Info("zhipu: chat request",
		zap.String("model", req.Model),
		zap.Int("message_count", len(req.Messages)),
	)
	resp, err := provider.RetryProviderCall(p.ProviderConfig.Network, func() (*provider.ChatResponse, error) {
		chatModel, messages, opts, err := p.newChatModel(ctx, req, false)
		if err != nil {
			return nil, err
		}
		zap.L().Info("zhipu: calling upstream generate",
			zap.String("model", req.Model),
			zap.String("base_url", p.ProviderConfig.BaseURL),
		)
		msg, err := chatModel.Generate(ctx, messages, opts...)
		if err != nil {
			return nil, statuserr.Wrap(openaibase.NormalizeError(err), 502)
		}
		return provider.ChatResponseFromEinoMessage(msg), nil
	})
	if err != nil {
		zap.L().Info("zhipu: chat failed", zap.String("model", req.Model), zap.Error(err))
		return nil, err
	}
	contentLen := 0
	finishReason := ""
	if resp != nil && resp.Message != nil {
		contentLen = len(resp.Message.Content)
		finishReason = provider.FinishReason(resp.Message)
	}
	zap.L().Info("zhipu: chat response received",
		zap.String("model", req.Model),
		zap.Int("content_length", contentLen),
		zap.String("finish_reason", finishReason),
	)
	return resp, nil
}

func (p *Provider) StreamChat(ctx context.Context, req *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	p.ensureBase()
	zap.L().Info("zhipu: stream request",
		zap.String("model", req.Model),
		zap.Int("message_count", len(req.Messages)),
		zap.String("base_url", p.ProviderConfig.BaseURL),
	)
	chatModel, messages, opts, err := p.newChatModel(ctx, req, true)
	if err != nil {
		return nil, err
	}
	stream, err := chatModel.Stream(ctx, messages, opts...)
	if err != nil {
		zap.L().Info("zhipu: stream failed", zap.String("model", req.Model), zap.Error(err))
		return nil, statuserr.Wrap(openaibase.NormalizeError(err), 502)
	}
	zap.L().Info("zhipu: stream started", zap.String("model", req.Model))
	return stream, nil
}

func (p *Provider) CreateResponses(ctx context.Context, req *provider.ResponsesRequest) (*provider.ResponsesResponse, error) {
	return provider.CreateResponsesViaChat(ctx, p, req)
}

func (p *Provider) StreamResponses(ctx context.Context, req *provider.ResponsesRequest) (*schema.StreamReader[*provider.ResponsesStreamEvent], error) {
	return provider.StreamResponsesViaChat(ctx, p, req)
}

func (p *Provider) newChatModel(ctx context.Context, req *provider.ChatRequest, stream bool) (einomodel.ToolCallingChatModel, []*schema.Message, []einomodel.Option, error) {
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
	extraFields := provider.ChatCompletionsExtraFieldsFromOptions(provider.ZhipuChatCompletionsFields, state.Options...)
	normalizeZhipuExtraFields(extraFields, p.apiProfile == apiProfileCodingPlan)
	if p.CCCompat {
		provider.StripCCUnsupportedChatFields(extraFields)
	}
	if _, supplied := extraFields["thinking"]; !supplied {
		thinkingType := requestThinkingType(state.Options)
		if thinkingType == "" {
			thinkingType = p.thinkingType()
		}
		if thinkingType != "" {
			extraFields = provider.MergeExtraFields(extraFields, map[string]any{
				"thinking": map[string]any{
					"type": thinkingType,
				},
			})
		}
	}
	if stream && state.CommonOptions != nil && len(state.CommonOptions.Tools) > 0 {
		if _, supplied := extraFields["tool_stream"]; !supplied {
			extraFields = provider.MergeExtraFields(extraFields, map[string]any{"tool_stream": true})
		}
	}
	if len(extraFields) > 0 {
		opts = append(opts, einoopenai.WithExtraFields(extraFields))
	}

	return chatModel, state.Messages, opts, nil
}

func (p *Provider) Capabilities() provider.ProviderCapabilities {
	if p.capabilities != (provider.ProviderCapabilities{}) {
		return p.capabilities
	}
	return defaultCapabilities(apiProfileStandard)
}

func defaultCapabilities(profile apiProfile) provider.ProviderCapabilities {
	capabilities := provider.ProviderCapabilities{
		Streaming:       true,
		Tools:           true,
		Vision:          true,
		Embeddings:      true,
		ContextWindow:   128000,
		MaxOutputTokens: 8192,
	}
	if profile == apiProfileCodingPlan {
		// The Coding Plan chat endpoint currently exposes text coding models;
		// vision is provided through a separate MCP service and embeddings are
		// not part of the Coding Plan Chat Completions surface.
		capabilities.Vision = false
		capabilities.Embeddings = false
		capabilities.ContextWindow = 1000000
		capabilities.MaxOutputTokens = 128000
	}
	return capabilities
}

func apiProfileFromOptions(options map[string]any, baseURL string) (apiProfile, error) {
	raw, ok := options["api_profile"]
	if !ok {
		return inferredAPIProfile(baseURL), nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("zhipu: option api_profile must be a string")
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		return inferredAPIProfile(baseURL), nil
	case string(apiProfileStandard):
		return apiProfileStandard, nil
	case string(apiProfileCodingPlan):
		return apiProfileCodingPlan, nil
	default:
		return "", fmt.Errorf("zhipu: option api_profile must be one of auto, standard, coding_plan")
	}
}

func inferredAPIProfile(baseURL string) apiProfile {
	if strings.Contains(baseURL, "/api/coding/") {
		return apiProfileCodingPlan
	}
	return apiProfileStandard
}

func capabilitiesFromOptions(options map[string]any, profile apiProfile) (provider.ProviderCapabilities, error) {
	capabilities, err := provider.CapabilitiesFromOptions(options, defaultCapabilities(profile),
		provider.CapabilityContextWindow,
		provider.CapabilityMaxOutputTokens,
		provider.CapabilityVision,
		provider.CapabilityEmbeddings,
	)
	if err != nil {
		return provider.ProviderCapabilities{}, fmt.Errorf("zhipu: %w", err)
	}
	return capabilities, nil
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

func (p *Provider) thinkingType() string {
	v, ok := p.ProviderConfig.Options["thinking_type"]
	if !ok {
		return ""
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

func requestThinkingType(opts []einomodel.Option) string {
	extra := provider.ChatExtraFieldsFromOptions(opts...)
	if extra != nil {
		if typ, _ := extra.Thinking["type"].(string); strings.TrimSpace(typ) != "" {
			return normalizeThinkingType(typ)
		}
		if typ, _ := extra.Reasoning["type"].(string); strings.TrimSpace(typ) != "" {
			return normalizeThinkingType(typ)
		}
		if effort := reasoningEffort(extra); effort == "none" {
			return "disabled"
		}
		if extra.ReasoningEffort != "" || len(extra.Reasoning) > 0 {
			return "enabled"
		}
	}
	if ctx := provider.ResponsesRequestContextFromOptions(opts...); ctx != nil && len(ctx.Reasoning) > 0 {
		return "enabled"
	}
	return ""
}

func normalizeThinkingType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "adaptive":
		return "enabled"
	case "none":
		return "disabled"
	case "enabled", "disabled":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func reasoningEffort(extra *provider.ChatExtraFields) string {
	if extra == nil {
		return ""
	}
	if effort := strings.ToLower(strings.TrimSpace(extra.ReasoningEffort)); effort != "" {
		return effort
	}
	effort, _ := extra.Reasoning["effort"].(string)
	return strings.ToLower(strings.TrimSpace(effort))
}

func normalizeZhipuExtraFields(fields map[string]any, codingPlan bool) {
	if thinking, ok := fields["thinking"].(map[string]any); ok {
		typ, _ := thinking["type"].(string)
		if normalized := normalizeThinkingType(typ); normalized != "" {
			// GLM's chat-completions dialect accepts only thinking.type.
			// Rebuild the object so Anthropic-only controls such as
			// budget_tokens and display never cross the wire boundary.
			fields["thinking"] = map[string]any{"type": normalized}
		} else {
			delete(fields, "thinking")
		}
	}
	if !codingPlan {
		return
	}
	effort, _ := fields["reasoning_effort"].(string)
	// Coding Plan currently accepts only high and max. Collapse OpenAI/Codex's
	// finer effort levels to the nearest supported GLM value while preserving
	// the extra-compute intent of xhigh/max.
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none":
		delete(fields, "reasoning_effort")
		if _, supplied := fields["thinking"]; !supplied {
			fields["thinking"] = map[string]any{"type": "disabled"}
		}
	case "minimal", "low", "medium", "high":
		fields["reasoning_effort"] = "high"
	case "xhigh", "max":
		fields["reasoning_effort"] = "max"
	}
}

var (
	_ provider.Provider          = (*Provider)(nil)
	_ provider.EmbeddingProvider = (*Provider)(nil)
	_ provider.ResponsesProvider = (*Provider)(nil)
)
