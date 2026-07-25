package runtimeapi

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// Registry owns the runtime-type to backend mapping for one AgentGateway.
// Registrations are expected during bootstrap; lookups are safe concurrently.
type Registry struct {
	mu       sync.RWMutex
	backends map[string]Backend
}

func NewRegistry() *Registry {
	return &Registry{backends: make(map[string]Backend)}
}

func (r *Registry) Register(backend Backend) error {
	return r.RegisterAll(backend)
}

// RegisterAll validates the whole batch before modifying the registry.
func (r *Registry) RegisterAll(backends ...Backend) error {
	if r == nil {
		return fmt.Errorf("runtime backend registry is not configured")
	}
	pending := make(map[string]Backend, len(backends))
	for i, backend := range backends {
		runtimeType, err := validateBackend(backend)
		if err != nil {
			return fmt.Errorf("runtime backend %d: %w", i, err)
		}
		if _, exists := pending[runtimeType]; exists {
			return fmt.Errorf("runtime backend %q is registered more than once", runtimeType)
		}
		pending[runtimeType] = backend
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.backends == nil {
		r.backends = make(map[string]Backend)
	}
	for runtimeType := range pending {
		if _, exists := r.backends[runtimeType]; exists {
			return fmt.Errorf("runtime backend %q is already registered", runtimeType)
		}
	}
	for runtimeType, backend := range pending {
		r.backends[runtimeType] = backend
	}
	return nil
}

// Resolve returns the backend registered for runtimeType. Unknown or malformed
// runtime types fail closed with runtime_not_executable.
func (r *Registry) Resolve(runtimeType string) (Backend, error) {
	runtimeType = strings.TrimSpace(runtimeType)
	if r == nil || runtimeType == "" {
		return nil, runtimeNotExecutable(runtimeType)
	}
	r.mu.RLock()
	backend := r.backends[runtimeType]
	r.mu.RUnlock()
	if backend == nil {
		return nil, runtimeNotExecutable(runtimeType)
	}
	return backend, nil
}

func (r *Registry) Has(runtimeType string) bool {
	_, err := r.Resolve(runtimeType)
	return err == nil
}

func (r *Registry) RuntimeTypes() []string {
	if r == nil {
		return []string{}
	}
	r.mu.RLock()
	out := make([]string, 0, len(r.backends))
	for runtimeType := range r.backends {
		out = append(out, runtimeType)
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// ValidateRequired verifies that every runtime type expected by a caller has a
// registered backend without requiring all manageable Agent types to be
// executable.
func (r *Registry) ValidateRequired(runtimeTypes ...string) error {
	for _, runtimeType := range runtimeTypes {
		if _, err := r.Resolve(runtimeType); err != nil {
			return err
		}
	}
	return nil
}

func validateBackend(backend Backend) (string, error) {
	if backend == nil || isNilBackend(backend) {
		return "", fmt.Errorf("backend is nil")
	}
	runtimeType := backend.RuntimeType()
	if runtimeType == "" {
		return "", fmt.Errorf("runtime type is required")
	}
	if strings.TrimSpace(runtimeType) != runtimeType {
		return "", fmt.Errorf("runtime type %q must not have surrounding whitespace", runtimeType)
	}
	if strings.ContainsAny(runtimeType, "/\\\x00") {
		return "", fmt.Errorf("runtime type %q contains an invalid character", runtimeType)
	}
	return runtimeType, nil
}

func isNilBackend(backend Backend) bool {
	value := reflect.ValueOf(backend)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
