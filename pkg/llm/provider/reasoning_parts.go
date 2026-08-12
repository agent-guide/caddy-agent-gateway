package provider

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

const (
	// ChatMessagePartTypeEncryptedReasoning carries opaque reasoning state that
	// must be replayed unchanged but is not safe to expose as readable text.
	// Anthropic redacted_thinking blocks are represented with this neutral
	// internal type so compatible providers can preserve them across tool turns.
	ChatMessagePartTypeEncryptedReasoning schema.ChatMessagePartType = "encrypted_reasoning"
	// ChatMessagePartTypeReasoningEnd marks the end of one streamed reasoning
	// block. It lets protocol adapters preserve multiple consecutive reasoning
	// blocks without folding their signatures together.
	ChatMessagePartTypeReasoningEnd schema.ChatMessagePartType = "reasoning_end"

	encryptedReasoningDataKey = "data"
	gatewayThinkingPrefix     = "agw-thinking-"
	reasoningPartsExtraKey    = "agent_gateway.reasoning_parts"
)

// ReasoningParts is the gateway-owned representation of structured reasoning
// carried in schema.Message.Extra. It intentionally does not use
// AssistantGenMultiContent, whose types and semantics belong to eino model
// components and differ between providers.
type ReasoningParts []schema.MessageOutputPart

func init() {
	compose.RegisterStreamChunkConcatFunc(func(groups []ReasoningParts) (ReasoningParts, error) {
		var merged ReasoningParts
		openReasoning := map[int]int{}
		for _, group := range groups {
			for _, part := range group {
				if part.StreamingMeta == nil {
					merged = append(merged, cloneReasoningPart(part))
					continue
				}
				index := part.StreamingMeta.Index
				switch part.Type {
				case schema.ChatMessagePartTypeReasoning:
					if position, ok := openReasoning[index]; ok {
						target := &merged[position]
						if target.Reasoning == nil {
							target.Reasoning = &schema.MessageOutputReasoning{}
						}
						if part.Reasoning != nil {
							target.Reasoning.Text += part.Reasoning.Text
							if part.Reasoning.Signature != "" {
								target.Reasoning.Signature += part.Reasoning.Signature
							}
						}
						continue
					}
					openReasoning[index] = len(merged)
					merged = append(merged, cloneReasoningPart(part))
				case ChatMessagePartTypeReasoningEnd:
					merged = append(merged, cloneReasoningPart(part))
					delete(openReasoning, index)
				default:
					merged = append(merged, cloneReasoningPart(part))
				}
			}
		}
		return merged, nil
	})
}

// cloneReasoningPart detaches the pointer fields that the concat function may
// update. Stream concat functions must not mutate their input chunks because
// callback/tap consumers may aggregate the same stream more than once.
func cloneReasoningPart(part schema.MessageOutputPart) schema.MessageOutputPart {
	cloned := part
	if part.Reasoning != nil {
		reasoning := *part.Reasoning
		cloned.Reasoning = &reasoning
	}
	if part.StreamingMeta != nil {
		meta := *part.StreamingMeta
		cloned.StreamingMeta = &meta
	}
	return cloned
}

// GatewayThinkingSignature returns a deterministic placeholder signature for
// reasoning produced by providers that do not expose an upstream signature.
// It is suitable for Anthropic client-facing responses, but must never be sent
// to an Anthropic upstream as though it were an authentic opaque signature.
func GatewayThinkingSignature(reasoning string) string {
	sum := sha256.Sum256([]byte(reasoning))
	return fmt.Sprintf("%s%x", gatewayThinkingPrefix, sum[:])
}

// IsGatewayThinkingSignature reports whether a signature was synthesized by
// GatewayThinkingSignature rather than supplied by an upstream model.
func IsGatewayThinkingSignature(signature string) bool {
	return strings.HasPrefix(signature, gatewayThinkingPrefix)
}

// NewReasoningOutputPart builds a structured reasoning part. index identifies
// the source stream content-block index when the part came from streaming.
func NewReasoningOutputPart(text, signature string, index *int) schema.MessageOutputPart {
	part := schema.MessageOutputPart{
		Type: schema.ChatMessagePartTypeReasoning,
		Reasoning: &schema.MessageOutputReasoning{
			Text:      text,
			Signature: signature,
		},
	}
	if index != nil {
		part.StreamingMeta = &schema.MessageStreamingMeta{Index: *index}
	}
	return part
}

// NewEncryptedReasoningOutputPart builds an opaque reasoning part. The data is
// deliberately not stored in ReasoningContent because callers must never edit,
// summarize, or expose it as model-authored text.
func NewEncryptedReasoningOutputPart(data string, index *int) schema.MessageOutputPart {
	part := schema.MessageOutputPart{
		Type:  ChatMessagePartTypeEncryptedReasoning,
		Extra: map[string]any{encryptedReasoningDataKey: data},
	}
	if index != nil {
		part.StreamingMeta = &schema.MessageStreamingMeta{Index: *index}
	}
	return part
}

// NewReasoningEndOutputPart marks a streamed reasoning content block complete.
func NewReasoningEndOutputPart(index int) schema.MessageOutputPart {
	return schema.MessageOutputPart{
		Type:          ChatMessagePartTypeReasoningEnd,
		StreamingMeta: &schema.MessageStreamingMeta{Index: index},
	}
}

// AttachReasoningParts appends gateway-owned structured reasoning metadata to
// msg without populating eino's provider-defined multimodal output field.
func AttachReasoningParts(msg *schema.Message, parts ...schema.MessageOutputPart) *schema.Message {
	if msg == nil || len(parts) == 0 {
		return msg
	}
	if msg.Extra == nil {
		msg.Extra = map[string]any{}
	}
	existing, _ := msg.Extra[reasoningPartsExtraKey].(ReasoningParts)
	combined := make(ReasoningParts, 0, len(existing)+len(parts))
	combined = append(combined, existing...)
	combined = append(combined, parts...)
	msg.Extra[reasoningPartsExtraKey] = combined
	return msg
}

// ReasoningPartsFromMessage returns a copy of the gateway-owned structured
// reasoning metadata attached to msg.
func ReasoningPartsFromMessage(msg *schema.Message) ReasoningParts {
	if msg == nil || len(msg.Extra) == 0 {
		return nil
	}
	parts, _ := msg.Extra[reasoningPartsExtraKey].(ReasoningParts)
	if len(parts) == 0 {
		return nil
	}
	return append(ReasoningParts(nil), parts...)
}

// EncryptedReasoningData returns the opaque payload from an encrypted
// reasoning part.
func EncryptedReasoningData(part schema.MessageOutputPart) string {
	if part.Type != ChatMessagePartTypeEncryptedReasoning || len(part.Extra) == 0 {
		return ""
	}
	data, _ := part.Extra[encryptedReasoningDataKey].(string)
	return data
}
