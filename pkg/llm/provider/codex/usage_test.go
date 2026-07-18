package codex

import (
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

func TestResponsesTokenUsageMapsDetailsAndTotalFallback(t *testing.T) {
	u := responsesTokenUsage(&provider.ResponsesResponseUsage{
		InputTokens:         12,
		OutputTokens:        8,
		InputTokensDetails:  provider.ResponsesInputTokensUsage{CachedTokens: 6},
		OutputTokensDetails: provider.ResponsesOutputTokensUsage{ReasoningTokens: 3},
	})
	if u.PromptTokens != 12 || u.CompletionTokens != 8 {
		t.Fatalf("Prompt/Completion = %d/%d, want 12/8", u.PromptTokens, u.CompletionTokens)
	}
	if u.TotalTokens != 20 {
		t.Fatalf("TotalTokens = %d, want fallback 20 when the wire total is zero", u.TotalTokens)
	}
	if u.PromptTokenDetails.CachedTokens != 6 {
		t.Fatalf("CachedTokens = %d, want 6", u.PromptTokenDetails.CachedTokens)
	}
	if u.CompletionTokensDetails.ReasoningTokens != 3 {
		t.Fatalf("ReasoningTokens = %d, want 3", u.CompletionTokensDetails.ReasoningTokens)
	}
}

func TestResponsesCompletionMessageCarriesUsage(t *testing.T) {
	msg := responsesCompletionMessage(&provider.ResponsesResponse{
		Usage: &provider.ResponsesResponseUsage{InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
	})
	if msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		t.Fatalf("completion message has no usage: %+v", msg)
	}
	if msg.ResponseMeta.Usage.TotalTokens != 6 {
		t.Fatalf("TotalTokens = %d, want 6", msg.ResponseMeta.Usage.TotalTokens)
	}
}
