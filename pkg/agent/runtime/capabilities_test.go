package runtime_test

import (
	"context"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/agent"
	agentruntime "github.com/agent-guide/agent-gateway/pkg/agent/runtime"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtime/runtimetest"
)

type sessionBackend struct {
	*runtimetest.Backend
}

func (sessionBackend) ListSessions(context.Context, agent.Agent, agentruntime.ListSessionsRequest) (agentruntime.ListSessionsResponse, error) {
	return agentruntime.ListSessionsResponse{}, nil
}

type fullBackend struct {
	sessionBackend
}

func (fullBackend) LoadTranscript(context.Context, agent.Agent, agentruntime.TranscriptRequest) (agentruntime.TranscriptResponse, error) {
	return agentruntime.TranscriptResponse{}, nil
}

func (fullBackend) ResolvePermission(context.Context, agent.Agent, agentruntime.PermissionDecision) error {
	return nil
}

func (fullBackend) CancelRun(context.Context, agent.Agent, agentruntime.CancelRequest) (agentruntime.CancelResult, error) {
	return agentruntime.CancelResult{}, nil
}

func (fullBackend) RuntimeSummary(context.Context, agent.Agent) (agentruntime.RuntimeSummary, error) {
	return agentruntime.RuntimeSummary{}, nil
}

func (fullBackend) Health(context.Context, agent.Agent) (agentruntime.Health, error) {
	return agentruntime.Health{}, nil
}

func TestDetectOptionalCapabilities(t *testing.T) {
	t.Parallel()

	base := runtimetest.NewBackend("base")
	got := agentruntime.DetectOptionalCapabilities(base)
	if got != (agentruntime.OptionalCapabilities{}) {
		t.Fatalf("base optional capabilities = %+v, want none", got)
	}

	sessionOnly := sessionBackend{Backend: runtimetest.NewBackend("sessions")}
	got = agentruntime.DetectOptionalCapabilities(sessionOnly)
	if !got.SessionList || got.Transcript || got.PermissionResolve || got.RunCancel || got.RuntimeInspect || got.HealthCheck {
		t.Fatalf("session optional capabilities = %+v", got)
	}

	full := fullBackend{sessionBackend{Backend: runtimetest.NewBackend("full")}}
	got = agentruntime.DetectOptionalCapabilities(full)
	want := agentruntime.OptionalCapabilities{
		SessionList:       true,
		Transcript:        true,
		PermissionResolve: true,
		RunCancel:         true,
		RuntimeInspect:    true,
		HealthCheck:       true,
	}
	if got != want {
		t.Fatalf("full optional capabilities = %+v, want %+v", got, want)
	}

	if got := agentruntime.DetectOptionalCapabilities(nil); got != (agentruntime.OptionalCapabilities{}) {
		t.Fatalf("nil optional capabilities = %+v, want none", got)
	}
}
