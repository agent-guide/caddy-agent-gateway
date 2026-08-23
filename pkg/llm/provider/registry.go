package provider

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ProviderFactory creates a Provider instance from config.
type ProviderFactory func(config ProviderConfig) (Provider, error)

// ErrProviderTypeDisabled is returned when a registered provider type is disabled.
var ErrProviderTypeDisabled = errors.New("provider type is disabled")

type ProviderTypeSetting struct {
	ProviderType string `json:"provider_type"`
	Enabled      bool   `json:"enabled"`
}

// ProtocolDialect identifies wire-level state that is only portable between
// providers capable of preserving the same upstream protocol dialect.
type ProtocolDialect string

const ProtocolDialectAnthropic ProtocolDialect = "anthropic"

// ProviderTypeCapabilities describes protocol-level fidelity that is stable
// for every instance of one registered provider type.
type ProviderTypeCapabilities struct {
	NativeDialects    map[ProtocolDialect]struct{}
	ReasoningDialects map[ProtocolDialect]struct{}
}

// NewProtocolDialectSet builds a provider capability or request requirement
// set without exposing dialect-specific fields in routing code.
func NewProtocolDialectSet(dialects ...ProtocolDialect) map[ProtocolDialect]struct{} {
	set := make(map[ProtocolDialect]struct{}, len(dialects))
	for _, dialect := range dialects {
		if dialect != "" {
			set[dialect] = struct{}{}
		}
	}
	return set
}

func (c ProviderTypeCapabilities) SupportsNativeDialect(dialect ProtocolDialect) bool {
	_, ok := c.NativeDialects[dialect]
	return ok
}

func (c ProviderTypeCapabilities) SupportsReasoningDialect(dialect ProtocolDialect) bool {
	_, ok := c.ReasoningDialects[dialect]
	return ok
}

func cloneProtocolDialects(src map[ProtocolDialect]struct{}) map[ProtocolDialect]struct{} {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[ProtocolDialect]struct{}, len(src))
	for dialect := range src {
		dst[dialect] = struct{}{}
	}
	return dst
}

var (
	mu                    sync.RWMutex
	factories             = map[string]ProviderFactory{}
	typeCapabilities      = map[string]ProviderTypeCapabilities{}
	disabledProviderTypes = map[string]struct{}{}
)

// RegisterProviderFactory registers a provider factory by name.
func RegisterProviderFactory(name string, factory ProviderFactory) {
	name = normalizeProviderType(name)
	if name == "" || factory == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	factories[name] = factory
}

// RegisterProviderTypeCapabilities declares protocol-level capabilities used
// before a provider instance is selected or a credential is charged.
func RegisterProviderTypeCapabilities(name string, capabilities ProviderTypeCapabilities) {
	name = normalizeProviderType(name)
	if name == "" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	capabilities.NativeDialects = cloneProtocolDialects(capabilities.NativeDialects)
	capabilities.ReasoningDialects = cloneProtocolDialects(capabilities.ReasoningDialects)
	typeCapabilities[name] = capabilities
}

// CapabilitiesForProviderType returns the registered protocol-level
// capabilities for name. Unknown and undeclared provider types fail closed.
func CapabilitiesForProviderType(name string) ProviderTypeCapabilities {
	name = normalizeProviderType(name)
	mu.RLock()
	defer mu.RUnlock()
	capabilities := typeCapabilities[name]
	capabilities.NativeDialects = cloneProtocolDialects(capabilities.NativeDialects)
	capabilities.ReasoningDialects = cloneProtocolDialects(capabilities.ReasoningDialects)
	return capabilities
}

// NewProvider creates a provider by name using registered factories.
func NewProvider(config ProviderConfig) (Provider, error) {
	name := normalizeProviderType(config.ProviderType)
	mu.RLock()
	factory, ok := factories[name]
	_, disabled := disabledProviderTypes[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", config.ProviderType)
	}
	if disabled {
		return nil, fmt.Errorf("%w: %s", ErrProviderTypeDisabled, config.ProviderType)
	}
	return factory(config)
}

// ListProviderTypes returns the names of all registered providers.
func ListProviderTypes() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(factories))
	for name := range factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsProviderTypeEnabled reports whether a registered provider type is enabled.
func IsProviderTypeEnabled(name string) (bool, bool) {
	name = normalizeProviderType(name)
	mu.RLock()
	defer mu.RUnlock()
	if _, ok := factories[name]; !ok {
		return false, false
	}
	_, disabled := disabledProviderTypes[name]
	return !disabled, true
}

// EnableAllProviderTypes clears the disabled set so every registered provider
// type is enabled. It is used when no startup provider_types policy is set.
func EnableAllProviderTypes() {
	mu.Lock()
	defer mu.Unlock()
	disabledProviderTypes = map[string]struct{}{}
}

// ConfigureProviderTypes applies startup-only provider type availability.
// If exclusive is true, every registered provider type not listed is disabled.
func ConfigureProviderTypes(settings []ProviderTypeSetting, exclusive bool) error {
	mu.Lock()
	defer mu.Unlock()

	nextDisabled := map[string]struct{}{}
	if !exclusive {
		for name := range disabledProviderTypes {
			nextDisabled[name] = struct{}{}
		}
	} else {
		for name := range factories {
			nextDisabled[name] = struct{}{}
		}
	}
	for _, setting := range settings {
		name := normalizeProviderType(setting.ProviderType)
		if name == "" {
			return fmt.Errorf("provider_type is required")
		}
		if _, ok := factories[name]; !ok {
			return fmt.Errorf("unknown provider: %s", name)
		}
		if setting.Enabled {
			delete(nextDisabled, name)
		} else {
			nextDisabled[name] = struct{}{}
		}
	}
	disabledProviderTypes = nextDisabled
	return nil
}

func normalizeProviderType(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
