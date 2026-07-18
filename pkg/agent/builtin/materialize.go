package builtin

import (
	"context"
	"fmt"
	"slices"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/agentsmd"
	"github.com/cloudwego/eino/adk/middlewares/dynamictool/toolsearch"
	"github.com/cloudwego/eino/adk/middlewares/patchtoolcalls"
	"github.com/cloudwego/eino/adk/middlewares/plantask"
	"github.com/cloudwego/eino/adk/middlewares/reduction"
	"github.com/cloudwego/eino/adk/middlewares/skill"
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
	handlers, toolsDynamic, err := middlewareHandlers(ctx, spec.middlewares, chatModel, tools)
	if err != nil {
		return nil, err
	}
	if len(tools) > 0 && !toolsDynamic {
		cfg.ToolsConfig = adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools}}
	}
	cfg.Handlers = append(cfg.Handlers, handlers...)
	return adk.NewChatModelAgent(ctx, cfg)
}

// middlewareHandlers assembles the enabled ADK middlewares. ADK runs handlers
// in registration order, and the order here is deliberate: patchtoolcalls
// completes dangling tool exchanges before anything else reads the history,
// reduction clears tool-output bloat before summarization counts tokens,
// skill and plantask contribute their tools (always statically visible —
// toolsearch only gates the node's own MCP tools), toolsearch manages tool
// visibility after the context managers settle the history, and agentsmd
// injects last so its content is invisible to all of them — transient per
// model call, never part of what gets compacted or cleared.
//
// nodeTools are the node's resolved MCP tools. When toolsearch is enabled
// they become the middleware's dynamic tools and toolsDynamic is true — the
// caller must then leave them out of ToolsConfig, or the model would see
// every tool statically and BeforeAgent would register each one twice.
func middlewareHandlers(ctx context.Context, mw *agent.BuiltinMiddlewares, chatModel einomodel.ToolCallingChatModel, nodeTools []tool.BaseTool) (handlers []adk.ChatModelAgentMiddleware, toolsDynamic bool, err error) {
	if mw == nil {
		return nil, false, nil
	}
	toolSearchOn := mw.ToolSearch != nil && mw.ToolSearch.Enabled
	if ptc := mw.PatchToolCalls; ptc != nil && ptc.Enabled {
		// First so the history is structurally complete (every tool call has a
		// result) before reduction scans tool exchanges and before
		// summarization hands the history to the model.
		patched, err := patchtoolcalls.New(ctx, &patchtoolcalls.Config{})
		if err != nil {
			return nil, false, fmt.Errorf("configure patchtoolcalls middleware: %w", err)
		}
		handlers = append(handlers, patched)
	}
	if rd := mw.Reduction; rd != nil && rd.Enabled {
		// Clear-only: a builtin agent has no file backend to offload cleared
		// content to (and no read_file tool to hand the model), so the
		// truncation/offload phase stays disabled and cleared tool outputs
		// become placeholders.
		excludeTools := rd.ClearExcludeTools
		if toolSearchOn {
			// toolsearch re-derives tool visibility from tool_search results in
			// the history; clearing them would silently hide loaded tools again.
			excludeTools = appendMissing(excludeTools, "tool_search")
		}
		red, err := reduction.New(ctx, &reduction.Config{
			SkipTruncation:            true,
			MaxTokensForClear:         int64(rd.MaxTokensForClear),
			ClearRetentionSuffixLimit: rd.ClearRetentionSuffixLimit,
			ClearExcludeTools:         excludeTools,
		})
		if err != nil {
			return nil, false, fmt.Errorf("configure reduction middleware: %w", err)
		}
		handlers = append(handlers, red)
	}
	if s := mw.Summarization; s != nil && s.Enabled {
		sumCfg := &summarization.Config{Model: chatModel}
		if s.TriggerTokens > 0 {
			sumCfg.Trigger = &summarization.TriggerCondition{ContextTokens: s.TriggerTokens}
		}
		sum, err := summarization.New(ctx, sumCfg)
		if err != nil {
			return nil, false, fmt.Errorf("configure summarization middleware: %w", err)
		}
		handlers = append(handlers, sum)
	}
	if sk := mw.Skill; sk != nil && sk.Enabled {
		// Definition validation rejects an enabled skill block without skills,
		// but a definition persisted around it must fail materialization, not
		// silently expose an empty skill tool.
		if len(sk.Skills) == 0 {
			return nil, false, fmt.Errorf("configure skill middleware: at least one skill is required")
		}
		skm, err := skill.NewMiddleware(ctx, &skill.Config{Backend: newSkillDocs(sk.Skills)})
		if err != nil {
			return nil, false, fmt.Errorf("configure skill middleware: %w", err)
		}
		handlers = append(handlers, skm)
	}
	if pt := mw.PlanTask; pt != nil && pt.Enabled {
		// The backend reads the session task board from the turn context
		// (ServeTurn binds it), so one cached middleware instance serves every
		// session without sharing state across conversations.
		ptm, err := plantask.New(ctx, &plantask.Config{Backend: planTaskBackend{}, BaseDir: planTaskBaseDir})
		if err != nil {
			return nil, false, fmt.Errorf("configure plantask middleware: %w", err)
		}
		handlers = append(handlers, ptm)
	}
	if toolSearchOn {
		// Client-side search only: the model-native variant moves tools to
		// deferred infos, which the gateway's providers do not expose.
		ts, err := toolsearch.New(ctx, &toolsearch.Config{DynamicTools: nodeTools})
		if err != nil {
			return nil, false, fmt.Errorf("configure toolsearch middleware: %w", err)
		}
		handlers = append(handlers, ts)
		toolsDynamic = true
	}
	if md := mw.AgentsMD; md != nil && md.Enabled {
		files := make([]string, 0, len(md.Docs))
		for _, doc := range md.Docs {
			files = append(files, doc.Path)
		}
		amd, err := agentsmd.New(ctx, &agentsmd.Config{
			Backend:             newAgentsMDDocs(md.Docs),
			AgentsMDFiles:       files,
			AllAgentsMDMaxBytes: md.MaxTotalBytes,
		})
		if err != nil {
			return nil, false, fmt.Errorf("configure agentsmd middleware: %w", err)
		}
		handlers = append(handlers, amd)
	}
	return handlers, toolsDynamic, nil
}

func appendMissing(list []string, name string) []string {
	if slices.Contains(list, name) {
		return list
	}
	out := make([]string, 0, len(list)+1)
	out = append(out, list...)
	return append(out, name)
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
	handlers, toolsDynamic, err := middlewareHandlers(ctx, spec.middlewares, chatModel, tools)
	if err != nil {
		return nil, err
	}
	if len(tools) > 0 && !toolsDynamic {
		cfg.ToolsConfig = adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools}}
	}
	cfg.Handlers = append(cfg.Handlers, handlers...)
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
