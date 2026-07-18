package agent

import (
	"strings"
	"testing"
)

func validBuiltinAgent() Agent {
	return Agent{
		ID:   "triage",
		Name: "Triage",
		Runtime: Runtime{
			Type: RuntimeTypeBuiltin,
			Builtin: &BuiltinRuntime{
				Model:    BuiltinModel{LLMRouteID: "chat-main", Model: "smart"},
				Topology: BuiltinTopology{Kind: TopologyKindSingle},
				Tools:    []BuiltinToolSelection{{MCPServiceID: "fs", Tools: []string{"read_file"}}},
			},
		},
		Routes:    Routes{LLMRouteIDs: []string{"chat-main"}},
		Resources: Resources{MCPServiceIDs: []string{"fs"}},
	}
}

func TestBuiltinAgentValidates(t *testing.T) {
	a := validBuiltinAgent()
	a.Normalize()
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestBuiltinValidationRules(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Agent)
		wantErr string
	}{
		{
			name:    "model route must be bound in routes.llm_route_ids",
			mutate:  func(a *Agent) { a.Routes.LLMRouteIDs = nil },
			wantErr: "must appear in routes.llm_route_ids",
		},
		{
			name:    "tool service must be bound in resources.mcp_service_ids",
			mutate:  func(a *Agent) { a.Resources.MCPServiceIDs = nil },
			wantErr: "must appear in resources.mcp_service_ids",
		},
		{
			name:    "missing model route id",
			mutate:  func(a *Agent) { a.Runtime.Builtin.Model.LLMRouteID = "" },
			wantErr: "model.llm_route_id is required",
		},
		{
			name: "supervisor requires sub agents",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Topology = BuiltinTopology{Kind: TopologyKindSupervisor}
			},
			wantErr: "requires at least one sub_agent",
		},
		{
			name: "planexecute must not declare sub agents",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Topology = BuiltinTopology{Kind: TopologyKindPlanExecute, SubAgents: []BuiltinSubAgent{{Name: "x"}}}
			},
			wantErr: "must not declare sub_agents",
		},
		{
			name: "plan_execute block is planexecute-only",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Topology = BuiltinTopology{Kind: TopologyKindDeep, PlanExecute: &BuiltinPlanExecute{}}
			},
			wantErr: "only meaningful for kind planexecute",
		},
		{
			name: "planexecute role model must be a bound route",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Topology = BuiltinTopology{Kind: TopologyKindPlanExecute, PlanExecute: &BuiltinPlanExecute{
					Planner: &BuiltinPlanExecuteRole{Model: &BuiltinModel{LLMRouteID: "unbound-route"}},
				}}
			},
			wantErr: "must appear in routes.llm_route_ids",
		},
		{
			name: "planexecute planner must not carry tools",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Topology = BuiltinTopology{Kind: TopologyKindPlanExecute, PlanExecute: &BuiltinPlanExecute{
					Planner: &BuiltinPlanExecuteRole{Tools: []BuiltinToolSelection{{MCPServiceID: "fs"}}},
				}}
			},
			wantErr: "only the executor runs tools",
		},
		{
			name: "planexecute replanner must not set max_iterations",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Topology = BuiltinTopology{Kind: TopologyKindPlanExecute, PlanExecute: &BuiltinPlanExecute{
					Replanner: &BuiltinPlanExecuteRole{MaxIterations: 3},
				}}
			},
			wantErr: "executor-only",
		},
		{
			name: "planexecute executor tool service must be bound",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Topology = BuiltinTopology{Kind: TopologyKindPlanExecute, PlanExecute: &BuiltinPlanExecute{
					Executor: &BuiltinPlanExecuteRole{Tools: []BuiltinToolSelection{{MCPServiceID: "unbound-svc"}}},
				}}
			},
			wantErr: "must appear in resources.mcp_service_ids",
		},
		{
			name: "custom requires a registered factory",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Topology = BuiltinTopology{Kind: TopologyKindCustom, Factory: "not-linked"}
			},
			wantErr: "not registered in this build",
		},
		{
			name: "custom topology is root-only",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Topology = BuiltinTopology{Kind: TopologyKindSequential, SubAgents: []BuiltinSubAgent{
					{Name: "child", Topology: &BuiltinTopology{Kind: TopologyKindCustom, Factory: "any"}},
				}}
			},
			wantErr: "only supported on the root topology",
		},
		{
			name: "duplicate sub agent names",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Topology = BuiltinTopology{Kind: TopologyKindSequential, SubAgents: []BuiltinSubAgent{{Name: "dup"}, {Name: "dup"}}}
			},
			wantErr: "duplicated",
		},
		{
			name: "agentsmd enabled requires docs",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Middlewares = &BuiltinMiddlewares{AgentsMD: &BuiltinAgentsMD{Enabled: true}}
			},
			wantErr: "requires at least one doc",
		},
		{
			name: "agentsmd docs require a path",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Middlewares = &BuiltinMiddlewares{AgentsMD: &BuiltinAgentsMD{
					Enabled: true,
					Docs:    []BuiltinAgentsMDDoc{{Path: "", Content: "rules"}},
				}}
			},
			wantErr: "docs require a path",
		},
		{
			name: "agentsmd doc paths must be unique",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Middlewares = &BuiltinMiddlewares{AgentsMD: &BuiltinAgentsMD{
					Enabled: true,
					Docs: []BuiltinAgentsMDDoc{
						{Path: "AGENTS.md", Content: "one"},
						{Path: "AGENTS.md", Content: "two"},
					},
				}}
			},
			wantErr: "duplicated",
		},
		{
			name: "agentsmd docs require content",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Middlewares = &BuiltinMiddlewares{AgentsMD: &BuiltinAgentsMD{
					Enabled: true,
					Docs:    []BuiltinAgentsMDDoc{{Path: "AGENTS.md", Content: "  \n"}},
				}}
			},
			wantErr: "requires content",
		},
		{
			name: "agentsmd docs are capped in total size",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Middlewares = &BuiltinMiddlewares{AgentsMD: &BuiltinAgentsMD{
					Enabled: true,
					Docs:    []BuiltinAgentsMDDoc{{Path: "AGENTS.md", Content: strings.Repeat("x", maxBuiltinInlineContentBytes+1)}},
				}}
			},
			wantErr: "total content limit",
		},
		{
			name: "reduction thresholds must be non-negative",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Middlewares = &BuiltinMiddlewares{Reduction: &BuiltinReduction{Enabled: true, MaxTokensForClear: -1}}
			},
			wantErr: "max_tokens_for_clear must be non-negative",
		},
		{
			name: "skill enabled requires skills",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Middlewares = &BuiltinMiddlewares{Skill: &BuiltinSkill{Enabled: true}}
			},
			wantErr: "requires at least one skill",
		},
		{
			name: "skills require a name",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Middlewares = &BuiltinMiddlewares{Skill: &BuiltinSkill{
					Enabled: true,
					Skills:  []BuiltinSkillDoc{{Name: "  ", Content: "steps"}},
				}}
			},
			wantErr: "skills require a name",
		},
		{
			name: "skill names must be unique",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Middlewares = &BuiltinMiddlewares{Skill: &BuiltinSkill{
					Enabled: true,
					Skills:  []BuiltinSkillDoc{{Name: "pdf", Content: "one"}, {Name: "pdf", Content: "two"}},
				}}
			},
			wantErr: "duplicated",
		},
		{
			name: "skills require content",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Middlewares = &BuiltinMiddlewares{Skill: &BuiltinSkill{
					Enabled: true,
					Skills:  []BuiltinSkillDoc{{Name: "pdf", Content: " \n"}},
				}}
			},
			wantErr: "requires content",
		},
		{
			name: "toolsearch requires declared tools",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Tools = nil
				a.Resources.MCPServiceIDs = nil
				a.Runtime.Builtin.Middlewares = &BuiltinMiddlewares{ToolSearch: &BuiltinToolSearch{Enabled: true}}
			},
			wantErr: "toolsearch requires the definition to declare tools",
		},
		{
			name: "permissions mode must be a known value",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Permissions = &BuiltinPermissions{Mode: "ask"}
			},
			wantErr: "permissions.mode must be",
		},
		{
			name: "permissions values must be non-negative",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Permissions = &BuiltinPermissions{Mode: PermissionModeInteractive, TimeoutSeconds: -1}
			},
			wantErr: "permissions values must be non-negative",
		},
		{
			name: "auto_approve_tools requires interactive mode",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Permissions = &BuiltinPermissions{AutoApproveTools: []string{"fs/read_file"}}
			},
			wantErr: "auto_approve_tools requires mode",
		},
		{
			name: "auto_approve_tools entries must be fully qualified",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Permissions = &BuiltinPermissions{Mode: PermissionModeInteractive, AutoApproveTools: []string{"read_file"}}
			},
			wantErr: "must be <mcp_service_id>/<tool_name>",
		},
		{
			name: "auto_approve_tools entries must resolve to declared tools",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Permissions = &BuiltinPermissions{Mode: PermissionModeInteractive, AutoApproveTools: []string{"fs/write_file"}}
			},
			wantErr: "does not resolve to a declared tool",
		},
		{
			name: "auto_approve_tools entries must be unique",
			mutate: func(a *Agent) {
				a.Runtime.Builtin.Permissions = &BuiltinPermissions{Mode: PermissionModeInteractive, AutoApproveTools: []string{"fs/read_file", "fs/read_file"}}
			},
			wantErr: "is duplicated",
		},
		{
			name: "acp runtime must not carry a builtin block",
			mutate: func(a *Agent) {
				a.Runtime.Type = RuntimeTypeACP
				a.Runtime.ACP = &ACPRuntime{ServiceID: "svc"}
			},
			wantErr: "runtime.builtin must be empty",
		},
		{
			name: "non-builtin runtime must not claim builtin routes",
			mutate: func(a *Agent) {
				a.Runtime.Type = RuntimeTypeHTTP
				a.Runtime.HTTP = &HTTPRuntime{Endpoint: "http://agent.example"}
				a.Runtime.Builtin = nil
				a.Routes.BuiltinRouteIDs = []string{"builtin-route"}
			},
			wantErr: "builtin_route_ids is only valid for builtin runtime agents",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := validBuiltinAgent()
			tc.mutate(&a)
			err := a.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestBuiltinPlanExecuteAndDeepValidate(t *testing.T) {
	pe := validBuiltinAgent()
	pe.Runtime.Builtin.Topology = BuiltinTopology{Kind: TopologyKindPlanExecute, MaxIterations: 5, PlanExecute: &BuiltinPlanExecute{
		Planner:  &BuiltinPlanExecuteRole{Model: &BuiltinModel{LLMRouteID: "chat-main", Model: "planner"}},
		Executor: &BuiltinPlanExecuteRole{Tools: []BuiltinToolSelection{{MCPServiceID: "fs"}}, MaxIterations: 8},
	}}
	pe.Normalize()
	if err := pe.Validate(); err != nil {
		t.Fatalf("Validate() planexecute error = %v, want nil", err)
	}

	deep := validBuiltinAgent()
	deep.Runtime.Builtin.Topology = BuiltinTopology{Kind: TopologyKindDeep}
	deep.Normalize()
	if err := deep.Validate(); err != nil {
		t.Fatalf("Validate() deep without sub_agents error = %v, want nil", err)
	}
	deep.Runtime.Builtin.Topology.SubAgents = []BuiltinSubAgent{{Name: "researcher", Description: "digs into details"}}
	if err := deep.Validate(); err != nil {
		t.Fatalf("Validate() deep with sub_agents error = %v, want nil", err)
	}
}

func TestBuiltinMiddlewaresValidateAndNormalize(t *testing.T) {
	a := validBuiltinAgent()
	a.Runtime.Builtin.Middlewares = &BuiltinMiddlewares{
		Summarization: &BuiltinSummarization{Enabled: true, TriggerTokens: 4096},
		AgentsMD: &BuiltinAgentsMD{
			Enabled: true,
			Docs: []BuiltinAgentsMDDoc{
				{Path: " ./AGENTS.md ", Content: "Root rules.\n@style/go.md"},
				{Path: "style//go.md", Content: "Go style rules."},
			},
		},
		Reduction: &BuiltinReduction{Enabled: true, MaxTokensForClear: 120000, ClearExcludeTools: []string{" read_file ", ""}},
	}
	a.Normalize()
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
	docs := a.Runtime.Builtin.Middlewares.AgentsMD.Docs
	if docs[0].Path != "AGENTS.md" || docs[1].Path != "style/go.md" {
		t.Fatalf("doc paths = %q/%q, want cleaned AGENTS.md and style/go.md", docs[0].Path, docs[1].Path)
	}
	if got := a.Runtime.Builtin.Middlewares.Reduction.ClearExcludeTools; len(got) != 1 || got[0] != "read_file" {
		t.Fatalf("clear_exclude_tools = %v, want [read_file]", got)
	}
}

func TestBuiltinCustomFactoryNameRegistryUnblocksValidation(t *testing.T) {
	RegisterBuiltinFactoryName("linked-factory")
	a := validBuiltinAgent()
	a.Runtime.Builtin.Topology = BuiltinTopology{Kind: TopologyKindCustom, Factory: "linked-factory"}
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want registered factory to pass", err)
	}
}

func TestBuiltinNormalizeClearsOtherRuntimesAndDefaultsTopology(t *testing.T) {
	a := validBuiltinAgent()
	a.Runtime.ACP = &ACPRuntime{ServiceID: "leftover"}
	a.Runtime.HTTP = &HTTPRuntime{Endpoint: "http://x"}
	a.Runtime.Builtin.Topology.Kind = ""
	a.Normalize()
	if a.Runtime.ACP != nil || a.Runtime.HTTP != nil {
		t.Fatal("Normalize() must clear non-builtin runtime blocks")
	}
	if a.Runtime.Builtin.Topology.Kind != TopologyKindSingle {
		t.Fatalf("topology kind = %q, want default single", a.Runtime.Builtin.Topology.Kind)
	}
}
