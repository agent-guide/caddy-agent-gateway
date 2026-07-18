// Package anthropic implements the Anthropic provider (Claude models).
//
// Chat and streaming delegate to the eino-ext claude component (backed by the
// official anthropic-sdk-go). The provider keeps only gateway-specific logic:
// per-request credential resolution, thinking-budget normalization, request
// metadata, and model discovery.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	einoclaude "github.com/cloudwego/eino-ext/components/model/claude"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

	"github.com/agent-guide/agent-gateway/internal/statuserr"
	"github.com/agent-guide/agent-gateway/pkg/httpclient"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropicbase"
)

const anthropicVersion = "2023-06-01"
const defaultMaxTokens = 4096

func init() {
	provider.RegisterProviderFactory("anthropic", New)
}

type Provider struct {
	provider.ProviderConfig
	client *http.Client
}

// New creates a new Anthropic provider.
func New(config provider.ProviderConfig) (provider.Provider, error) {
	if config.BaseURL == "" {
		config.BaseURL = "https://api.anthropic.com"
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	config.Network.Defaults()

	return &Provider{
		ProviderConfig: config,
		client:         httpclient.BuildHTTPClient(config.Network),
	}, nil
}

func (p *Provider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	return provider.RetryProviderCall(p.ProviderConfig.Network, func() (*provider.ChatResponse, error) {
		chatModel, messages, opts, err := p.newChatModel(ctx, req)
		if err != nil {
			return nil, err
		}
		msg, err := chatModel.Generate(ctx, messages, opts...)
		if err != nil {
			return nil, wrapProviderError(err, "anthropic: request failed")
		}
		return provider.ChatResponseFromEinoMessage(msg), nil
	})
}

func (p *Provider) StreamChat(ctx context.Context, req *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	chatModel, messages, opts, err := p.newChatModel(ctx, req)
	if err != nil {
		return nil, err
	}
	stream, err := chatModel.Stream(ctx, messages, opts...)
	if err != nil {
		return nil, wrapProviderError(err, "anthropic: stream request failed")
	}
	return stream, nil
}

// wrapProviderError converts Anthropic SDK API errors into
// provider.UpstreamError so the upstream HTTP status passes through to
// clients and 4xx errors are not retried; other errors become 502.
func wrapProviderError(err error, context string) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return &provider.UpstreamError{
			Status:     apiErr.StatusCode,
			StatusText: http.StatusText(apiErr.StatusCode),
			Body:       upstreamErrorBody(apiErr),
		}
	}
	return statuserr.Wrap(fmt.Errorf("%s: %w", context, err), http.StatusBadGateway)
}

// upstreamErrorBody extracts the raw upstream JSON body from the SDK error
// message ("<method> <url>: <status> <text> <raw-json>").
func upstreamErrorBody(apiErr *anthropic.Error) string {
	msg := apiErr.Error()
	if i := strings.Index(msg, "{"); i >= 0 {
		return msg[i:]
	}
	return msg
}

func (p *Provider) CreateResponses(ctx context.Context, req *provider.ResponsesRequest) (*provider.ResponsesResponse, error) {
	return provider.CreateResponsesViaChat(ctx, p, req)
}

func (p *Provider) StreamResponses(ctx context.Context, req *provider.ResponsesRequest) (*schema.StreamReader[*provider.ResponsesStreamEvent], error) {
	return provider.StreamResponsesViaChat(ctx, p, req)
}

// newChatModel builds a per-request eino-ext claude chat model. The model is
// rebuilt per call because the API key can be overridden per request through
// credential scheduling.
func (p *Provider) newChatModel(ctx context.Context, req *provider.ChatRequest) (einomodel.ToolCallingChatModel, []*schema.Message, []einomodel.Option, error) {
	state, err := provider.ResolveChatRequest(ctx, p.ProviderConfig, req)
	if err != nil {
		return nil, nil, nil, err
	}

	thinking := requestThinking(state)
	extendedThinking := thinking != nil && thinking.Type == "enabled"

	maxTokens := defaultMaxTokens
	if state.CommonOptions != nil && state.CommonOptions.MaxTokens != nil && *state.CommonOptions.MaxTokens > 0 {
		maxTokens = *state.CommonOptions.MaxTokens
	}
	if extendedThinking {
		maxTokens, thinking.BudgetTokens = anthropicbase.ClampThinkingBudget(maxTokens, thinking.BudgetTokens)
	}

	baseURL := p.ProviderConfig.BaseURL
	cfg := &einoclaude.Config{
		BaseURL:                &baseURL,
		APIKey:                 provider.APIKeyFromContextOrConfig(ctx, p.ProviderConfig.APIKey),
		Model:                  state.ModelName,
		MaxTokens:              maxTokens,
		HTTPClient:             p.client,
		AdditionalHeaderFields: p.ProviderConfig.Network.ExtraHeaders,
	}

	// Extended thinking rejects temperature/top_p/top_k modifications, so
	// sampling options are dropped when thinking is enabled.
	if state.CommonOptions != nil {
		if !extendedThinking {
			cfg.Temperature = state.CommonOptions.Temperature
			cfg.TopP = state.CommonOptions.TopP
		}
		cfg.StopSequences = state.CommonOptions.Stop
	}
	if !extendedThinking {
		if chatOpts := provider.GetChatOptions(state.Options...); chatOpts != nil && chatOpts.TopK > 0 {
			topK := int32(chatOpts.TopK)
			cfg.TopK = &topK
		}
	}

	if thinking != nil {
		cfg.ThinkingConfig = thinkingConfigParam(thinking)
	}

	disableParallel := disableParallelToolUse(state)
	if disableParallel {
		disable := true
		cfg.DisableParallelToolUse = &disable
	}

	if format := anthropicbase.OutputFormatFromState(state); format != nil {
		js := &jsonschema.Schema{}
		if err := json.Unmarshal(format.Schema, js); err != nil {
			return nil, nil, nil, fmt.Errorf("anthropic: parse response format schema: %w", err)
		}
		cfg.ResponseFormat = &einoclaude.ResponseFormat{Schema: js}
	}

	extraRequestFields := map[string]any{}
	if userID := requestUserID(state); userID != "" {
		// The claude component has no request metadata support; inject it
		// through the SDK's JSON request patch (sjson path semantics).
		extraRequestFields["metadata"] = map[string]any{"user_id": userID}
	}

	var opts []einomodel.Option
	var tools []*schema.ToolInfo
	var toolChoice *schema.ToolChoice
	var allowedToolNames []string
	if state.CommonOptions != nil {
		tools = state.CommonOptions.Tools
		toolChoice = state.CommonOptions.ToolChoice
		allowedToolNames = state.CommonOptions.AllowedToolNames
	}
	if len(tools) > 0 {
		opts = append(opts, einomodel.WithTools(tools))
	}
	// Anthropic carries disable_parallel_tool_use inside tool_choice, so an
	// explicit auto choice is required for the flag to reach the wire.
	if toolChoice == nil && disableParallel && len(tools) > 0 {
		allowed := schema.ToolChoiceAllowed
		toolChoice = &allowed
	}
	if toolChoice != nil {
		opts = append(opts, einomodel.WithToolChoice(*toolChoice, allowedToolNames...))
		// The claude component drops disable_parallel_tool_use on the
		// forced-named-tool choice shape; restore it via a JSON request patch.
		if disableParallel && *toolChoice == schema.ToolChoiceForced {
			extraRequestFields["tool_choice.disable_parallel_tool_use"] = true
		}
	}
	if len(extraRequestFields) > 0 {
		cfg.AdditionalRequestFields = extraRequestFields
	}

	chatModel, err := einoclaude.NewChatModel(ctx, cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("anthropic: build chat model: %w", err)
	}
	return chatModel, state.Messages, opts, nil
}

func thinkingConfigParam(tc *anthropicbase.ThinkingConfig) *anthropic.ThinkingConfigParamUnion {
	if tc.Type == "disabled" {
		return &anthropic.ThinkingConfigParamUnion{OfDisabled: &anthropic.ThinkingConfigDisabledParam{}}
	}
	union := anthropic.ThinkingConfigParamOfEnabled(int64(tc.BudgetTokens))
	return &union
}

// ListModels fetches available Claude models from GET /v1/models.
func (p *Provider) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.ProviderConfig.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	p.setHeaders(httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: request failed: %w", err)
	}
	defer resp.Body.Close()

	if err := provider.CheckResponse(resp); err != nil {
		return nil, err
	}

	var modelsResp anthropicbase.ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		return nil, fmt.Errorf("anthropic: decode models: %w", err)
	}

	out := make([]provider.ModelInfo, len(modelsResp.Data))
	for i, m := range modelsResp.Data {
		out[i] = provider.ModelInfo{
			ID:           m.ID,
			Name:         m.DisplayName,
			DisplayName:  m.DisplayName,
			Capabilities: provider.ModelCapabilitiesFromProviderSummary(p.Capabilities()),
		}
	}
	return out, nil
}

func (p *Provider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{
		Streaming:       true,
		Tools:           true,
		Vision:          true,
		ContextWindow:   200000,
		MaxOutputTokens: 8192,
	}
}

func (p *Provider) Config() provider.ProviderConfig {
	return p.ProviderConfig
}

func (p *Provider) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if apiKey := provider.APIKeyFromContextOrConfig(req.Context(), p.ProviderConfig.APIKey); apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	req.Header.Set("anthropic-version", anthropicVersion)
	for k, v := range p.ProviderConfig.Network.ExtraHeaders {
		req.Header.Set(k, v)
	}
}

func requestUserID(state *provider.ChatRequestState) string {
	if state == nil {
		return ""
	}
	if extra := provider.ChatExtraFieldsFromOptions(state.Options...); extra != nil {
		if user := strings.TrimSpace(extra.User); user != "" {
			return user
		}
		if user := metadataUserID(extra.Metadata); user != "" {
			return user
		}
	}
	if ctx := provider.ResponsesRequestContextFromOptions(state.Options...); ctx != nil {
		if user := strings.TrimSpace(ctx.User); user != "" {
			return user
		}
		if user := metadataUserID(ctx.Metadata); user != "" {
			return user
		}
	}
	return ""
}

func metadataUserID(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	userID, _ := metadata["user_id"].(string)
	return strings.TrimSpace(userID)
}

func disableParallelToolUse(state *provider.ChatRequestState) bool {
	if state == nil {
		return false
	}
	if extra := provider.ChatExtraFieldsFromOptions(state.Options...); extra != nil && extra.ParallelToolCalls != nil {
		return !*extra.ParallelToolCalls
	}
	if ctx := provider.ResponsesRequestContextFromOptions(state.Options...); ctx != nil && ctx.ParallelToolCalls != nil {
		return !*ctx.ParallelToolCalls
	}
	return false
}

func requestThinking(state *provider.ChatRequestState) *anthropicbase.ThinkingConfig {
	if state == nil {
		return nil
	}
	if extra := provider.ChatExtraFieldsFromOptions(state.Options...); extra != nil {
		if thinking := thinkingFromReasoning(extra.Reasoning); thinking != nil {
			return thinking
		}
		if thinking := thinkingFromEffort(extra.ReasoningEffort); thinking != nil {
			return thinking
		}
	}
	if ctx := provider.ResponsesRequestContextFromOptions(state.Options...); ctx != nil {
		if thinking := thinkingFromReasoning(ctx.Reasoning); thinking != nil {
			return thinking
		}
	}
	return nil
}

func thinkingFromReasoning(reasoning map[string]any) *anthropicbase.ThinkingConfig {
	if len(reasoning) == 0 {
		return nil
	}
	if typ, _ := reasoning["type"].(string); strings.EqualFold(strings.TrimSpace(typ), "disabled") {
		return &anthropicbase.ThinkingConfig{Type: "disabled"}
	}
	if budget := intFromAny(reasoning["budget_tokens"]); budget > 0 {
		return &anthropicbase.ThinkingConfig{Type: "enabled", BudgetTokens: budget}
	}
	if budget := intFromAny(reasoning["max_tokens"]); budget > 0 {
		return &anthropicbase.ThinkingConfig{Type: "enabled", BudgetTokens: budget}
	}
	if effort, _ := reasoning["effort"].(string); effort != "" {
		return thinkingFromEffort(effort)
	}
	return nil
}

func thinkingFromEffort(effort string) *anthropicbase.ThinkingConfig {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal", "low":
		return &anthropicbase.ThinkingConfig{Type: "enabled", BudgetTokens: 1024}
	case "medium":
		return &anthropicbase.ThinkingConfig{Type: "enabled", BudgetTokens: 4096}
	case "high":
		return &anthropicbase.ThinkingConfig{Type: "enabled", BudgetTokens: 8192}
	default:
		return nil
	}
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

var (
	_ provider.Provider          = (*Provider)(nil)
	_ provider.ResponsesProvider = (*Provider)(nil)
)
