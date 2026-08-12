package anthropicbase

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

type StreamState struct {
	pendingToolCalls map[int]*pendingToolCall
	reasoningBlocks  map[int]struct{}
	usage            MessagesUsage
}

type pendingToolCall struct {
	index int
	id    string
	name  string
	input strings.Builder
}

func ReadMessageStream(body io.ReadCloser, sw *schema.StreamWriter[*schema.Message], errorPrefix string) {
	defer body.Close()
	defer sw.Close()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	state := &StreamState{
		pendingToolCalls: make(map[int]*pendingToolCall),
		reasoningBlocks:  make(map[int]struct{}),
	}
	var eventName string
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: "):
			data.WriteString(strings.TrimPrefix(line, "data: "))
		case line == "":
			if err := EmitStreamEvent(eventName, data.String(), sw, state, errorPrefix); err != nil {
				sw.Send(nil, err)
				return
			}
			eventName = ""
			data.Reset()
		}
	}
	if err := scanner.Err(); err != nil {
		sw.Send(nil, fmt.Errorf("%s: read stream: %w", errorPrefix, err))
	}
}

func EmitStreamEvent(eventName string, payload string, sw *schema.StreamWriter[*schema.Message], state *StreamState, errorPrefix string) error {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil
	}
	if state == nil {
		state = &StreamState{
			pendingToolCalls: make(map[int]*pendingToolCall),
			reasoningBlocks:  make(map[int]struct{}),
		}
	}
	if state.pendingToolCalls == nil {
		state.pendingToolCalls = make(map[int]*pendingToolCall)
	}
	if state.reasoningBlocks == nil {
		state.reasoningBlocks = make(map[int]struct{})
	}

	switch eventName {
	case "message_start":
		var event struct {
			Message struct {
				Usage MessagesUsage `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return fmt.Errorf("%s: decode message_start: %w", errorPrefix, err)
		}
		state.usage = event.Message.Usage

	case "content_block_start":
		var event struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
				Data string `json:"data"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return fmt.Errorf("%s: decode content_block_start: %w", errorPrefix, err)
		}
		switch event.ContentBlock.Type {
		case "thinking":
			state.reasoningBlocks[event.Index] = struct{}{}
			part := provider.NewReasoningOutputPart("", "", &event.Index)
			sw.Send(provider.AttachReasoningParts(&schema.Message{Role: schema.Assistant}, part), nil)
		case "redacted_thinking":
			if event.ContentBlock.Data != "" {
				part := provider.NewEncryptedReasoningOutputPart(event.ContentBlock.Data, &event.Index)
				sw.Send(provider.AttachReasoningParts(&schema.Message{Role: schema.Assistant}, part), nil)
			}
		case "tool_use":
			state.pendingToolCalls[event.Index] = &pendingToolCall{
				index: event.Index,
				id:    event.ContentBlock.ID,
				name:  event.ContentBlock.Name,
			}
		}

	case "content_block_delta":
		var event struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return fmt.Errorf("%s: decode stream delta: %w", errorPrefix, err)
		}
		switch event.Delta.Type {
		case "thinking_delta":
			part := provider.NewReasoningOutputPart(event.Delta.Thinking, "", &event.Index)
			sw.Send(provider.AttachReasoningParts(&schema.Message{
				Role:             schema.Assistant,
				ReasoningContent: event.Delta.Thinking,
			}, part), nil)
		case "signature_delta":
			part := provider.NewReasoningOutputPart("", event.Delta.Signature, &event.Index)
			sw.Send(provider.AttachReasoningParts(&schema.Message{Role: schema.Assistant}, part), nil)
		case "text_delta":
			if event.Delta.Text != "" {
				sw.Send(&schema.Message{Role: schema.Assistant, Content: event.Delta.Text}, nil)
			}
		case "input_json_delta":
			if ptc, ok := state.pendingToolCalls[event.Index]; ok {
				ptc.input.WriteString(event.Delta.PartialJSON)
			}
		}

	case "content_block_stop":
		var event struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return fmt.Errorf("%s: decode content_block_stop: %w", errorPrefix, err)
		}
		if ptc, ok := state.pendingToolCalls[event.Index]; ok {
			inputStr := ptc.input.String()
			if inputStr == "" {
				inputStr = "{}"
			}
			sw.Send(&schema.Message{
				Role: schema.Assistant,
				ToolCalls: []schema.ToolCall{{
					ID:   ptc.id,
					Type: "function",
					Function: schema.FunctionCall{
						Name:      ptc.name,
						Arguments: inputStr,
					},
				}},
			}, nil)
			delete(state.pendingToolCalls, event.Index)
		}
		if _, ok := state.reasoningBlocks[event.Index]; ok {
			part := provider.NewReasoningEndOutputPart(event.Index)
			sw.Send(provider.AttachReasoningParts(&schema.Message{Role: schema.Assistant}, part), nil)
			delete(state.reasoningBlocks, event.Index)
		}

	case "message_delta":
		var event struct {
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return fmt.Errorf("%s: decode message delta: %w", errorPrefix, err)
		}
		if event.Usage.OutputTokens > 0 || event.Delta.StopReason != "" || state.usage != (MessagesUsage{}) {
			finalUsage := state.usage
			finalUsage.OutputTokens = event.Usage.OutputTokens
			sw.Send(&schema.Message{
				Role:    schema.Assistant,
				Content: "",
				ResponseMeta: &schema.ResponseMeta{
					FinishReason: event.Delta.StopReason,
					Usage:        finalUsage.TokenUsage(),
				},
			}, nil)
		}
	}
	return nil
}
