package openai

import (
	"encoding/json"
	"strings"

	einojsonschema "github.com/eino-contrib/jsonschema"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

// Converter converts between OpenAI API format and internal format.
type Converter struct{}

// ChatCompletionRequest is the OpenAI chat completion request format.
type ChatCompletionRequest struct {
	Model             string           `json:"model"`
	Messages          []ChatMessage    `json:"messages"`
	Tools             []ToolDefinition `json:"tools,omitempty"`
	ToolChoice        json.RawMessage  `json:"tool_choice,omitempty"`
	MaxTokens         int              `json:"max_tokens,omitempty"`
	Temperature       float64          `json:"temperature,omitempty"`
	TopP              float64          `json:"top_p,omitempty"`
	Stream            bool             `json:"stream,omitempty"`
	Stop              []string         `json:"stop,omitempty"`
	ResponseFormat    json.RawMessage  `json:"response_format,omitempty"`
	Reasoning         json.RawMessage  `json:"reasoning,omitempty"`
	ReasoningEffort   string           `json:"reasoning_effort,omitempty"`
	User              string           `json:"user,omitempty"`
	Metadata          map[string]any   `json:"metadata,omitempty"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls,omitempty"`
	Store             *bool            `json:"store,omitempty"`
}

// ChatMessage is a single message in an OpenAI chat request or response.
type ChatMessage struct {
	Role       string            `json:"role"`
	Content    any               `json:"content,omitempty"`
	ToolCalls  []schema.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
}

type ToolDefinition struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *ImageURLPart `json:"image_url,omitempty"`
}

type ImageURLPart struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// ChatCompletionResponse is the OpenAI chat completion response format.
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice is a single choice in an OpenAI chat completion response.
type Choice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// Usage is token usage information in an OpenAI response.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ToInternal converts an OpenAI ChatCompletionRequest to the internal ChatRequest.
func (c *Converter) ToInternal(req *ChatCompletionRequest) *provider.ChatRequest {
	msgs := make([]*schema.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, convertMessage(m)...)
	}
	var opts []einomodel.Option
	if req.Temperature != 0 {
		opts = append(opts, einomodel.WithTemperature(float32(req.Temperature)))
	}
	if req.TopP != 0 {
		opts = append(opts, einomodel.WithTopP(float32(req.TopP)))
	}
	if req.MaxTokens > 0 {
		opts = append(opts, einomodel.WithMaxTokens(req.MaxTokens))
	}
	if len(req.Stop) > 0 {
		opts = append(opts, einomodel.WithStop(req.Stop))
	}
	if len(req.Tools) > 0 {
		opts = append(opts, einomodel.WithTools(toolDefsToToolInfos(req.Tools)))
	}
	if len(req.ToolChoice) > 0 {
		if tc, names, ok := parseOpenAIToolChoice(req.ToolChoice); ok {
			opts = append(opts, einomodel.WithToolChoice(tc, names...))
		}
	}
	if extra := chatExtraFields(req); extra != nil {
		opts = append(opts, provider.WithChatExtraFields(extra))
	}
	return &provider.ChatRequest{
		Model:    req.Model,
		Messages: msgs,
		Options:  opts,
	}
}

func chatExtraFields(req *ChatCompletionRequest) *provider.ChatExtraFields {
	extra := &provider.ChatExtraFields{
		ReasoningEffort:   strings.TrimSpace(req.ReasoningEffort),
		User:              strings.TrimSpace(req.User),
		Metadata:          cloneMap(req.Metadata),
		ParallelToolCalls: cloneBoolPtr(req.ParallelToolCalls),
		Store:             cloneBoolPtr(req.Store),
	}
	if len(req.ResponseFormat) > 0 {
		var format any
		if err := json.Unmarshal(req.ResponseFormat, &format); err == nil {
			extra.ResponseFormat = format
		}
	}
	if len(req.Reasoning) > 0 {
		var reasoning map[string]any
		if err := json.Unmarshal(req.Reasoning, &reasoning); err == nil {
			extra.Reasoning = reasoning
		}
	}
	if extra.ResponseFormat == nil && len(extra.Reasoning) == 0 && extra.ReasoningEffort == "" &&
		extra.User == "" && len(extra.Metadata) == 0 && extra.ParallelToolCalls == nil && extra.Store == nil {
		return nil
	}
	return extra
}

func cloneBoolPtr(src *bool) *bool {
	if src == nil {
		return nil
	}
	v := *src
	return &v
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

func toolDefsToToolInfos(defs []ToolDefinition) []*schema.ToolInfo {
	tools := make([]*schema.ToolInfo, 0, len(defs))
	for _, td := range defs {
		if td.Type != "" && td.Type != "function" {
			continue
		}
		if strings.TrimSpace(td.Function.Name) == "" {
			continue
		}
		var js einojsonschema.Schema
		if len(td.Function.Parameters) > 0 {
			if err := json.Unmarshal(td.Function.Parameters, &js); err != nil {
				tools = append(tools, &schema.ToolInfo{Name: td.Function.Name, Desc: td.Function.Description})
				continue
			}
			tools = append(tools, &schema.ToolInfo{
				Name:        td.Function.Name,
				Desc:        td.Function.Description,
				ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&js),
			})
			continue
		}
		tools = append(tools, &schema.ToolInfo{Name: td.Function.Name, Desc: td.Function.Description})
	}
	return tools
}

func parseOpenAIToolChoice(raw json.RawMessage) (schema.ToolChoice, []string, bool) {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		switch asString {
		case "auto":
			return schema.ToolChoiceAllowed, nil, true
		case "required":
			return schema.ToolChoiceForced, nil, true
		case "none":
			return schema.ToolChoiceForbidden, nil, true
		default:
			return "", nil, false
		}
	}

	var tc struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function,omitempty"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return "", nil, false
	}
	if tc.Type == "function" && strings.TrimSpace(tc.Function.Name) != "" {
		return schema.ToolChoiceForced, []string{tc.Function.Name}, true
	}
	return "", nil, false
}

func convertMessage(m ChatMessage) []*schema.Message {
	role := schema.RoleType(m.Role)
	switch role {
	case schema.Tool:
		return []*schema.Message{{
			Role:       schema.Tool,
			Content:    contentText(m.Content),
			ToolCallID: m.ToolCallID,
		}}
	case schema.Assistant:
		return []*schema.Message{{
			Role:      schema.Assistant,
			Content:   contentText(m.Content),
			ToolCalls: m.ToolCalls,
		}}
	case schema.User:
		msg := &schema.Message{Role: schema.User}
		if parts := inputPartsFromContent(m.Content); len(parts) > 0 {
			if len(parts) == 1 && parts[0].Type == schema.ChatMessagePartTypeText {
				msg.Content = parts[0].Text
			} else {
				msg.UserInputMultiContent = parts
			}
			return []*schema.Message{msg}
		}
		msg.Content = contentText(m.Content)
		return []*schema.Message{msg}
	case schema.System:
		return []*schema.Message{{Role: schema.System, Content: contentText(m.Content)}}
	default:
		if m.Role == "developer" {
			return []*schema.Message{{Role: schema.System, Content: contentText(m.Content)}}
		}
		return []*schema.Message{{Role: role, Content: contentText(m.Content)}}
	}
}

func inputPartsFromContent(content any) []schema.MessageInputPart {
	rawParts, ok := content.([]any)
	if !ok {
		return nil
	}
	out := make([]schema.MessageInputPart, 0, len(rawParts))
	for _, raw := range rawParts {
		partMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		partType, _ := partMap["type"].(string)
		switch partType {
		case "text", "input_text":
			text, _ := partMap["text"].(string)
			if text != "" {
				out = append(out, schema.MessageInputPart{
					Type: schema.ChatMessagePartTypeText,
					Text: text,
				})
			}
		case "image_url", "input_image":
			imgObj, _ := partMap["image_url"].(map[string]any)
			if imgObj == nil {
				continue
			}
			url, _ := imgObj["url"].(string)
			if strings.TrimSpace(url) == "" {
				continue
			}
			detail, _ := imgObj["detail"].(string)
			out = append(out, schema.MessageInputPart{
				Type: schema.ChatMessagePartTypeImageURL,
				Image: &schema.MessageInputImage{
					MessagePartCommon: schema.MessagePartCommon{URL: &url},
					Detail:            schema.ImageURLDetail(detail),
				},
			})
		}
	}
	return out
}

func contentText(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, raw := range v {
			partMap, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			partType, _ := partMap["type"].(string)
			if partType != "" && partType != "text" && partType != "input_text" {
				continue
			}
			text, _ := partMap["text"].(string)
			if text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// FromInternal converts an internal ChatResponse to a ChatCompletionResponse.
func (c *Converter) FromInternal(resp *provider.ChatResponse, model string) *ChatCompletionResponse {
	usage := provider.UsageFromMessage(resp.Message)
	return &ChatCompletionResponse{
		ID:     "",
		Object: "chat.completion",
		Model:  model,
		Choices: []Choice{{
			Index: 0,
			Message: ChatMessage{
				Role:      "assistant",
				Content:   messageText(resp.Message),
				ToolCalls: toolCallsOrNil(resp.Message),
			},
			FinishReason: provider.FinishReason(resp.Message),
		}},
		Usage: Usage{
			PromptTokens:     usage.InputTokens,
			CompletionTokens: usage.OutputTokens,
			TotalTokens:      usage.TotalTokens,
		},
	}
}

func toolCallsOrNil(msg *schema.Message) []schema.ToolCall {
	if msg == nil || len(msg.ToolCalls) == 0 {
		return nil
	}
	return msg.ToolCalls
}

func messageText(msg *schema.Message) string {
	if msg == nil {
		return ""
	}
	return msg.Content
}
