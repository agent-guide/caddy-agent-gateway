package dispatcher

import (
	"errors"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
)

func TestNormalizeAgentLookupError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want runtimeapi.ErrorCode
	}{
		{name: "agent missing", err: agent.ErrAgentNotConfigured, want: runtimeapi.ErrorAgentNotFound},
		{name: "store missing", err: configstore.ErrNotFound, want: runtimeapi.ErrorAgentNotFound},
		{name: "store failure", err: errors.New("sqlite temporarily unavailable"), want: runtimeapi.ErrorBackendUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := runtimeapi.ErrorCodeOf(normalizeAgentLookupError(tt.err))
			if !ok || got != tt.want {
				t.Fatalf("error code = %q, %v; want %q", got, ok, tt.want)
			}
		})
	}
}
