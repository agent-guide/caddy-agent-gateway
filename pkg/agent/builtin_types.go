package agent

import (
	"fmt"
	"strings"
	"sync"
)

// Builtin topology kinds. They enumerate what eino ADK exposes as
// parameterizable structure (docs/design/agents-control-plane.md §5.7.2).
const (
	TopologyKindSingle      = "single"
	TopologyKindSequential  = "sequential"
	TopologyKindParallel    = "parallel"
	TopologyKindLoop        = "loop"
	TopologyKindSupervisor  = "supervisor"
	TopologyKindPlanExecute = "planexecute"
	TopologyKindDeep        = "deep"
	// TopologyKindCustom selects a compiled-in agent factory by name
	// (§5.7.3); the factory must be registered in the linked binary.
	TopologyKindCustom = "custom"
)

// BuiltinRuntime is the persisted definition of an ADK-hosted agent: the
// gateway ships one generic ADK host, and "starting" the agent means the host
// materializes the ADK object graph from this definition (§5.7.1).
type BuiltinRuntime struct {
	Model        BuiltinModel           `json:"model"`
	SystemPrompt string                 `json:"system_prompt,omitempty"`
	Generation   *BuiltinGeneration     `json:"generation,omitempty"`
	Tools        []BuiltinToolSelection `json:"tools,omitempty"`
	Topology     BuiltinTopology        `json:"topology"`
	Middlewares  *BuiltinMiddlewares    `json:"middlewares,omitempty"`
	Limits       *BuiltinLimits         `json:"limits,omitempty"`
}

// BuiltinModel resolves through a gateway LLM route, never a raw provider, so
// credential scheduling, candidate fallback, and LLM usage events apply
// unchanged. Model is the route target name (logical model in model-target
// routes, upstream model in direct-provider routes); empty falls back to the
// route's default resolution.
type BuiltinModel struct {
	LLMRouteID string `json:"llm_route_id"`
	Model      string `json:"model,omitempty"`
}

type BuiltinGeneration struct {
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature *float32 `json:"temperature,omitempty"`
	TopP        *float32 `json:"top_p,omitempty"`
}

// BuiltinToolSelection references a gateway-managed MCP service. Tools lists
// the allowed tool names; empty means every tool the service exposes.
// Selection by name is fail-closed at materialization time.
type BuiltinToolSelection struct {
	MCPServiceID string   `json:"mcp_service_id"`
	Tools        []string `json:"tools,omitempty"`
}

// BuiltinTopology selects the ADK structure. Sub-agents are inline child
// definitions only — never references to other Agent objects (§5.7.2).
type BuiltinTopology struct {
	Kind string `json:"kind"`
	// Factory names a compiled-in custom agent factory; required and only
	// meaningful when Kind is "custom".
	Factory string `json:"factory,omitempty"`
	// MaxIterations bounds the topology's iteration loop: loop rounds for
	// kind loop, execute-replan rounds for kind planexecute, and reasoning
	// iterations for kind deep. 0 uses the ADK default.
	MaxIterations int               `json:"max_iterations,omitempty"`
	SubAgents     []BuiltinSubAgent `json:"sub_agents,omitempty"`
	// PlanExecute overrides the planexecute role nodes; only meaningful when
	// Kind is "planexecute". Every role inherits the enclosing node's model
	// when unset, and the executor inherits the enclosing node's tools when
	// it declares none, so the whole block is optional.
	PlanExecute *BuiltinPlanExecute `json:"plan_execute,omitempty"`
}

// BuiltinPlanExecute configures the three planexecute roles. The planner and
// replanner emit structured plans through tool calling, so their models must
// be tool-capable; the executor is the only role that runs MCP tools.
type BuiltinPlanExecute struct {
	Planner   *BuiltinPlanExecuteRole `json:"planner,omitempty"`
	Executor  *BuiltinPlanExecuteRole `json:"executor,omitempty"`
	Replanner *BuiltinPlanExecuteRole `json:"replanner,omitempty"`
}

// BuiltinPlanExecuteRole overrides one planexecute role. Tools and
// MaxIterations are executor-only: the planner and replanner interact with
// the model through the prebuilt's fixed plan/respond tool schemas and
// cannot carry MCP tools.
type BuiltinPlanExecuteRole struct {
	Model      *BuiltinModel      `json:"model,omitempty"`
	Generation *BuiltinGeneration `json:"generation,omitempty"`
	// Tools replace the enclosing node's tool selection for the executor.
	Tools []BuiltinToolSelection `json:"tools,omitempty"`
	// MaxIterations bounds the executor's inner tool-call loop; 0 uses the
	// ADK default.
	MaxIterations int `json:"max_iterations,omitempty"`
}

// BuiltinSubAgent is a nested definition object (same schema, minus limits).
// It exists only as an internal node of the enclosing agent: no first-class
// identity, no separate usage attribution, no admin surface. Model is
// optional and inherits the parent definition's model when nil.
type BuiltinSubAgent struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description,omitempty"`
	Model        *BuiltinModel          `json:"model,omitempty"`
	SystemPrompt string                 `json:"system_prompt,omitempty"`
	Generation   *BuiltinGeneration     `json:"generation,omitempty"`
	Tools        []BuiltinToolSelection `json:"tools,omitempty"`
	// Topology of the sub-agent itself; nil means a single chat-model agent.
	Topology *BuiltinTopology `json:"topology,omitempty"`
}

// BuiltinMiddlewares toggles the ADK middlewares that are safe as pure
// configuration; each is off by default.
type BuiltinMiddlewares struct {
	Summarization *BuiltinSummarization `json:"summarization,omitempty"`
}

// BuiltinSummarization enables the ADK context-compaction middleware using
// the agent's own chat model for summary generation.
type BuiltinSummarization struct {
	Enabled bool `json:"enabled"`
	// TriggerTokens overrides the token threshold that activates
	// summarization; 0 uses the ADK default.
	TriggerTokens int `json:"trigger_tokens,omitempty"`
}

// BuiltinLimits bound a builtin agent's execution; both are fail-closed
// (reject, not queue). Zero values take the host defaults.
type BuiltinLimits struct {
	MaxConcurrentTurns int `json:"max_concurrent_turns,omitempty"`
	TurnTimeoutSeconds int `json:"turn_timeout_seconds,omitempty"`
}

func (b *BuiltinRuntime) normalize() {
	if b == nil {
		return
	}
	b.Model.normalize()
	b.SystemPrompt = strings.TrimSpace(b.SystemPrompt)
	normalizeToolSelections(b.Tools)
	b.Topology.normalize()
}

func (m *BuiltinModel) normalize() {
	if m == nil {
		return
	}
	m.LLMRouteID = strings.TrimSpace(m.LLMRouteID)
	m.Model = strings.TrimSpace(m.Model)
}

func normalizeToolSelections(tools []BuiltinToolSelection) {
	for i := range tools {
		tools[i].MCPServiceID = strings.TrimSpace(tools[i].MCPServiceID)
		tools[i].Tools = normalizeIDs(tools[i].Tools)
	}
}

func (t *BuiltinTopology) normalize() {
	if t == nil {
		return
	}
	t.Kind = strings.TrimSpace(t.Kind)
	if t.Kind == "" {
		t.Kind = TopologyKindSingle
	}
	t.Factory = strings.TrimSpace(t.Factory)
	if t.PlanExecute != nil {
		t.PlanExecute.Planner.normalize()
		t.PlanExecute.Executor.normalize()
		t.PlanExecute.Replanner.normalize()
	}
	for i := range t.SubAgents {
		sub := &t.SubAgents[i]
		sub.Name = strings.TrimSpace(sub.Name)
		sub.Description = strings.TrimSpace(sub.Description)
		sub.SystemPrompt = strings.TrimSpace(sub.SystemPrompt)
		sub.Model.normalize()
		normalizeToolSelections(sub.Tools)
		sub.Topology.normalize()
	}
}

func (r *BuiltinPlanExecuteRole) normalize() {
	if r == nil {
		return
	}
	r.Model.normalize()
	normalizeToolSelections(r.Tools)
}

// validateBuiltin checks the builtin definition against the agent's declared
// route and resource bindings (§5.7.2 schema rules).
func (a Agent) validateBuiltin() error {
	b := a.Runtime.Builtin
	if b == nil {
		return fmt.Errorf("runtime.builtin is required for builtin runtime")
	}
	llmRoutes := idSet(a.Routes.LLMRouteIDs)
	mcpServices := idSet(a.Resources.MCPServiceIDs)
	if err := validateBuiltinModel(&b.Model, true, llmRoutes); err != nil {
		return err
	}
	if err := validateBuiltinTools(b.Tools, mcpServices); err != nil {
		return err
	}
	if b.Generation != nil && b.Generation.MaxTokens < 0 {
		return fmt.Errorf("runtime.builtin.generation.max_tokens must be non-negative")
	}
	if err := validateBuiltinTopology(&b.Topology, llmRoutes, mcpServices, 1); err != nil {
		return err
	}
	if b.Limits != nil {
		if b.Limits.MaxConcurrentTurns < 0 || b.Limits.TurnTimeoutSeconds < 0 {
			return fmt.Errorf("runtime.builtin.limits values must be non-negative")
		}
	}
	if b.Middlewares != nil && b.Middlewares.Summarization != nil && b.Middlewares.Summarization.TriggerTokens < 0 {
		return fmt.Errorf("runtime.builtin.middlewares.summarization.trigger_tokens must be non-negative")
	}
	return nil
}

func validateBuiltinModel(m *BuiltinModel, required bool, llmRoutes map[string]struct{}) error {
	if m == nil {
		if required {
			return fmt.Errorf("runtime.builtin.model.llm_route_id is required")
		}
		return nil
	}
	if m.LLMRouteID == "" {
		if required {
			return fmt.Errorf("runtime.builtin.model.llm_route_id is required")
		}
		return nil
	}
	if _, ok := llmRoutes[m.LLMRouteID]; !ok {
		return fmt.Errorf("runtime.builtin model route %q must appear in routes.llm_route_ids", m.LLMRouteID)
	}
	return nil
}

func validateBuiltinTools(tools []BuiltinToolSelection, mcpServices map[string]struct{}) error {
	for _, sel := range tools {
		if sel.MCPServiceID == "" {
			return fmt.Errorf("runtime.builtin tools entries require mcp_service_id")
		}
		if _, ok := mcpServices[sel.MCPServiceID]; !ok {
			return fmt.Errorf("runtime.builtin tool service %q must appear in resources.mcp_service_ids", sel.MCPServiceID)
		}
	}
	return nil
}

// maxBuiltinTopologyDepth bounds nested sub-agent definitions so a definition
// cannot describe an unbounded materialization.
const maxBuiltinTopologyDepth = 4

func validateBuiltinTopology(t *BuiltinTopology, llmRoutes, mcpServices map[string]struct{}, depth int) error {
	if t == nil {
		return nil
	}
	if depth > maxBuiltinTopologyDepth {
		return fmt.Errorf("runtime.builtin topology exceeds the maximum nesting depth %d", maxBuiltinTopologyDepth)
	}
	kind := t.Kind
	if kind == "" {
		kind = TopologyKindSingle
	}
	if t.PlanExecute != nil && kind != TopologyKindPlanExecute {
		return fmt.Errorf("topology.plan_execute is only meaningful for kind planexecute")
	}
	switch kind {
	case TopologyKindSingle:
		if len(t.SubAgents) > 0 {
			return fmt.Errorf("topology.kind single must not declare sub_agents")
		}
		if t.Factory != "" {
			return fmt.Errorf("topology.factory is only meaningful for kind custom")
		}
	case TopologyKindSequential, TopologyKindParallel, TopologyKindLoop, TopologyKindSupervisor:
		if len(t.SubAgents) == 0 {
			return fmt.Errorf("topology.kind %s requires at least one sub_agent", kind)
		}
		if t.Factory != "" {
			return fmt.Errorf("topology.factory is only meaningful for kind custom")
		}
	case TopologyKindPlanExecute:
		if len(t.SubAgents) > 0 {
			return fmt.Errorf("topology.kind planexecute must not declare sub_agents; configure roles via topology.plan_execute")
		}
		if t.Factory != "" {
			return fmt.Errorf("topology.factory is only meaningful for kind custom")
		}
		if err := validateBuiltinPlanExecute(t.PlanExecute, llmRoutes, mcpServices); err != nil {
			return err
		}
	case TopologyKindDeep:
		// Sub-agents are optional: the deep prebuilt ships a general-purpose
		// sub-agent by default.
		if t.Factory != "" {
			return fmt.Errorf("topology.factory is only meaningful for kind custom")
		}
	case TopologyKindCustom:
		if depth > 1 {
			// A factory receives the whole BuiltinRuntime definition; a nested
			// custom node would silently get the root definition instead of
			// its own, so custom is root-only.
			return fmt.Errorf("topology.kind custom is only supported on the root topology")
		}
		if t.Factory == "" {
			return fmt.Errorf("topology.kind custom requires topology.factory")
		}
		if !BuiltinFactoryRegistered(t.Factory) {
			return fmt.Errorf("builtin agent factory %q is not registered in this build", t.Factory)
		}
	default:
		return fmt.Errorf("unsupported topology.kind %q", kind)
	}
	if t.MaxIterations < 0 {
		return fmt.Errorf("topology.max_iterations must be non-negative")
	}
	seen := map[string]struct{}{}
	for i := range t.SubAgents {
		sub := &t.SubAgents[i]
		if sub.Name == "" {
			return fmt.Errorf("topology sub_agents require a name")
		}
		if _, dup := seen[sub.Name]; dup {
			return fmt.Errorf("topology sub_agent name %q is duplicated", sub.Name)
		}
		seen[sub.Name] = struct{}{}
		if err := validateBuiltinModel(sub.Model, false, llmRoutes); err != nil {
			return err
		}
		if err := validateBuiltinTools(sub.Tools, mcpServices); err != nil {
			return err
		}
		if sub.Generation != nil && sub.Generation.MaxTokens < 0 {
			return fmt.Errorf("sub_agent %q generation.max_tokens must be non-negative", sub.Name)
		}
		if err := validateBuiltinTopology(sub.Topology, llmRoutes, mcpServices, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func validateBuiltinPlanExecute(pe *BuiltinPlanExecute, llmRoutes, mcpServices map[string]struct{}) error {
	if pe == nil {
		return nil
	}
	roles := []struct {
		name     string
		role     *BuiltinPlanExecuteRole
		executor bool
	}{
		{"planner", pe.Planner, false},
		{"executor", pe.Executor, true},
		{"replanner", pe.Replanner, false},
	}
	for _, r := range roles {
		if r.role == nil {
			continue
		}
		if err := validateBuiltinModel(r.role.Model, false, llmRoutes); err != nil {
			return fmt.Errorf("plan_execute.%s: %w", r.name, err)
		}
		if r.role.Generation != nil && r.role.Generation.MaxTokens < 0 {
			return fmt.Errorf("plan_execute.%s generation.max_tokens must be non-negative", r.name)
		}
		if !r.executor {
			if len(r.role.Tools) > 0 {
				return fmt.Errorf("plan_execute.%s must not declare tools; only the executor runs tools", r.name)
			}
			if r.role.MaxIterations != 0 {
				return fmt.Errorf("plan_execute.%s must not set max_iterations; it is executor-only", r.name)
			}
			continue
		}
		if err := validateBuiltinTools(r.role.Tools, mcpServices); err != nil {
			return fmt.Errorf("plan_execute.%s: %w", r.name, err)
		}
		if r.role.MaxIterations < 0 {
			return fmt.Errorf("plan_execute.%s max_iterations must be non-negative", r.name)
		}
	}
	return nil
}

func idSet(ids []string) map[string]struct{} {
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

// builtinFactoryNames is the name registry that lets definition validation
// reject a custom factory absent from the linked binary — the same contract
// provider_type has. The host package (pkg/agent/builtin) registers the
// factory implementation and its name together.
var builtinFactoryNames sync.Map

// RegisterBuiltinFactoryName records a compiled-in custom agent factory name.
// Called by the host package's RegisterFactory; safe for concurrent use.
func RegisterBuiltinFactoryName(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	builtinFactoryNames.Store(name, struct{}{})
}

// BuiltinFactoryRegistered reports whether a custom factory name is linked
// into this build.
func BuiltinFactoryRegistered(name string) bool {
	_, ok := builtinFactoryNames.Load(name)
	return ok
}
