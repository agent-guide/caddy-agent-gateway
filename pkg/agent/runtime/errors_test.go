package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	agentruntime "github.com/agent-guide/agent-gateway/pkg/agent/runtime"
)

func TestNormalizedErrorSupportsErrorsIsAndCodeExtraction(t *testing.T) {
	t.Parallel()

	native := errors.New("native detail")
	err := agentruntime.WrapError(agentruntime.ErrorBackendUnavailable, "backend unavailable", native)
	wrapped := fmt.Errorf("serve turn: %w", err)

	if !errors.Is(wrapped, agentruntime.ErrBackendUnavailable) {
		t.Fatalf("errors.Is(%v, ErrBackendUnavailable) = false", wrapped)
	}
	if !errors.Is(wrapped, native) {
		t.Fatalf("errors.Is(%v, native) = false", wrapped)
	}
	if code, ok := agentruntime.ErrorCodeOf(wrapped); !ok || code != agentruntime.ErrorBackendUnavailable {
		t.Fatalf("ErrorCodeOf() = %q, %v", code, ok)
	}
	if got := err.Error(); got != "backend unavailable" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestNormalizeErrorClosesOverNativeFailures(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		err  error
		code agentruntime.ErrorCode
	}{
		{context.Canceled, agentruntime.ErrorTurnCancelled},
		{context.DeadlineExceeded, agentruntime.ErrorBackendTimeout},
		{errors.New("exec /private/tool --token secret"), agentruntime.ErrorTurnFailed},
	} {
		normalized := agentruntime.NormalizeError(tt.err)
		if code, ok := agentruntime.ErrorCodeOf(normalized); !ok || code != tt.code {
			t.Errorf("NormalizeError(%v) = %v (%q), want %q", tt.err, normalized, code, tt.code)
		}
		if !errors.Is(normalized, tt.err) {
			t.Errorf("NormalizeError(%v) did not retain cause", tt.err)
		}
	}
}

func TestHTTPStatusAndPublicErrorRedaction(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		code   agentruntime.ErrorCode
		status int
	}{
		{agentruntime.ErrorAgentNotFound, 404},
		{agentruntime.ErrorInvalidRequest, 400},
		{agentruntime.ErrorRuntimeNotExecutable, 501},
		{agentruntime.ErrorCapabilityNotSupported, 501},
		{agentruntime.ErrorSessionBusy, 429},
		{agentruntime.ErrorSessionLimitExceeded, 429},
		{agentruntime.ErrorPermissionExpired, 410},
		{agentruntime.ErrorTurnLimitExceeded, 429},
		{agentruntime.ErrorBackendUnavailable, 503},
		{agentruntime.ErrorBackendTimeout, 504},
		{agentruntime.ErrorTurnFailed, 502},
		{agentruntime.ErrorTurnCancelled, 499},
	} {
		err := agentruntime.WrapError(tt.code, "token=secret /private/workspace", errors.New("command --password hunter2"))
		if got := agentruntime.HTTPStatus(err); got != tt.status {
			t.Errorf("HTTPStatus(%q) = %d, want %d", tt.code, got, tt.status)
		}
		public := agentruntime.PublicError(err)
		if public.ErrorType != tt.code {
			t.Errorf("PublicError(%q).ErrorType = %q", tt.code, public.ErrorType)
		}
		if strings.Contains(public.Message, "secret") || strings.Contains(public.Message, "/private") || strings.Contains(public.Message, "hunter2") {
			t.Errorf("PublicError(%q) leaked sensitive detail: %+v", tt.code, public)
		}
	}
}
