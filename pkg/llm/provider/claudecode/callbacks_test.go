package claudecode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/httpclient"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// recordingHandler captures chat-model callback timings the way the gateway's
// einotap (or any vendor handler) would observe them.
type recordingHandler struct {
	starts  int
	ends    int
	errors  int
	lastEnd *einomodel.CallbackOutput
}

func (h *recordingHandler) OnStart(ctx context.Context, info *callbacks.RunInfo, _ callbacks.CallbackInput) context.Context {
	if info != nil && info.Component == components.ComponentOfChatModel {
		h.starts++
	}
	return ctx
}

func (h *recordingHandler) OnEnd(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
	if info != nil && info.Component == components.ComponentOfChatModel {
		h.ends++
		h.lastEnd = einomodel.ConvCallbackOutput(output)
	}
	return ctx
}

func (h *recordingHandler) OnError(ctx context.Context, info *callbacks.RunInfo, _ error) context.Context {
	if info != nil && info.Component == components.ComponentOfChatModel {
		h.errors++
	}
	return ctx
}

func (h *recordingHandler) OnStartWithStreamInput(ctx context.Context, _ *callbacks.RunInfo,
	input *schema.StreamReader[callbacks.CallbackInput]) context.Context {
	input.Close()
	return ctx
}

func (h *recordingHandler) OnEndWithStreamOutput(ctx context.Context, _ *callbacks.RunInfo,
	output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
	output.Close()
	return ctx
}

func TestChatFiresModelCallbackAspects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":12,"output_tokens":34,"cache_read_input_tokens":5}}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		APIKey:  "sk-test",
		BaseURL: server.URL,
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	handler := &recordingHandler{}
	ctx := callbacks.InitCallbacks(t.Context(), nil, handler)
	if _, err := prov.Chat(ctx, &provider.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []*schema.Message{schema.UserMessage("hello")},
	}); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if handler.starts != 1 || handler.ends != 1 || handler.errors != 0 {
		t.Fatalf("timings = %d starts / %d ends / %d errors, want 1/1/0", handler.starts, handler.ends, handler.errors)
	}
	if handler.lastEnd == nil || handler.lastEnd.TokenUsage == nil {
		t.Fatalf("OnEnd output = %+v, want token usage attached", handler.lastEnd)
	}
	tu := handler.lastEnd.TokenUsage
	if tu.PromptTokens != 17 || tu.CompletionTokens != 34 {
		t.Fatalf("token usage = %d/%d, want prompt 17 (input+cache read) and completion 34", tu.PromptTokens, tu.CompletionTokens)
	}
	if tu.PromptTokenDetails.CachedTokens != 5 {
		t.Fatalf("CachedTokens = %d, want 5", tu.PromptTokenDetails.CachedTokens)
	}
}
