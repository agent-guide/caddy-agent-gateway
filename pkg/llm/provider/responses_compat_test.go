package provider

import (
	"context"
	"encoding/json"
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

func TestResponsesToChatRequestPreservesToolsAndMultimodalInput(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-4.1",
		Input: []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "describe this"},
					map[string]any{"type": "input_image", "image_url": map[string]any{
						"url":    "https://example.com/cat.png",
						"detail": "high",
					}},
				},
			},
		},
		Tools: []ResponsesToolDefinition{{
			Type: "function",
			Function: &ResponsesToolFunction{
				Name:        "lookup",
				Description: "Look up facts",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
			},
		}},
		ToolChoice:         json.RawMessage(`{"type":"function","name":"lookup"}`),
		PreviousResponseID: "resp_prev",
		Store:              boolPtr(true),
		Text: map[string]any{
			"format": map[string]any{"type": "json_schema"},
		},
		Metadata: map[string]any{"trace_id": "abc123"},
		User:     "user-1",
		Reasoning: map[string]any{
			"effort": "medium",
		},
		ParallelToolCalls: boolPtr(true),
		Truncation:        "auto",
	}

	chatReq, err := ResponsesToChatRequest(req)
	if err != nil {
		t.Fatalf("ResponsesToChatRequest() error = %v", err)
	}
	if chatReq.Model != "gpt-4.1" {
		t.Fatalf("model = %q, want gpt-4.1", chatReq.Model)
	}
	if len(chatReq.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(chatReq.Messages))
	}
	msg := chatReq.Messages[0]
	if msg.Role != schema.User || len(msg.UserInputMultiContent) != 2 {
		t.Fatalf("message = %+v, want user multimodal content", msg)
	}
	if got := msg.UserInputMultiContent[1].Image.URL; got == nil || *got != "https://example.com/cat.png" {
		t.Fatalf("image url = %#v, want https://example.com/cat.png", got)
	}

	opts := einomodel.GetCommonOptions(nil, chatReq.Options...)
	if len(opts.Tools) != 1 || opts.Tools[0].Name != "lookup" {
		t.Fatalf("tools = %+v, want lookup", opts.Tools)
	}
	if opts.ToolChoice == nil || *opts.ToolChoice != schema.ToolChoiceForced {
		t.Fatalf("tool choice = %#v, want forced", opts.ToolChoice)
	}
	if len(opts.AllowedToolNames) != 1 || opts.AllowedToolNames[0] != "lookup" {
		t.Fatalf("allowed tool names = %#v, want [lookup]", opts.AllowedToolNames)
	}
	respCtx := ResponsesRequestContextFromOptions(chatReq.Options...)
	if respCtx == nil || respCtx.User != "user-1" {
		t.Fatalf("responses context = %+v, want user-1", respCtx)
	}
	if respCtx.PreviousResponseID != "resp_prev" {
		t.Fatalf("previous_response_id = %q, want resp_prev", respCtx.PreviousResponseID)
	}
	if respCtx.Store == nil || !*respCtx.Store {
		t.Fatalf("store = %#v, want true", respCtx.Store)
	}
	if respCtx.Metadata["trace_id"] != "abc123" {
		t.Fatalf("metadata = %+v, want trace_id", respCtx.Metadata)
	}
	if respCtx.Reasoning["effort"] != "medium" {
		t.Fatalf("reasoning = %+v, want effort=medium", respCtx.Reasoning)
	}
	if respCtx.ParallelToolCalls == nil || !*respCtx.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls = %#v, want true", respCtx.ParallelToolCalls)
	}
	if respCtx.Text["format"] == nil || respCtx.Truncation != "auto" {
		t.Fatalf("responses context text/truncation = %+v, want preserved", respCtx)
	}
}

func TestResponsesToChatRequestRejectsMalformedInputImage(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-4.1",
		Input: []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_image", "image_url": map[string]any{}},
				},
			},
		},
	}

	_, err := ResponsesToChatRequest(req)
	if err == nil || err.Error() != "input_image requires image_url.url" {
		t.Fatalf("error = %v, want input_image requires image_url.url", err)
	}
}

func TestResponsesToChatRequestFoldsNonUserMultiPartText(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-4.1",
		Input: []any{
			map[string]any{
				"type": "message",
				"role": "developer",
				"content": []any{
					map[string]any{"type": "input_text", "text": "first"},
					map[string]any{"type": "input_text", "text": "second"},
				},
			},
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "hello"},
					map[string]any{"type": "input_text", "text": "world"},
				},
			},
		},
	}

	chatReq, err := ResponsesToChatRequest(req)
	if err != nil {
		t.Fatalf("ResponsesToChatRequest() error = %v", err)
	}
	if len(chatReq.Messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(chatReq.Messages))
	}
	developer := chatReq.Messages[0]
	if developer.Role != schema.RoleType("developer") || developer.Content != "first\nsecond" || len(developer.UserInputMultiContent) != 0 {
		t.Fatalf("developer message = %+v, want folded text content", developer)
	}
	user := chatReq.Messages[1]
	if user.Role != schema.User || user.Content != "hello\nworld" || len(user.UserInputMultiContent) != 0 {
		t.Fatalf("user message = %+v, want folded text content", user)
	}
}

func TestResponsesToChatRequestParsesToolCallHistory(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-4.1",
		Input: []any{
			map[string]any{
				"type":      "function_call",
				"call_id":   "call_1",
				"name":      "shell",
				"arguments": `{"cmd":"cat note.txt"}`,
			},
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_1",
				"output": []any{
					map[string]any{"type": "output_text", "text": "secret-token: AGW-ZHIPU-42"},
				},
			},
		},
	}

	chatReq, err := ResponsesToChatRequest(req)
	if err != nil {
		t.Fatalf("ResponsesToChatRequest() error = %v", err)
	}
	if len(chatReq.Messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(chatReq.Messages))
	}
	call := chatReq.Messages[0]
	if call.Role != schema.Assistant || len(call.ToolCalls) != 1 {
		t.Fatalf("function_call message = %+v, want assistant tool call", call)
	}
	if call.ToolCalls[0].ID != "call_1" || call.ToolCalls[0].Function.Name != "shell" || call.ToolCalls[0].Function.Arguments != `{"cmd":"cat note.txt"}` {
		t.Fatalf("tool call = %+v, want parsed function call", call.ToolCalls[0])
	}
	output := chatReq.Messages[1]
	if output.Role != schema.Tool || output.ToolCallID != "call_1" || output.Content != "secret-token: AGW-ZHIPU-42" {
		t.Fatalf("tool output = %+v, want parsed output text", output)
	}
}

func TestResponsesToChatRequestCoalescesParallelToolCalls(t *testing.T) {
	req := &ResponsesRequest{
		Model: "deepseek-v4-pro",
		Input: []any{
			map[string]any{
				"type":      "function_call",
				"call_id":   "call_1",
				"name":      "shell",
				"arguments": `{"cmd":"cat a.txt"}`,
			},
			map[string]any{
				"type":      "function_call",
				"call_id":   "call_2",
				"name":      "shell",
				"arguments": `{"cmd":"cat b.txt"}`,
			},
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_1",
				"output":  "alpha",
			},
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_2",
				"output":  "beta",
			},
		},
	}

	chatReq, err := ResponsesToChatRequest(req)
	if err != nil {
		t.Fatalf("ResponsesToChatRequest() error = %v", err)
	}
	// The two parallel function_call items must collapse into one assistant
	// message so each tool_call_id is answered by the tool messages that follow.
	if len(chatReq.Messages) != 3 {
		t.Fatalf("message count = %d, want 3", len(chatReq.Messages))
	}
	call := chatReq.Messages[0]
	if call.Role != schema.Assistant || len(call.ToolCalls) != 2 {
		t.Fatalf("first message = %+v, want assistant with 2 tool calls", call)
	}
	if call.ToolCalls[0].ID != "call_1" || call.ToolCalls[1].ID != "call_2" {
		t.Fatalf("tool call ids = %q,%q, want call_1,call_2", call.ToolCalls[0].ID, call.ToolCalls[1].ID)
	}
	if chatReq.Messages[1].Role != schema.Tool || chatReq.Messages[1].ToolCallID != "call_1" {
		t.Fatalf("second message = %+v, want tool output for call_1", chatReq.Messages[1])
	}
	if chatReq.Messages[2].Role != schema.Tool || chatReq.Messages[2].ToolCallID != "call_2" {
		t.Fatalf("third message = %+v, want tool output for call_2", chatReq.Messages[2])
	}
}

func TestResponsesToChatRequestCoalescesReasoningWithAdjacentAssistantText(t *testing.T) {
	req := &ResponsesRequest{
		Model: "glm-5.2",
		Input: []any{
			map[string]any{
				"type": "reasoning",
				"summary": []any{map[string]any{
					"type": "summary_text", "text": "inspect first",
				}},
			},
			map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{
					"type": "output_text", "text": "prior answer",
				}},
			},
		},
	}

	chatReq, err := ResponsesToChatRequest(req)
	if err != nil {
		t.Fatalf("ResponsesToChatRequest() error = %v", err)
	}
	if len(chatReq.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(chatReq.Messages))
	}
	msg := chatReq.Messages[0]
	if msg.Role != schema.Assistant || msg.ReasoningContent != "inspect first" || msg.Content != "prior answer" {
		t.Fatalf("assistant message = %+v, want coalesced reasoning and text", msg)
	}
}

func TestResponsesToChatRequestAcceptsAssistantOutputText(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-4.1",
		Input: []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "output_text", "text": "prior answer"},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "follow up"},
				},
			},
		},
	}

	chatReq, err := ResponsesToChatRequest(req)
	if err != nil {
		t.Fatalf("ResponsesToChatRequest() error = %v", err)
	}
	if len(chatReq.Messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(chatReq.Messages))
	}
	if chatReq.Messages[0].Role != schema.Assistant || chatReq.Messages[0].Content != "prior answer" {
		t.Fatalf("assistant message = %+v, want output_text content", chatReq.Messages[0])
	}
	if chatReq.Messages[1].Role != schema.User || chatReq.Messages[1].Content != "follow up" {
		t.Fatalf("user message = %+v, want input_text content", chatReq.Messages[1])
	}
}

func TestResponsesFromChatResponsePreservesToolCalls(t *testing.T) {
	resp := ResponsesFromChatResponse(&ChatResponse{
		Message: &schema.Message{
			Role:    schema.Assistant,
			Content: "calling tool",
			ToolCalls: []schema.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: schema.FunctionCall{
					Name:      "lookup",
					Arguments: `{"q":"cat"}`,
				},
			}},
		},
	}, "gpt-4.1")

	if len(resp.Output) != 2 {
		t.Fatalf("output count = %d, want 2", len(resp.Output))
	}
	if resp.Output[0].Type != "message" || resp.Output[0].Content[0].Text != "calling tool" {
		t.Fatalf("first output = %+v, want message output", resp.Output[0])
	}
	if resp.Output[1].Type != "function_call" || resp.Output[1].Name != "lookup" || resp.Output[1].Arguments != `{"q":"cat"}` {
		t.Fatalf("second output = %+v, want function_call lookup", resp.Output[1])
	}
}

func TestResponsesCompatibilityPreservesReasoningSummaryForReplay(t *testing.T) {
	resp := ResponsesFromChatResponse(&ChatResponse{Message: &schema.Message{
		Role: schema.Assistant, ReasoningContent: "inspect the repository",
		ToolCalls: []schema.ToolCall{{
			ID: "call_1", Type: "function",
			Function: schema.FunctionCall{Name: "read_file", Arguments: `{"path":"README.md"}`},
		}},
	}}, "glm-5.2")
	if len(resp.Output) != 2 || resp.Output[0].Type != "reasoning" || resp.Output[0].Summary[0].Text != "inspect the repository" {
		t.Fatalf("response output = %+v", resp.Output)
	}

	input := make([]any, 0, len(resp.Output)+1)
	for _, item := range resp.Output {
		encoded, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("marshal item: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("unmarshal item: %v", err)
		}
		input = append(input, decoded)
	}
	input = append(input, map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "contents"})
	chatReq, err := ResponsesToChatRequest(&ResponsesRequest{Model: "glm-5.2", Input: input})
	if err != nil {
		t.Fatalf("ResponsesToChatRequest() error = %v", err)
	}
	if len(chatReq.Messages) != 2 || chatReq.Messages[0].ReasoningContent != "inspect the repository" || len(chatReq.Messages[0].ToolCalls) != 1 {
		t.Fatalf("replayed messages = %+v", chatReq.Messages)
	}
}

func TestStreamResponsesViaChatEmitsReasoningSummaryEvents(t *testing.T) {
	prov := &testResponsesCompatProvider{streamResp: schema.StreamReaderFromArray([]*schema.Message{
		{Role: schema.Assistant, ReasoningContent: "inspect "},
		{Role: schema.Assistant, ReasoningContent: "first"},
	})}
	stream, err := StreamResponsesViaChat(nil, prov, &ResponsesRequest{Model: "glm-5.2", Input: "hello"})
	if err != nil {
		t.Fatalf("StreamResponsesViaChat() error = %v", err)
	}
	defer stream.Close()
	var delta string
	var completed *ResponsesResponse
	for {
		event, recvErr := stream.Recv()
		if recvErr != nil {
			break
		}
		if event.Type == "response.reasoning_summary_text.delta" {
			delta += event.Delta
		}
		if event.Type == "response.completed" {
			completed = event.Response
		}
	}
	if delta != "inspect first" {
		t.Fatalf("reasoning delta = %q", delta)
	}
	if completed == nil || len(completed.Output) != 1 || completed.Output[0].Summary[0].Text != "inspect first" {
		t.Fatalf("completed response = %+v", completed)
	}
}

func TestStreamResponsesViaChatClosesReasoningBeforeTextItem(t *testing.T) {
	prov := &testResponsesCompatProvider{streamResp: schema.StreamReaderFromArray([]*schema.Message{
		{Role: schema.Assistant, ReasoningContent: "inspect"},
		{Role: schema.Assistant, Content: "done"},
	})}
	stream, err := StreamResponsesViaChat(nil, prov, &ResponsesRequest{Model: "glm-5.2", Input: "hello"})
	if err != nil {
		t.Fatalf("StreamResponsesViaChat() error = %v", err)
	}
	defer stream.Close()

	var eventTypes []string
	for {
		event, recvErr := stream.Recv()
		if recvErr != nil {
			break
		}
		eventTypes = append(eventTypes, event.Type)
	}
	want := []string{
		"response.created",
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.output_text.delta",
		"response.output_item.done",
		"response.completed",
	}
	if len(eventTypes) != len(want) {
		t.Fatalf("event types = %#v, want %#v", eventTypes, want)
	}
	for i := range want {
		if eventTypes[i] != want[i] {
			t.Fatalf("event types = %#v, want %#v", eventTypes, want)
		}
	}
}

func TestResponsesRequestFromChatStatePreservesToolCallHistory(t *testing.T) {
	state := &ChatRequestState{
		ModelName: "gpt-4.1",
		Messages: []*schema.Message{
			schema.UserMessage("start"),
			{
				Role: schema.Assistant,
				ToolCalls: []schema.ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: schema.FunctionCall{
						Name:      "lookup",
						Arguments: `{"q":"cat"}`,
					},
				}},
			},
			{
				Role:       schema.Tool,
				ToolCallID: "call_1",
				Content:    "tool output",
			},
		},
	}

	req := ResponsesRequestFromChatState(state, true)
	items, ok := req.Input.([]any)
	if !ok {
		t.Fatalf("input = %T, want []any", req.Input)
	}
	if len(items) != 3 {
		t.Fatalf("input count = %d, want 3", len(items))
	}
	call, _ := items[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "lookup" || call["arguments"] != `{"q":"cat"}` {
		t.Fatalf("function_call item = %+v", call)
	}
	output, _ := items[2].(map[string]any)
	if output["type"] != "function_call_output" || output["call_id"] != "call_1" || output["output"] != "tool output" {
		t.Fatalf("function_call_output item = %+v", output)
	}
}

func TestStreamResponsesViaChatEmitsFunctionCallEvents(t *testing.T) {
	prov := &testResponsesCompatProvider{
		streamResp: schema.StreamReaderFromArray([]*schema.Message{{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: schema.FunctionCall{
					Name:      "lookup",
					Arguments: `{"q":"cat"}`,
				},
			}},
			ResponseMeta: &schema.ResponseMeta{FinishReason: "tool_calls"},
		}}),
	}

	stream, err := StreamResponsesViaChat(nil, prov, &ResponsesRequest{
		Model: "gpt-4.1",
		Input: "hello",
	})
	if err != nil {
		t.Fatalf("StreamResponsesViaChat() error = %v", err)
	}
	defer stream.Close()

	var eventTypes []string
	var functionItem *ResponsesResponseOutput
	var delta string
	for {
		ev, err := stream.Recv()
		if err != nil {
			break
		}
		eventTypes = append(eventTypes, ev.Type)
		if ev.Type == "response.output_item.added" {
			functionItem = ev.Item
		}
		if ev.Type == "response.function_call_arguments.delta" {
			delta = ev.Delta
		}
	}

	wantTypes := []string{
		"response.created",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.output_item.done",
		"response.completed",
	}
	if len(eventTypes) != len(wantTypes) {
		t.Fatalf("event types = %#v, want %#v", eventTypes, wantTypes)
	}
	for i := range wantTypes {
		if eventTypes[i] != wantTypes[i] {
			t.Fatalf("event types = %#v, want %#v", eventTypes, wantTypes)
		}
	}
	if functionItem == nil || functionItem.Type != "function_call" || functionItem.Name != "lookup" {
		t.Fatalf("function item = %+v, want function_call lookup", functionItem)
	}
	if delta != `{"q":"cat"}` {
		t.Fatalf("delta = %q, want function call arguments", delta)
	}
}

func TestStreamResponsesViaChatEmitsTextItemLifecycle(t *testing.T) {
	prov := &testResponsesCompatProvider{
		streamResp: schema.StreamReaderFromArray([]*schema.Message{
			{Role: schema.Assistant, Content: "he"},
			{Role: schema.Assistant, Content: "llo"},
		}),
	}

	stream, err := StreamResponsesViaChat(nil, prov, &ResponsesRequest{Model: "gpt-4.1", Input: "hello"})
	if err != nil {
		t.Fatalf("StreamResponsesViaChat() error = %v", err)
	}
	defer stream.Close()

	var eventTypes []string
	var delta string
	var doneItem *ResponsesResponseOutput
	for {
		ev, err := stream.Recv()
		if err != nil {
			break
		}
		eventTypes = append(eventTypes, ev.Type)
		if ev.Type == "response.output_text.delta" {
			delta += ev.Delta
		}
		if ev.Type == "response.output_item.done" {
			doneItem = ev.Item
		}
	}

	wantTypes := []string{
		"response.created",
		"response.output_item.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_item.done",
		"response.completed",
	}
	if len(eventTypes) != len(wantTypes) {
		t.Fatalf("event types = %#v, want %#v", eventTypes, wantTypes)
	}
	for i := range wantTypes {
		if eventTypes[i] != wantTypes[i] {
			t.Fatalf("event types = %#v, want %#v", eventTypes, wantTypes)
		}
	}
	if delta != "hello" {
		t.Fatalf("delta = %q, want hello", delta)
	}
	if doneItem == nil || len(doneItem.Content) != 1 || doneItem.Content[0].Text != "hello" {
		t.Fatalf("done item = %+v, want completed text item", doneItem)
	}
}

func TestStreamResponsesViaChatKeepsToolIndexesStableWhenTextFollowsTool(t *testing.T) {
	prov := &testResponsesCompatProvider{
		streamResp: schema.StreamReaderFromArray([]*schema.Message{
			{
				Role: schema.Assistant,
				ToolCalls: []schema.ToolCall{{
					ID:       "call_1",
					Type:     "function",
					Function: schema.FunctionCall{Name: "lookup", Arguments: `{"q":"cat"}`},
				}},
			},
			{Role: schema.Assistant, Content: "done"},
		}),
	}

	stream, err := StreamResponsesViaChat(nil, prov, &ResponsesRequest{Model: "gpt-4.1", Input: "hello"})
	if err != nil {
		t.Fatalf("StreamResponsesViaChat() error = %v", err)
	}
	defer stream.Close()

	var toolAddedIndex, toolDeltaIndex, toolDoneIndex = -1, -1, -1
	var toolAddedID, toolDeltaID, toolDoneID string
	for {
		ev, err := stream.Recv()
		if err != nil {
			break
		}
		switch ev.Type {
		case "response.output_item.added":
			if ev.Item != nil && ev.Item.Type == "function_call" {
				toolAddedIndex = ev.OutputIndex
				toolAddedID = ev.ItemID
			}
		case "response.function_call_arguments.delta":
			toolDeltaIndex = ev.OutputIndex
			toolDeltaID = ev.ItemID
		case "response.output_item.done":
			if ev.Item != nil && ev.Item.Type == "function_call" {
				toolDoneIndex = ev.OutputIndex
				toolDoneID = ev.ItemID
			}
		}
	}

	if toolAddedIndex < 0 || toolDeltaIndex < 0 || toolDoneIndex < 0 {
		t.Fatalf("tool indexes added=%d delta=%d done=%d, want all present", toolAddedIndex, toolDeltaIndex, toolDoneIndex)
	}
	if toolAddedIndex != toolDeltaIndex || toolAddedIndex != toolDoneIndex {
		t.Fatalf("tool indexes added=%d delta=%d done=%d, want stable", toolAddedIndex, toolDeltaIndex, toolDoneIndex)
	}
	if toolAddedID == "" || toolAddedID != toolDeltaID || toolAddedID != toolDoneID {
		t.Fatalf("tool ids added=%q delta=%q done=%q, want stable", toolAddedID, toolDeltaID, toolDoneID)
	}
}

func TestResponsesRequestFromChatStateRoundTripsResponsesContext(t *testing.T) {
	chatReq, err := ResponsesToChatRequest(&ResponsesRequest{
		Model: "gpt-4.1",
		Input: "hello",
		Tools: []ResponsesToolDefinition{{
			Type: "function",
			Function: &ResponsesToolFunction{
				Name:       "lookup",
				Parameters: json.RawMessage(`{"type":"object"}`),
			},
		}},
		ToolChoice:         json.RawMessage(`{"type":"function","name":"lookup"}`),
		PreviousResponseID: "resp_prev",
		Store:              boolPtr(false),
		Text:               map[string]any{"verbosity": "high"},
		Metadata:           map[string]any{"trace_id": "abc123"},
		User:               "user-1",
		Reasoning:          map[string]any{"effort": "medium"},
		ParallelToolCalls:  boolPtr(true),
		Truncation:         "auto",
	})
	if err != nil {
		t.Fatalf("ResponsesToChatRequest() error = %v", err)
	}

	state, err := ResolveChatRequest(context.Background(), ProviderConfig{}, chatReq)
	if err != nil {
		t.Fatalf("ResolveChatRequest() error = %v", err)
	}
	req := ResponsesRequestFromChatState(state, true)
	if req.PreviousResponseID != "resp_prev" {
		t.Fatalf("previous_response_id = %q, want resp_prev", req.PreviousResponseID)
	}
	if req.Store == nil || *req.Store {
		t.Fatalf("store = %#v, want false", req.Store)
	}
	if req.User != "user-1" || req.Metadata["trace_id"] != "abc123" {
		t.Fatalf("rebuilt request = %+v, want preserved user/metadata", req)
	}
	if req.Reasoning["effort"] != "medium" || req.Text["verbosity"] != "high" {
		t.Fatalf("rebuilt request = %+v, want preserved reasoning/text", req)
	}
	if req.ParallelToolCalls == nil || !*req.ParallelToolCalls || req.Truncation != "auto" {
		t.Fatalf("rebuilt request = %+v, want preserved parallel_tool_calls/truncation", req)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function == nil || req.Tools[0].Function.Name != "lookup" {
		t.Fatalf("rebuilt tools = %+v, want lookup", req.Tools)
	}
	if string(req.ToolChoice) != `{"name":"lookup","type":"function"}` && string(req.ToolChoice) != `{"type":"function","name":"lookup"}` {
		t.Fatalf("rebuilt tool choice = %s, want function lookup", string(req.ToolChoice))
	}
}

func TestStreamResponsesViaChatMergesToolCallDeltas(t *testing.T) {
	prov := &testResponsesCompatProvider{
		streamResp: schema.StreamReaderFromArray([]*schema.Message{
			{
				Role: schema.Assistant,
				ToolCalls: []schema.ToolCall{{
					Index:    intPtr(0),
					ID:       "call_1",
					Type:     "function",
					Function: schema.FunctionCall{Name: "lookup", Arguments: `{"q":`},
				}},
			},
			{
				Role: schema.Assistant,
				ToolCalls: []schema.ToolCall{{
					Index:    intPtr(0),
					Function: schema.FunctionCall{Arguments: `"cat"}`},
				}},
				ResponseMeta: &schema.ResponseMeta{FinishReason: "tool_calls"},
			},
		}),
	}

	stream, err := StreamResponsesViaChat(nil, prov, &ResponsesRequest{Model: "gpt-4.1", Input: "hello"})
	if err != nil {
		t.Fatalf("StreamResponsesViaChat() error = %v", err)
	}
	defer stream.Close()

	var added, done int
	var deltas string
	var functionItem *ResponsesResponseOutput
	for {
		ev, err := stream.Recv()
		if err != nil {
			break
		}
		switch ev.Type {
		case "response.output_item.added":
			added++
			functionItem = ev.Item
		case "response.function_call_arguments.delta":
			deltas += ev.Delta
		case "response.output_item.done":
			done++
			functionItem = ev.Item
		}
	}

	if added != 1 || done != 1 {
		t.Fatalf("added=%d done=%d, want exactly one of each (deltas must merge into one item)", added, done)
	}
	if deltas != `{"q":"cat"}` {
		t.Fatalf("merged delta = %q, want full arguments", deltas)
	}
	if functionItem == nil || functionItem.Name != "lookup" || functionItem.ID != "call_1" {
		t.Fatalf("function item = %+v, want lookup/call_1", functionItem)
	}
	if functionItem.Arguments != `{"q":"cat"}` {
		t.Fatalf("function item arguments = %q, want merged arguments", functionItem.Arguments)
	}
}

func TestStreamResponsesViaChatMergesToolCallDeltasWithoutIndexOrID(t *testing.T) {
	prov := &testResponsesCompatProvider{
		streamResp: schema.StreamReaderFromArray([]*schema.Message{
			{
				Role: schema.Assistant,
				ToolCalls: []schema.ToolCall{{
					Type:     "function",
					Function: schema.FunctionCall{Name: "lookup", Arguments: `{"q":`},
				}},
			},
			{
				Role: schema.Assistant,
				ToolCalls: []schema.ToolCall{{
					Function: schema.FunctionCall{Arguments: `"cat"}`},
				}},
				ResponseMeta: &schema.ResponseMeta{FinishReason: "tool_calls"},
			},
		}),
	}

	stream, err := StreamResponsesViaChat(nil, prov, &ResponsesRequest{Model: "gpt-4.1", Input: "hello"})
	if err != nil {
		t.Fatalf("StreamResponsesViaChat() error = %v", err)
	}
	defer stream.Close()

	var added, done int
	var functionItem *ResponsesResponseOutput
	for {
		ev, err := stream.Recv()
		if err != nil {
			break
		}
		switch ev.Type {
		case "response.output_item.added":
			added++
			functionItem = ev.Item
		case "response.output_item.done":
			done++
			functionItem = ev.Item
		}
	}

	if added != 1 || done != 1 {
		t.Fatalf("added=%d done=%d, want one fallback function output item", added, done)
	}
	if functionItem == nil || functionItem.Name != "lookup" || functionItem.Arguments != `{"q":"cat"}` {
		t.Fatalf("function item = %+v, want merged lookup arguments", functionItem)
	}
}

func TestResponsesStreamEventsSnapshotMutablePayloads(t *testing.T) {
	resp := &ResponsesResponse{
		ID:      "resp_1",
		RawJSON: json.RawMessage(`{"id":"resp_1"}`),
		Usage:   &ResponsesResponseUsage{InputTokens: 3},
		Output: []ResponsesResponseOutput{{
			ID:      "item_1",
			Summary: []ResponsesReasoningSummaryPart{{Type: "summary_text", Text: "before"}},
			Content: []ResponsesResponseContentPart{{
				Type:        "output_text",
				Text:        "before",
				Annotations: []any{"before"},
				Summary:     []any{"before"},
				RawJSON:     json.RawMessage(`{"text":"before"}`),
			}},
		}},
	}
	created := ResponsesCreatedEvent(resp)
	completed := ResponsesCompletedEvent(resp)
	added := ResponsesOutputItemAddedEvent(0, &resp.Output[0])
	done := ResponsesOutputItemDoneEvent(0, &resp.Output[0])
	partAdded := ResponsesReasoningSummaryPartAddedEvent("item_1", 0, &resp.Output[0].Summary[0])
	partDone := ResponsesReasoningSummaryPartDoneEvent("item_1", 0, &resp.Output[0].Summary[0])

	resp.ID = "mutated"
	resp.RawJSON[0] = '['
	resp.Usage.InputTokens = 99
	resp.Output[0].Summary[0].Text = "mutated"
	resp.Output[0].Content[0].Text = "mutated"
	resp.Output[0].Content[0].Annotations[0] = "mutated"
	resp.Output[0].Content[0].Summary[0] = "mutated"
	resp.Output[0].Content[0].RawJSON[0] = '['

	for _, event := range []*ResponsesStreamEvent{created, completed} {
		if event.Response.ID != "resp_1" || event.Response.Usage.InputTokens != 3 {
			t.Fatalf("%s response = %+v, want immutable snapshot", event.Type, event.Response)
		}
		if string(event.Response.RawJSON) != `{"id":"resp_1"}` || event.Response.Output[0].Content[0].Text != "before" {
			t.Fatalf("%s nested response mutated: %+v", event.Type, event.Response)
		}
	}
	for _, event := range []*ResponsesStreamEvent{added, done} {
		if event.Item.Summary[0].Text != "before" || event.Item.Content[0].Text != "before" ||
			event.Item.Content[0].Annotations[0] != "before" || event.Item.Content[0].Summary[0] != "before" ||
			string(event.Item.Content[0].RawJSON) != `{"text":"before"}` {
			t.Fatalf("%s item mutated: %+v", event.Type, event.Item)
		}
	}
	for _, event := range []*ResponsesStreamEvent{partAdded, partDone} {
		if event.Part.Text != "before" {
			t.Fatalf("%s part mutated: %+v", event.Type, event.Part)
		}
	}
}

func TestSplitInstructionsAndInputPreservesImages(t *testing.T) {
	url := "https://example.com/cat.png"
	messages := []*schema.Message{
		{Role: schema.System, Content: "be brief"},
		{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: "describe this"},
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{URL: &url},
				Detail:            schema.ImageURLDetail("high"),
			}},
		}},
	}

	instructions, input := splitInstructionsAndInput(messages)
	if instructions != "be brief" {
		t.Fatalf("instructions = %q, want be brief", instructions)
	}
	if len(input) != 1 {
		t.Fatalf("input items = %d, want 1", len(input))
	}
	item, _ := input[0].(map[string]any)
	parts, _ := item["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("content parts = %+v, want text + image", parts)
	}
	image, _ := parts[1].(map[string]any)
	if image["type"] != "input_image" || image["image_url"] != url || image["detail"] != "high" {
		t.Fatalf("image part = %+v, want input_image with url and detail", image)
	}

	// The round-trip parser must accept the emitted string image_url shape.
	msg, err := responseInputItemToMessage(item)
	if err != nil {
		t.Fatalf("responseInputItemToMessage() error = %v", err)
	}
	if len(msg.UserInputMultiContent) != 2 {
		t.Fatalf("round-trip parts = %+v, want text + image", msg.UserInputMultiContent)
	}
	got := msg.UserInputMultiContent[1].Image
	if got == nil || got.URL == nil || *got.URL != url || string(got.Detail) != "high" {
		t.Fatalf("round-trip image = %+v, want preserved url/detail", got)
	}
}

func boolPtr(v bool) *bool {
	return &v
}

func intPtr(v int) *int {
	return &v
}

type testResponsesCompatProvider struct {
	chatResp   *ChatResponse
	streamResp *schema.StreamReader[*schema.Message]
}

func (p *testResponsesCompatProvider) Chat(_ context.Context, _ *ChatRequest) (*ChatResponse, error) {
	return p.chatResp, nil
}

func (p *testResponsesCompatProvider) StreamChat(_ context.Context, _ *ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	return p.streamResp, nil
}

func (p *testResponsesCompatProvider) ListModels(context.Context) ([]ModelInfo, error) {
	return nil, nil
}

func (p *testResponsesCompatProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{Streaming: true, Tools: true}
}

func (p *testResponsesCompatProvider) Config() ProviderConfig {
	return ProviderConfig{ProviderType: "test"}
}
