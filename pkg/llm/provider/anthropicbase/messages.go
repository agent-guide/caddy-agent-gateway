package anthropicbase

import (
	"encoding/json"
	"strings"

	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

type BuildMessagesOptions struct {
	DefaultMaxTokens       int
	Stream                 bool
	System                 []SystemBlock
	Metadata               *RequestMetadata
	Thinking               *ThinkingConfig
	ContextManagement      *ContextManagement
	OutputConfig           *OutputConfig
	CacheUserText          bool
	DisableParallelToolUse bool
}

const (
	minThinkingBudgetTokens = 1024
	minThinkingAnswerTokens = 512
)

func BuildMessagesRequest(state *provider.ChatRequestState, opts BuildMessagesOptions) *MessagesRequest {
	maxTokens := opts.DefaultMaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	if state.CommonOptions != nil && state.CommonOptions.MaxTokens != nil && *state.CommonOptions.MaxTokens > 0 {
		maxTokens = *state.CommonOptions.MaxTokens
	}

	req := &MessagesRequest{
		Model:             state.ModelName,
		MaxTokens:         maxTokens,
		Messages:          make([]MessageItem, 0, len(state.Messages)),
		System:            append([]SystemBlock(nil), opts.System...),
		Metadata:          opts.Metadata,
		Thinking:          opts.Thinking,
		ContextManagement: opts.ContextManagement,
		OutputConfig:      opts.OutputConfig,
		Stream:            opts.Stream,
	}

	// Extended thinking (type "enabled") requires budget_tokens < max_tokens and
	// rejects temperature/top_p/top_k modifications, so normalize the budget and
	// suppress sampling options before copying them onto the request.
	extendedThinking := req.Thinking != nil && req.Thinking.Type == "enabled"
	if extendedThinking {
		normalizeThinkingBudget(req)
	}

	if state.CommonOptions != nil {
		if !extendedThinking {
			if state.CommonOptions.Temperature != nil {
				req.Temperature = float64(*state.CommonOptions.Temperature)
			}
			if state.CommonOptions.TopP != nil {
				req.TopP = float64(*state.CommonOptions.TopP)
			}
		}
		if len(state.CommonOptions.Stop) > 0 {
			req.StopSequences = state.CommonOptions.Stop
		}
		req.Tools = ToolInfosToToolDefs(state.CommonOptions.Tools)
		if state.CommonOptions.ToolChoice != nil {
			req.ToolChoice = BuildToolChoice(state.CommonOptions.ToolChoice, state.CommonOptions.AllowedToolNames, opts.DisableParallelToolUse)
		} else if opts.DisableParallelToolUse && len(req.Tools) > 0 {
			auto := schema.ToolChoiceAllowed
			req.ToolChoice = BuildToolChoice(&auto, nil, true)
		}
	} else {
		req.Tools = []ToolDef{}
	}

	if !extendedThinking {
		if chatOpts := provider.GetChatOptions(state.Options...); chatOpts != nil && chatOpts.TopK > 0 {
			req.TopK = chatOpts.TopK
		}
	}

	req.Messages = ConvertMessages(state.Messages, req, opts.CacheUserText)
	return req
}

// normalizeThinkingBudget applies ClampThinkingBudget to an in-flight request.
func normalizeThinkingBudget(req *MessagesRequest) {
	th := req.Thinking
	if th == nil || th.Type != "enabled" || th.BudgetTokens <= 0 {
		return
	}
	req.MaxTokens, th.BudgetTokens = ClampThinkingBudget(req.MaxTokens, th.BudgetTokens)
}

// ClampThinkingBudget enforces Anthropic's extended-thinking constraints on a
// (max_tokens, budget_tokens) pair: budget_tokens must be at least 1024 and
// strictly less than max_tokens. It prefers to honor an explicit max_tokens by
// shrinking the thinking budget, and only grows max_tokens when the cap is too
// small to host even minimal thinking.
func ClampThinkingBudget(maxTokens, budgetTokens int) (int, int) {
	if budgetTokens <= 0 {
		return maxTokens, budgetTokens
	}
	budget := max(budgetTokens, minThinkingBudgetTokens)
	if maxTokens < budget+minThinkingAnswerTokens {
		if room := maxTokens - minThinkingAnswerTokens; room >= minThinkingBudgetTokens {
			budget = room
		} else {
			maxTokens = budget + minThinkingAnswerTokens
		}
	}
	return maxTokens, budget
}

// OutputFormatFromState derives an Anthropic structured-output format from the
// inbound chat response_format or Responses text.format fields. It returns nil
// when no JSON-schema-constrained output was requested. Anthropic only supports
// the json_schema format, so json_object and text requests yield nil.
func OutputFormatFromState(state *provider.ChatRequestState) *OutputFormat {
	if state == nil {
		return nil
	}
	if extra := provider.ChatExtraFieldsFromOptions(state.Options...); extra != nil {
		if format := outputFormatFromResponseFormat(extra.ResponseFormat); format != nil {
			return format
		}
	}
	if ctx := provider.ResponsesRequestContextFromOptions(state.Options...); ctx != nil {
		if format := outputFormatFromResponsesText(ctx.Text); format != nil {
			return format
		}
	}
	return nil
}

func outputFormatFromResponseFormat(responseFormat any) *OutputFormat {
	spec, ok := responseFormat.(map[string]any)
	if !ok {
		return nil
	}
	if typ, _ := spec["type"].(string); typ != "json_schema" {
		return nil
	}
	return outputFormatFromSchemaHolder(spec)
}

func outputFormatFromResponsesText(text map[string]any) *OutputFormat {
	format, ok := text["format"].(map[string]any)
	if !ok {
		return nil
	}
	if typ, _ := format["type"].(string); typ != "json_schema" {
		return nil
	}
	return outputFormatFromSchemaHolder(format)
}

// outputFormatFromSchemaHolder extracts the JSON Schema from either the OpenAI
// chat shape ({json_schema:{schema:{...}}}) or the flatter Responses/Anthropic
// shape ({schema:{...}}).
func outputFormatFromSchemaHolder(spec map[string]any) *OutputFormat {
	var schemaValue any
	if nested, ok := spec["json_schema"].(map[string]any); ok {
		schemaValue = nested["schema"]
	}
	if schemaValue == nil {
		schemaValue = spec["schema"]
	}
	if schemaValue == nil {
		return nil
	}
	raw, err := json.Marshal(schemaValue)
	if err != nil {
		return nil
	}
	return &OutputFormat{Type: "json_schema", Schema: raw}
}

func ToolInfosToToolDefs(tools []*schema.ToolInfo) []ToolDef {
	out := make([]ToolDef, 0, len(tools))
	for _, ti := range tools {
		if ti == nil {
			continue
		}
		js, err := ti.ToJSONSchema()
		if err != nil {
			continue
		}
		raw, err := json.Marshal(js)
		if err != nil {
			continue
		}
		out = append(out, ToolDef{Name: ti.Name, Description: ti.Desc, InputSchema: raw})
	}
	return out
}

func BuildToolChoice(tc *schema.ToolChoice, allowedNames []string, disableParallelToolUse bool) json.RawMessage {
	if tc == nil {
		return nil
	}

	var payload map[string]any
	switch *tc {
	case schema.ToolChoiceForbidden:
		payload = map[string]any{"type": "none"}
	case schema.ToolChoiceForced:
		if len(allowedNames) > 0 {
			payload = map[string]any{"type": "tool", "name": allowedNames[0]}
		} else {
			payload = map[string]any{"type": "any"}
		}
	default:
		payload = map[string]any{"type": "auto"}
	}
	if disableParallelToolUse {
		payload["disable_parallel_tool_use"] = true
	}
	b, _ := json.Marshal(payload)
	return b
}

func ConvertMessages(msgs []*schema.Message, req *MessagesRequest, cacheUserText bool) []MessageItem {
	var out []MessageItem
	i := 0
	for i < len(msgs) {
		msg := msgs[i]
		if msg == nil {
			i++
			continue
		}
		switch msg.Role {
		case schema.System:
			req.System = append(req.System, SystemBlock{Type: "text", Text: strings.TrimSpace(msg.Content)})
			i++
		case schema.Assistant:
			out = append(out, convertAssistantMessage(msg))
			i++
		case schema.Tool:
			j := i
			for j < len(msgs) && msgs[j] != nil && msgs[j].Role == schema.Tool {
				j++
			}
			out = append(out, convertToolResultMessages(msgs[i:j]))
			i = j
		default:
			item := convertUserMessage(msg, cacheUserText)
			if len(item.Content) > 0 {
				out = append(out, item)
			}
			i++
		}
	}
	return mergeAdjacentSameRole(out)
}

// mergeAdjacentSameRole collapses consecutive MessageItems that share a role
// into a single message by concatenating their content blocks. The Anthropic
// Messages API requires user and assistant turns to alternate, and in
// particular every tool_use block must be immediately followed by its
// tool_result in the next message. Codex replays parallel tool calls as
// separate same-role items, so without this merge an assistant tool_use can be
// followed by another assistant message instead of the user tool_result, which
// the API rejects as malformed.
func mergeAdjacentSameRole(items []MessageItem) []MessageItem {
	if len(items) <= 1 {
		return items
	}
	merged := make([]MessageItem, 0, len(items))
	for _, item := range items {
		if n := len(merged); n > 0 && merged[n-1].Role == item.Role {
			merged[n-1].Content = append(merged[n-1].Content, item.Content...)
			continue
		}
		merged = append(merged, item)
	}
	return merged
}

func convertAssistantMessage(msg *schema.Message) MessageItem {
	var blocks []ContentBlock
	text := strings.TrimSpace(msg.Content)
	if text != "" {
		blocks = append(blocks, ContentBlock{Type: "text", Text: text})
	}
	for _, tc := range msg.ToolCalls {
		input := json.RawMessage(tc.Function.Arguments)
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		blocks = append(blocks, ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: input,
		})
	}
	return MessageItem{Role: "assistant", Content: blocks}
}

func convertToolResultMessages(msgs []*schema.Message) MessageItem {
	var blocks []ContentBlock
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		blocks = append(blocks, ContentBlock{
			Type:      "tool_result",
			ToolUseID: msg.ToolCallID,
			Content:   msg.Content,
		})
	}
	return MessageItem{Role: "user", Content: blocks}
}

func convertUserMessage(msg *schema.Message, cacheUserText bool) MessageItem {
	var blocks []ContentBlock
	cacheControl := userTextCacheControl(cacheUserText)

	if len(msg.UserInputMultiContent) > 0 {
		for _, part := range msg.UserInputMultiContent {
			switch part.Type {
			case schema.ChatMessagePartTypeText:
				if strings.TrimSpace(part.Text) != "" {
					blocks = append(blocks, ContentBlock{Type: "text", Text: part.Text, CacheControl: cacheControl})
				}
			case schema.ChatMessagePartTypeImageURL:
				if part.Image != nil {
					src := &ImageSource{}
					if part.Image.Base64Data != nil {
						src.Type = "base64"
						src.Data = *part.Image.Base64Data
						src.MediaType = part.Image.MIMEType
					} else if part.Image.URL != nil {
						src.Type = "url"
						src.URL = *part.Image.URL
					}
					blocks = append(blocks, ContentBlock{Type: "image", Source: src})
				}
			}
		}
		return MessageItem{Role: "user", Content: blocks}
	}

	text := strings.TrimSpace(msg.Content)
	if text != "" {
		blocks = append(blocks, ContentBlock{Type: "text", Text: text, CacheControl: cacheControl})
	}
	return MessageItem{Role: "user", Content: blocks}
}

func userTextCacheControl(enabled bool) *CacheControl {
	if !enabled {
		return nil
	}
	return &CacheControl{Type: "ephemeral"}
}

func (r *MessagesResponse) ToChatResponse() *provider.ChatResponse {
	if r == nil {
		return &provider.ChatResponse{}
	}

	var textParts []string
	var toolCalls []schema.ToolCall
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				textParts = append(textParts, b.Text)
			}
		case "tool_use":
			inputStr := string(b.Input)
			if inputStr == "" {
				inputStr = "{}"
			}
			toolCalls = append(toolCalls, schema.ToolCall{
				ID:   b.ID,
				Type: "function",
				Function: schema.FunctionCall{
					Name:      b.Name,
					Arguments: inputStr,
				},
			})
		}
	}

	return &provider.ChatResponse{
		Message: &schema.Message{
			Role:      schema.Assistant,
			Content:   strings.Join(textParts, "\n"),
			ToolCalls: toolCalls,
			ResponseMeta: &schema.ResponseMeta{
				FinishReason: r.StopReason,
				Usage:        r.Usage.TokenUsage(),
			},
		},
	}
}
