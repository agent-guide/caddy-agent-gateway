package runtimeapi_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi/runtimeapitest"
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

	registry := runtimeapi.NewRegistry()
	acp := runtimeapitest.NewBackend("acp")
	builtin := runtimeapitest.NewBackend("builtin")
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

	registry := runtimeapi.NewRegistry()
	acp := runtimeapitest.NewBackend("acp")
	if err := registry.Register(acp); err != nil {
		t.Fatalf("Register(acp) error = %v", err)
	}
	err := registry.RegisterAll(
		runtimeapitest.NewBackend("builtin"),
		runtimeapitest.NewBackend("acp"),
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

	registry := runtimeapi.NewRegistry()
	err := registry.RegisterAll(
		runtimeapitest.NewBackend("same"),
		runtimeapitest.NewBackend("same"),
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

	registry := runtimeapi.NewRegistry()
	for _, runtimeType := range []string{"http", "", " http "} {
		backend, err := registry.Resolve(runtimeType)
		if backend != nil {
			t.Fatalf("Resolve(%q) backend = %v, want nil", runtimeType, backend)
		}
		if !errors.Is(err, runtimeapi.ErrRuntimeNotExecutable) {
			t.Fatalf("Resolve(%q) error = %v, want runtime_not_executable", runtimeType, err)
		}
		if code, ok := runtimeapi.ErrorCodeOf(err); !ok || code != runtimeapi.ErrorRuntimeNotExecutable {
			t.Fatalf("Resolve(%q) code = %q, %v", runtimeType, code, ok)
		}
	}
}

func TestRegistryRejectsInvalidBackends(t *testing.T) {
	t.Parallel()

	var typedNil *runtimeapitest.Backend
	tests := []struct {
		name    string
		backend runtimeapi.Backend
	}{
		{name: "nil"},
		{name: "typed nil", backend: typedNil},
		{name: "empty type", backend: runtimeapitest.NewBackend("")},
		{name: "whitespace", backend: runtimeapitest.NewBackend(" acp")},
		{name: "slash", backend: runtimeapitest.NewBackend("native/acp")},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			registry := runtimeapi.NewRegistry()
			if err := registry.Register(tt.backend); err == nil {
				t.Fatal("Register() = nil, want validation error")
			}
			if len(registry.RuntimeTypes()) != 0 {
				t.Fatal("invalid backend modified registry")
			}
		})
	}
}
