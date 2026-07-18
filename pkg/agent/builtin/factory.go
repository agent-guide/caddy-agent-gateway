package builtin

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"

	"github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/mcp/einotool"
)

// FactoryDeps hands a custom factory the same gateway-governed building
// blocks the declarative materializer uses.
type FactoryDeps struct {
	Models ChatModelResolver
	Tools  einotool.ToolCaller
}

// Factory builds a custom ADK agent from a builtin definition. It is the
// escape hatch for agents that need custom Go logic
// (docs/design/agents-control-plane.md §5.7.3): implement the factory,
// register it, and blank-import the package in cmd/agw/main.go,
// cmd/agwd/main.go, and cmd/agwctl/cmd_gateway.go.
type Factory func(ctx context.Context, deps FactoryDeps, def *agent.BuiltinRuntime) (adk.Agent, error)

var (
	factoryMu sync.RWMutex
	factories = map[string]Factory{}
)

// RegisterFactory registers a compiled-in custom agent factory, mirroring
// provider.RegisterProviderFactory. It also records the name in pkg/agent's
// name registry so definition validation can reject unknown factories.
func RegisterFactory(name string, f Factory) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("builtin: factory name is required")
	}
	if f == nil {
		return fmt.Errorf("builtin: factory %q is nil", name)
	}
	factoryMu.Lock()
	defer factoryMu.Unlock()
	if _, exists := factories[name]; exists {
		return fmt.Errorf("builtin: factory %q is already registered", name)
	}
	factories[name] = f
	agent.RegisterBuiltinFactoryName(name)
	return nil
}

func lookupFactory(name string) (Factory, bool) {
	factoryMu.RLock()
	defer factoryMu.RUnlock()
	f, ok := factories[name]
	return f, ok
}
