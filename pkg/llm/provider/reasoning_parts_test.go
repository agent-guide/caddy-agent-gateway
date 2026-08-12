package provider

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestReasoningPartsConcatDoesNotMutateInput(t *testing.T) {
	index := 0
	first := NewReasoningOutputPart("aaa", "", &index)
	second := NewReasoningOutputPart("bbb", "opaque-", &index)
	third := NewReasoningOutputPart("", "signature", &index)
	groups := []ReasoningParts{{first}, {second}, {third}}

	concat := func() *schema.Message {
		chunks := []*schema.Message{
			AttachReasoningParts(&schema.Message{Role: schema.Assistant}, groups[0]...),
			AttachReasoningParts(&schema.Message{Role: schema.Assistant}, groups[1]...),
			AttachReasoningParts(&schema.Message{Role: schema.Assistant}, groups[2]...),
		}
		merged, err := schema.ConcatMessages(chunks)
		if err != nil {
			t.Fatalf("ConcatMessages() error = %v", err)
		}
		return merged
	}

	merged := concat()
	parts := ReasoningPartsFromMessage(merged)
	if len(parts) != 1 || parts[0].Reasoning == nil || parts[0].Reasoning.Text != "aaabbb" || parts[0].Reasoning.Signature != "opaque-signature" {
		t.Fatalf("first concat reasoning = %+v, want aaabbb with concatenated opaque signature", parts)
	}
	if got := groups[0][0].Reasoning.Text; got != "aaa" {
		t.Fatalf("first input reasoning = %q after concat, want aaa", got)
	}

	merged = concat()
	parts = ReasoningPartsFromMessage(merged)
	if len(parts) != 1 || parts[0].Reasoning == nil || parts[0].Reasoning.Text != "aaabbb" {
		t.Fatalf("second concat reasoning = %+v, want stable aaabbb", parts)
	}
}
