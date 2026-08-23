package anthropicmsg

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestProfilesShareMessagesStreamContract(t *testing.T) {
	requestBody, err := json.Marshal(MessagesRequest{
		Model: "client-model", MaxTokens: 32, Stream: true,
		Messages: []MessageItem{{Role: "user", Content: MessageContent{{Type: "text", Text: "hello"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	serve := func(t *testing.T, profile Profile) string {
		t.Helper()
		handler := NewHandler(profile)
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(requestBody))
		prepared, _, err := handler.PrepareLLMApiRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		provider := &testStreamingProvider{chunks: []*schema.Message{
			{Role: schema.Assistant, Content: "answer", ResponseMeta: &schema.ResponseMeta{FinishReason: "stop"}},
		}}
		recorder := httptest.NewRecorder()
		if err := handler.ServeLLMApi(recorder, request, provider, prepared); err != nil {
			t.Fatal(err)
		}
		return recorder.Body.String()
	}

	standard := serve(t, StandardProfile())
	claudeCode := serve(t, ClaudeCodeProfile())
	messageID := regexp.MustCompile(`"id":"msg_[^"]+"`)
	standard = messageID.ReplaceAllString(standard, `"id":"msg_normalized"`)
	claudeCode = messageID.ReplaceAllString(claudeCode, `"id":"msg_normalized"`)
	if standard != claudeCode {
		t.Fatalf("profile stream drift\nstandard:\n%s\ncc:\n%s", standard, claudeCode)
	}
}

func TestProfilesDifferOnlyOnDeclaredTokenCountingShim(t *testing.T) {
	if StandardProfile().EstimateCountTokens {
		t.Fatal("standard profile unexpectedly estimates count_tokens")
	}
	if !ClaudeCodeProfile().EstimateCountTokens {
		t.Fatal("Claude Code profile is missing count_tokens estimate shim")
	}
	if StandardProfile().Name == ClaudeCodeProfile().Name {
		t.Fatal("profiles must retain distinct registered names")
	}
}
