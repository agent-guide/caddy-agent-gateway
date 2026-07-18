package builtin

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/adk/prebuilt/deep"
	"github.com/cloudwego/eino/adk/prebuilt/planexecute"
	"github.com/cloudwego/eino/adk/prebuilt/supervisor"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/mcp/einotool"
)

// nodeSpec unifies the root definition and inline sub-agent definitions for
// materialization.
type nodeSpec struct {
	name         string
	description  string
	model        *agent.BuiltinModel
	systemPrompt string
	generation   *agent.BuiltinGeneration
	tools        []agent.BuiltinToolSelection
	topology     *agent.BuiltinTopology
	middlewares  *agent.BuiltinMiddlewares
}

func rootSpec(a agent.Agent) nodeSpec {
	def := a.Runtime.Builtin
	name := a.Name
	if name == "" {
		name = a.ID
	}
	return nodeSpec{
		name:         name,
		description:  a.Description,
		model:        &def.Model,
		systemPrompt: def.SystemPrompt,
		generation:   def.Generation,
		tools:        def.Tools,
		topology:     &def.Topology,
		middlewares:  def.Middlewares,
	}
}

func subSpec(sub agent.BuiltinSubAgent) nodeSpec {
	return nodeSpec{
		name:         sub.Name,
		description:  sub.Description,
		model:        sub.Model,
		systemPrompt: sub.SystemPrompt,
		generation:   sub.Generation,
		tools:        sub.Tools,
		topology:     sub.Topology,
	}
}

// buildNode materializes one topology node. inheritModel is the enclosing
// definition's model reference, used when the node does not declare its own.
// root marks the definition's root topology: custom factories receive the
// whole BuiltinRuntime, so custom is root-only (validation rejects nested
// custom nodes; this check backstops definitions persisted before it).
func (h *Host) buildNode(ctx context.Context, agentID string, spec nodeSpec, inheritModel *agent.BuiltinModel, def *agent.BuiltinRuntime, root bool) (adk.Agent, error) {
	kind := agent.TopologyKindSingle
	if spec.topology != nil && spec.topology.Kind != "" {
		kind = spec.topology.Kind
	}
	switch kind {
	case agent.TopologyKindSingle:
		return h.buildChatModelAgent(ctx, agentID, spec, inheritModel)
	case agent.TopologyKindCustom:
		if !root {
			return nil, fmt.Errorf("topology.kind custom is only supported on the root topology")
		}
		factory, ok := lookupFactory(spec.topology.Factory)
		if !ok {
			return nil, fmt.Errorf("builtin agent factory %q is not registered in this build", spec.topology.Factory)
		}
		return factory(ctx, FactoryDeps{Models: h.models, Tools: h.tools}, def)
	case agent.TopologyKindSequential, agent.TopologyKindParallel, agent.TopologyKindLoop:
		children, err := h.buildChildren(ctx, agentID, spec, inheritModel, def)
		if err != nil {
			return nil, err
		}
		switch kind {
		case agent.TopologyKindSequential:
			return adk.NewSequentialAgent(ctx, &adk.SequentialAgentConfig{Name: spec.name, Description: spec.description, SubAgents: children})
		case agent.TopologyKindParallel:
			return adk.NewParallelAgent(ctx, &adk.ParallelAgentConfig{Name: spec.name, Description: spec.description, SubAgents: children})
		default:
			return adk.NewLoopAgent(ctx, &adk.LoopAgentConfig{Name: spec.name, Description: spec.description, SubAgents: children, MaxIterations: spec.topology.MaxIterations})
		}
	case agent.TopologyKindSupervisor:
		children, err := h.buildChildren(ctx, agentID, spec, inheritModel, def)
		if err != nil {
			return nil, err
		}
		head, err := h.buildChatModelAgent(ctx, agentID, spec, inheritModel)
		if err != nil {
			return nil, err
		}
		return supervisor.New(ctx, &supervisor.Config{Supervisor: head, SubAgents: children})
	case agent.TopologyKindPlanExecute:
		return h.buildPlanExecute(ctx, agentID, spec, inheritModel)
	case agent.TopologyKindDeep:
		return h.buildDeep(ctx, agentID, spec, inheritModel, def)
	default:
		return nil, fmt.Errorf("topology.kind %q is not supported by the builtin host", kind)
	}
}

func (h *Host) buildChildren(ctx context.Context, agentID string, spec nodeSpec, inheritModel *agent.BuiltinModel, def *agent.BuiltinRuntime) ([]adk.Agent, error) {
	modelRef := spec.model
	if modelRef == nil {
		modelRef = inheritModel
	}
	children := make([]adk.Agent, 0, len(spec.topology.SubAgents))
	for _, sub := range spec.topology.SubAgents {
		child, err := h.buildNode(ctx, agentID, subSpec(sub), modelRef, def, false)
		if err != nil {
			return nil, fmt.Errorf("sub_agent %q: %w", sub.Name, err)
		}
		children = append(children, child)
	}
	return children, nil
}

func (h *Host) buildChatModelAgent(ctx context.Context, agentID string, spec nodeSpec, inheritModel *agent.BuiltinModel) (adk.Agent, error) {
	modelRef := spec.model
	if modelRef == nil || modelRef.LLMRouteID == "" {
		modelRef = inheritModel
	}
	if modelRef == nil || modelRef.LLMRouteID == "" {
		return nil, fmt.Errorf("node %q has no model route to resolve", spec.name)
	}
	// A node that carries tools must resolve to a tool-capable model. The
	// supervisor head transfers to sub-agents through tool calls, so it needs
	// the capability even with no MCP tools configured.
	requireTools := len(spec.tools) > 0
	if spec.topology != nil && spec.topology.Kind == agent.TopologyKindSupervisor {
		requireTools = true
	}
	chatModel, err := h.resolveModel(ctx, agentID, modelRef, spec.generation, requireTools)
	if err != nil {
		return nil, err
	}
	tools, err := h.resolveTools(ctx, agentID, spec.tools)
	if err != nil {
		return nil, err
	}
	cfg := &adk.ChatModelAgentConfig{
		Name:        spec.name,
		Description: spec.description,
		Instruction: spec.systemPrompt,
		Model:       chatModel,
	}
	if len(tools) > 0 {
		cfg.ToolsConfig = adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools}}
	}
	sum, err := summarizationHandler(ctx, spec.middlewares, chatModel)
	if err != nil {
		return nil, err
	}
	if sum != nil {
		cfg.Handlers = append(cfg.Handlers, sum)
	}
	return adk.NewChatModelAgent(ctx, cfg)
}

func summarizationHandler(ctx context.Context, mw *agent.BuiltinMiddlewares, chatModel einomodel.ToolCallingChatModel) (adk.ChatModelAgentMiddleware, error) {
	if mw == nil || mw.Summarization == nil || !mw.Summarization.Enabled {
		return nil, nil
	}
	sumCfg := &summarization.Config{Model: chatModel}
	if mw.Summarization.TriggerTokens > 0 {
		sumCfg.Trigger = &summarization.TriggerCondition{ContextTokens: mw.Summarization.TriggerTokens}
	}
	sum, err := summarization.New(ctx, sumCfg)
	if err != nil {
		return nil, fmt.Errorf("configure summarization middleware: %w", err)
	}
	return sum, nil
}

// buildPlanExecute materializes the plan-execute-replan prebuilt. Roles
// inherit the node's model unless overridden through topology.plan_execute;
// the executor inherits the node's tools unless its role declares its own.
// The planner and replanner emit plans through tool calling, so their models
// resolve with RequireTools regardless of MCP tool selection.
func (h *Host) buildPlanExecute(ctx context.Context, agentID string, spec nodeSpec, inheritModel *agent.BuiltinModel) (adk.Agent, error) {
	nodeModel := spec.model
	if nodeModel == nil || nodeModel.LLMRouteID == "" {
		nodeModel = inheritModel
	}
	var plannerRole, executorRole, replannerRole *agent.BuiltinPlanExecuteRole
	if pe := spec.topology.PlanExecute; pe != nil {
		plannerRole, executorRole, replannerRole = pe.Planner, pe.Executor, pe.Replanner
	}
	resolveRole := func(name string, role *agent.BuiltinPlanExecuteRole, requireTools bool) (einomodel.ToolCallingChatModel, error) {
		modelRef, gen := nodeModel, spec.generation
		if role != nil {
			if role.Model != nil && role.Model.LLMRouteID != "" {
				modelRef = role.Model
			}
			if role.Generation != nil {
				gen = role.Generation
			}
		}
		if modelRef == nil || modelRef.LLMRouteID == "" {
			return nil, fmt.Errorf("plan_execute role %q of node %q has no model route to resolve", name, spec.name)
		}
		return h.resolveModel(ctx, agentID, modelRef, gen, requireTools)
	}

	executorTools := spec.tools
	if executorRole != nil && len(executorRole.Tools) > 0 {
		executorTools = executorRole.Tools
	}
	tools, err := h.resolveTools(ctx, agentID, executorTools)
	if err != nil {
		return nil, err
	}

	plannerModel, err := resolveRole("planner", plannerRole, true)
	if err != nil {
		return nil, err
	}
	planner, err := planexecute.NewPlanner(ctx, &planexecute.PlannerConfig{ToolCallingChatModel: plannerModel})
	if err != nil {
		return nil, fmt.Errorf("build planner: %w", err)
	}

	executorModel, err := resolveRole("executor", executorRole, len(tools) > 0)
	if err != nil {
		return nil, err
	}
	execCfg := &planexecute.ExecutorConfig{Model: executorModel}
	if len(tools) > 0 {
		execCfg.ToolsConfig = adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools}}
	}
	if executorRole != nil {
		execCfg.MaxIterations = executorRole.MaxIterations
	}
	executor, err := planexecute.NewExecutor(ctx, execCfg)
	if err != nil {
		return nil, fmt.Errorf("build executor: %w", err)
	}

	replannerModel, err := resolveRole("replanner", replannerRole, true)
	if err != nil {
		return nil, err
	}
	replanner, err := planexecute.NewReplanner(ctx, &planexecute.ReplannerConfig{ChatModel: replannerModel})
	if err != nil {
		return nil, fmt.Errorf("build replanner: %w", err)
	}

	return planexecute.New(ctx, &planexecute.Config{
		Planner:       planner,
		Executor:      executor,
		Replanner:     replanner,
		MaxIterations: spec.topology.MaxIterations,
	})
}

// buildDeep materializes the deep prebuilt: a tool-driven head agent with a
// write_todos planning tool and a task tool that fans out to sub-agents. The
// head always resolves with RequireTools. Filesystem and shell backends stay
// unset — a builtin agent has no workspace to hand out.
func (h *Host) buildDeep(ctx context.Context, agentID string, spec nodeSpec, inheritModel *agent.BuiltinModel, def *agent.BuiltinRuntime) (adk.Agent, error) {
	modelRef := spec.model
	if modelRef == nil || modelRef.LLMRouteID == "" {
		modelRef = inheritModel
	}
	if modelRef == nil || modelRef.LLMRouteID == "" {
		return nil, fmt.Errorf("node %q has no model route to resolve", spec.name)
	}
	chatModel, err := h.resolveModel(ctx, agentID, modelRef, spec.generation, true)
	if err != nil {
		return nil, err
	}
	tools, err := h.resolveTools(ctx, agentID, spec.tools)
	if err != nil {
		return nil, err
	}
	children, err := h.buildChildren(ctx, agentID, spec, inheritModel, def)
	if err != nil {
		return nil, err
	}
	cfg := &deep.Config{
		Name:         spec.name,
		Description:  spec.description,
		ChatModel:    chatModel,
		Instruction:  spec.systemPrompt,
		SubAgents:    children,
		MaxIteration: spec.topology.MaxIterations,
	}
	if len(tools) > 0 {
		cfg.ToolsConfig = adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools}}
	}
	sum, err := summarizationHandler(ctx, spec.middlewares, chatModel)
	if err != nil {
		return nil, err
	}
	if sum != nil {
		cfg.Handlers = append(cfg.Handlers, sum)
	}
	return deep.New(ctx, cfg)
}

func (h *Host) resolveModel(ctx context.Context, agentID string, ref *agent.BuiltinModel, gen *agent.BuiltinGeneration, requireTools bool) (einomodel.ToolCallingChatModel, error) {
	if h.models == nil {
		return nil, fmt.Errorf("builtin host has no chat model resolver")
	}
	chatModel, err := h.models.ResolveChatModel(ctx, ref.LLMRouteID, ref.Model, requireTools)
	if err != nil {
		return nil, fmt.Errorf("resolve model route %q: %w", ref.LLMRouteID, err)
	}
	if opts := generationOptions(gen); len(opts) > 0 {
		chatModel = &optionedModel{inner: chatModel, opts: opts}
	}
	return newObservedModel(chatModel, h.observer, ref.LLMRouteID, agentID), nil
}

func (h *Host) resolveTools(ctx context.Context, agentID string, selections []agent.BuiltinToolSelection) ([]tool.BaseTool, error) {
	if len(selections) == 0 {
		return nil, nil
	}
	if h.tools == nil {
		return nil, fmt.Errorf("builtin host has no MCP tool source")
	}
	var out []tool.BaseTool
	for _, sel := range selections {
		tools, err := einotool.Tools(ctx, h.tools, sel.MCPServiceID, sel.Tools...)
		if err != nil {
			return nil, err
		}
		for _, t := range tools {
			info, err := t.Info(ctx)
			if err != nil {
				return nil, fmt.Errorf("tool info of service %q: %w", sel.MCPServiceID, err)
			}
			out = append(out, newObservedTool(t, h.observer, sel.MCPServiceID, info.Name, agentID))
		}
	}
	return out, nil
}

func generationOptions(gen *agent.BuiltinGeneration) []einomodel.Option {
	if gen == nil {
		return nil
	}
	var opts []einomodel.Option
	if gen.MaxTokens > 0 {
		opts = append(opts, einomodel.WithMaxTokens(gen.MaxTokens))
	}
	if gen.Temperature != nil {
		opts = append(opts, einomodel.WithTemperature(*gen.Temperature))
	}
	if gen.TopP != nil {
		opts = append(opts, einomodel.WithTopP(*gen.TopP))
	}
	return opts
}

// optionedModel prepends definition-level generation options to every call;
// per-call options come after so they win on conflicts.
type optionedModel struct {
	inner einomodel.ToolCallingChatModel
	opts  []einomodel.Option
}

func (m *optionedModel) Generate(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	return m.inner.Generate(ctx, input, m.merged(opts)...)
}

func (m *optionedModel) Stream(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	return m.inner.Stream(ctx, input, m.merged(opts)...)
}

func (m *optionedModel) merged(opts []einomodel.Option) []einomodel.Option {
	out := make([]einomodel.Option, 0, len(m.opts)+len(opts))
	out = append(out, m.opts...)
	out = append(out, opts...)
	return out
}

func (m *optionedModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	bound, err := m.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &optionedModel{inner: bound, opts: m.opts}, nil
}
