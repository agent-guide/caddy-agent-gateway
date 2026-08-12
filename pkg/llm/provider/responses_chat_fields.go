package provider

import (
	"strings"

	einomodel "github.com/cloudwego/eino/components/model"
)

// ReasoningFieldStyle selects how reasoning is expressed on an OpenAI-compatible
// chat-completions wire shape.
type ReasoningFieldStyle int

const (
	// ReasoningEffortField emits reasoning as a "reasoning_effort" string, the
	// standard OpenAI chat-completions shape.
	ReasoningEffortField ReasoningFieldStyle = iota
	// ReasoningObjectField emits reasoning as a "reasoning" object, the shape
	// used by OpenAI-compatible upstreams that accept a structured reasoning
	// config (e.g. OpenRouter).
	ReasoningObjectField
)

// ChatCompletionsFieldPolicy describes the reasoning controls accepted by one
// chat-completions wire dialect. Anthropic-native `thinking` must be opted into
// explicitly instead of leaking into every OpenAI-compatible provider.
type ChatCompletionsFieldPolicy struct {
	ReasoningStyle ReasoningFieldStyle
	AllowThinking  bool
	AllowedEfforts []string
	EffortAliases  map[string]string
	// ThinkingBudgetField maps Anthropic thinking.budget_tokens into a field
	// on a structured reasoning object. It is ignored for effort-style dialects.
	ThinkingBudgetField string
}

var (
	OpenAIChatCompletionsFields = ChatCompletionsFieldPolicy{
		ReasoningStyle: ReasoningEffortField,
		AllowedEfforts: []string{"none", "minimal", "low", "medium", "high", "xhigh"},
		EffortAliases:  map[string]string{"max": "xhigh"},
	}
	OpenRouterChatCompletionsFields = ChatCompletionsFieldPolicy{
		ReasoningStyle:      ReasoningObjectField,
		ThinkingBudgetField: "max_tokens",
	}
	ZhipuChatCompletionsFields = ChatCompletionsFieldPolicy{
		ReasoningStyle: ReasoningEffortField,
		AllowThinking:  true,
		AllowedEfforts: []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"},
	}
)

// ChatCompletionsExtraFieldsFromOptions maps preserved request context onto
// chat-completions-style request fields shared by OpenAI-compatible upstreams.
// It folds in both the Responses API request context (when a Responses request
// was bridged onto chat) and the inbound chat extra fields, emitting only fields
// expressible on the chat wire shape. The reasoning style selects how reasoning
// is encoded for the target dialect.
func ChatCompletionsExtraFieldsFromOptions(policy ChatCompletionsFieldPolicy, opts ...einomodel.Option) map[string]any {
	fields := map[string]any{}

	if ctx := ResponsesRequestContextFromOptions(opts...); ctx != nil {
		if format := responseFormatFromResponsesText(ctx.Text); format != nil {
			fields["response_format"] = format
		}
		switch policy.ReasoningStyle {
		case ReasoningObjectField:
			if len(ctx.Reasoning) > 0 {
				fields["reasoning"] = normalizedReasoningObject(ctx.Reasoning, policy)
			}
		default:
			if effort := normalizeReasoningEffort(reasoningEffortFromResponsesReasoning(ctx.Reasoning), policy); effort != "" {
				fields["reasoning_effort"] = effort
			}
		}
		if strings.TrimSpace(ctx.User) != "" {
			fields["user"] = ctx.User
		}
		if len(ctx.Metadata) > 0 {
			fields["metadata"] = cloneMap(ctx.Metadata)
		}
		if ctx.ParallelToolCalls != nil {
			fields["parallel_tool_calls"] = *ctx.ParallelToolCalls
		}
		if ctx.Store != nil {
			fields["store"] = *ctx.Store
		}
	}

	if chatExtra := ChatExtraFieldsFromOptions(opts...); chatExtra != nil {
		if chatExtra.ResponseFormat != nil {
			fields["response_format"] = chatExtra.ResponseFormat
		}
		if len(chatExtra.Reasoning) > 0 {
			if policy.ReasoningStyle == ReasoningObjectField {
				fields["reasoning"] = normalizedReasoningObject(chatExtra.Reasoning, policy)
			} else if effort := normalizeReasoningEffort(reasoningEffortFromResponsesReasoning(chatExtra.Reasoning), policy); effort != "" {
				fields["reasoning_effort"] = effort
			}
		}
		if effort := normalizeReasoningEffort(chatExtra.ReasoningEffort, policy); effort != "" {
			if policy.ReasoningStyle == ReasoningObjectField {
				reasoningObject, _ := fields["reasoning"].(map[string]any)
				if reasoningObject == nil {
					reasoningObject = map[string]any{}
				}
				reasoningObject["effort"] = effort
				fields["reasoning"] = reasoningObject
			} else {
				fields["reasoning_effort"] = effort
			}
		}
		if policy.AllowThinking && len(chatExtra.Thinking) > 0 {
			fields["thinking"] = cloneMap(chatExtra.Thinking)
		}
		if policy.ReasoningStyle == ReasoningObjectField && policy.ThinkingBudgetField != "" {
			if budget, ok := PositiveIntValue(chatExtra.Thinking["budget_tokens"]); ok {
				reasoningObject, _ := fields["reasoning"].(map[string]any)
				if reasoningObject == nil {
					reasoningObject = map[string]any{}
				}
				if _, exists := reasoningObject[policy.ThinkingBudgetField]; !exists {
					reasoningObject[policy.ThinkingBudgetField] = budget
				}
				fields["reasoning"] = reasoningObject
			}
		}
		if chatExtra.ToolStream != nil {
			fields["tool_stream"] = *chatExtra.ToolStream
		}
		if len(chatExtra.StreamOptions) > 0 {
			fields["stream_options"] = cloneMap(chatExtra.StreamOptions)
		}
		if user := strings.TrimSpace(chatExtra.User); user != "" {
			fields["user"] = user
		}
		if len(chatExtra.Metadata) > 0 {
			fields["metadata"] = cloneMap(chatExtra.Metadata)
		}
		if chatExtra.ParallelToolCalls != nil {
			fields["parallel_tool_calls"] = *chatExtra.ParallelToolCalls
		}
		if chatExtra.Store != nil {
			fields["store"] = *chatExtra.Store
		}
	}

	if len(fields) == 0 {
		return nil
	}
	return fields
}

func normalizedReasoningObject(reasoning map[string]any, policy ChatCompletionsFieldPolicy) map[string]any {
	normalized := cloneMap(reasoning)
	if effort, _ := normalized["effort"].(string); effort != "" {
		if value := normalizeReasoningEffort(effort, policy); value != "" {
			normalized["effort"] = value
		} else {
			delete(normalized, "effort")
		}
	}
	return normalized
}

func normalizeReasoningEffort(effort string, policy ChatCompletionsFieldPolicy) string {
	normalized := strings.ToLower(strings.TrimSpace(effort))
	if normalized == "" {
		return ""
	}
	if alias := policy.EffortAliases[normalized]; alias != "" {
		normalized = alias
	}
	if policy.AllowedEfforts == nil {
		return normalized
	}
	for _, allowed := range policy.AllowedEfforts {
		if normalized == allowed {
			return normalized
		}
	}
	return ""
}

// StripCCUnsupportedChatFields removes the OpenAI-style `metadata` and `user`
// chat-completions request fields. Some OpenAI-compatible upstreams (e.g. GLM)
// reject these with a generic 400, while Claude Code always populates
// `metadata.user_id`. Providers running in cc-compat mode call this to drop the
// unsupported fields before the upstream request. It is a no-op on a nil map.
func StripCCUnsupportedChatFields(fields map[string]any) {
	delete(fields, "metadata")
	delete(fields, "user")
}

// MergeExtraFields shallow-merges one or more request-body extension maps.
// Later maps override earlier keys.
func MergeExtraFields(base map[string]any, overlays ...map[string]any) map[string]any {
	merged := cloneMap(base)
	for _, overlay := range overlays {
		if len(overlay) == 0 {
			continue
		}
		if merged == nil {
			merged = map[string]any{}
		}
		for k, v := range overlay {
			merged[k] = v
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// responseFormatFromResponsesText converts the Responses API "text.format" object
// into the chat-completions "response_format" shape. The Responses API flattens
// json_schema fields (name/schema/strict) at the format level, while chat
// completions nests them under a "json_schema" key. Other format types
// (text, json_object) are already compatible and pass through unchanged.
func responseFormatFromResponsesText(text map[string]any) any {
	if len(text) == 0 {
		return nil
	}
	raw, ok := text["format"]
	if !ok {
		return nil
	}
	format, ok := raw.(map[string]any)
	if !ok {
		return raw
	}
	if formatType, _ := format["type"].(string); formatType != "json_schema" {
		return format
	}

	jsonSchema := map[string]any{}
	for _, key := range []string{"name", "description", "schema", "strict"} {
		if v, ok := format[key]; ok {
			jsonSchema[key] = v
		}
	}
	return map[string]any{
		"type":        "json_schema",
		"json_schema": jsonSchema,
	}
}

func reasoningEffortFromResponsesReasoning(reasoning map[string]any) string {
	if len(reasoning) == 0 {
		return ""
	}
	effort, ok := reasoning["effort"].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(effort)
}
