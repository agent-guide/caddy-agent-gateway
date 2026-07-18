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
