package runtimeapi_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
)

func TestNormalizedErrorSupportsErrorsIsAndCodeExtraction(t *testing.T) {
	t.Parallel()

	native := errors.New("native detail")
	err := runtimeapi.WrapError(runtimeapi.ErrorBackendUnavailable, "backend unavailable", native)
	wrapped := fmt.Errorf("serve turn: %w", err)

	if !errors.Is(wrapped, runtimeapi.ErrBackendUnavailable) {
		t.Fatalf("errors.Is(%v, ErrBackendUnavailable) = false", wrapped)
	}
	if !errors.Is(wrapped, native) {
		t.Fatalf("errors.Is(%v, native) = false", wrapped)
	}
	if code, ok := runtimeapi.ErrorCodeOf(wrapped); !ok || code != runtimeapi.ErrorBackendUnavailable {
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
		code runtimeapi.ErrorCode
	}{
		{context.Canceled, runtimeapi.ErrorTurnCancelled},
		{context.DeadlineExceeded, runtimeapi.ErrorBackendTimeout},
		{errors.New("exec /private/tool --token secret"), runtimeapi.ErrorTurnFailed},
	} {
		normalized := runtimeapi.NormalizeError(tt.err)
		if code, ok := runtimeapi.ErrorCodeOf(normalized); !ok || code != tt.code {
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
		code   runtimeapi.ErrorCode
		status int
	}{
		{runtimeapi.ErrorAgentNotFound, 404},
		{runtimeapi.ErrorInvalidRequest, 400},
		{runtimeapi.ErrorRuntimeNotExecutable, 501},
		{runtimeapi.ErrorCapabilityNotSupported, 501},
		{runtimeapi.ErrorSessionBusy, 429},
		{runtimeapi.ErrorSessionLimitExceeded, 429},
		{runtimeapi.ErrorPermissionExpired, 410},
		{runtimeapi.ErrorTurnLimitExceeded, 429},
		{runtimeapi.ErrorBackendUnavailable, 503},
		{runtimeapi.ErrorBackendTimeout, 504},
		{runtimeapi.ErrorTurnFailed, 502},
		{runtimeapi.ErrorTurnCancelled, 499},
	} {
		err := runtimeapi.WrapError(tt.code, "token=secret /private/workspace", errors.New("command --password hunter2"))
		if got := runtimeapi.HTTPStatus(err); got != tt.status {
			t.Errorf("HTTPStatus(%q) = %d, want %d", tt.code, got, tt.status)
		}
		public := runtimeapi.PublicError(err)
		if public.ErrorType != tt.code {
			t.Errorf("PublicError(%q).ErrorType = %q", tt.code, public.ErrorType)
		}
		if strings.Contains(public.Message, "secret") || strings.Contains(public.Message, "/private") || strings.Contains(public.Message, "hunter2") {
			t.Errorf("PublicError(%q) leaked sensitive detail: %+v", tt.code, public)
		}
	}
}
