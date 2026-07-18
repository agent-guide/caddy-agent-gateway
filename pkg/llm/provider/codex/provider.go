package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/llm/credentialmgr"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/openaibase"
)

const defaultBaseURL = "https://chatgpt.com/backend-api/codex"

func init() {
	provider.RegisterProviderFactory("codex", New)
}

type Provider struct {
	*openaibase.Base
	ccCompat bool
}

func New(config provider.ProviderConfig) (provider.Provider, error) {
	if config.BaseURL == "" {
		config.BaseURL = defaultBaseURL
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	config.Network.Defaults()

	compactMode, err := provider.CompactModeFromOptions(config.Options)
	if err != nil {
		return nil, err
	}

	base := openaibase.NewBase(config)
	base.SetAuthHeaders = newCodexAuthHeaders(config)

	return &Provider{Base: base, ccCompat: compactMode == provider.CompactModeCC}, nil
}

func newCodexAuthHeaders(config provider.ProviderConfig) func(ctx context.Context, req *http.Request) {
	return func(ctx context.Context, req *http.Request) {
		accessToken := extractAccessToken(ctx)
		if accessToken == "" {
			accessToken = strings.TrimSpace(config.APIKey)
		}
		if accessToken != "" {
			req.Header.Set("Authorization", "Bearer "+accessToken)
		}

		if accountID := extractAccountID(ctx); accountID != "" {
			req.Header.Set("Chatgpt-Account-Id", accountID)
		}

		req.Header.Set("Originator", "codex-tui")
	}
}

func (p *Provider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	p.ensureBase()
	state, err := provider.ResolveChatRequest(ctx, p.ProviderConfig, req)
	if err != nil {
		return nil, err
	}
	ctx = provider.OnChatStart(ctx, "codex", state.ModelName, state.Messages)
	respReq := chatStateToResponsesRequest(state, false)
	resp, err := p.CreateResponses(ctx, respReq)
	if err != nil {
		provider.OnChatError(ctx, err)
		return nil, err
	}
	chatResp := responsesToChatResponse(resp)
	provider.OnChatEnd(ctx, state.ModelName, chatResp.Message)
	return chatResp, nil
}

func (p *Provider) StreamChat(ctx context.Context, req *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	p.ensureBase()
	state, err := provider.ResolveChatRequest(ctx, p.ProviderConfig, req)
	if err != nil {
		return nil, err
	}
	ctx = provider.OnChatStart(ctx, "codex", state.ModelName, state.Messages)
	respReq := chatStateToResponsesRequest(state, true)
	eventStream, err := p.StreamResponses(ctx, respReq)
	if err != nil {
		provider.OnChatError(ctx, err)
		return nil, err
	}
	stream := responsesEventStreamToMessageStream(eventStream, p.ccCompat)
	return provider.OnChatStreamEnd(ctx, state.ModelName, stream), nil
}

func (p *Provider) ListModels(_ context.Context) ([]provider.ModelInfo, error) {
	p.ensureBase()
	model := strings.TrimSpace(p.ProviderConfig.DefaultModel)
	if model == "" {
		return nil, nil
	}
	return []provider.ModelInfo{{
		ID:           model,
		Name:         model,
		DisplayName:  model,
		Capabilities: provider.ModelCapabilitiesFromProviderSummary(p.Capabilities()),
	}}, nil
}

func (p *Provider) CreateResponses(ctx context.Context, req *provider.ResponsesRequest) (*provider.ResponsesResponse, error) {
	p.ensureBase()
	return p.Base.DoCreateResponses(ctx, sanitizeResponsesRequest(req, p.ccCompat))
}

func (p *Provider) StreamResponses(ctx context.Context, req *provider.ResponsesRequest) (*schema.StreamReader[*provider.ResponsesStreamEvent], error) {
	p.ensureBase()
	return p.Base.DoStreamResponses(ctx, sanitizeResponsesRequest(req, p.ccCompat))
}

func sanitizeResponsesRequest(req *provider.ResponsesRequest, ccCompat bool) *provider.ResponsesRequest {
	// The Codex CLI backend is not a standard OpenAI Responses endpoint. The
	// real CLI omits max_output_tokens and always disables storage; preserve
	// that wire shape to avoid backend 400s from otherwise valid gateway fields.
	req.MaxOutputTokens = 0
	req.Metadata = nil
	req.Input = sanitizeResponsesInput(req.Input)
	req.Tools = sanitizeResponsesTools(req.Tools)
	req.ToolChoice = filterUnsupportedHostedToolChoice(req.ToolChoice)
	if ccCompat {
		req.Tools = filterClaudeCodeStatefulTools(req.Tools)
		req.ToolChoice = filterClaudeCodeStatefulToolChoice(req.ToolChoice)
	}
	req.ToolChoice = filterUnavailableToolChoice(req.ToolChoice, req.Tools)
	req.Text = sanitizeResponsesText(req.Text)
	parallelFalse := false
	req.ParallelToolCalls = &parallelFalse
	storeFalse := false
	req.Store = &storeFalse
	return req
}

func sanitizeResponsesInput(input any) any {
	return sanitizeResponsesInputValue(input, "")
}

func sanitizeResponsesInputValue(input any, role string) any {
	switch v := input.(type) {
	case []any:
		for i := range v {
			v[i] = sanitizeResponsesInputValue(v[i], role)
		}
		return v
	case []map[string]any:
		for i := range v {
			v[i] = sanitizeResponsesInputValue(v[i], role).(map[string]any)
		}
		return v
	case map[string]any:
		itemRole := role
		if r, _ := v["role"].(string); strings.TrimSpace(r) != "" {
			itemRole = r
		}
		if itemRole == string(schema.Tool) {
			itemRole = string(schema.User)
			v["role"] = itemRole
		}
		if typ, _ := v["type"].(string); typ == "input_text" && itemRole == string(schema.Assistant) {
			v["type"] = "output_text"
		} else if typ == "output_text" && itemRole != string(schema.Assistant) {
			v["type"] = "input_text"
		}
		if content, ok := v["content"]; ok {
			v["content"] = sanitizeResponsesInputValue(content, itemRole)
		}
		return v
	default:
		return input
	}
}

func sanitizeResponsesTools(tools []provider.ResponsesToolDefinition) []provider.ResponsesToolDefinition {
	out := tools[:0]
	for i := range tools {
		fn := tools[i].Function
		if fn == nil {
			if isUnsupportedHostedTool(tools[i].Type) || isUnsupportedHostedTool(tools[i].Name) {
				continue
			}
			out = append(out, tools[i])
			continue
		}
		if isUnsupportedHostedTool(tools[i].Type) || isUnsupportedHostedTool(tools[i].Name) || isUnsupportedHostedTool(fn.Name) {
			continue
		}
		if strings.TrimSpace(tools[i].Name) == "" {
			tools[i].Name = fn.Name
		}
		if tools[i].Description == "" {
			tools[i].Description = fn.Description
		}
		if len(tools[i].Parameters) == 0 {
			tools[i].Parameters = fn.Parameters
		}
		tools[i].Function = nil
		out = append(out, tools[i])
	}
	return out
}

func isUnsupportedHostedTool(toolType string) bool {
	switch strings.ToLower(strings.TrimSpace(toolType)) {
	case "tool_search":
		return true
	default:
		return false
	}
}

func filterUnsupportedHostedToolChoice(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var choice struct {
		Type     string `json:"type,omitempty"`
		Name     string `json:"name,omitempty"`
		Function *struct {
			Name string `json:"name,omitempty"`
		} `json:"function,omitempty"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return raw
	}
	functionName := ""
	if choice.Function != nil {
		functionName = choice.Function.Name
	}
	if isUnsupportedHostedTool(choice.Type) || isUnsupportedHostedTool(choice.Name) || isUnsupportedHostedTool(functionName) {
		return nil
	}
	return raw
}

func filterClaudeCodeStatefulTools(tools []provider.ResponsesToolDefinition) []provider.ResponsesToolDefinition {
	out := tools[:0]
	for _, tool := range tools {
		if isClaudeCodeStatefulTool(responsesToolName(tool)) {
			continue
		}
		out = append(out, tool)
	}
	return out
}

func filterClaudeCodeStatefulToolChoice(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	name := toolChoiceName(raw)
	if name != "" && isClaudeCodeStatefulTool(name) {
		return nil
	}
	return raw
}

func filterUnavailableToolChoice(raw json.RawMessage, tools []provider.ResponsesToolDefinition) json.RawMessage {
	if len(raw) == 0 || !isForcedToolChoice(raw) {
		return raw
	}
	if len(tools) == 0 {
		return nil
	}
	name := toolChoiceName(raw)
	if name == "" {
		return raw
	}
	for _, tool := range tools {
		if strings.EqualFold(strings.TrimSpace(responsesToolName(tool)), strings.TrimSpace(name)) {
			return raw
		}
	}
	return nil
}

func isForcedToolChoice(raw json.RawMessage) bool {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.EqualFold(strings.TrimSpace(asString), "required")
	}
	var choice struct {
		Type string `json:"type,omitempty"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(choice.Type)) {
	case "required", "function", "tool":
		return true
	default:
		return false
	}
}

func toolChoiceName(raw json.RawMessage) string {
	var choice struct {
		Name     string `json:"name,omitempty"`
		Function *struct {
			Name string `json:"name,omitempty"`
		} `json:"function,omitempty"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return ""
	}
	if strings.TrimSpace(choice.Name) != "" {
		return choice.Name
	}
	if choice.Function != nil {
		return choice.Function.Name
	}
	return ""
}

func responsesToolName(tool provider.ResponsesToolDefinition) string {
	if strings.TrimSpace(tool.Name) != "" {
		return tool.Name
	}
	if tool.Function != nil {
		return tool.Function.Name
	}
	return ""
}

func isClaudeCodeStatefulTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "agent", "task", "team", "teamcreate", "teamdelete", "spawnteam", "spawn_team":
		return true
	default:
		return false
	}
}

func sanitizeResponsesText(text map[string]any) map[string]any {
	format, _ := text["format"].(map[string]any)
	if len(format) == 0 {
		return text
	}
	if typ, _ := format["type"].(string); typ != "json_schema" {
		return text
	}
	if strings.TrimSpace(stringFromAny(format["name"])) == "" {
		format["name"] = "response"
	}
	return text
}

func stringFromAny(v any) string {
	s, _ := v.(string)
	return s
}

func (p *Provider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{
		Streaming:       true,
		Tools:           false,
		Vision:          false,
		Embeddings:      false,
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

func extractAccessToken(ctx context.Context) string {
	cred, ok := provider.CredentialFromContext(ctx)
	if !ok || cred == nil {
		return ""
	}
	if cred.Type == credentialmgr.TypeCLIAuthToken {
		if cred.Metadata != nil {
			if token, _ := cred.Metadata["access_token"].(string); strings.TrimSpace(token) != "" {
				return strings.TrimSpace(token)
			}
		}
	}
	return strings.TrimSpace(cred.APIKey())
}

func extractAccountID(ctx context.Context) string {
	cred, ok := provider.CredentialFromContext(ctx)
	if !ok || cred == nil || cred.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(cred.Attributes["account_id"])
}

func chatStateToResponsesRequest(state *provider.ChatRequestState, stream bool) *provider.ResponsesRequest {
	return provider.ResponsesRequestFromChatState(state, stream)
}

func responsesToChatResponse(resp *provider.ResponsesResponse) *provider.ChatResponse {
	if resp == nil {
		return &provider.ChatResponse{}
	}
	var text strings.Builder
	for _, out := range resp.Output {
		for _, c := range out.Content {
			text.WriteString(c.Text)
		}
	}
	msg := &schema.Message{
		Role:    schema.Assistant,
		Content: text.String(),
	}
	if resp.Usage != nil {
		msg.ResponseMeta = &schema.ResponseMeta{
			FinishReason: "stop",
			Usage:        responsesTokenUsage(resp.Usage),
		}
	}
	return &provider.ChatResponse{Message: msg}
}

func responsesEventStreamToMessageStream(eventStream *schema.StreamReader[*provider.ResponsesStreamEvent], ccCompat bool) *schema.StreamReader[*schema.Message] {
	sr, sw := schema.Pipe[*schema.Message](16)
	go func() {
		defer eventStream.Close()
		defer sw.Close()
		pendingToolCalls := map[int]*schema.ToolCall{}
		sentToolCalls := map[int]bool{}
		emittedText := false
		for {
			event, err := eventStream.Recv()
			if err != nil {
				if err == io.EOF {
					return
				}
				sw.Send(nil, err)
				return
			}
			if event == nil {
				continue
			}
			switch event.Type {
			case "response.output_text.delta":
				if event.Delta != "" {
					emittedText = true
					sw.Send(&schema.Message{
						Role:    schema.Assistant,
						Content: event.Delta,
					}, nil)
				}
			case "response.output_item.added":
				if event.Item != nil && event.Item.Type == "function_call" {
					pendingToolCalls[event.OutputIndex] = responsesToolCallFromOutput(event.Item, event.OutputIndex)
				}
			case "response.function_call_arguments.delta":
				tc := pendingToolCalls[event.OutputIndex]
				if tc == nil {
					tc = responsesToolCallFromOutput(&provider.ResponsesResponseOutput{
						ID:        event.ItemID,
						Type:      "function_call",
						Arguments: event.Delta,
					}, event.OutputIndex)
					pendingToolCalls[event.OutputIndex] = tc
					continue
				}
				tc.Function.Arguments += event.Delta
			case "response.output_item.done":
				if event.Item != nil && event.Item.Type == "function_call" {
					if sentToolCalls[event.OutputIndex] {
						delete(pendingToolCalls, event.OutputIndex)
						continue
					}
					tc := pendingToolCalls[event.OutputIndex]
					if tc == nil {
						tc = responsesToolCallFromOutput(event.Item, event.OutputIndex)
					} else {
						mergeResponsesToolCallOutput(tc, event.Item)
					}
					sendResponsesToolCall(sw, tc, ccCompat)
					sentToolCalls[event.OutputIndex] = true
					delete(pendingToolCalls, event.OutputIndex)
				} else if event.Item != nil && event.Item.Type == "message" && !emittedText {
					if text := responsesTextFromOutput(*event.Item); text != "" {
						emittedText = true
						sw.Send(&schema.Message{
							Role:    schema.Assistant,
							Content: text,
						}, nil)
					}
				}
			case "response.function_call_arguments.done":
				if sentToolCalls[event.OutputIndex] {
					delete(pendingToolCalls, event.OutputIndex)
					continue
				}
				if tc := pendingToolCalls[event.OutputIndex]; tc != nil {
					if event.Delta != "" {
						tc.Function.Arguments = event.Delta
					}
					sendResponsesToolCall(sw, tc, ccCompat)
					sentToolCalls[event.OutputIndex] = true
					delete(pendingToolCalls, event.OutputIndex)
				}
			case "response.completed":
				for outputIndex, tc := range pendingToolCalls {
					if sentToolCalls[outputIndex] {
						delete(pendingToolCalls, outputIndex)
						continue
					}
					sendResponsesToolCall(sw, tc, ccCompat)
					sentToolCalls[outputIndex] = true
					delete(pendingToolCalls, outputIndex)
				}
				if event.Response != nil && !emittedText {
					if text := responsesTextFromResponse(event.Response); text != "" {
						emittedText = true
						sw.Send(&schema.Message{
							Role:    schema.Assistant,
							Content: text,
						}, nil)
					}
				}
				sw.Send(responsesCompletionMessage(event.Response), nil)
				return
			case "response.failed", "response.incomplete":
				sw.Send(nil, errors.New("codex responses stream ended with "+event.Type))
				return
			}
		}
	}()
	return sr
}

func responsesCompletionMessage(resp *provider.ResponsesResponse) *schema.Message {
	msg := &schema.Message{
		Role: schema.Assistant,
		ResponseMeta: &schema.ResponseMeta{
			FinishReason: "stop",
		},
	}
	if resp != nil && resp.Usage != nil {
		msg.ResponseMeta.Usage = responsesTokenUsage(resp.Usage)
	}
	return msg
}

func responsesTokenUsage(u *provider.ResponsesResponseUsage) *schema.TokenUsage {
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens
	}
	return &schema.TokenUsage{
		PromptTokens: u.InputTokens,
		PromptTokenDetails: schema.PromptTokenDetails{
			CachedTokens: u.InputTokensDetails.CachedTokens,
		},
		CompletionTokens: u.OutputTokens,
		CompletionTokensDetails: schema.CompletionTokensDetails{
			ReasoningTokens: u.OutputTokensDetails.ReasoningTokens,
		},
		TotalTokens: total,
	}
}

func responsesTextFromResponse(resp *provider.ResponsesResponse) string {
	if resp == nil {
		return ""
	}
	var text strings.Builder
	for _, out := range resp.Output {
		text.WriteString(responsesTextFromOutput(out))
	}
	return text.String()
}

func responsesTextFromOutput(out provider.ResponsesResponseOutput) string {
	var text strings.Builder
	for _, part := range out.Content {
		text.WriteString(part.Text)
	}
	return text.String()
}

func sendResponsesToolCall(sw *schema.StreamWriter[*schema.Message], tc *schema.ToolCall, ccCompat bool) {
	if sw == nil || tc == nil {
		return
	}
	if ccCompat && isClaudeCodeStatefulTool(tc.Function.Name) {
		return
	}
	sw.Send(&schema.Message{
		Role:      schema.Assistant,
		ToolCalls: []schema.ToolCall{*tc},
		ResponseMeta: &schema.ResponseMeta{
			FinishReason: "tool_calls",
		},
	}, nil)
}

func responsesToolCallFromOutput(item *provider.ResponsesResponseOutput, outputIndex int) *schema.ToolCall {
	if item == nil {
		return &schema.ToolCall{}
	}
	id := item.CallID
	if id == "" {
		id = item.ID
	}
	idx := outputIndex
	return &schema.ToolCall{
		ID:    id,
		Type:  "function",
		Index: &idx,
		Function: schema.FunctionCall{
			Name:      item.Name,
			Arguments: item.Arguments,
		},
	}
}

func mergeResponsesToolCallOutput(tc *schema.ToolCall, item *provider.ResponsesResponseOutput) {
	if tc == nil || item == nil {
		return
	}
	if tc.ID == "" {
		if item.CallID != "" {
			tc.ID = item.CallID
		} else {
			tc.ID = item.ID
		}
	}
	if tc.Function.Name == "" {
		tc.Function.Name = item.Name
	}
	if item.Arguments != "" {
		tc.Function.Arguments = item.Arguments
	}
}

var (
	_ provider.Provider          = (*Provider)(nil)
	_ provider.ResponsesProvider = (*Provider)(nil)
)
