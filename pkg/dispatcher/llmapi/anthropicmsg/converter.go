package anthropicmsg

import (
	"encoding/json"
	"strings"

	einojsonschema "github.com/eino-contrib/jsonschema"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropicbase"
)

// Converter converts between Anthropic API format and internal format.
type Converter struct{}

// ToInternal converts an Anthropic MessagesRequest to the internal ChatRequest.
func (c *Converter) ToInternal(req *MessagesRequest) *provider.ChatRequest {
	msgs := make([]*schema.Message, 0, len(req.Messages)+1)
	if systemText := req.System.Text(); systemText != "" {
		msgs = append(msgs, schema.SystemMessage(systemText))
	}
	for _, m := range req.Messages {
		nativeBlocks := nativeContentBlocks(m.Content)
		msgs = append(msgs, convertMessageItem(m, nativeBlocks)...)
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
	if len(req.StopSequences) > 0 {
		opts = append(opts, einomodel.WithStop(req.StopSequences))
	}

	if len(req.Tools) > 0 {
		if tools := toolDefsToToolInfos(req.Tools); len(tools) > 0 {
			opts = append(opts, einomodel.WithTools(tools))
		}
	}
	if len(req.ToolChoice) > 0 {
		if tc, names, ok := parseAnthropicToolChoice(req.ToolChoice); ok {
			opts = append(opts, einomodel.WithToolChoice(tc, names...))
		}
	}

	if req.TopK > 0 {
		opts = append(opts, provider.WithTopK(req.TopK))
	}

	if extra := chatExtraFields(req); extra != nil {
		opts = append(opts, provider.WithChatExtraFields(extra))
	}

	return &provider.ChatRequest{
		Model:         req.Model,
		Messages:      msgs,
		Options:       opts,
		ProtocolState: requestProtocolState(req),
	}
}

func requestProtocolState(req *MessagesRequest) *provider.ProtocolState {
	if req == nil {
		return nil
	}
	requirements := deriveAnthropicRequirements(req)
	state := &provider.ProtocolState{Requirements: requirements}
	if toolsRequireRawAnthropicReplay(req.Tools) || toolChoiceRequiresRawAnthropicReplay(req.ToolChoice) {
		for i, tool := range req.Tools {
			state.Envelopes = append(state.Envelopes, provider.NativeEnvelope{
				Dialect: provider.ProtocolDialectAnthropic, Scope: provider.NativeScopeRequest, Kind: provider.NativeKindToolDefinition,
				Location: provider.NativeLocation{ToolIndex: i}, Raw: tool.rawJSON(),
			})
		}
		if len(req.ToolChoice) > 0 {
			state.Envelopes = append(state.Envelopes, provider.NativeEnvelope{
				Dialect: provider.ProtocolDialectAnthropic, Scope: provider.NativeScopeRequest, Kind: provider.NativeKindToolChoice,
				Raw: append(json.RawMessage(nil), req.ToolChoice...),
			})
		}
	}
	if len(state.Envelopes) == 0 && state.Requirements.Empty() {
		return nil
	}
	return state
}

// chatExtraFields carries inbound Anthropic-native fields that have no eino
// common-option equivalent (thinking, metadata, structured-output format) so
// the provider can re-emit them on the upstream request.
func chatExtraFields(req *MessagesRequest) *provider.ChatExtraFields {
	extra := &provider.ChatExtraFields{}
	if thinking := thinkingFields(req.Thinking); len(thinking) > 0 {
		extra.Thinking = thinking
	}
	if user := userIDFromMetadata(req.Metadata); user != "" {
		extra.Metadata = map[string]any{"user_id": user}
	}
	if effort, format := fieldsFromOutputConfig(req.OutputConfig); effort != "" || format != nil {
		extra.ReasoningEffort = effort
		extra.ResponseFormat = format
	}
	if disabled, ok := disableParallelToolUseFromToolChoice(req.ToolChoice); ok {
		parallel := !disabled
		extra.ParallelToolCalls = &parallel
	}
	if len(extra.Thinking) == 0 && extra.ReasoningEffort == "" && len(extra.Metadata) == 0 && extra.ResponseFormat == nil && extra.ParallelToolCalls == nil {
		return nil
	}
	return extra
}

func toolsRequireRawAnthropicReplay(tools []ToolDefinition) bool {
	for _, tool := range tools {
		if tool.isServerTool() || rawObjectHasNonNullUnknownFields(tool.raw,
			"type", "name", "description", "input_schema") {
			return true
		}
	}
	return false
}

func toolChoiceRequiresRawAnthropicReplay(raw json.RawMessage) bool {
	return rawObjectHasNonNullUnknownFields(raw, "type", "name", "disable_parallel_tool_use")
}

func rawObjectHasNonNullUnknownFields(raw json.RawMessage, known ...string) bool {
	if len(raw) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return true
	}
	for _, field := range known {
		delete(fields, field)
	}
	for _, value := range fields {
		if !isJSONNull(value) {
			return true
		}
	}
	return false
}

func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

func disableParallelToolUseFromToolChoice(raw json.RawMessage) (bool, bool) {
	if len(raw) == 0 {
		return false, false
	}
	var tc struct {
		DisableParallelToolUse *bool `json:"disable_parallel_tool_use,omitempty"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil || tc.DisableParallelToolUse == nil {
		return false, false
	}
	return *tc.DisableParallelToolUse, true
}

// thinkingFields preserves only the supported Anthropic thinking controls.
// Keeping this separate from OpenAI/OpenRouter reasoning objects prevents
// protocol-specific fields from leaking across upstream wire formats.
func thinkingFields(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var thinking struct {
		Type         string `json:"type"`
		BudgetTokens any    `json:"budget_tokens"`
		Display      string `json:"display"`
	}
	if err := json.Unmarshal(raw, &thinking); err != nil {
		return nil
	}
	fields := map[string]any{}
	if value := strings.ToLower(strings.TrimSpace(thinking.Type)); value != "" {
		switch value {
		case "adaptive", "enabled", "disabled":
			fields["type"] = value
		}
	}
	if budget, ok := provider.PositiveIntValue(thinking.BudgetTokens); ok {
		fields["budget_tokens"] = budget
	}
	if value := strings.ToLower(strings.TrimSpace(thinking.Display)); value != "" {
		switch value {
		case "summarized", "omitted":
			fields["display"] = value
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func userIDFromMetadata(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var metadata struct {
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return ""
	}
	return strings.TrimSpace(metadata.UserID)
}

// fieldsFromOutputConfig extracts the current Anthropic effort and structured
// output controls so compatible providers can re-emit them upstream.
func fieldsFromOutputConfig(raw json.RawMessage) (string, any) {
	if len(raw) == 0 {
		return "", nil
	}
	var outputConfig struct {
		Effort string          `json:"effort"`
		Format json.RawMessage `json:"format"`
	}
	if err := json.Unmarshal(raw, &outputConfig); err != nil {
		return "", nil
	}
	if len(outputConfig.Format) == 0 {
		return strings.TrimSpace(outputConfig.Effort), nil
	}
	var format any
	if err := json.Unmarshal(outputConfig.Format, &format); err != nil {
		return strings.TrimSpace(outputConfig.Effort), nil
	}
	return strings.TrimSpace(outputConfig.Effort), format
}

func toolDefsToToolInfos(defs []ToolDefinition) []*schema.ToolInfo {
	tools := make([]*schema.ToolInfo, 0, len(defs))
	for _, td := range defs {
		// Anthropic server tools are identified by a versioned type and omit
		// input_schema. Keep them only in request-scoped ProtocolState; turning
		// them into a parameterless client tool would lose their execution model.
		if td.isServerTool() && isEmptyJSON(td.InputSchema) {
			continue
		}
		var js einojsonschema.Schema
		inputSchema, err := provider.NormalizeObjectToolInputSchema(td.InputSchema)
		if err != nil {
			tools = append(tools, &schema.ToolInfo{Name: td.Name, Desc: td.Description})
			continue
		}
		if err := json.Unmarshal(inputSchema, &js); err != nil {
			tools = append(tools, &schema.ToolInfo{Name: td.Name, Desc: td.Description})
			continue
		}
		tools = append(tools, &schema.ToolInfo{
			Name:        td.Name,
			Desc:        td.Description,
			ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&js),
		})
	}
	return tools
}

func isEmptyJSON(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null"
}

func parseAnthropicToolChoice(raw json.RawMessage) (schema.ToolChoice, []string, bool) {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name,omitempty"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return "", nil, false
	}
	switch tc.Type {
	case "auto":
		return schema.ToolChoiceAllowed, nil, true
	case "any":
		return schema.ToolChoiceForced, nil, true
	case "tool":
		return schema.ToolChoiceForced, []string{tc.Name}, true
	case "none":
		return schema.ToolChoiceForbidden, nil, true
	default:
		return "", nil, false
	}
}

// convertMessageItem converts one Anthropic MessageItem to one or more schema.Messages.
func convertMessageItem(m MessageItem, nativeBlocks []json.RawMessage) []*schema.Message {
	switch m.Role {
	case "assistant":
		return convertAssistantItem(m.Content, nativeBlocks)
	case "user":
		return convertUserItem(m.Content, nativeBlocks)
	default:
		// Treat unknown roles as user.
		return convertUserItem(m.Content, nativeBlocks)
	}
}

func convertAssistantItem(content MessageContent, nativeBlocks []json.RawMessage) []*schema.Message {
	var textParts []string
	var reasoningParts []string
	var structuredReasoning []schema.MessageOutputPart
	var toolCalls []schema.ToolCall
	for _, block := range content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				textParts = append(textParts, block.Text)
			}
		case "thinking":
			if block.Thinking != "" {
				reasoningParts = append(reasoningParts, block.Thinking)
			}
			if block.Thinking != "" || block.Signature != "" {
				structuredReasoning = append(structuredReasoning,
					provider.NewReasoningOutputPart(block.Thinking, block.Signature, nil))
			}
		case "redacted_thinking":
			if block.Data != "" {
				structuredReasoning = append(structuredReasoning,
					provider.NewEncryptedReasoningOutputPart(block.Data, nil))
			}
		case "tool_use":
			inputStr := ""
			if len(block.Input) > 0 {
				inputStr = string(block.Input)
			}
			toolCalls = append(toolCalls, schema.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: schema.FunctionCall{
					Name:      block.Name,
					Arguments: inputStr,
				},
			})
		}
	}

	if len(textParts) == 0 && len(structuredReasoning) == 0 && len(toolCalls) == 0 && len(nativeBlocks) == 0 {
		return nil
	}
	msg := &schema.Message{
		Role:             schema.Assistant,
		Content:          strings.Join(textParts, "\n"),
		ToolCalls:        toolCalls,
		ReasoningContent: strings.Join(reasoningParts, "\n"),
	}
	provider.AttachReasoningParts(msg, structuredReasoning...)
	anthropicbase.AttachAnthropicContentBlocks(msg, nativeBlocks)
	return []*schema.Message{msg}
}

func convertUserItem(content MessageContent, nativeBlocks []json.RawMessage) []*schema.Message {
	if len(nativeBlocks) > 0 {
		// Native replay must keep the original ordered user block array on one
		// message. In particular, tool_result associations remain present in the
		// attached raw blocks even though they are not duplicated as schema.Tool
		// messages (which would make the upstream result appear twice).
		msg := &schema.Message{Role: schema.User, Content: content.Text()}
		anthropicbase.AttachAnthropicContentBlocks(msg, nativeBlocks)
		return []*schema.Message{msg}
	}
	var inputParts []schema.MessageInputPart
	var result []*schema.Message

	flushInputParts := func() {
		if len(inputParts) == 0 {
			return
		}
		msg := &schema.Message{Role: schema.User}
		if len(inputParts) == 1 && inputParts[0].Type == schema.ChatMessagePartTypeText {
			msg.Content = inputParts[0].Text
		} else {
			msg.UserInputMultiContent = inputParts
		}
		result = append(result, msg)
		inputParts = nil
	}

	for _, block := range content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				inputParts = append(inputParts, schema.MessageInputPart{
					Type: schema.ChatMessagePartTypeText,
					Text: block.Text,
				})
			}
		case "image":
			if block.Source != nil {
				part := schema.MessageInputPart{
					Type:  schema.ChatMessagePartTypeImageURL,
					Image: &schema.MessageInputImage{},
				}
				switch block.Source.Type {
				case "base64":
					part.Image.Base64Data = &block.Source.Data
					part.Image.MIMEType = block.Source.MediaType
				case "url":
					part.Image.URL = &block.Source.URL
				}
				inputParts = append(inputParts, part)
			}
		case "tool_result":
			flushInputParts()
			result = append(result, &schema.Message{
				Role:       schema.Tool,
				Content:    block.Content.Text(),
				ToolCallID: block.ToolUseID,
			})
		}
	}
	flushInputParts()
	return result
}

func nativeContentBlocks(content MessageContent) []json.RawMessage {
	hasNative := false
	for _, block := range content {
		if block.hasUnmodeledFields() {
			hasNative = true
			continue
		}
		switch block.Type {
		case "text", "thinking", "redacted_thinking", "image", "tool_use", "tool_result":
		default:
			hasNative = true
		}
	}
	if !hasNative {
		return nil
	}
	blocks := make([]json.RawMessage, 0, len(content))
	for _, block := range content {
		blocks = append(blocks, block.rawJSON())
	}
	return blocks
}

func (b ContentBlock) hasUnmodeledFields() bool {
	if len(b.raw) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b.raw, &fields) != nil {
		// Raw native state that cannot be inspected must never be treated as
		// safely representable by the generic message model.
		return true
	}
	// Keep this list synchronized with ContentBlock's JSON fields. Anything not
	// represented by that struct requires exact Anthropic replay.
	for _, known := range []string{
		"type", "text", "thinking", "signature", "data", "source", "id", "name",
		"input", "tool_use_id", "content", "is_error", "cache_control",
	} {
		delete(fields, known)
	}
	for _, value := range fields {
		if !isJSONNull(value) {
			return true
		}
	}
	return false
}

// FromInternal converts an internal ChatResponse to an Anthropic MessagesResponse.
func (c *Converter) FromInternal(resp *provider.ChatResponse, model string) *MessagesResponse {
	content := contentFromMessage(resp.Message)
	usage := provider.UsageFromMessage(resp.Message)
	stopReason := mapFinishReason(provider.FinishReason(resp.Message))
	if stopReason == "" && resp.Message != nil && len(resp.Message.ToolCalls) > 0 {
		stopReason = "tool_use"
	}
	return &MessagesResponse{
		ID:         newAnthropicMessageID(),
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    content,
		StopReason: stopReason,
		Usage: UsageResponse{
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
		},
	}
}

// mapFinishReason maps OpenAI-style finish reasons to Anthropic stop reasons.
func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call", "tool_use":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	case "end_turn", "max_tokens", "stop_sequence":
		return reason
	default:
		if reason == "" {
			return ""
		}
		return "end_turn"
	}
}

func contentFromMessage(msg *schema.Message) []ContentBlockResponse {
	return reduceResponseItems(responseItemsFromMessage(msg))
}

func contentBlocksFromMessage(msg *schema.Message) []ContentBlockResponse {
	if msg == nil {
		return []ContentBlockResponse{}
	}
	if native := anthropicbase.AnthropicContentBlocksFromMessage(msg); len(native) > 0 {
		blocks := make([]ContentBlockResponse, 0, len(native))
		for _, raw := range native {
			blocks = append(blocks, ContentBlockResponse{Raw: raw})
		}
		return blocks
	}
	var blocks []ContentBlockResponse
	structuredReasoning := false
	for _, part := range provider.ReasoningPartsFromMessage(msg) {
		switch part.Type {
		case schema.ChatMessagePartTypeReasoning:
			if part.Reasoning == nil || (part.Reasoning.Text == "" && part.Reasoning.Signature == "") {
				continue
			}
			structuredReasoning = true
			signature := part.Reasoning.Signature
			if signature == "" {
				signature = gatewayThinkingSignature(part.Reasoning.Text)
			}
			blocks = append(blocks, ContentBlockResponse{
				Type:      "thinking",
				Thinking:  part.Reasoning.Text,
				Signature: signature,
			})
		case provider.ChatMessagePartTypeEncryptedReasoning:
			data := provider.EncryptedReasoningData(part)
			if data == "" {
				continue
			}
			structuredReasoning = true
			blocks = append(blocks, ContentBlockResponse{
				Type: "redacted_thinking",
				Data: data,
			})
		}
	}
	if !structuredReasoning && msg.ReasoningContent != "" {
		blocks = append(blocks, ContentBlockResponse{
			Type:      "thinking",
			Thinking:  msg.ReasoningContent,
			Signature: gatewayThinkingSignature(msg.ReasoningContent),
		})
	}
	if msg.Content != "" {
		blocks = append(blocks, ContentBlockResponse{Type: "text", Text: msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		// Arguments is already a JSON string; pass it through as raw JSON.
		var inputRaw json.RawMessage
		if tc.Function.Arguments != "" {
			inputRaw = json.RawMessage(tc.Function.Arguments)
		} else {
			inputRaw = json.RawMessage("{}")
		}
		blocks = append(blocks, ContentBlockResponse{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: inputRaw,
		})
	}
	return blocks
}

func gatewayThinkingSignature(reasoning string) string {
	return provider.GatewayThinkingSignature(reasoning)
}

func contentText(blocks []ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// --- Anthropic API types ---

type MessagesRequest struct {
	Model         string           `json:"model"`
	MaxTokens     int              `json:"max_tokens"`
	Messages      []MessageItem    `json:"messages"`
	System        MessageContent   `json:"system,omitempty"`
	Temperature   float64          `json:"temperature,omitempty"`
	TopP          float64          `json:"top_p,omitempty"`
	TopK          int              `json:"top_k,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
	StopSequences []string         `json:"stop_sequences,omitempty"`
	Tools         []ToolDefinition `json:"tools,omitempty"`
	ToolChoice    json.RawMessage  `json:"tool_choice,omitempty"`
	Metadata      json.RawMessage  `json:"metadata,omitempty"`
	Thinking      json.RawMessage  `json:"thinking,omitempty"`
	OutputConfig  json.RawMessage  `json:"output_config,omitempty"`
}

type MessageItem struct {
	Role    string         `json:"role"`
	Content MessageContent `json:"content"`
}

// MessageContent holds the content of a message. Per the Anthropic API spec,
// content may be either a plain string or an array of content blocks.
type MessageContent []ContentBlock

// UnmarshalJSON accepts both a JSON string and a JSON array of content blocks.
func (mc *MessageContent) UnmarshalJSON(data []byte) error {
	var blocks []ContentBlock
	if err := json.Unmarshal(data, &blocks); err == nil {
		*mc = blocks
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s != "" {
		*mc = []ContentBlock{{Type: "text", Text: s}}
	}
	return nil
}

// Text joins all text blocks into a single string.
func (mc MessageContent) Text() string {
	return contentText(mc)
}

type ContentBlock struct {
	Type string `json:"type"`
	// type=text
	Text string `json:"text,omitempty"`
	// type=thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	// type=redacted_thinking
	Data string `json:"data,omitempty"`
	// type=image
	Source *ImageSource `json:"source,omitempty"`
	// type=tool_use (assistant messages)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// type=tool_result (user messages)
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   MessageContent `json:"content,omitempty"`
	IsError   bool           `json:"is_error,omitempty"`
	// shared
	CacheControl *CacheControl `json:"cache_control,omitempty"`
	raw          json.RawMessage
}

func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	type wireContentBlock ContentBlock
	var decoded wireContentBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*b = ContentBlock(decoded)
	b.raw = append(b.raw[:0], data...)
	return nil
}

func (b ContentBlock) rawJSON() json.RawMessage {
	if len(b.raw) > 0 {
		return append(json.RawMessage(nil), b.raw...)
	}
	type wireContentBlock ContentBlock
	raw, _ := json.Marshal(wireContentBlock(b))
	return raw
}

type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type CacheControl struct {
	Type string `json:"type"`
}

type ToolDefinition struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	raw         json.RawMessage
}

func (t *ToolDefinition) UnmarshalJSON(data []byte) error {
	type wireToolDefinition ToolDefinition
	var decoded wireToolDefinition
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*t = ToolDefinition(decoded)
	t.raw = append(t.raw[:0], data...)
	return nil
}

func (t ToolDefinition) rawJSON() json.RawMessage {
	if t.isServerTool() {
		if len(t.raw) > 0 {
			return append(json.RawMessage(nil), t.raw...)
		}
		type wireToolDefinition ToolDefinition
		raw, _ := json.Marshal(wireToolDefinition(t))
		return raw
	}
	if !isEmptyJSON(t.InputSchema) {
		if len(t.raw) > 0 {
			return append(json.RawMessage(nil), t.raw...)
		}
		type wireToolDefinition ToolDefinition
		raw, _ := json.Marshal(wireToolDefinition(t))
		return raw
	}
	if len(t.raw) > 0 {
		var fields map[string]any
		if json.Unmarshal(t.raw, &fields) == nil {
			normalized, _ := provider.NormalizeObjectToolInputSchema(nil)
			var schemaValue any
			_ = json.Unmarshal(normalized, &schemaValue)
			fields["input_schema"] = schemaValue
			raw, _ := json.Marshal(fields)
			return raw
		}
	}
	t.InputSchema, _ = provider.NormalizeObjectToolInputSchema(nil)
	type wireToolDefinition ToolDefinition
	raw, _ := json.Marshal(wireToolDefinition(t))
	return raw
}

func (t ToolDefinition) isServerTool() bool {
	typ := strings.TrimSpace(t.Type)
	return typ != "" && typ != "custom"
}

type MessagesResponse struct {
	ID         string                 `json:"id"`
	Type       string                 `json:"type"`
	Role       string                 `json:"role"`
	Content    []ContentBlockResponse `json:"content"`
	Model      string                 `json:"model"`
	StopReason string                 `json:"stop_reason,omitempty"`
	Usage      UsageResponse          `json:"usage"`
}

type ContentBlockResponse struct {
	Type string `json:"type"`
	// type=text
	Text string `json:"text,omitempty"`
	// type=thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	// type=redacted_thinking
	Data string `json:"data,omitempty"`
	// type=tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	Raw   json.RawMessage `json:"-"`
}

func (b ContentBlockResponse) MarshalJSON() ([]byte, error) {
	if len(b.Raw) > 0 {
		return b.Raw, nil
	}
	type wireContentBlockResponse ContentBlockResponse
	return json.Marshal(wireContentBlockResponse(b))
}

type UsageResponse struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
