package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agent-guide/agent-gateway/internal/statuserr"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	einojsonschema "github.com/eino-contrib/jsonschema"
)

// CreateResponsesViaChat adapts a Responses API request onto the provider Chat API.
// Providers opt into this compatibility path explicitly by calling this helper.
func CreateResponsesViaChat(ctx context.Context, prov Provider, req *ResponsesRequest) (*ResponsesResponse, error) {
	chatReq, err := ResponsesToChatRequest(req)
	if err != nil {
		return nil, err
	}
	resp, err := prov.Chat(ctx, chatReq)
	if err != nil {
		return nil, err
	}
	return ResponsesFromChatResponse(resp, chatReq.Model), nil
}

// StreamResponsesViaChat adapts a streaming Responses API request onto the provider
// Chat stream API. Providers opt into this compatibility path explicitly by
// calling this helper.
func StreamResponsesViaChat(ctx context.Context, prov Provider, req *ResponsesRequest) (*schema.StreamReader[*ResponsesStreamEvent], error) {
	chatReq, err := ResponsesToChatRequest(req)
	if err != nil {
		return nil, err
	}
	stream, err := prov.StreamChat(ctx, chatReq)
	if err != nil {
		return nil, err
	}

	sr, sw := schema.Pipe[*ResponsesStreamEvent](16)
	go func() {
		defer stream.Close()
		defer sw.Close()

		resp := ResponsesFromChatResponse(&ChatResponse{}, chatReq.Model)
		resp.Output = nil
		sw.Send(ResponsesCreatedEvent(resp), nil)

		toolOutputIndexByKey := map[string]int{}
		var toolOutputIndexes []int
		fallbackToolCallKey := ""
		textOutputIndex := -1
		reasoningOutputIndex := -1
		reasoningDone := false
		nonReasoningStarted := false
		finishReasoning := func() {
			if reasoningOutputIndex < 0 || reasoningDone {
				return
			}
			item := resp.Output[reasoningOutputIndex]
			sw.Send(ResponsesReasoningSummaryPartDoneEvent(item.ID, reasoningOutputIndex, &item.Summary[0]), nil)
			sw.Send(ResponsesOutputItemDoneEvent(reasoningOutputIndex, &item), nil)
			reasoningDone = true
		}

		for {
			chunk, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					finishReasoning()
					if textOutputIndex >= 0 {
						item := resp.Output[textOutputIndex]
						sw.Send(ResponsesOutputItemDoneEvent(textOutputIndex, &item), nil)
					}
					for _, outputIndex := range toolOutputIndexes {
						item := resp.Output[outputIndex]
						sw.Send(ResponsesOutputItemDoneEvent(outputIndex, &item), nil)
					}
					sw.Send(ResponsesCompletedEvent(resp), nil)
					return
				}
				sw.Send(ResponsesCompletedEvent(resp), err)
				return
			}

			if chunk == nil {
				continue
			}
			// Chat-compatible reasoning is expected to precede text and tools. A
			// Responses item cannot be reopened after its done event, so discard
			// malformed late reasoning rather than emit an invalid event sequence.
			if reasoning := reasoningText(chunk); reasoning != "" && !nonReasoningStarted {
				if reasoningOutputIndex < 0 {
					item := reasoningOutput("")
					resp.Output = append(resp.Output, item)
					reasoningOutputIndex = len(resp.Output) - 1
					sw.Send(ResponsesOutputItemAddedEvent(reasoningOutputIndex, &item), nil)
					sw.Send(ResponsesReasoningSummaryPartAddedEvent(item.ID, reasoningOutputIndex, &item.Summary[0]), nil)
				}
				resp.Output[reasoningOutputIndex].Summary[0].Text += reasoning
				sw.Send(ResponsesReasoningSummaryDeltaEvent(resp.Output[reasoningOutputIndex].ID, reasoningOutputIndex, reasoning), nil)
			}
			if text := messageText(chunk); text != "" {
				finishReasoning()
				nonReasoningStarted = true
				ensureTextOutput(resp)
				textIndex := firstTextOutputIndex(resp)
				if textOutputIndex < 0 {
					textOutputIndex = textIndex
					item := resp.Output[textIndex]
					sw.Send(ResponsesOutputItemAddedEvent(textIndex, &item), nil)
				}
				resp.Output[textIndex].Content[0].Text += text
				sw.Send(ResponsesDeltaEvent(resp.Output[textIndex].ID, textIndex, text), nil)
			}
			for _, tc := range chunk.ToolCalls {
				finishReasoning()
				nonReasoningStarted = true
				key := toolCallKey(tc, &fallbackToolCallKey)
				outputIndex, seen := toolOutputIndexByKey[key]
				if !seen {
					item := functionCallOutputFromToolCall(tc)
					resp.Output = append(resp.Output, item)
					outputIndex = len(resp.Output) - 1
					toolOutputIndexByKey[key] = outputIndex
					toolOutputIndexes = append(toolOutputIndexes, outputIndex)
					sw.Send(ResponsesOutputItemAddedEvent(outputIndex, &item), nil)
				} else {
					mergeToolCallInto(&resp.Output[outputIndex], tc)
				}
				if tc.Function.Arguments != "" {
					sw.Send(ResponsesFunctionCallArgumentsDeltaEvent(resp.Output[outputIndex].ID, outputIndex, tc.Function.Arguments), nil)
				}
			}
			resp.Usage = responsesUsageFromMessage(chunk)
		}
	}()
	return sr, nil
}

// toolCallKey returns a key that is stable across streamed tool-call deltas.
// eino sets Index in stream mode; ID is the next-best correlator, and a
// single pending fallback call is the last resort for fragments that carry
// neither. Multiple concurrent tool calls without either Index or ID cannot be
// safely disambiguated, so preserving one merged call is less lossy than
// emitting every argument fragment as a separate output item.
func toolCallKey(tc schema.ToolCall, fallback *string) string {
	if tc.Index != nil {
		return "idx:" + strconv.Itoa(*tc.Index)
	}
	if tc.ID != "" {
		return "id:" + tc.ID
	}
	if *fallback == "" {
		*fallback = "seq:0"
	}
	return *fallback
}

func mergeToolCallInto(item *ResponsesResponseOutput, tc schema.ToolCall) {
	if item.ID == "" && tc.ID != "" {
		item.ID = tc.ID
		item.CallID = tc.ID
	}
	if item.Name == "" && tc.Function.Name != "" {
		item.Name = tc.Function.Name
	}
	item.Arguments += tc.Function.Arguments
}

// ResponsesToChatRequest converts a minimal Responses API request into ChatRequest.
func ResponsesToChatRequest(req *ResponsesRequest) (*ChatRequest, error) {
	if req == nil {
		return nil, statuserr.New(http.StatusBadRequest, "invalid request")
	}

	messages, err := responsesInputToMessages(req.Input)
	if err != nil {
		return nil, err
	}
	messages = coalesceResponsesToolCalls(messages)

	chatReq := &ChatRequest{
		Model:    req.Model,
		Messages: messages,
	}
	if instructions := strings.TrimSpace(req.Instructions); instructions != "" {
		chatReq.Messages = append([]*schema.Message{{
			Role:    schema.System,
			Content: instructions,
		}}, chatReq.Messages...)
	}

	var opts []einomodel.Option
	if req.Temperature != 0 {
		opts = append(opts, einomodel.WithTemperature(float32(req.Temperature)))
	}
	if req.TopP != 0 {
		opts = append(opts, einomodel.WithTopP(float32(req.TopP)))
	}
	if req.MaxOutputTokens > 0 {
		opts = append(opts, einomodel.WithMaxTokens(req.MaxOutputTokens))
	}
	if len(req.Tools) > 0 {
		opts = append(opts, einomodel.WithTools(responsesToolDefsToToolInfos(req.Tools)))
	}
	if len(req.ToolChoice) > 0 {
		if tc, names, ok := parseResponsesToolChoice(req.ToolChoice); ok {
			opts = append(opts, einomodel.WithToolChoice(tc, names...))
		}
	}
	if ctx := responsesRequestContextFromRequest(req); ctx != nil {
		opts = append(opts, WithResponsesRequestContext(ctx))
	}
	chatReq.Options = opts
	return chatReq, nil
}

// ResponsesRequestFromChatState rebuilds a provider-level Responses request from
// a resolved ChatRequestState. This is the generic inverse of the
// ResponsesToChatRequest compatibility path and is intended for providers that
// bridge chat calls onto an upstream Responses API.
func ResponsesRequestFromChatState(state *ChatRequestState, stream bool) *ResponsesRequest {
	instructions, input := splitInstructionsAndInput(state.Messages)
	req := &ResponsesRequest{
		Model:        state.ModelName,
		Input:        input,
		Instructions: instructions,
		Stream:       stream,
	}
	if state.CommonOptions != nil {
		if state.CommonOptions.Temperature != nil {
			req.Temperature = float64(*state.CommonOptions.Temperature)
		}
		if state.CommonOptions.TopP != nil {
			req.TopP = float64(*state.CommonOptions.TopP)
		}
		if state.CommonOptions.MaxTokens != nil {
			req.MaxOutputTokens = *state.CommonOptions.MaxTokens
		}
		if len(state.CommonOptions.Tools) > 0 {
			req.Tools = toolInfosToResponsesToolDefs(state.CommonOptions.Tools)
		}
		if state.CommonOptions.ToolChoice != nil {
			req.ToolChoice = buildResponsesToolChoice(state.CommonOptions.ToolChoice, state.CommonOptions.AllowedToolNames)
		}
	}
	if ctx := ResponsesRequestContextFromOptions(state.Options...); ctx != nil {
		req.PreviousResponseID = ctx.PreviousResponseID
		req.Store = cloneBoolPtr(ctx.Store)
		req.Text = cloneMap(ctx.Text)
		req.Metadata = cloneMap(ctx.Metadata)
		req.User = ctx.User
		req.Reasoning = cloneMap(ctx.Reasoning)
		req.Truncation = ctx.Truncation
		if ctx.ParallelToolCalls != nil {
			v := *ctx.ParallelToolCalls
			req.ParallelToolCalls = &v
		}
	}
	if extra := ChatExtraFieldsFromOptions(state.Options...); extra != nil {
		// ChatExtraFields are the protocol-level chat fields captured from the
		// original request, so they intentionally override overlapping context
		// that may have been produced by a prior Responses compatibility hop.
		if extra.ResponseFormat != nil {
			req.Text = responseTextFromChatResponseFormat(extra.ResponseFormat)
		}
		if len(extra.Reasoning) > 0 {
			req.Reasoning = cloneMap(extra.Reasoning)
		}
		if effort := strings.TrimSpace(extra.ReasoningEffort); effort != "" {
			if req.Reasoning == nil {
				req.Reasoning = map[string]any{}
			}
			req.Reasoning["effort"] = effort
		}
		if user := strings.TrimSpace(extra.User); user != "" {
			req.User = user
		}
		if len(extra.Metadata) > 0 {
			req.Metadata = cloneMap(extra.Metadata)
		}
		if extra.ParallelToolCalls != nil {
			v := *extra.ParallelToolCalls
			req.ParallelToolCalls = &v
		}
		if extra.Store != nil {
			v := *extra.Store
			req.Store = &v
		}
	}
	return req
}

// ResponsesFromChatResponse converts a Chat response into a minimal Responses envelope.
func ResponsesFromChatResponse(resp *ChatResponse, model string) *ResponsesResponse {
	var msg *schema.Message
	if resp != nil {
		msg = resp.Message
	}
	now := time.Now()
	out := &ResponsesResponse{
		ID:        fmt.Sprintf("resp_%d", now.UnixNano()),
		Object:    "response",
		CreatedAt: now.Unix(),
		Model:     model,
		Usage:     responsesUsageFromMessage(msg),
	}
	if text := messageText(msg); text != "" {
		out.Output = append(out.Output, ResponsesResponseOutput{
			ID:     fmt.Sprintf("msg_%d", now.UnixNano()),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponsesResponseContentPart{{
				Type:        "output_text",
				Text:        text,
				Annotations: []any{},
			}},
		})
	}
	if reasoning := reasoningText(msg); reasoning != "" {
		out.Output = append([]ResponsesResponseOutput{reasoningOutput(reasoning)}, out.Output...)
	}
	for _, tc := range toolCallsOrEmpty(msg) {
		out.Output = append(out.Output, functionCallOutputFromToolCall(tc))
	}
	if len(out.Output) == 0 {
		out.Output = []ResponsesResponseOutput{emptyTextOutput()}
	}
	return out
}

func ResponsesCreatedEvent(resp *ResponsesResponse) *ResponsesStreamEvent {
	return &ResponsesStreamEvent{
		Type:     "response.created",
		Response: resp,
	}
}

func ResponsesDeltaEvent(itemID string, outputIndex int, delta string) *ResponsesStreamEvent {
	return &ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		ItemID:       itemID,
		OutputIndex:  outputIndex,
		ContentIndex: 0,
		Delta:        delta,
	}
}

func ResponsesReasoningSummaryPartAddedEvent(itemID string, outputIndex int, part *ResponsesReasoningSummaryPart) *ResponsesStreamEvent {
	return &ResponsesStreamEvent{
		Type: "response.reasoning_summary_part.added", ItemID: itemID,
		OutputIndex: outputIndex, SummaryIndex: 0, Part: part,
	}
}

func ResponsesReasoningSummaryDeltaEvent(itemID string, outputIndex int, delta string) *ResponsesStreamEvent {
	return &ResponsesStreamEvent{
		Type: "response.reasoning_summary_text.delta", ItemID: itemID,
		OutputIndex: outputIndex, SummaryIndex: 0, Delta: delta,
	}
}

func ResponsesReasoningSummaryPartDoneEvent(itemID string, outputIndex int, part *ResponsesReasoningSummaryPart) *ResponsesStreamEvent {
	return &ResponsesStreamEvent{
		Type: "response.reasoning_summary_part.done", ItemID: itemID,
		OutputIndex: outputIndex, SummaryIndex: 0, Part: part,
	}
}

func ResponsesOutputItemAddedEvent(outputIndex int, item *ResponsesResponseOutput) *ResponsesStreamEvent {
	return &ResponsesStreamEvent{
		Type:        "response.output_item.added",
		ItemID:      item.ID,
		OutputIndex: outputIndex,
		Item:        item,
	}
}

func ResponsesOutputItemDoneEvent(outputIndex int, item *ResponsesResponseOutput) *ResponsesStreamEvent {
	return &ResponsesStreamEvent{
		Type:        "response.output_item.done",
		ItemID:      item.ID,
		OutputIndex: outputIndex,
		Item:        item,
	}
}

func ResponsesFunctionCallArgumentsDeltaEvent(itemID string, outputIndex int, delta string) *ResponsesStreamEvent {
	return &ResponsesStreamEvent{
		Type:        "response.function_call_arguments.delta",
		ItemID:      itemID,
		OutputIndex: outputIndex,
		Delta:       delta,
	}
}

func ResponsesCompletedEvent(resp *ResponsesResponse) *ResponsesStreamEvent {
	return &ResponsesStreamEvent{
		Type:     "response.completed",
		Response: resp,
	}
}

func responsesInputToMessages(input any) ([]*schema.Message, error) {
	switch v := input.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, statuserr.New(http.StatusBadRequest, "input is required")
		}
		return []*schema.Message{{
			Role:    schema.User,
			Content: v,
		}}, nil
	case []any:
		msgs := make([]*schema.Message, 0, len(v))
		for _, item := range v {
			msg, err := responseInputItemToMessage(item)
			if err != nil {
				return nil, err
			}
			if msg != nil {
				msgs = append(msgs, msg)
			}
		}
		if len(msgs) == 0 {
			return nil, statuserr.New(http.StatusBadRequest, "input is required")
		}
		return msgs, nil
	default:
		return nil, statuserr.New(http.StatusBadRequest, "unsupported input type for responses api")
	}
}

// coalesceResponsesToolCalls merges a run of consecutive assistant messages
// that carry tool calls into a single assistant message. The Responses API
// emits one function_call input item per parallel tool call, which
// responseInputItemToMessage turns into one assistant message each. Chat
// Completions instead requires a single assistant message whose tool_calls are
// all answered by the tool messages that immediately follow; leaving them split
// makes strict upstreams (e.g. deepseek) reject the request because the first
// assistant tool_calls message is followed by another assistant message instead
// of its tool replies.
func coalesceResponsesToolCalls(messages []*schema.Message) []*schema.Message {
	out := make([]*schema.Message, 0, len(messages))
	for _, msg := range messages {
		if msg != nil && msg.Role == schema.Assistant && len(out) > 0 {
			if prev := out[len(out)-1]; prev.Role == schema.Assistant &&
				(len(prev.ToolCalls) > 0 || len(msg.ToolCalls) > 0 || prev.ReasoningContent != "" || msg.ReasoningContent != "") {
				prev.ToolCalls = append(prev.ToolCalls, msg.ToolCalls...)
				if msg.ReasoningContent != "" {
					if prev.ReasoningContent != "" {
						prev.ReasoningContent += "\n" + msg.ReasoningContent
					} else {
						prev.ReasoningContent = msg.ReasoningContent
					}
				}
				if text := strings.TrimSpace(msg.Content); text != "" {
					if strings.TrimSpace(prev.Content) != "" {
						prev.Content += "\n" + msg.Content
					} else {
						prev.Content = msg.Content
					}
				}
				continue
			}
		}
		out = append(out, msg)
	}
	return out
}

func splitInstructionsAndInput(messages []*schema.Message) (string, []any) {
	var instructions []string
	items := make([]any, 0, len(messages))
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		if msg.Role == schema.System {
			if text := strings.TrimSpace(msg.Content); text != "" {
				instructions = append(instructions, text)
			}
			continue
		}
		items = append(items, responsesInputItemsFromMessage(msg)...)
	}
	return strings.Join(instructions, "\n"), items
}

func responsesInputItemsFromMessage(msg *schema.Message) []any {
	if msg == nil {
		return nil
	}
	if msg.Role == schema.Tool {
		if strings.TrimSpace(msg.ToolCallID) == "" {
			return []any{map[string]any{
				"type":    "message",
				"role":    string(schema.User),
				"content": responsesContentPartsFromMessage(msg),
			}}
		}
		return []any{map[string]any{
			"type":    "function_call_output",
			"call_id": msg.ToolCallID,
			"output":  msg.Content,
		}}
	}

	items := make([]any, 0, 1+len(msg.ToolCalls))
	if msg.Content != "" || len(msg.UserInputMultiContent) > 0 || len(msg.ToolCalls) == 0 {
		items = append(items, map[string]any{
			"type":    "message",
			"role":    string(msg.Role),
			"content": responsesContentPartsFromMessage(msg),
		})
	}
	for _, tc := range msg.ToolCalls {
		callID := strings.TrimSpace(tc.ID)
		if callID == "" {
			callID = strings.TrimSpace(tc.Function.Name)
		}
		if callID == "" || strings.TrimSpace(tc.Function.Name) == "" {
			continue
		}
		items = append(items, map[string]any{
			"type":      "function_call",
			"call_id":   callID,
			"name":      tc.Function.Name,
			"arguments": tc.Function.Arguments,
		})
	}
	return items
}

func responsesContentPartsFromMessage(msg *schema.Message) []any {
	if len(msg.UserInputMultiContent) > 0 {
		parts := make([]any, 0, len(msg.UserInputMultiContent))
		for _, part := range msg.UserInputMultiContent {
			switch part.Type {
			case schema.ChatMessagePartTypeText:
				parts = append(parts, map[string]any{"type": "input_text", "text": part.Text})
			case schema.ChatMessagePartTypeImageURL:
				if part.Image == nil || part.Image.URL == nil {
					continue
				}
				image := map[string]any{"type": "input_image", "image_url": *part.Image.URL}
				if detail := string(part.Image.Detail); detail != "" {
					image["detail"] = detail
				}
				parts = append(parts, image)
			}
		}
		if len(parts) > 0 {
			return parts
		}
	}
	return []any{map[string]any{"type": "input_text", "text": msg.Content}}
}

func responseInputItemToMessage(item any) (*schema.Message, error) {
	obj, ok := item.(map[string]any)
	if !ok {
		return nil, statuserr.New(http.StatusBadRequest, "unsupported input item type for responses api")
	}

	role, _ := obj["role"].(string)
	if typ, _ := obj["type"].(string); typ == "reasoning" {
		return &schema.Message{Role: schema.Assistant, ReasoningContent: reasoningTextFromResponseItem(obj)}, nil
	} else if typ == "function_call_output" {
		output := responsesTextFromValue(obj["output"])
		callID, _ := obj["call_id"].(string)
		return &schema.Message{Role: schema.Tool, Content: output, ToolCallID: callID}, nil
	} else if typ == "function_call" {
		callID, _ := obj["call_id"].(string)
		if strings.TrimSpace(callID) == "" {
			callID, _ = obj["id"].(string)
		}
		name, _ := obj["name"].(string)
		arguments := responsesTextFromValue(obj["arguments"])
		if strings.TrimSpace(name) == "" && strings.TrimSpace(callID) == "" && strings.TrimSpace(arguments) == "" {
			return nil, nil
		}
		return &schema.Message{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{{
				ID:   callID,
				Type: "function",
				Function: schema.FunctionCall{
					Name:      name,
					Arguments: arguments,
				},
			}},
		}, nil
	}
	if strings.TrimSpace(role) == "" {
		role = string(schema.User)
	}

	switch content := obj["content"].(type) {
	case string:
		return &schema.Message{Role: schema.RoleType(role), Content: content}, nil
	case []any:
		var textParts []string
		var inputParts []schema.MessageInputPart
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok {
				return nil, statuserr.New(http.StatusBadRequest, "unsupported input content part for responses api")
			}
			partType, _ := part["type"].(string)
			switch partType {
			case "", "input_text", "output_text", "text":
				text, _ := part["text"].(string)
				if strings.TrimSpace(text) != "" {
					textParts = append(textParts, text)
					inputParts = append(inputParts, schema.MessageInputPart{
						Type: schema.ChatMessagePartTypeText,
						Text: text,
					})
				}
			case "input_image", "image_url":
				url, detail, err := imageURLFromInputPart(part)
				if err != nil {
					return nil, err
				}
				inputParts = append(inputParts, schema.MessageInputPart{
					Type: schema.ChatMessagePartTypeImageURL,
					Image: &schema.MessageInputImage{
						MessagePartCommon: schema.MessagePartCommon{URL: &url},
						Detail:            schema.ImageURLDetail(detail),
					},
				})
			default:
				return nil, statuserr.New(http.StatusBadRequest, fmt.Sprintf("unsupported input content type %q for responses api", partType))
			}
		}
		msg := &schema.Message{Role: schema.RoleType(role)}
		if len(inputParts) == 1 && inputParts[0].Type == schema.ChatMessagePartTypeText {
			msg.Content = inputParts[0].Text
			return msg, nil
		}
		if msg.Role != schema.User {
			msg.Content = strings.Join(textParts, "\n")
			return msg, nil
		}
		if len(inputParts) > 0 {
			if inputPartsAreTextOnly(inputParts) {
				msg.Content = strings.Join(textParts, "\n")
				return msg, nil
			}
			msg.UserInputMultiContent = inputParts
			return msg, nil
		}
		return &schema.Message{Role: schema.RoleType(role), Content: strings.Join(textParts, "\n")}, nil
	default:
		return nil, statuserr.New(http.StatusBadRequest, "unsupported input content for responses api")
	}
}

func inputPartsAreTextOnly(parts []schema.MessageInputPart) bool {
	for _, part := range parts {
		if part.Type != schema.ChatMessagePartTypeText {
			return false
		}
	}
	return true
}

func responsesTextFromValue(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	case []any:
		var parts []string
		for _, raw := range typed {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			text, _ := part["text"].(string)
			if strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text, _ := typed["text"].(string); strings.TrimSpace(text) != "" {
			return text
		}
	}
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func reasoningTextFromResponseItem(item map[string]any) string {
	if summary, ok := item["summary"].([]any); ok {
		return responsesTextFromValue(summary)
	}
	if content, ok := item["content"]; ok {
		return responsesTextFromValue(content)
	}
	return ""
}

func responseTextFromChatResponseFormat(responseFormat any) map[string]any {
	if responseFormat == nil {
		return nil
	}
	format, ok := responseFormat.(map[string]any)
	if !ok {
		return map[string]any{"format": responseFormat}
	}
	if formatType, _ := format["type"].(string); formatType != "json_schema" {
		return map[string]any{"format": cloneMap(format)}
	}
	out := map[string]any{"type": "json_schema"}
	if nested, _ := format["json_schema"].(map[string]any); len(nested) > 0 {
		for k, v := range nested {
			out[k] = v
		}
	} else {
		for k, v := range format {
			if k != "json_schema" {
				out[k] = v
			}
		}
	}
	return map[string]any{"format": out}
}

// imageURLFromInputPart extracts an image URL and optional detail from a
// Responses input_image content part. It accepts both the canonical Responses
// shape (image_url as a string with a sibling detail) and the chat-style object
// shape (image_url as {url, detail}).
func imageURLFromInputPart(part map[string]any) (string, string, error) {
	switch v := part["image_url"].(type) {
	case string:
		url := strings.TrimSpace(v)
		if url == "" {
			return "", "", statuserr.New(http.StatusBadRequest, "input_image requires image_url")
		}
		detail, _ := part["detail"].(string)
		return url, detail, nil
	case map[string]any:
		url, _ := v["url"].(string)
		if strings.TrimSpace(url) == "" {
			return "", "", statuserr.New(http.StatusBadRequest, "input_image requires image_url.url")
		}
		detail, _ := v["detail"].(string)
		return url, detail, nil
	default:
		return "", "", statuserr.New(http.StatusBadRequest, "input_image requires image_url")
	}
}

func responsesToolDefsToToolInfos(defs []ResponsesToolDefinition) []*schema.ToolInfo {
	tools := make([]*schema.ToolInfo, 0, len(defs))
	for _, td := range defs {
		name := strings.TrimSpace(td.Name)
		desc := td.Description
		params := td.Parameters
		if td.Function != nil {
			name = strings.TrimSpace(td.Function.Name)
			desc = td.Function.Description
			params = td.Function.Parameters
		}
		if td.Type != "" && td.Type != "function" {
			continue
		}
		if name == "" {
			continue
		}
		if len(params) == 0 {
			tools = append(tools, &schema.ToolInfo{Name: name, Desc: desc})
			continue
		}
		var js einojsonschema.Schema
		if err := json.Unmarshal(params, &js); err != nil {
			tools = append(tools, &schema.ToolInfo{Name: name, Desc: desc})
			continue
		}
		tools = append(tools, &schema.ToolInfo{
			Name:        name,
			Desc:        desc,
			ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&js),
		})
	}
	return tools
}

func toolInfosToResponsesToolDefs(defs []*schema.ToolInfo) []ResponsesToolDefinition {
	tools := make([]ResponsesToolDefinition, 0, len(defs))
	for _, td := range defs {
		if td == nil {
			continue
		}
		var params json.RawMessage
		if td.ParamsOneOf != nil {
			js, err := td.ParamsOneOf.ToJSONSchema()
			if err == nil && js != nil {
				raw, err := json.Marshal(js)
				if err == nil {
					params = raw
				}
			}
		}
		tools = append(tools, ResponsesToolDefinition{
			Type: "function",
			Function: &ResponsesToolFunction{
				Name:        td.Name,
				Description: td.Desc,
				Parameters:  params,
			},
		})
	}
	return tools
}

func parseResponsesToolChoice(raw json.RawMessage) (schema.ToolChoice, []string, bool) {
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
		Name     string `json:"name,omitempty"`
		Function struct {
			Name string `json:"name"`
		} `json:"function,omitempty"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return "", nil, false
	}
	name := strings.TrimSpace(tc.Name)
	if name == "" {
		name = strings.TrimSpace(tc.Function.Name)
	}
	if (tc.Type == "function" || tc.Type == "tool") && name != "" {
		return schema.ToolChoiceForced, []string{name}, true
	}
	return "", nil, false
}

func buildResponsesToolChoice(choice *schema.ToolChoice, allowed []string) json.RawMessage {
	if choice == nil {
		return nil
	}
	payload := map[string]any{}
	switch *choice {
	case schema.ToolChoiceForced:
		if len(allowed) == 1 && strings.TrimSpace(allowed[0]) != "" {
			payload["type"] = "function"
			payload["name"] = allowed[0]
		} else {
			payload["type"] = "required"
		}
	case schema.ToolChoiceForbidden:
		payload["type"] = "none"
	default:
		payload["type"] = "auto"
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return raw
}

func responsesRequestContextFromRequest(req *ResponsesRequest) *ResponsesRequestContext {
	if req == nil {
		return nil
	}
	if strings.TrimSpace(req.PreviousResponseID) == "" && req.Store == nil &&
		len(req.Text) == 0 && len(req.Metadata) == 0 && strings.TrimSpace(req.User) == "" &&
		len(req.Reasoning) == 0 && req.ParallelToolCalls == nil && req.Truncation == nil {
		return nil
	}
	return &ResponsesRequestContext{
		PreviousResponseID: strings.TrimSpace(req.PreviousResponseID),
		Store:              cloneBoolPtr(req.Store),
		Text:               cloneMap(req.Text),
		Metadata:           cloneMap(req.Metadata),
		User:               req.User,
		Reasoning:          cloneMap(req.Reasoning),
		ParallelToolCalls:  req.ParallelToolCalls,
		Truncation:         req.Truncation,
	}
}

func cloneBoolPtr(src *bool) *bool {
	if src == nil {
		return nil
	}
	v := *src
	return &v
}

func responsesUsageFromMessage(msg *schema.Message) *ResponsesResponseUsage {
	usage := UsageFromMessage(msg)
	return &ResponsesResponseUsage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.TotalTokens,
		InputTokensDetails: ResponsesInputTokensUsage{
			CachedTokens: usage.CachedTokens,
		},
		OutputTokensDetails: ResponsesOutputTokensUsage{
			ReasoningTokens: usage.ReasoningTokens,
		},
	}
}

func messageText(msg *schema.Message) string {
	if msg == nil {
		return ""
	}
	return msg.Content
}

func reasoningText(msg *schema.Message) string {
	if msg == nil {
		return ""
	}
	return msg.ReasoningContent
}

func toolCallsOrEmpty(msg *schema.Message) []schema.ToolCall {
	if msg == nil || len(msg.ToolCalls) == 0 {
		return nil
	}
	return msg.ToolCalls
}

func functionCallOutputFromToolCall(tc schema.ToolCall) ResponsesResponseOutput {
	return ResponsesResponseOutput{
		ID:        tc.ID,
		Type:      "function_call",
		Status:    "completed",
		CallID:    tc.ID,
		Name:      tc.Function.Name,
		Arguments: tc.Function.Arguments,
	}
}

func emptyTextOutput() ResponsesResponseOutput {
	return ResponsesResponseOutput{
		ID:     fmt.Sprintf("msg_%d", time.Now().UnixNano()),
		Type:   "message",
		Role:   "assistant",
		Status: "completed",
		Content: []ResponsesResponseContentPart{{
			Type:        "output_text",
			Text:        "",
			Annotations: []any{},
		}},
	}
}

func reasoningOutput(reasoning string) ResponsesResponseOutput {
	return ResponsesResponseOutput{
		ID:     fmt.Sprintf("rs_%d", time.Now().UnixNano()),
		Type:   "reasoning",
		Status: "completed",
		Summary: []ResponsesReasoningSummaryPart{{
			Type: "summary_text",
			Text: reasoning,
		}},
	}
}

func ensureTextOutput(resp *ResponsesResponse) {
	if resp == nil {
		return
	}
	for _, item := range resp.Output {
		if item.Type == "message" && item.Role == "assistant" && len(item.Content) > 0 && item.Content[0].Type == "output_text" {
			return
		}
	}
	resp.Output = append(resp.Output, emptyTextOutput())
}

func firstTextOutputIndex(resp *ResponsesResponse) int {
	for i, item := range resp.Output {
		if item.Type == "message" && item.Role == "assistant" && len(item.Content) > 0 && item.Content[0].Type == "output_text" {
			return i
		}
	}
	return 0
}
