package provider

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestUsageFromMessagePreservesUpstreamTotal(t *testing.T) {
	// An upstream that reports only a total must not have it erased.
	msg := &schema.Message{ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{TotalTokens: 42}}}
	got := UsageFromMessage(msg)
	if got.TotalTokens != 42 {
		t.Fatalf("TotalTokens = %d, want upstream-reported 42", got.TotalTokens)
	}
}

func TestUsageFromMessageFallsBackToPromptPlusCompletion(t *testing.T) {
	msg := &schema.Message{ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
		PromptTokens:     10,
		CompletionTokens: 5,
		PromptTokenDetails: schema.PromptTokenDetails{
			CachedTokens: 4,
		},
		CompletionTokensDetails: schema.CompletionTokensDetails{
			ReasoningTokens: 2,
		},
	}}}
	got := UsageFromMessage(msg)
	if got.TotalTokens != 15 {
		t.Fatalf("TotalTokens = %d, want fallback 15 when the upstream total is zero", got.TotalTokens)
	}
	if got.CachedTokens != 4 || got.ReasoningTokens != 2 {
		t.Fatalf("details = cached %d reasoning %d, want 4 and 2", got.CachedTokens, got.ReasoningTokens)
	}
}

func TestResponsesUsageFromMessagePreservesTotalAndDetails(t *testing.T) {
	msg := &schema.Message{ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      21,
		PromptTokenDetails: schema.PromptTokenDetails{
			CachedTokens: 4,
		},
		CompletionTokensDetails: schema.CompletionTokensDetails{
			ReasoningTokens: 2,
		},
	}}}

	got := responsesUsageFromMessage(msg)
	if got.InputTokens != 10 || got.OutputTokens != 5 || got.TotalTokens != 21 {
		t.Fatalf("tokens = %d/%d/%d, want 10/5/21", got.InputTokens, got.OutputTokens, got.TotalTokens)
	}
	if got.InputTokensDetails.CachedTokens != 4 || got.OutputTokensDetails.ReasoningTokens != 2 {
		t.Fatalf("details = cached %d reasoning %d, want 4 and 2", got.InputTokensDetails.CachedTokens, got.OutputTokensDetails.ReasoningTokens)
	}
}
