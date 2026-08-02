package scheduler

import (
	"context"
	"testing"
	"time"
)

func TestPickUsesCredentialScope(t *testing.T) {
	s := NewScheduler(nil)
	s.RegisterCredential(&ManagedCredential{
		Credential: Credential{
			ID:           "openai-main",
			ProviderType: "openai",
			ProviderID:   "openai-main",
			Scope:        "id:openai-main",
			Type:         "api_key",
		},
	})

	picked, err := s.Pick(context.Background(), Filter{
		CredentialScope: "id:openai-main",
		Model:           "gpt-test",
	}, nil)
	if err != nil {
		t.Fatalf("Pick returned error: %v", err)
	}
	if picked == nil || picked.ID != "openai-main" {
		t.Fatalf("picked credential = %#v, want openai-main", picked)
	}
}

func TestPickUsesExplicitCredentialScope(t *testing.T) {
	s := NewScheduler(nil)
	s.RegisterCredential(&ManagedCredential{
		Credential: Credential{
			ID:           "openai-shared",
			ProviderType: "openai",
			ProviderID:   "openai-main",
			Scope:        "type:openai",
			Type:         "api_key",
		},
	})

	picked, err := s.Pick(context.Background(), Filter{
		CredentialScope: "type:openai",
		Model:           "gpt-test",
	}, nil)
	if err != nil {
		t.Fatalf("Pick returned error: %v", err)
	}
	if picked == nil || picked.ID != "openai-shared" {
		t.Fatalf("picked credential = %#v, want openai-shared", picked)
	}
}

func TestCredentialWideFailureBlocksEveryModel(t *testing.T) {
	s := NewScheduler(nil)
	s.RegisterCredential(&ManagedCredential{
		Credential: Credential{
			ID:           "oauth-openai",
			ProviderType: "openai",
			ProviderID:   "openai-main",
			Scope:        "id:openai-main",
			Type:         "oauth_token",
		},
	})

	retryAfter := time.Minute
	s.MarkResult(context.Background(), Result{
		CredentialID:   "oauth-openai",
		Model:          "gpt-4.1",
		CredentialWide: true,
		RetryAfter:     &retryAfter,
		Error: &Error{
			Code:       "refresh_failed",
			Message:    "refresh failed",
			Retryable:  true,
			HTTPStatus: 502,
		},
	})
	// A request selected before the refresh failure may still complete. Its
	// success must not clear the newer credential-wide block.
	s.MarkResult(context.Background(), Result{
		CredentialID: "oauth-openai",
		Model:        "gpt-4o",
		Success:      true,
	})

	for _, model := range []string{"gpt-4.1", "gpt-4o"} {
		picked, err := s.Pick(context.Background(), Filter{
			CredentialScope: "id:openai-main",
			Model:           model,
		}, nil)
		if picked != nil || err == nil {
			t.Fatalf("Pick(%q) = (%#v, %v), want credential unavailable", model, picked, err)
		}
	}
}
