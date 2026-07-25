package runtimeapi_test

import (
	"context"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi/runtimeapitest"
)

type sessionBackend struct {
	*runtimeapitest.Backend
}

func (sessionBackend) ListSessions(context.Context, agent.Agent, runtimeapi.ListSessionsRequest) (runtimeapi.ListSessionsResponse, error) {
	return runtimeapi.ListSessionsResponse{}, nil
}

type fullBackend struct {
	sessionBackend
}

func (fullBackend) LoadTranscript(context.Context, agent.Agent, runtimeapi.TranscriptRequest) (runtimeapi.TranscriptResponse, error) {
	return runtimeapi.TranscriptResponse{}, nil
}

func (fullBackend) ResolvePermission(context.Context, agent.Agent, runtimeapi.PermissionDecision) error {
	return nil
}

func (fullBackend) CancelRun(context.Context, agent.Agent, runtimeapi.CancelRequest) (runtimeapi.CancelResult, error) {
	return runtimeapi.CancelResult{}, nil
}

func (fullBackend) RuntimeSummary(context.Context, agent.Agent) (runtimeapi.RuntimeSummary, error) {
	return runtimeapi.RuntimeSummary{}, nil
}

func (fullBackend) Health(context.Context, agent.Agent) (runtimeapi.Health, error) {
	return runtimeapi.Health{}, nil
}

func TestDetectOptionalCapabilities(t *testing.T) {
	t.Parallel()

	base := runtimeapitest.NewBackend("base")
	got := runtimeapi.DetectOptionalCapabilities(base)
	if got != (runtimeapi.OptionalCapabilities{}) {
		t.Fatalf("base optional capabilities = %+v, want none", got)
	}

	sessionOnly := sessionBackend{Backend: runtimeapitest.NewBackend("sessions")}
	got = runtimeapi.DetectOptionalCapabilities(sessionOnly)
	if !got.SessionList || got.Transcript || got.PermissionResolve || got.RunCancel || got.RuntimeInspect || got.HealthCheck {
		t.Fatalf("session optional capabilities = %+v", got)
	}

	full := fullBackend{sessionBackend{Backend: runtimeapitest.NewBackend("full")}}
	got = runtimeapi.DetectOptionalCapabilities(full)
	want := runtimeapi.OptionalCapabilities{
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

	if got := runtimeapi.DetectOptionalCapabilities(nil); got != (runtimeapi.OptionalCapabilities{}) {
		t.Fatalf("nil optional capabilities = %+v, want none", got)
	}
}
