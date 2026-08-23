package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	anthropicapi "github.com/agent-guide/agent-gateway/pkg/dispatcher/llmapi/anthropic"
	"github.com/agent-guide/agent-gateway/pkg/dispatcher/llmapi/anthropicmsg"
	ccapi "github.com/agent-guide/agent-gateway/pkg/dispatcher/llmapi/cc"
	openaiapi "github.com/agent-guide/agent-gateway/pkg/dispatcher/llmapi/openai"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

func TestChatStateToResponsesRequestMovesSystemMessagesToInstructions(t *testing.T) {
	state := &provider.ChatRequestState{
		ModelName: "gpt-5.4",
		Messages: []*schema.Message{
			schema.SystemMessage("You are Claude Code."),
			schema.SystemMessage("Follow the user's instructions."),
			schema.UserMessage("hello"),
		},
	}

	req := chatStateToResponsesRequest(state, false)

	if req.Instructions != "You are Claude Code.\nFollow the user's instructions." {
		t.Fatalf("instructions = %q", req.Instructions)
	}
	items, ok := req.Input.([]any)
	if !ok {
		t.Fatalf("input type = %T, want []any", req.Input)
	}
	if len(items) != 1 {
		t.Fatalf("input item count = %d, want 1", len(items))
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("input item type = %T, want map[string]any", items[0])
	}
	if got := item["role"]; got != "user" {
		t.Fatalf("role = %#v, want %q", got, "user")
	}
}

func TestSanitizeResponsesRequestLeavesMissingInstructionsEmpty(t *testing.T) {
	req := sanitizeResponsesRequest(&provider.ResponsesRequest{
		Model: "gpt-5.4",
		Input: []any{},
	}, false)

	if req.Instructions != "" {
		t.Fatalf("instructions = %q, want empty", req.Instructions)
	}
}

func TestSanitizeResponsesRequestEnforcesCodexBackendControls(t *testing.T) {
	storeTrue := true
	req := sanitizeResponsesRequest(&provider.ResponsesRequest{
		Model: "gpt-5.4",
		Input: []any{map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": "hello",
			}},
		}, map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": "previous answer",
			}},
		}, map[string]any{
			"type": "message",
			"role": "tool",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": "tool output",
			}},
		}},
		MaxOutputTokens: 1234,
		Metadata:        map[string]any{"trace_id": "abc123"},
		Text:            map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}}},
		ToolChoice:      json.RawMessage(`{"type":"tool_search","name":"tool_search"}`),
		Tools: []provider.ResponsesToolDefinition{{
			Type: "function",
			Function: &provider.ResponsesToolFunction{
				Name:        "lookup",
				Description: "Lookup data",
				Parameters:  json.RawMessage(`{"type":"object"}`),
			},
		}, {
			Type: "function",
			Function: &provider.ResponsesToolFunction{
				Name:        "Agent",
				Description: "Launch subagent",
				Parameters:  json.RawMessage(`{"type":"object"}`),
			},
		}, {
			Type:        "tool_search",
			Name:        "tool_search",
			Description: "Search the repository",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
		Store: &storeTrue,
	}, false)

	if req.MaxOutputTokens != 0 {
		t.Fatalf("max_output_tokens = %d, want 0", req.MaxOutputTokens)
	}
	if req.Store == nil || *req.Store != false {
		t.Fatalf("store = %#v, want false", req.Store)
	}
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls != false {
		t.Fatalf("parallel_tool_calls = %#v, want false", req.ParallelToolCalls)
	}
	if req.Metadata != nil {
		t.Fatalf("metadata = %#v, want nil", req.Metadata)
	}
	inputItems, _ := req.Input.([]any)
	userItem, _ := inputItems[0].(map[string]any)
	userContent, _ := userItem["content"].([]any)
	userContentPart, _ := userContent[0].(map[string]any)
	if userContentPart["type"] != "input_text" {
		t.Fatalf("user input content type = %#v, want input_text", userContentPart["type"])
	}
	assistantItem, _ := inputItems[1].(map[string]any)
	assistantContent, _ := assistantItem["content"].([]any)
	assistantContentPart, _ := assistantContent[0].(map[string]any)
	if assistantContentPart["type"] != "output_text" {
		t.Fatalf("assistant input content type = %#v, want output_text", assistantContentPart["type"])
	}
	toolItem, _ := inputItems[2].(map[string]any)
	if toolItem["role"] != "user" {
		t.Fatalf("tool result role = %#v, want user", toolItem["role"])
	}
	toolContent, _ := toolItem["content"].([]any)
	toolContentPart, _ := toolContent[0].(map[string]any)
	if toolContentPart["type"] != "input_text" {
		t.Fatalf("tool result content type = %#v, want input_text", toolContentPart["type"])
	}
	if len(req.Tools) != 2 || req.Tools[0].Name != "lookup" || req.Tools[1].Name != "Agent" || req.Tools[0].Function != nil || req.Tools[1].Function != nil {
		t.Fatalf("tools = %#v, want only flattened lookup and Agent tools", req.Tools)
	}
	if req.Tools[0].Description != "Lookup data" {
		t.Fatalf("function tool description = %q, want preserved", req.Tools[0].Description)
	}
	if string(req.Tools[0].Parameters) != `{"type":"object"}` {
		t.Fatalf("tool parameters = %s, want object schema", string(req.Tools[0].Parameters))
	}
	if len(req.ToolChoice) != 0 {
		t.Fatalf("tool_choice = %s, want omitted for unsupported hosted tool", string(req.ToolChoice))
	}
	format, _ := req.Text["format"].(map[string]any)
	if format["name"] != "response" {
		t.Fatalf("text.format = %#v, want default name", format)
	}
}

func TestSanitizeResponsesRequestFiltersUnsupportedHostedToolVariants(t *testing.T) {
	for _, tc := range []struct {
		name       string
		toolType   string
		toolChoice json.RawMessage
	}{
		{
			name:       "case insensitive type and name",
			toolType:   "Tool_Search",
			toolChoice: json.RawMessage(`{"type":"Tool_Search","name":"Tool_Search"}`),
		},
		{
			name:       "nested function name",
			toolType:   "tool_search",
			toolChoice: json.RawMessage(`{"type":"function","function":{"name":"tool_search"}}`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := sanitizeResponsesRequest(&provider.ResponsesRequest{
				Model:      "gpt-5.4",
				Input:      []any{},
				ToolChoice: tc.toolChoice,
				Tools: []provider.ResponsesToolDefinition{{
					Type: tc.toolType,
					Name: "tool_search",
				}},
			}, false)

			if len(req.Tools) != 0 {
				t.Fatalf("tools = %#v, want unsupported hosted tool omitted", req.Tools)
			}
			if len(req.ToolChoice) != 0 {
				t.Fatalf("tool_choice = %s, want omitted for unsupported hosted tool", string(req.ToolChoice))
			}
		})
	}
}

func TestSanitizeResponsesRequestFiltersUnsupportedHostedToolsFromProtocolInputs(t *testing.T) {
	t.Run("openai chat completions", func(t *testing.T) {
		handler := openaiapi.NewHandler()
		body, err := json.Marshal(openaiapi.ChatCompletionRequest{
			Model: "gpt-5.4",
			Messages: []openaiapi.ChatMessage{{
				Role:    "user",
				Content: "hello",
			}},
			Tools: []openaiapi.ToolDefinition{{
				Type: "function",
				Function: openaiapi.ToolFunction{
					Name:        "tool_search",
					Description: "Hosted search",
					Parameters:  json.RawMessage(`{"type":"object"}`),
				},
			}},
			ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"tool_search"}}`),
		})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		prepared, _, err := handler.PrepareLLMApiRequest(req)
		if err != nil {
			t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
		}

		state, err := provider.ResolveChatRequest(context.Background(), provider.ProviderConfig{}, prepared.ChatRequest)
		if err != nil {
			t.Fatalf("ResolveChatRequest returned error: %v", err)
		}
		sanitized := sanitizeResponsesRequest(chatStateToResponsesRequest(state, false), false)
		if len(sanitized.Tools) != 0 {
			t.Fatalf("tools = %#v, want unsupported hosted tool omitted", sanitized.Tools)
		}
		if len(sanitized.ToolChoice) != 0 {
			t.Fatalf("tool_choice = %s, want omitted", string(sanitized.ToolChoice))
		}
	})

	t.Run("anthropic messages", func(t *testing.T) {
		handler := anthropicapi.NewHandler(nil)
		body, err := json.Marshal(anthropicmsg.MessagesRequest{
			Model:     "claude-sonnet-4-5",
			MaxTokens: 16,
			Messages: []anthropicmsg.MessageItem{{
				Role:    "user",
				Content: anthropicmsg.MessageContent{{Type: "text", Text: "hello"}},
			}},
			Tools: []anthropicmsg.ToolDefinition{{
				Name:        "tool_search",
				Description: "Hosted search",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
			ToolChoice: json.RawMessage(`{"type":"tool","name":"tool_search"}`),
		})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
		prepared, _, err := handler.PrepareLLMApiRequest(req)
		if err != nil {
			t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
		}

		state, err := provider.ResolveChatRequest(context.Background(), provider.ProviderConfig{}, prepared.ChatRequest)
		if err != nil {
			t.Fatalf("ResolveChatRequest returned error: %v", err)
		}
		sanitized := sanitizeResponsesRequest(chatStateToResponsesRequest(state, false), false)
		if len(sanitized.Tools) != 0 {
			t.Fatalf("tools = %#v, want unsupported hosted tool omitted", sanitized.Tools)
		}
		if len(sanitized.ToolChoice) != 0 {
			t.Fatalf("tool_choice = %s, want omitted", string(sanitized.ToolChoice))
		}
	})

	t.Run("cc messages", func(t *testing.T) {
		handler := ccapi.NewHandler(nil)
		body, err := json.Marshal(anthropicmsg.MessagesRequest{
			Model:     "claude-sonnet-4-5",
			MaxTokens: 16,
			Messages: []anthropicmsg.MessageItem{{
				Role:    "user",
				Content: anthropicmsg.MessageContent{{Type: "text", Text: "hello"}},
			}},
			Tools: []anthropicmsg.ToolDefinition{{
				Name:        "tool_search",
				Description: "Hosted search",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
			ToolChoice: json.RawMessage(`{"type":"tool","name":"tool_search"}`),
		})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
		prepared, _, err := handler.PrepareLLMApiRequest(req)
		if err != nil {
			t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
		}

		state, err := provider.ResolveChatRequest(context.Background(), provider.ProviderConfig{}, prepared.ChatRequest)
		if err != nil {
			t.Fatalf("ResolveChatRequest returned error: %v", err)
		}
		sanitized := sanitizeResponsesRequest(chatStateToResponsesRequest(state, false), false)
		if len(sanitized.Tools) != 0 {
			t.Fatalf("tools = %#v, want unsupported hosted tool omitted", sanitized.Tools)
		}
		if len(sanitized.ToolChoice) != 0 {
			t.Fatalf("tool_choice = %s, want omitted", string(sanitized.ToolChoice))
		}
	})
}

func TestSanitizeResponsesRequestCCCompatFiltersStatefulClaudeCodeTools(t *testing.T) {
	req := sanitizeResponsesRequest(&provider.ResponsesRequest{
		Model:      "gpt-5.4",
		Input:      []any{},
		ToolChoice: json.RawMessage(`{"type":"function","name":"Agent"}`),
		Tools: []provider.ResponsesToolDefinition{{
			Type: "function",
			Name: "lookup",
		}, {
			Type: "function",
			Name: "Agent",
		}, {
			Type: "function",
			Name: "spawnTeam",
		}},
	}, true)

	if len(req.Tools) != 1 || req.Tools[0].Name != "lookup" {
		t.Fatalf("tools = %#v, want only lookup", req.Tools)
	}
	if len(req.ToolChoice) != 0 {
		t.Fatalf("tool_choice = %s, want omitted", string(req.ToolChoice))
	}
}

func TestSanitizeResponsesRequestCCCompatFiltersNestedStatefulToolChoice(t *testing.T) {
	req := sanitizeResponsesRequest(&provider.ResponsesRequest{
		Model:      "gpt-5.4",
		Input:      []any{},
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"Agent"}}`),
		Tools: []provider.ResponsesToolDefinition{{
			Type: "function",
			Name: "lookup",
		}, {
			Type: "function",
			Name: "Agent",
		}},
	}, true)

	if len(req.Tools) != 1 || req.Tools[0].Name != "lookup" {
		t.Fatalf("tools = %#v, want only lookup", req.Tools)
	}
	if len(req.ToolChoice) != 0 {
		t.Fatalf("tool_choice = %s, want omitted", string(req.ToolChoice))
	}
}

func TestSanitizeResponsesRequestClearsForcedToolChoiceWhenToolsAreUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		toolChoice json.RawMessage
	}{
		{
			name:       "required string with no remaining tools",
			toolChoice: json.RawMessage(`"required"`),
		},
		{
			name:       "forced name filtered from tools",
			toolChoice: json.RawMessage(`{"type":"function","function":{"name":"tool_search"}}`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := sanitizeResponsesRequest(&provider.ResponsesRequest{
				Model:      "gpt-5.4",
				Input:      []any{},
				ToolChoice: tc.toolChoice,
				Tools: []provider.ResponsesToolDefinition{{
					Type: "function",
					Function: &provider.ResponsesToolFunction{
						Name: "tool_search",
					},
				}},
			}, false)

			if len(req.Tools) != 0 {
				t.Fatalf("tools = %#v, want all tools filtered", req.Tools)
			}
			if len(req.ToolChoice) != 0 {
				t.Fatalf("tool_choice = %s, want omitted", string(req.ToolChoice))
			}
		})
	}
}

func TestListModelsFallsBackToConfiguredDefaultModel(t *testing.T) {
	prov, err := New(provider.ProviderConfig{
		ProviderType: "codex",
		DefaultModel: "gpt-5.4",
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	models, err := prov.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels returned error: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("model count = %d, want 1", len(models))
	}
	if models[0].ID != "gpt-5.4" || models[0].Name != "gpt-5.4" || models[0].DisplayName != "gpt-5.4" {
		t.Fatalf("models[0] = %#v, want default model metadata", models[0])
	}
}

func TestChatCarriesRequestControlsToResponsesPayload(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_test",
			"object":"response",
			"created_at":1710000000,
			"model":"codex-mini",
			"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],
			"usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5}
		}`))
	}))
	defer server.Close()

	bFalse := false
	bTrue := true
	prov, err := New(provider.ProviderConfig{
		ProviderType: "codex",
		APIKey:       "test-key",
		BaseURL:      server.URL,
		DefaultModel: "default-model",
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	_, err = prov.Chat(context.Background(), &provider.ChatRequest{
		Model: "request-model",
		Messages: []*schema.Message{
			schema.SystemMessage("system instructions"),
			schema.UserMessage("hello"),
		},
		Options: []einomodel.Option{
			einomodel.WithModel("option-model"),
			einomodel.WithTemperature(0.7),
			einomodel.WithTopP(0.9),
			einomodel.WithMaxTokens(1234),
			einomodel.WithStop([]string{"END"}),
			provider.WithTopK(17),
			provider.WithChatExtraFields(&provider.ChatExtraFields{
				ResponseFormat: map[string]any{
					"type": "json_schema",
					"json_schema": map[string]any{
						"name":   "answer",
						"schema": map[string]any{"type": "object"},
						"strict": true,
					},
				},
				Reasoning:         map[string]any{"summary": "auto"},
				ReasoningEffort:   "high",
				User:              "user-1",
				Metadata:          map[string]any{"trace_id": "abc123"},
				ParallelToolCalls: &bFalse,
				Store:             &bTrue,
			}),
		},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}

	if captured["model"] != "request-model" {
		t.Fatalf("model = %#v, want request-model", captured["model"])
	}
	if _, ok := captured["max_output_tokens"]; ok {
		t.Fatalf("max_output_tokens = %#v, want omitted", captured["max_output_tokens"])
	}
	if _, ok := captured["top_k"]; ok {
		t.Fatalf("top_k = %#v, want omitted", captured["top_k"])
	}
	if _, ok := captured["stop"]; ok {
		t.Fatalf("stop = %#v, want omitted", captured["stop"])
	}
	if captured["store"] != false {
		t.Fatalf("store = %#v, want false", captured["store"])
	}
	if captured["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %#v, want false", captured["parallel_tool_calls"])
	}
	if captured["user"] != "user-1" {
		t.Fatalf("user = %#v, want user-1", captured["user"])
	}
	if _, ok := captured["metadata"]; ok {
		t.Fatalf("metadata = %#v, want omitted", captured["metadata"])
	}
	reasoning, _ := captured["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %+v, want effort and summary", reasoning)
	}
	text, _ := captured["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "answer" || format["strict"] != true {
		t.Fatalf("text.format = %+v, want flattened json_schema", format)
	}
}

func TestResponsesEventStreamToMessageStreamEmitsToolCalls(t *testing.T) {
	sr, sw := schema.Pipe[*provider.ResponsesStreamEvent](16)
	stream := responsesEventStreamToMessageStream(sr, false)

	sw.Send(&provider.ResponsesStreamEvent{
		Type:        "response.output_item.added",
		OutputIndex: 1,
		ItemID:      "fc_1",
		Item: &provider.ResponsesResponseOutput{
			ID:     "fc_1",
			Type:   "function_call",
			CallID: "call_1",
			Name:   "lookup",
		},
	}, nil)
	sw.Send(&provider.ResponsesStreamEvent{
		Type:        "response.function_call_arguments.delta",
		OutputIndex: 1,
		ItemID:      "fc_1",
		Delta:       `{"q":`,
	}, nil)
	sw.Send(&provider.ResponsesStreamEvent{
		Type:        "response.function_call_arguments.delta",
		OutputIndex: 1,
		ItemID:      "fc_1",
		Delta:       `"cat"}`,
	}, nil)
	sw.Send(&provider.ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 1,
		ItemID:      "fc_1",
		Item: &provider.ResponsesResponseOutput{
			ID:     "fc_1",
			Type:   "function_call",
			CallID: "call_1",
			Name:   "lookup",
		},
	}, nil)
	sw.Close()

	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want one", msg.ToolCalls)
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "lookup" || tc.Function.Arguments != `{"q":"cat"}` {
		t.Fatalf("tool call = %+v, want lookup with merged args", tc)
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("second Recv() error = %v, want EOF", err)
	}
}

func TestResponsesEventStreamToMessageStreamDeduplicatesToolCallTerminalEvents(t *testing.T) {
	sr, sw := schema.Pipe[*provider.ResponsesStreamEvent](16)
	stream := responsesEventStreamToMessageStream(sr, false)

	sw.Send(&provider.ResponsesStreamEvent{
		Type:        "response.output_item.added",
		OutputIndex: 1,
		ItemID:      "fc_1",
		Item: &provider.ResponsesResponseOutput{
			ID:     "fc_1",
			Type:   "function_call",
			CallID: "call_1",
			Name:   "lookup",
		},
	}, nil)
	sw.Send(&provider.ResponsesStreamEvent{
		Type:        "response.function_call_arguments.delta",
		OutputIndex: 1,
		ItemID:      "fc_1",
		Delta:       `{"q":"cat"}`,
	}, nil)
	sw.Send(&provider.ResponsesStreamEvent{
		Type:        "response.function_call_arguments.done",
		OutputIndex: 1,
		ItemID:      "fc_1",
	}, nil)
	sw.Send(&provider.ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 1,
		ItemID:      "fc_1",
		Item: &provider.ResponsesResponseOutput{
			ID:        "fc_1",
			Type:      "function_call",
			CallID:    "call_1",
			Name:      "lookup",
			Arguments: `{"q":"cat"}`,
		},
	}, nil)
	sw.Close()

	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want one", msg.ToolCalls)
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "lookup" {
		t.Fatalf("tool call = %+v, want lookup once", tc)
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("second Recv() error = %v, want EOF", err)
	}
}

func TestResponsesEventStreamToMessageStreamEmitsAgentToolCalls(t *testing.T) {
	sr, sw := schema.Pipe[*provider.ResponsesStreamEvent](16)
	stream := responsesEventStreamToMessageStream(sr, false)

	sw.Send(&provider.ResponsesStreamEvent{
		Type:        "response.output_item.added",
		OutputIndex: 1,
		ItemID:      "fc_1",
		Item: &provider.ResponsesResponseOutput{
			ID:     "fc_1",
			Type:   "function_call",
			CallID: "call_1",
			Name:   "Agent",
		},
	}, nil)
	sw.Send(&provider.ResponsesStreamEvent{
		Type:        "response.function_call_arguments.done",
		OutputIndex: 1,
		ItemID:      "fc_1",
		Delta:       `{"name":"researcher"}`,
	}, nil)
	sw.Close()

	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want one", msg.ToolCalls)
	}
	if msg.ToolCalls[0].Function.Name != "Agent" {
		t.Fatalf("tool call = %+v, want Agent", msg.ToolCalls[0])
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("second Recv() error = %v, want EOF", err)
	}
}

func TestResponsesEventStreamToMessageStreamCCCompatFiltersAgentToolCalls(t *testing.T) {
	sr, sw := schema.Pipe[*provider.ResponsesStreamEvent](16)
	stream := responsesEventStreamToMessageStream(sr, true)

	sw.Send(&provider.ResponsesStreamEvent{
		Type:        "response.output_item.added",
		OutputIndex: 1,
		ItemID:      "fc_1",
		Item: &provider.ResponsesResponseOutput{
			ID:     "fc_1",
			Type:   "function_call",
			CallID: "call_1",
			Name:   "Agent",
		},
	}, nil)
	sw.Send(&provider.ResponsesStreamEvent{
		Type:        "response.function_call_arguments.done",
		OutputIndex: 1,
		ItemID:      "fc_1",
		Delta:       `{"name":"researcher"}`,
	}, nil)
	sw.Close()

	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("Recv() error = %v, want EOF", err)
	}
}

func TestResponsesEventStreamToMessageStreamCompletesOnResponseCompleted(t *testing.T) {
	sr, sw := schema.Pipe[*provider.ResponsesStreamEvent](16)
	stream := responsesEventStreamToMessageStream(sr, false)

	sw.Send(&provider.ResponsesStreamEvent{
		Type: "response.completed",
		Response: &provider.ResponsesResponse{
			Usage: &provider.ResponsesResponseUsage{
				InputTokens:  12,
				OutputTokens: 34,
				TotalTokens:  46,
			},
			Output: []provider.ResponsesResponseOutput{{
				Type: "message",
				Role: "assistant",
				Content: []provider.ResponsesResponseContentPart{{
					Type: "output_text",
					Text: "done",
				}},
			}},
		},
	}, nil)

	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv() error = %v", err)
	}
	if msg.Content != "done" {
		t.Fatalf("content = %q, want done", msg.Content)
	}
	msg, err = stream.Recv()
	if err != nil {
		t.Fatalf("second Recv() error = %v", err)
	}
	if msg.ResponseMeta == nil || msg.ResponseMeta.FinishReason != "stop" {
		t.Fatalf("response meta = %+v, want stop", msg.ResponseMeta)
	}
	if msg.ResponseMeta.Usage == nil {
		t.Fatal("final streamed usage = nil, want token usage on response.completed")
	}
	if msg.ResponseMeta.Usage.PromptTokens != 12 || msg.ResponseMeta.Usage.CompletionTokens != 34 || msg.ResponseMeta.Usage.TotalTokens != 46 {
		t.Fatalf("usage = %+v, want prompt=12 completion=34 total=46", msg.ResponseMeta.Usage)
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("third Recv() error = %v, want EOF", err)
	}

	sw.Close()
}
