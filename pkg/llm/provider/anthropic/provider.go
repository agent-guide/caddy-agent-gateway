// Package anthropic implements the Anthropic provider (Claude models).
//
// Chat and streaming delegate to the eino-ext claude component (backed by the
// official anthropic-sdk-go). The provider keeps only gateway-specific logic:
// per-request credential resolution, thinking-budget normalization, request
// metadata, and model discovery.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// eino-ext/claude exposes reading thinking text but keeps the storage keys and
// signature accessor private. Keep these component-specific keys inside this
// adapter; the rest of the gateway uses provider.ReasoningParts. Provider tests
// pin this dependency contract so an eino upgrade cannot fail silently.
const (
	einoClaudeThinkingKey          = "_eino_claude_thinking"
	einoClaudeThinkingSignatureKey = "_eino_claude_thinking_signature"
	einoClaudeBreakpointKey        = "_eino_claude_breakpoint"
	einoClaudeBreakpointTTLKey     = "_eino_claude_breakpoint_ttl"
	einoClaudeToolSearchEventsKey  = "_eino_claude_tool_search_events"
)

func init() {
	provider.RegisterProviderFactory("anthropic", New)
	provider.RegisterProviderTypeCapabilities("anthropic", provider.ProviderTypeCapabilities{
		Dialect: provider.ProtocolDialectAnthropic,
		ProtocolFeatures: map[provider.ProtocolFeature]struct{}{
			provider.FeatureAnthropicReasoningReplay: {},
			provider.FeatureAnthropicBodyRelay:       {},
		},
	})
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
		capture := &messagesResponseCapture{}
		chatModel, messages, opts, err := p.newChatModel(ctx, req, capture)
		if err != nil {
			return nil, err
		}
		msg, err := chatModel.Generate(ctx, messages, opts...)
		if err != nil {
			return nil, wrapProviderError(err, "anthropic: request failed")
		}
		if capture.response != nil {
			attachCapturedReasoning(msg, capture.response)
		} else {
			attachEinoClaudeReasoning(msg)
		}
		anthropicbase.AttachAnthropicResponseBody(msg, capture.body)
		return provider.ChatResponseFromEinoMessage(msg), nil
	})
}

func (p *Provider) StreamChat(ctx context.Context, req *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	chatModel, messages, opts, err := p.newChatModel(ctx, req, nil)
	if err != nil {
		return nil, err
	}
	stream, err := chatModel.Stream(ctx, messages, opts...)
	if err != nil {
		return nil, wrapProviderError(err, "anthropic: stream request failed")
	}
	reasoningState := &einoReasoningStreamState{}
	return schema.StreamReaderWithConvert(stream, func(msg *schema.Message) (*schema.Message, error) {
		reasoningState.attach(msg)
		return msg, nil
	}), nil
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
func (p *Provider) newChatModel(ctx context.Context, req *provider.ChatRequest, capture *messagesResponseCapture) (einomodel.ToolCallingChatModel, []*schema.Message, []einomodel.Option, error) {
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
	httpClient := p.client
	if capture != nil {
		cloned := *p.client
		transport := cloned.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		cloned.Transport = captureRoundTripper{base: transport, capture: capture}
		httpClient = &cloned
	}
	cfg := &einoclaude.Config{
		BaseURL:                &baseURL,
		APIKey:                 provider.APIKeyFromContextOrConfig(ctx, p.ProviderConfig.APIKey),
		Model:                  state.ModelName,
		MaxTokens:              maxTokens,
		HTTPClient:             httpClient,
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
	nativeAnthropicTools := false
	if nativeTools, nativeChoice := anthropicbase.AnthropicRequestTools(state.ProtocolState); len(nativeTools) > 0 {
		toolValues := make([]any, 0, len(nativeTools))
		for i, raw := range nativeTools {
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, nil, nil, fmt.Errorf("anthropic: parse native tool %d: %w", i, err)
			}
			toolValues = append(toolValues, value)
		}
		extraRequestFields["tools"] = toolValues
		nativeAnthropicTools = true
		if len(nativeChoice) > 0 {
			var toolChoiceValue any
			if err := json.Unmarshal(nativeChoice, &toolChoiceValue); err != nil {
				return nil, nil, nil, fmt.Errorf("anthropic: parse native tool_choice: %w", err)
			}
			extraRequestFields["tool_choice"] = toolChoiceValue
		}
	}
	modelMessages, exactMessages, err := adaptSignedReasoningMessages(state.Messages)
	if err != nil {
		return nil, nil, nil, err
	}
	if exactMessages != nil {
		// eino-ext/claude's AssistantGenMultiContent branch only accepts
		// multimodal text/images and cannot represent redacted or multiple
		// thinking blocks. Patch the validated SDK body only for those shapes;
		// ordinary single signed thinking stays on eino's native path.
		extraRequestFields["messages"] = exactMessages
	}
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
	if toolChoice == nil && disableParallel && len(tools) > 0 && !nativeAnthropicTools {
		allowed := schema.ToolChoiceAllowed
		toolChoice = &allowed
	}
	if toolChoice != nil && !nativeAnthropicTools {
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
	return chatModel, modelMessages, opts, nil
}

// messagesResponseCapture preserves the raw content block sequence before the
// eino adapter flattens non-streaming responses into a single thinking value.
// The regular eino message remains authoritative for text, tools, usage, and
// provider-specific blocks; only structured reasoning is restored from here.
type messagesResponseCapture struct {
	response *anthropicbase.MessagesResponse
	body     json.RawMessage
}

type captureRoundTripper struct {
	base    http.RoundTripper
	capture *messagesResponseCapture
}

func (t captureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if readErr != nil {
		return resp, nil
	}
	var captured anthropicbase.MessagesResponse
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && json.Unmarshal(body, &captured) == nil {
		t.capture.response = &captured
		t.capture.body = append(json.RawMessage(nil), body...)
	}
	return resp, nil
}

func attachCapturedReasoning(msg *schema.Message, captured *anthropicbase.MessagesResponse) {
	if msg == nil || captured == nil {
		return
	}
	capturedMessage := captured.ToChatResponse().Message
	if capturedMessage == nil {
		return
	}
	msg.ReasoningContent = capturedMessage.ReasoningContent
	provider.AttachReasoningParts(msg, provider.ReasoningPartsFromMessage(capturedMessage)...)
}

func adaptSignedReasoningMessages(messages []*schema.Message) ([]*schema.Message, []anthropicbase.MessageItem, error) {
	adapted := messages
	clonedSlice := false
	requiresExactMessages := false

	for i, msg := range messages {
		parts := provider.ReasoningPartsFromMessage(msg)
		if len(parts) == 0 {
			continue
		}

		var authentic []schema.MessageOutputPart
		for _, part := range parts {
			switch part.Type {
			case schema.ChatMessagePartTypeReasoning:
				if part.Reasoning != nil && part.Reasoning.Signature != "" &&
					!provider.IsGatewayThinkingSignature(part.Reasoning.Signature) {
					authentic = append(authentic, part)
					// eino-ext's native message converter requires non-empty
					// thinking text. Anthropic display=omitted deliberately
					// returns an empty thinking field with a real signature, so
					// that shape must use the exact wire converter.
					if part.Reasoning.Text == "" {
						requiresExactMessages = true
					}
				}
			case provider.ChatMessagePartTypeEncryptedReasoning:
				if provider.EncryptedReasoningData(part) != "" {
					requiresExactMessages = true
				}
			}
		}
		if len(authentic) > 1 {
			requiresExactMessages = true
			continue
		}
		if len(authentic) != 1 || authentic[0].Reasoning.Text == "" || msg == nil {
			continue
		}

		if !clonedSlice {
			adapted = append([]*schema.Message(nil), messages...)
			clonedSlice = true
		}
		cloned := *msg
		cloned.Extra = cloneMessageExtra(msg.Extra)
		cloned.Extra[einoClaudeThinkingKey] = authentic[0].Reasoning.Text
		cloned.Extra[einoClaudeThinkingSignatureKey] = authentic[0].Reasoning.Signature
		adapted[i] = &cloned
	}

	if !requiresExactMessages {
		return adapted, nil, nil
	}
	if err := validateExactReplayMessages(messages); err != nil {
		return nil, nil, err
	}
	wireRequest := &anthropicbase.MessagesRequest{}
	return messages, anthropicbase.ConvertMessages(messages, wireRequest, false, einoMessageCacheControl), nil
}

// validateExactReplayMessages prevents the signed/redacted replay patch from
// silently replacing richer eino message shapes with the smaller set supported
// by anthropicbase.ConvertMessages. Losing a document, rich tool result, media
// part, citation metadata, or server-tool event is worse than rejecting the
// request before it reaches the upstream.
func validateExactReplayMessages(messages []*schema.Message) error {
	for i, msg := range messages {
		if msg == nil {
			continue
		}
		if len(msg.MultiContent) > 0 {
			return fmt.Errorf("anthropic: exact thinking replay cannot preserve deprecated multi_content in message %d", i)
		}
		if _, ok := msg.Extra[einoClaudeToolSearchEventsKey]; ok {
			return fmt.Errorf("anthropic: exact thinking replay cannot preserve server-tool events in message %d", i)
		}

		switch msg.Role {
		case schema.Assistant:
			for _, part := range msg.AssistantGenMultiContent {
				switch part.Type {
				case schema.ChatMessagePartTypeText:
					if len(part.Extra) > 0 {
						return fmt.Errorf("anthropic: exact thinking replay cannot preserve assistant text metadata in message %d", i)
					}
				case schema.ChatMessagePartTypeImageURL:
					if part.Image == nil || len(part.Extra) > 0 || len(part.Image.Extra) > 0 {
						return fmt.Errorf("anthropic: exact thinking replay cannot preserve assistant image metadata in message %d", i)
					}
				case schema.ChatMessagePartTypeReasoning, provider.ChatMessagePartTypeEncryptedReasoning, provider.ChatMessagePartTypeReasoningEnd:
					// Gateway reasoning is replayed from provider.ReasoningParts;
					// tolerate duplicated eino parts but do not treat them as content.
				default:
					return fmt.Errorf("anthropic: exact thinking replay cannot preserve assistant part type %q in message %d", part.Type, i)
				}
			}
		case schema.Tool:
			if len(msg.UserInputMultiContent) > 0 {
				return fmt.Errorf("anthropic: exact thinking replay cannot preserve structured tool result in message %d", i)
			}
		default:
			for _, part := range msg.UserInputMultiContent {
				switch part.Type {
				case schema.ChatMessagePartTypeText:
					if len(part.Extra) > 0 {
						return fmt.Errorf("anthropic: exact thinking replay cannot preserve user text metadata in message %d", i)
					}
				case schema.ChatMessagePartTypeImageURL:
					if part.Image == nil || len(part.Extra) > 0 || len(part.Image.Extra) > 0 {
						return fmt.Errorf("anthropic: exact thinking replay cannot preserve user image metadata in message %d", i)
					}
				default:
					return fmt.Errorf("anthropic: exact thinking replay cannot preserve user part type %q in message %d", part.Type, i)
				}
			}
		}
	}
	return nil
}

func einoMessageCacheControl(msg *schema.Message) *anthropicbase.CacheControl {
	if msg == nil || len(msg.Extra) == 0 {
		return nil
	}
	breakpoint, _ := msg.Extra[einoClaudeBreakpointKey].(bool)
	if !breakpoint {
		return nil
	}
	ttl, _ := msg.Extra[einoClaudeBreakpointTTLKey].(string)
	return &anthropicbase.CacheControl{Type: "ephemeral", TTL: strings.TrimSpace(ttl)}
}

func cloneMessageExtra(extra map[string]any) map[string]any {
	cloned := make(map[string]any, len(extra)+2)
	for key, value := range extra {
		cloned[key] = value
	}
	return cloned
}

func attachEinoClaudeReasoning(msg *schema.Message) {
	if msg == nil || len(msg.Extra) == 0 {
		return
	}
	thinking, _ := einoclaude.GetThinking(msg)
	signature, _ := msg.Extra[einoClaudeThinkingSignatureKey].(string)
	if thinking == "" && signature == "" {
		return
	}
	provider.AttachReasoningParts(msg, provider.NewReasoningOutputPart(thinking, signature, nil))
}

// einoReasoningStreamState restores stable block indexes that eino-ext does
// not expose on its schema.Message chunks. Text and signature deltas for one
// block must share an index so schema.ConcatMessages can reconstruct the exact
// signed thinking block for a later tool turn.
type einoReasoningStreamState struct {
	nextIndex        int
	currentIndex     int
	hasCurrent       bool
	signatureStarted bool
}

func (s *einoReasoningStreamState) attach(msg *schema.Message) {
	if msg == nil {
		return
	}
	thinking, hasThinking := einoclaude.GetThinking(msg)
	signature, hasSignature := msg.Extra[einoClaudeThinkingSignatureKey].(string)
	// eino represents a thinking content_block_start by setting both private
	// fields to empty strings. Their presence is the only block-boundary signal
	// it retains, and is especially important when display=omitted produces no
	// thinking deltas before the signature.
	if hasThinking && hasSignature {
		s.currentIndex = s.nextIndex
		s.nextIndex++
		s.hasCurrent = true
		s.signatureStarted = false
		index := s.currentIndex
		provider.AttachReasoningParts(msg, provider.NewReasoningOutputPart(thinking, signature, &index))
		return
	}
	if thinking == "" && signature == "" {
		if msg.Content != "" || len(msg.ToolCalls) > 0 {
			s.hasCurrent = false
			s.signatureStarted = false
		}
		return
	}

	// A thinking delta after a signature starts the next interleaved block.
	// Multiple signature fragments remain attached to the current index and are
	// concatenated by provider.ReasoningParts.
	if !s.hasCurrent || (thinking != "" && s.signatureStarted) {
		s.currentIndex = s.nextIndex
		s.nextIndex++
		s.hasCurrent = true
		s.signatureStarted = false
	}
	index := s.currentIndex
	provider.AttachReasoningParts(msg, provider.NewReasoningOutputPart(thinking, signature, &index))
	if signature != "" {
		s.signatureStarted = true
	}
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
		if thinking := thinkingFromReasoning(extra.Thinking); thinking != nil {
			return thinking
		}
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
