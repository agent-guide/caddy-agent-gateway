package runtimeapi_test

import (
	"errors"
	"fmt"
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
