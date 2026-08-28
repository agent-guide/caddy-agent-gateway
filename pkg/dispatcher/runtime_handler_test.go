package dispatcher

import (
	"errors"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/agent"
	agentruntime "github.com/agent-guide/agent-gateway/pkg/agent/runtime"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
)

func TestNormalizeAgentLookupError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want agentruntime.ErrorCode
	}{
		{name: "agent missing", err: agent.ErrAgentNotConfigured, want: agentruntime.ErrorAgentNotFound},
		{name: "store missing", err: configstore.ErrNotFound, want: agentruntime.ErrorAgentNotFound},
		{name: "store failure", err: errors.New("sqlite temporarily unavailable"), want: agentruntime.ErrorBackendUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := agentruntime.ErrorCodeOf(normalizeAgentLookupError(tt.err))
			if !ok || got != tt.want {
				t.Fatalf("error code = %q, %v; want %q", got, ok, tt.want)
			}
		})
	}
}
