package virtualkey

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimitsRejectUnknownFields(t *testing.T) {
	for _, input := range []string{
		`{"id":"vk","key":"secret","rate_limits":{"lmm":{"requests_per_minute":1,"burst":1}}}`,
		`{"id":"vk","key":"secret","rate_limits":{"llm":{"requests_per_minute":1,"brust":1}}}`,
	} {
		var key VirtualKey
		if err := json.Unmarshal([]byte(input), &key); err == nil {
			t.Fatalf("json.Unmarshal(%s) returned nil error", input)
		}
	}
}

func TestValidateConfigurationRateLimits(t *testing.T) {
	valid := VirtualKey{RateLimits: &VirtualKeyRateLimits{
		LLM: &RateLimit{RequestsPerMinute: 60, Burst: 10},
	}}
	if err := valid.ValidateConfiguration(); err != nil {
		t.Fatalf("valid policy returned error: %v", err)
	}
	for _, limit := range []RateLimit{
		{RequestsPerMinute: 0, Burst: 1},
		{RequestsPerMinute: 1, Burst: 0},
	} {
		key := VirtualKey{RateLimits: &VirtualKeyRateLimits{MCP: &limit}}
		if err := key.ValidateConfiguration(); err == nil {
			t.Fatalf("ValidateConfiguration(%+v) returned nil error", limit)
		}
	}
}

func TestExtractAPIKeysFromBearerToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer lk-test")
	if got := ExtractAPIKeys(req); len(got) != 1 || got[0] != "lk-test" {
		t.Fatalf("ExtractAPIKeys = %#v, want [lk-test]", got)
	}
}

func TestExtractAPIKeysReturnsBothHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-api-key", "sk-unrelated")
	req.Header.Set("Authorization", "Bearer vk-real")
	got := ExtractAPIKeys(req)
	if len(got) != 2 || got[0] != "sk-unrelated" || got[1] != "vk-real" {
		t.Fatalf("ExtractAPIKeys = %#v, want [sk-unrelated vk-real]", got)
	}
}

func TestExtractAPIKeysDedupesAndSkipsEmpty(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-api-key", "vk-same")
	req.Header.Set("Authorization", "Bearer vk-same")
	if got := ExtractAPIKeys(req); len(got) != 1 || got[0] != "vk-same" {
		t.Fatalf("ExtractAPIKeys = %#v, want [vk-same]", got)
	}

	none := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if got := ExtractAPIKeys(none); got != nil {
		t.Fatalf("ExtractAPIKeys = %#v, want nil", got)
	}
}
