package provider

import (
	"context"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/credential"
)

func TestAPIKeyFromContextOrConfigUsesConfigFallbackByDefault(t *testing.T) {
	got := APIKeyFromContextOrConfig(context.Background(), "config-key")
	if got != "config-key" {
		t.Fatalf("APIKeyFromContextOrConfig() = %q, want config-key", got)
	}
}

func TestAPIKeyFromContextOrConfigPrefersContextCredential(t *testing.T) {
	ctx := WithCredential(context.Background(), &credential.Credential{
		Attributes: map[string]string{"api_key": "managed-key"},
	})
	got := APIKeyFromContextOrConfig(ctx, "config-key")
	if got != "managed-key" {
		t.Fatalf("APIKeyFromContextOrConfig() = %q, want managed-key", got)
	}
}
