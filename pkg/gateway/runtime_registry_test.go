package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi/runtimeapitest"
)

func TestAgentGatewayOwnsRuntimeRegistry(t *testing.T) {
	t.Parallel()

	gateway := NewAgentGateway()
	if gateway.RuntimeRegistry() == nil {
		t.Fatal("RuntimeRegistry() = nil")
	}

	backend := runtimeapitest.NewBackend("fake")
	if err := gateway.Bootstrap(context.Background(), BootstrapOptions{
		RuntimeBackends: []runtimeapi.Backend{backend},
	}); err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	got, err := gateway.RuntimeRegistry().Resolve("fake")
	if err != nil {
		t.Fatalf("Resolve(fake) error = %v", err)
	}
	if got != backend {
		t.Fatal("Resolve(fake) returned a different backend")
	}
}

func TestAgentGatewayBootstrapRejectsDuplicateRuntimeBackends(t *testing.T) {
	t.Parallel()

	gateway := NewAgentGateway()
	err := gateway.Bootstrap(context.Background(), BootstrapOptions{
		RuntimeBackends: []runtimeapi.Backend{
			runtimeapitest.NewBackend("duplicate"),
			runtimeapitest.NewBackend("duplicate"),
		},
	})
	if err == nil {
		t.Fatal("Bootstrap() = nil, want duplicate runtime backend error")
	}
	if gateway.RuntimeRegistry().Has("duplicate") {
		t.Fatal("duplicate backend batch partially modified gateway registry")
	}
}

func TestAgentGatewayResetReplacesRuntimeRegistry(t *testing.T) {
	t.Parallel()

	gateway := NewAgentGateway()
	backend := runtimeapitest.NewBackend("fake")
	if err := gateway.RuntimeRegistry().Register(backend); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	old := gateway.RuntimeRegistry()
	gateway.Reset()
	current := gateway.RuntimeRegistry()
	if current == nil || current == old {
		t.Fatal("Reset() did not install a fresh runtime registry")
	}
	if _, err := current.Resolve("fake"); !errors.Is(err, runtimeapi.ErrRuntimeNotExecutable) {
		t.Fatalf("Resolve(fake) after Reset error = %v", err)
	}
}
