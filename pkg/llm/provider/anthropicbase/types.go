// Package anthropicbase provides shared Anthropic Messages API wire helpers.
package anthropicbase

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/cloudwego/eino/schema"
)

type MessagesRequest struct {
	Model             string             `json:"model"`
	MaxTokens         int                `json:"max_tokens"`
	Messages          []MessageItem      `json:"messages"`
	System            []SystemBlock      `json:"system,omitempty"`
	Tools             []ToolDef          `json:"tools"`
	ToolChoice        json.RawMessage    `json:"tool_choice,omitempty"`
	Metadata          *RequestMetadata   `json:"metadata,omitempty"`
	Thinking          *ThinkingConfig    `json:"thinking,omitempty"`
	ContextManagement *ContextManagement `json:"context_management,omitempty"`
	OutputConfig      *OutputConfig      `json:"output_config,omitempty"`
	Temperature       float64            `json:"temperature,omitempty"`
	TopP              float64            `json:"top_p,omitempty"`
	TopK              int                `json:"top_k,omitempty"`
	StopSequences     []string           `json:"stop_sequences,omitempty"`
	Stream            bool               `json:"stream,omitempty"`
}

type MessageItem struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

type ContentBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	Data         string          `json:"data,omitempty"`
	CacheControl *CacheControl   `json:"cache_control,omitempty"`
	Source       *ImageSource    `json:"source,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      any             `json:"content,omitempty"`
	Raw          json.RawMessage `json:"-"`
	// decoded is the modeled subset of Raw as it was received. MarshalJSON
	// diffs against it so later mutations, including clearing a field, win over
	// Raw while unmodeled Anthropic fields survive untouched.
	decoded json.RawMessage
}

func (b ContentBlock) MarshalJSON() ([]byte, error) {
	type wireContentBlock ContentBlock
	known, err := json.Marshal(wireContentBlock(b))
	if err != nil || len(b.Raw) == 0 {
		return known, err
	}
	return overlayRawJSONObject(b.Raw, b.decoded, known)
}

func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	type wireContentBlock ContentBlock
	var decoded wireContentBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*b = ContentBlock(decoded)
	b.Raw = append(b.Raw[:0], data...)
	b.decoded, _ = json.Marshal(decoded)
	return nil
}

// OpaqueContentBlock wraps a content block the modeled struct cannot decode so
// it is still replayed byte for byte instead of being dropped.
func OpaqueContentBlock(raw json.RawMessage) ContentBlock {
	return ContentBlock{Raw: append(json.RawMessage(nil), raw...)}
}

type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type SystemBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text,omitempty"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	Raw         json.RawMessage `json:"-"`
	// decoded mirrors ContentBlock.decoded: the modeled subset of Raw as
	// received, used to detect explicit mutations.
	decoded json.RawMessage
}

// MarshalJSON retains the unmodeled fields of a native Anthropic tool, while
// explicit mutations (for example compact=codex name aliases) override the
// values decoded from Raw.
func (t ToolDef) MarshalJSON() ([]byte, error) {
	type wireToolDef ToolDef
	known, err := json.Marshal(wireToolDef(t))
	if err != nil || len(t.Raw) == 0 {
		return known, err
	}
	return overlayRawJSONObject(t.Raw, t.decoded, known)
}

func (t *ToolDef) UnmarshalJSON(data []byte) error {
	type wireToolDef ToolDef
	var decoded wireToolDef
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*t = ToolDef(decoded)
	t.Raw = append(t.Raw[:0], data...)
	t.decoded, _ = json.Marshal(decoded)
	return nil
}

// OpaqueToolDef wraps a tool definition the modeled struct cannot decode so it
// is still forwarded byte for byte instead of being dropped.
func OpaqueToolDef(raw json.RawMessage) ToolDef {
	return ToolDef{Raw: append(json.RawMessage(nil), raw...)}
}

type RequestMetadata struct {
	UserID string `json:"user_id"`
}

type ThinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"`
}

type ContextManagement struct {
	Edits []ContextManagementEdit `json:"edits,omitempty"`
}

type ContextManagementEdit struct {
	Type string `json:"type"`
	Keep string `json:"keep"`
}

type OutputConfig struct {
	Effort string        `json:"effort,omitempty"`
	Format *OutputFormat `json:"format,omitempty"`
}

// OutputFormat carries an Anthropic structured-output format. Only the
// json_schema type is supported by the Messages API.
type OutputFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

type ResponseBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

func (b *ResponseBlock) UnmarshalJSON(data []byte) error {
	type wireResponseBlock ResponseBlock
	var decoded wireResponseBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*b = ResponseBlock(decoded)
	b.Raw = append(b.Raw[:0], data...)
	return nil
}

// responseBlockModeledFields must stay synchronized with both ResponseBlock's
// JSON tags and ToChatResponse's per-type conversion. A field modeled for one
// block type is still unmodeled on another type and must trigger exact replay.
var responseBlockModeledFields = map[string]map[string]struct{}{
	"text":              {"type": {}, "text": {}},
	"thinking":          {"type": {}, "thinking": {}, "signature": {}},
	"redacted_thinking": {"type": {}, "data": {}},
	"tool_use":          {"type": {}, "id": {}, "name": {}, "input": {}},
}

// requiresNativeReplay reports whether this block would lose information when
// it is rebuilt from the generic message model. Only such blocks pin a
// conversation to an Anthropic-native provider, so an ordinary text or
// tool_use answer stays portable across providers.
func (b ResponseBlock) requiresNativeReplay() bool {
	modeledFields, ok := responseBlockModeledFields[b.Type]
	if !ok {
		return true
	}
	if len(b.Raw) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b.Raw, &fields) != nil {
		return true
	}
	for known := range modeledFields {
		delete(fields, known)
	}
	for _, value := range fields {
		if strings.TrimSpace(string(value)) != "null" {
			return true
		}
	}
	return false
}

// overlayRawJSONObject rebuilds a JSON object field-for-field from the value it
// decoded, applying only the modeled fields that changed since decoding. Key
// order is not preserved. before is
// the modeled subset as received; a nil before means the value was never
// decoded, in which case only non-zero modeled fields are applied so an opaque
// raw payload round-trips untouched.
func overlayRawJSONObject(raw, before, after json.RawMessage) ([]byte, error) {
	var base map[string]json.RawMessage
	if err := json.Unmarshal(raw, &base); err != nil {
		return nil, err
	}
	var current map[string]json.RawMessage
	if err := json.Unmarshal(after, &current); err != nil {
		return nil, err
	}
	if len(before) == 0 {
		for key, value := range current {
			if !isZeroJSONValue(value) {
				base[key] = value
			}
		}
		return json.Marshal(base)
	}
	var original map[string]json.RawMessage
	if err := json.Unmarshal(before, &original); err != nil {
		return nil, err
	}
	for key, value := range current {
		if !bytes.Equal(original[key], value) {
			base[key] = value
		}
	}
	for key := range original {
		if _, ok := current[key]; !ok {
			delete(base, key)
		}
	}
	return json.Marshal(base)
}

func isZeroJSONValue(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte(`""`))
}

type MessagesResponse struct {
	Content    []ResponseBlock `json:"content"`
	StopReason string          `json:"stop_reason"`
	Usage      MessagesUsage   `json:"usage"`
}

// MessagesUsage is the Anthropic Messages usage payload. input_tokens excludes
// cache reads and cache writes; the effective prompt size is the sum of all
// three fields.
type MessagesUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// TokenUsage converts the wire usage into eino token usage. Prompt tokens are
// input + cache read + cache creation — the same accounting the eino-ext
// claude component uses — so the anthropic and claudecode providers meter
// identically. CachedTokens carries the cache-read subset.
func (u MessagesUsage) TokenUsage() *schema.TokenUsage {
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return &schema.TokenUsage{
		PromptTokens: prompt,
		PromptTokenDetails: schema.PromptTokenDetails{
			CachedTokens: u.CacheReadInputTokens,
		},
		CompletionTokens: u.OutputTokens,
		TotalTokens:      prompt + u.OutputTokens,
	}
}

type ModelsResponse struct {
	Data []ModelData `json:"data"`
}

type ModelData struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}
