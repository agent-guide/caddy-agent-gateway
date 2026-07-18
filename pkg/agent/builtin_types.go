package agent

import (
	"fmt"
	"path/filepath"
	"slices"
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
	Permissions  *BuiltinPermissions    `json:"permissions,omitempty"`
	Limits       *BuiltinLimits         `json:"limits,omitempty"`
}

// Builtin tool-permission modes (§5.7.7). There is no "deny" mode: builtin
// tools are operator-declared allowlists already; a fully denied toolset is a
// definition without tools.
const (
	PermissionModeAutoApprove = "auto_approve"
	PermissionModeInteractive = "interactive"
)

// BuiltinPermissions is the root-level human-in-the-loop policy over the
// definition's MCP tool executions; it applies to every topology node.
// Gateway-local middleware tools (skill, plantask task tools, tool_search)
// are always exempt. In interactive mode an unapproved tool call suspends the
// turn through an ADK checkpoint interrupt — no turn slot, stream, or
// goroutine is held while a human decides — and resumes through the turn
// endpoint. Every lifecycle edge (decision timeout, definition update,
// pending capacity, unanswered calls) fails closed.
type BuiltinPermissions struct {
	// Mode is "auto_approve" (default — tools execute as the model asks) or
	// "interactive" (each MCP tool call needs an explicit decision).
	Mode string `json:"mode,omitempty"`
	// TimeoutSeconds is the pending-decision TTL. Zero takes the host default.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// MaxPending caps simultaneously pending permissions for the agent.
	// Pending permissions hold no turn slots, so without a cap they could
	// accumulate without bound. Zero takes the host default.
	MaxPending int `json:"max_pending,omitempty"`
	// AutoApproveTools bypasses interactive gating for fully-qualified
	// "<mcp_service_id>/<tool_name>" entries (bare names could collide across
	// services). Every entry must resolve to a declared tool selection.
	AutoApproveTools []string `json:"auto_approve_tools,omitempty"`
}

// Interactive reports whether the definition gates tool executions.
func (p *BuiltinPermissions) Interactive() bool {
	return p != nil && p.Mode == PermissionModeInteractive
}

// BuiltinModel resolves through a gateway LLM route, never a raw provider, so
// credential scheduling, candidate fallback, and LLM usage events apply
// unchanged. Model is the route target name (logical model in model-target
// routes, upstream model in direct-provider routes); empty falls back to the
// route's default resolution.
type BuiltinModel struct {
	LLMRouteID string             `json:"llm_route_id"`
	Model      string             `json:"model,omitempty"`
	Retry      *BuiltinModelRetry `json:"retry,omitempty"`
}

// maxBuiltinModelRetries caps per-call retry attempts: every retry re-runs
// the whole RoutedProvider candidate/credential fallback underneath, so the
// worst-case upstream call count is bounded by both layers multiplied.
const maxBuiltinModelRetries = 5

// BuiltinModelRetry enables node-level model-call retry through the ADK
// retry wrapper (eino-reuse.md §4.3). It complements — not replaces — the
// gateway's candidate fallback: RoutedProvider advances between candidates
// within one call, while this retries the whole call after that fallback is
// exhausted. Retryability mirrors the gateway's failure classification (429
// and 5xx retry; client-correctable errors fail immediately), and backoff
// keeps the ADK default. Sub-agents inherit it with the model reference.
// Not supported on planexecute role models: the eino prebuilt exposes no
// retry seam there, and a silent no-op is worse than a validation error.
type BuiltinModelRetry struct {
	// MaxRetries is the number of retry attempts after the initial call
	// (1..5).
	MaxRetries int `json:"max_retries"`
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
// configuration; each is off by default. They apply to the root definition's
// chat-model nodes (a single node, the supervisor head, the deep head).
type BuiltinMiddlewares struct {
	Summarization  *BuiltinSummarization  `json:"summarization,omitempty"`
	AgentsMD       *BuiltinAgentsMD       `json:"agentsmd,omitempty"`
	Reduction      *BuiltinReduction      `json:"reduction,omitempty"`
	ToolSearch     *BuiltinToolSearch     `json:"toolsearch,omitempty"`
	PlanTask       *BuiltinPlanTask       `json:"plantask,omitempty"`
	Skill          *BuiltinSkill          `json:"skill,omitempty"`
	PatchToolCalls *BuiltinPatchToolCalls `json:"patchtoolcalls,omitempty"`
}

// BuiltinSummarization enables the ADK context-compaction middleware using
// the agent's own chat model for summary generation.
type BuiltinSummarization struct {
	Enabled bool `json:"enabled"`
	// TriggerTokens overrides the token threshold that activates
	// summarization; 0 uses the ADK default.
	TriggerTokens int `json:"trigger_tokens,omitempty"`
}

// BuiltinAgentsMD enables the ADK agentsmd middleware over inline virtual
// documents: the content is injected transiently at model-call time, so it is
// excluded from summarization/reduction and never persisted to the session
// history. Docs are inline by design — a builtin agent has no workspace, and
// host filesystem paths would let a config-store object read arbitrary
// gateway-visible files into model context.
type BuiltinAgentsMD struct {
	Enabled bool `json:"enabled"`
	// Docs are the ordered virtual documents. Paths label the injected
	// sections and anchor @import references between docs; an @import that
	// resolves to no doc is skipped with a load warning, not an error.
	Docs []BuiltinAgentsMDDoc `json:"docs,omitempty"`
	// MaxTotalBytes caps the cumulative injected content; once exceeded,
	// remaining docs are skipped. 0 means no cap.
	MaxTotalBytes int `json:"max_total_bytes,omitempty"`
}

// BuiltinAgentsMDDoc is one inline virtual document.
type BuiltinAgentsMDDoc struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// BuiltinReduction enables the clear phase of the ADK tool-reduction
// middleware: when the estimated context exceeds the token threshold, older
// tool-call arguments and outputs are replaced with placeholders. Clearing is
// lossy — a builtin agent has no file backend to offload cleared content to,
// so the truncation/offload phase stays disabled.
type BuiltinReduction struct {
	Enabled bool `json:"enabled"`
	// MaxTokensForClear is the estimated-token threshold (a chars/4
	// heuristic, not a tokenizer count) that activates clearing; 0 uses the
	// ADK default.
	MaxTokensForClear int `json:"max_tokens_for_clear,omitempty"`
	// ClearRetentionSuffixLimit keeps the most recent N tool-calling
	// exchanges uncleared; 0 uses the ADK default of 1.
	ClearRetentionSuffixLimit int `json:"clear_retention_suffix_limit,omitempty"`
	// ClearExcludeTools lists tool names whose calls are never cleared.
	ClearExcludeTools []string `json:"clear_exclude_tools,omitempty"`
}

// BuiltinToolSearch enables the ADK dynamictool/toolsearch middleware: the
// node's MCP tools are withheld from the model's tool list and exposed
// through a tool_search meta-tool the model queries to load tools on demand.
// Useful when the referenced MCP services expose many tools. Client-side
// search only — the model-native variant needs deferred-tool support the
// gateway's providers do not expose. The tool list changes between calls as
// tools are loaded, which can invalidate the upstream prompt cache.
type BuiltinToolSearch struct {
	Enabled bool `json:"enabled"`
}

// BuiltinSkill enables the ADK skill middleware over inline virtual skills:
// the model gets a skill tool whose description advertises every skill's name
// and description, and invoking it returns the skill's instructions as the
// tool result. Inline execution only — the definition exposes no context/
// agent/model frontmatter, so fork-mode sub-agent execution and per-skill
// model overrides are structurally impossible. Skills are inline for the same
// reason agentsmd docs are: a builtin agent has no workspace, and host
// filesystem paths would let a config-store object read arbitrary
// gateway-visible files into model context.
type BuiltinSkill struct {
	Enabled bool `json:"enabled"`
	// Skills are the selectable inline skills.
	Skills []BuiltinSkillDoc `json:"skills,omitempty"`
}

// BuiltinSkillDoc is one inline skill: the name the model selects, the
// description advertised in the skill tool, and the markdown instructions
// returned when the skill is invoked.
type BuiltinSkillDoc struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Content     string `json:"content"`
}

// BuiltinPlanTask enables the ADK plantask middleware: the model gets
// TaskCreate/TaskGet/TaskUpdate/TaskList tools for maintaining a structured
// task list. The task board is stored in the session (in-memory,
// session-scoped), so it shares the session's restart-loss and eviction
// semantics and never leaks between conversations.
type BuiltinPlanTask struct {
	Enabled bool `json:"enabled"`
}

// BuiltinPatchToolCalls enables the ADK patchtoolcalls middleware: before
// every model call, tool calls in the history that have no corresponding tool
// result get a placeholder tool message inserted, so a history that ends up
// structurally incomplete never makes a strict upstream (tool_use/tool_result
// pairing) reject the request. Purely defensive — the host only commits
// successful turn transcripts, which are complete today.
type BuiltinPatchToolCalls struct {
	Enabled bool `json:"enabled"`
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
	b.Middlewares.normalize()
	if p := b.Permissions; p != nil {
		p.Mode = strings.ToLower(strings.TrimSpace(p.Mode))
		p.AutoApproveTools = normalizeIDs(p.AutoApproveTools)
	}
}

func (m *BuiltinMiddlewares) normalize() {
	if m == nil {
		return
	}
	if m.AgentsMD != nil {
		for i := range m.AgentsMD.Docs {
			// The agentsmd loader matches @import targets against cleaned
			// paths, so store the cleaned form; an empty path stays empty for
			// validation to reject.
			p := strings.TrimSpace(m.AgentsMD.Docs[i].Path)
			if p != "" {
				p = filepath.Clean(p)
			}
			m.AgentsMD.Docs[i].Path = p
		}
	}
	if m.Reduction != nil {
		m.Reduction.ClearExcludeTools = normalizeIDs(m.Reduction.ClearExcludeTools)
	}
	if m.Skill != nil {
		for i := range m.Skill.Skills {
			m.Skill.Skills[i].Name = strings.TrimSpace(m.Skill.Skills[i].Name)
			m.Skill.Skills[i].Description = strings.TrimSpace(m.Skill.Skills[i].Description)
		}
	}
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
	if b.Topology.Kind == TopologyKindPlanExecute && b.Model.Retry != nil {
		// The planexecute prebuilt exposes no retry seam; the roles would
		// silently inherit and drop the block otherwise.
		return fmt.Errorf("runtime.builtin model.retry is not supported when topology.kind is planexecute; roles inherit the node model")
	}
	if err := validateBuiltinTopology(&b.Topology, llmRoutes, mcpServices, 1); err != nil {
		return err
	}
	if b.Limits != nil {
		if b.Limits.MaxConcurrentTurns < 0 || b.Limits.TurnTimeoutSeconds < 0 {
			return fmt.Errorf("runtime.builtin.limits values must be non-negative")
		}
	}
	if err := validateBuiltinPermissions(b.Permissions, collectToolSelections(b)); err != nil {
		return err
	}
	if err := validateBuiltinMiddlewares(b.Middlewares, len(b.Tools) > 0); err != nil {
		return err
	}
	return nil
}

// collectToolSelections gathers every tool selection of the definition — the
// approval gate covers every topology node, so auto-approve entries may
// reference sub-agent and planexecute-executor tools too.
func collectToolSelections(b *BuiltinRuntime) []BuiltinToolSelection {
	var out []BuiltinToolSelection
	out = append(out, b.Tools...)
	var walk func(t *BuiltinTopology)
	walk = func(t *BuiltinTopology) {
		if t == nil {
			return
		}
		if pe := t.PlanExecute; pe != nil && pe.Executor != nil {
			out = append(out, pe.Executor.Tools...)
		}
		for i := range t.SubAgents {
			out = append(out, t.SubAgents[i].Tools...)
			walk(t.SubAgents[i].Topology)
		}
	}
	walk(&b.Topology)
	return out
}

// validateBuiltinPermissions checks the HITL block (§5.7.7). Auto-approve
// entries are fully qualified and must resolve to a declared tool selection
// anywhere in the topology; when the selection enumerates tool names the
// entry's tool must be among them (an empty selection exposes every service
// tool, so any name passes and resolution stays fail-closed at
// materialization).
func validateBuiltinPermissions(p *BuiltinPermissions, tools []BuiltinToolSelection) error {
	if p == nil {
		return nil
	}
	switch p.Mode {
	case "", PermissionModeAutoApprove, PermissionModeInteractive:
	default:
		return fmt.Errorf("runtime.builtin.permissions.mode must be %q or %q", PermissionModeAutoApprove, PermissionModeInteractive)
	}
	if p.TimeoutSeconds < 0 || p.MaxPending < 0 {
		return fmt.Errorf("runtime.builtin.permissions values must be non-negative")
	}
	if len(p.AutoApproveTools) > 0 && !p.Interactive() {
		return fmt.Errorf("runtime.builtin.permissions.auto_approve_tools requires mode %q", PermissionModeInteractive)
	}
	seen := map[string]struct{}{}
	for _, entry := range p.AutoApproveTools {
		serviceID, toolName, ok := strings.Cut(entry, "/")
		if !ok || strings.TrimSpace(serviceID) == "" || strings.TrimSpace(toolName) == "" {
			return fmt.Errorf("runtime.builtin.permissions.auto_approve_tools entry %q must be <mcp_service_id>/<tool_name>", entry)
		}
		if _, dup := seen[entry]; dup {
			return fmt.Errorf("runtime.builtin.permissions.auto_approve_tools entry %q is duplicated", entry)
		}
		seen[entry] = struct{}{}
		matched := false
		for _, sel := range tools {
			if sel.MCPServiceID != serviceID {
				continue
			}
			if len(sel.Tools) == 0 || slices.Contains(sel.Tools, toolName) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("runtime.builtin.permissions.auto_approve_tools entry %q does not resolve to a declared tool", entry)
		}
	}
	return nil
}

// maxBuiltinInlineContentBytes bounds one middleware's inline content payload
// (agentsmd docs, skills) so a definition cannot bloat the config store or
// the injected context unreasonably.
const maxBuiltinInlineContentBytes = 1 << 20

// validateBuiltinMiddlewares checks the middleware block. rootHasTools
// reports whether the root definition declares tool selections — middlewares
// attach to the root definition's chat-model nodes, which carry exactly those
// tools, so toolsearch without them has nothing to search.
func validateBuiltinMiddlewares(mw *BuiltinMiddlewares, rootHasTools bool) error {
	if mw == nil {
		return nil
	}
	if ts := mw.ToolSearch; ts != nil && ts.Enabled && !rootHasTools {
		return fmt.Errorf("runtime.builtin.middlewares.toolsearch requires the definition to declare tools")
	}
	if mw.Summarization != nil && mw.Summarization.TriggerTokens < 0 {
		return fmt.Errorf("runtime.builtin.middlewares.summarization.trigger_tokens must be non-negative")
	}
	if md := mw.AgentsMD; md != nil {
		if md.MaxTotalBytes < 0 {
			return fmt.Errorf("runtime.builtin.middlewares.agentsmd.max_total_bytes must be non-negative")
		}
		if md.Enabled && len(md.Docs) == 0 {
			return fmt.Errorf("runtime.builtin.middlewares.agentsmd requires at least one doc when enabled")
		}
		seen := map[string]struct{}{}
		total := 0
		for _, doc := range md.Docs {
			if doc.Path == "" || doc.Path == "." {
				return fmt.Errorf("runtime.builtin.middlewares.agentsmd docs require a path")
			}
			if _, dup := seen[doc.Path]; dup {
				return fmt.Errorf("runtime.builtin.middlewares.agentsmd doc path %q is duplicated", doc.Path)
			}
			seen[doc.Path] = struct{}{}
			if strings.TrimSpace(doc.Content) == "" {
				return fmt.Errorf("runtime.builtin.middlewares.agentsmd doc %q requires content", doc.Path)
			}
			total += len(doc.Content)
		}
		if total > maxBuiltinInlineContentBytes {
			return fmt.Errorf("runtime.builtin.middlewares.agentsmd docs exceed the total content limit of %d bytes", maxBuiltinInlineContentBytes)
		}
	}
	if rd := mw.Reduction; rd != nil {
		if rd.MaxTokensForClear < 0 {
			return fmt.Errorf("runtime.builtin.middlewares.reduction.max_tokens_for_clear must be non-negative")
		}
		if rd.ClearRetentionSuffixLimit < 0 {
			return fmt.Errorf("runtime.builtin.middlewares.reduction.clear_retention_suffix_limit must be non-negative")
		}
	}
	if sk := mw.Skill; sk != nil {
		if sk.Enabled && len(sk.Skills) == 0 {
			return fmt.Errorf("runtime.builtin.middlewares.skill requires at least one skill when enabled")
		}
		seen := map[string]struct{}{}
		total := 0
		for _, doc := range sk.Skills {
			if strings.TrimSpace(doc.Name) == "" {
				return fmt.Errorf("runtime.builtin.middlewares.skill skills require a name")
			}
			if _, dup := seen[doc.Name]; dup {
				return fmt.Errorf("runtime.builtin.middlewares.skill name %q is duplicated", doc.Name)
			}
			seen[doc.Name] = struct{}{}
			if strings.TrimSpace(doc.Content) == "" {
				return fmt.Errorf("runtime.builtin.middlewares.skill %q requires content", doc.Name)
			}
			total += len(doc.Content)
		}
		if total > maxBuiltinInlineContentBytes {
			return fmt.Errorf("runtime.builtin.middlewares.skill skills exceed the total content limit of %d bytes", maxBuiltinInlineContentBytes)
		}
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
	if r := m.Retry; r != nil {
		if r.MaxRetries < 1 || r.MaxRetries > maxBuiltinModelRetries {
			return fmt.Errorf("runtime.builtin model retry.max_retries must be between 1 and %d", maxBuiltinModelRetries)
		}
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
		if sub.Topology != nil && sub.Topology.Kind == TopologyKindPlanExecute && sub.Model != nil && sub.Model.Retry != nil {
			return fmt.Errorf("sub_agent %q: model.retry is not supported when topology.kind is planexecute; roles inherit the node model", sub.Name)
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
		if r.role.Model != nil && r.role.Model.Retry != nil {
			return fmt.Errorf("plan_execute.%s: model.retry is not supported for planexecute roles", r.name)
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
