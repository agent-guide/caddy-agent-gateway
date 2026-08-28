package runtime_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/agent"
	agentruntime "github.com/agent-guide/agent-gateway/pkg/agent/runtime"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtime/runtimetest"
)

func TestBuiltInRuntimeTypeConstantsRemainRegistryKeys(t *testing.T) {
	t.Parallel()

	got := []string{agent.RuntimeTypeACP, agent.RuntimeTypeBuiltin, agent.RuntimeTypeHTTP}
	want := []string{"acp", "builtin", "http"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime type constants = %v, want registry keys %v", got, want)
	}
}

func TestRegistryRegisterResolveAndList(t *testing.T) {
	t.Parallel()

	registry := agentruntime.NewRegistry()
	acp := runtimetest.NewBackend("acp")
	builtin := runtimetest.NewBackend("builtin")
	if err := registry.RegisterAll(builtin, acp); err != nil {
		t.Fatalf("RegisterAll() error = %v", err)
	}

	got, err := registry.Resolve("acp")
	if err != nil {
		t.Fatalf("Resolve(acp) error = %v", err)
	}
	if got != acp {
		t.Fatal("Resolve(acp) returned a different backend")
	}
	if want := []string{"acp", "builtin"}; !reflect.DeepEqual(registry.RuntimeTypes(), want) {
		t.Fatalf("RuntimeTypes() = %v, want %v", registry.RuntimeTypes(), want)
	}
	if err := registry.ValidateRequired("acp", "builtin"); err != nil {
		t.Fatalf("ValidateRequired() error = %v", err)
	}
}

func TestRegistryDuplicateRegistrationIsAtomic(t *testing.T) {
	t.Parallel()

	registry := agentruntime.NewRegistry()
	acp := runtimetest.NewBackend("acp")
	if err := registry.Register(acp); err != nil {
		t.Fatalf("Register(acp) error = %v", err)
	}
	err := registry.RegisterAll(
		runtimetest.NewBackend("builtin"),
		runtimetest.NewBackend("acp"),
	)
	if err == nil {
		t.Fatal("RegisterAll() = nil, want duplicate registration error")
	}
	if registry.Has("builtin") {
		t.Fatal("RegisterAll() partially registered builtin after duplicate failure")
	}
	if got, resolveErr := registry.Resolve("acp"); resolveErr != nil || got != acp {
		t.Fatalf("existing acp backend changed: backend=%v err=%v", got, resolveErr)
	}
}

func TestRegistryRejectsDuplicateWithinBatch(t *testing.T) {
	t.Parallel()

	registry := agentruntime.NewRegistry()
	err := registry.RegisterAll(
		runtimetest.NewBackend("same"),
		runtimetest.NewBackend("same"),
	)
	if err == nil {
		t.Fatal("RegisterAll() = nil, want duplicate registration error")
	}
	if got := registry.RuntimeTypes(); len(got) != 0 {
		t.Fatalf("RuntimeTypes() = %v, want empty registry", got)
	}
}

func TestRegistryUnknownRuntimeFailsClosed(t *testing.T) {
	t.Parallel()

	registry := agentruntime.NewRegistry()
	for _, runtimeType := range []string{"http", "", " http "} {
		backend, err := registry.Resolve(runtimeType)
		if backend != nil {
			t.Fatalf("Resolve(%q) backend = %v, want nil", runtimeType, backend)
		}
		if !errors.Is(err, agentruntime.ErrRuntimeNotExecutable) {
			t.Fatalf("Resolve(%q) error = %v, want runtime_not_executable", runtimeType, err)
		}
		if code, ok := agentruntime.ErrorCodeOf(err); !ok || code != agentruntime.ErrorRuntimeNotExecutable {
			t.Fatalf("Resolve(%q) code = %q, %v", runtimeType, code, ok)
		}
	}
}

func TestRegistryRejectsInvalidBackends(t *testing.T) {
	t.Parallel()

	var typedNil *runtimetest.Backend
	tests := []struct {
		name    string
		backend agentruntime.Backend
	}{
		{name: "nil"},
		{name: "typed nil", backend: typedNil},
		{name: "empty type", backend: runtimetest.NewBackend("")},
		{name: "whitespace", backend: runtimetest.NewBackend(" acp")},
		{name: "slash", backend: runtimetest.NewBackend("native/acp")},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			registry := agentruntime.NewRegistry()
			if err := registry.Register(tt.backend); err == nil {
				t.Fatal("Register() = nil, want validation error")
			}
			if len(registry.RuntimeTypes()) != 0 {
				t.Fatal("invalid backend modified registry")
			}
		})
	}
}
