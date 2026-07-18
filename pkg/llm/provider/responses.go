package provider

import (
	"context"
	"encoding/json"

	"github.com/cloudwego/eino/schema"
)

// ResponsesProvider is an optional interface for providers that expose
// OpenAI-compatible Responses API semantics directly.
type ResponsesProvider interface {
	Provider
	CreateResponses(ctx context.Context, req *ResponsesRequest) (*ResponsesResponse, error)
	StreamResponses(ctx context.Context, req *ResponsesRequest) (*schema.StreamReader[*ResponsesStreamEvent], error)
}

// ResponsesRequest is the minimal provider-level request model for the OpenAI
// Responses API. Input is intentionally preserved as structured JSON to avoid
// collapsing it into the chat-only ChatRequest abstraction.
type ResponsesRequest struct {
	Model              string                    `json:"model"`
	Input              any                       `json:"input"`
	Tools              []ResponsesToolDefinition `json:"tools,omitempty"`
	ToolChoice         json.RawMessage           `json:"tool_choice,omitempty"`
	MaxOutputTokens    int                       `json:"max_output_tokens,omitempty"`
	Temperature        float64                   `json:"temperature,omitempty"`
	TopP               float64                   `json:"top_p,omitempty"`
	Stream             bool                      `json:"stream,omitempty"`
	Instructions       string                    `json:"instructions,omitempty"`
	PreviousResponseID string                    `json:"previous_response_id,omitempty"`
	Store              *bool                     `json:"store,omitempty"`
	Text               map[string]any            `json:"text,omitempty"`
	Metadata           map[string]any            `json:"metadata,omitempty"`
	User               string                    `json:"user,omitempty"`
	Reasoning          map[string]any            `json:"reasoning,omitempty"`
	ParallelToolCalls  *bool                     `json:"parallel_tool_calls,omitempty"`
	Truncation         any                       `json:"truncation,omitempty"`
}

type ResponsesToolDefinition struct {
	Type        string                 `json:"type"`
	Name        string                 `json:"name,omitempty"`
	Description string                 `json:"description,omitempty"`
	Parameters  json.RawMessage        `json:"parameters,omitempty"`
	Function    *ResponsesToolFunction `json:"function,omitempty"`
}

type ResponsesToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ResponsesResponse mirrors the OpenAI-compatible Responses API envelope.
type ResponsesResponse struct {
	ID        string                    `json:"id"`
	Object    string                    `json:"object"`
	CreatedAt int64                     `json:"created_at"`
	Model     string                    `json:"model"`
	Output    []ResponsesResponseOutput `json:"output"`
	Usage     *ResponsesResponseUsage   `json:"usage,omitempty"`
	RawJSON   json.RawMessage           `json:"-"`
}

type ResponsesResponseOutput struct {
	ID        string                         `json:"id,omitempty"`
	Type      string                         `json:"type"`
	Role      string                         `json:"role,omitempty"`
	Status    string                         `json:"status,omitempty"`
	Content   []ResponsesResponseContentPart `json:"content,omitempty"`
	CallID    string                         `json:"call_id,omitempty"`
	Name      string                         `json:"name,omitempty"`
	Arguments string                         `json:"arguments,omitempty"`
}

type ResponsesResponseContentPart struct {
	Type        string          `json:"type"`
	Text        string          `json:"text,omitempty"`
	Annotations []any           `json:"annotations,omitempty"`
	Refusal     string          `json:"refusal,omitempty"`
	Summary     []any           `json:"summary,omitempty"`
	RawJSON     json.RawMessage `json:"-"`
}

type ResponsesResponseUsage struct {
	InputTokens         int                        `json:"input_tokens"`
	OutputTokens        int                        `json:"output_tokens"`
	TotalTokens         int                        `json:"total_tokens"`
	InputTokensDetails  ResponsesInputTokensUsage  `json:"input_tokens_details,omitzero"`
	OutputTokensDetails ResponsesOutputTokensUsage `json:"output_tokens_details,omitzero"`
}

type ResponsesInputTokensUsage struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

type ResponsesOutputTokensUsage struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// ResponsesStreamEvent is the minimal event model currently required by the
// gateway for OpenAI-compatible Responses API streaming.
type ResponsesStreamEvent struct {
	Type         string                   `json:"type"`
	Response     *ResponsesResponse       `json:"response,omitempty"`
	Item         *ResponsesResponseOutput `json:"item,omitempty"`
	Delta        string                   `json:"delta,omitempty"`
	ItemID       string                   `json:"item_id,omitempty"`
	OutputIndex  int                      `json:"output_index,omitempty"`
	ContentIndex int                      `json:"content_index,omitempty"`
	RawJSON      json.RawMessage          `json:"-"`
}
