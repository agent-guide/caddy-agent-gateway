# Gateway Request Pipeline And External Orchestration

## 1. Status And Decision

This document defines the proposed Pipeline/Workflow boundary for
`agent-gateway`. It is
not implemented.

The architecture has two deliberately separate orchestration layers:

1. **Gateway Request Pipelines** are synchronous, request-bound, non-durable
   resource pipelines executed inside `agent-gateway`. They compose `llm`,
   `mcp`, and registered local `transform` steps and may preserve one
   protocol-native LLM response stream.
2. **Business Workflows** are caller-independent, durable processes owned by an
   upper-layer product such as an AI-native project workbench. A durable
   execution platform such as
   [Temporal](https://github.com/temporalio/temporal) owns scheduling,
   recovery, human tasks, multi-Agent handoff, and long-running state. Its
   Workers call Agent Gateway through the normal AgentRoute, LLM-route, and MCP
   data planes.

The split is based on **lifetime and ownership**, not graph size. A twenty-node
bounded preprocessing graph that must finish inside one HTTP request can be a
Gateway Request Pipeline. A one-node Agent operation that must survive caller
disconnect or gateway restart is a Business Workflow and belongs in the
external durable layer.

Agent Gateway does not implement a second durable execution engine. In
particular, it does not own durable Business Workflow Run/Task Run tables, a schedule
loop, restart replay, multi-process claiming, human-task state, or Project and
Team concepts.

## 2. System Boundary

### 2.1 Target Architecture

```text
users / project members
          |
          v
AI-native workbench
  - teams, projects, business tasks
  - RBAC, approvals, notifications
  - business database and UI projections
  - Temporal client
          |
          | start / signal / cancel / query
          v
Temporal service (or another durable workflow engine)
          |
          | task queue
          v
workbench-owned Workflow Workers
          |
          | Activities call stable gateway data-plane APIs
          v
AgentGuide Gateway
  - Agent identity, AgentRoute and runtime capabilities
  - LLM/MCP routing, credentials, policy and usage
  - synchronous Gateway Request Pipelines
          |
          +--> ACP / Codex / OpenCode
          +--> builtin Agent
          +--> HTTP Agent
          +--> LLM providers and MCP services
```

The workbench is the business orchestrator. Temporal is the durable execution
authority. Agent Gateway is the governed AI execution plane. Temporal does not
call Agent Gateway by itself: a Worker owned and deployed by the workbench
executes Activities and calls the gateway.

The workbench must not independently call Temporal and Agent Gateway to advance
the same business task. The Temporal Workflow decides when a node runs; its
Worker performs the gateway call and reports the result. This keeps one
orchestration authority and avoids dual-write races.

### 2.2 Ownership Matrix

| Concern | Owner |
|---|---|
| Team, Project, membership, business Task | upper-layer workbench |
| Human assignment, approval UI, notification, SLA | upper-layer workbench |
| Durable history, timers, retry, schedule, recovery | Temporal/external engine |
| Business Workflow code and Workers | upper-layer workbench |
| Agent identity, runtime and capability discovery | Agent Gateway |
| Agent turn execution and native events | Agent Gateway runtime backend |
| LLM/MCP routing, credentials and policy | Agent Gateway |
| Request-bound LLM/MCP/transform composition | Gateway Request Pipeline |
| Token/tool usage and interaction spans | Agent Gateway observability |

### 2.3 Data Ownership

The workbench database stores Projects, business Tasks, approvals, comments,
notifications, and mappings to external workflow ids and gateway interaction
ids. Temporal manages its own event history, timers, task queues, schedules,
and visibility state. Agent Gateway stores Agents, routes, providers,
credentials, MCP services, VirtualKeys, configuration definitions, and usage
events.

An upper layer may project external Workflow state into its database for UI
queries, but that projection is not an independent state machine. Agent Gateway
may expose correlation and usage data for a business run, but it does not copy
or drive the external engine's Workflow state.

## 3. Gateway Request Pipeline Scope

### 3.1 Goals

- Provide one static, typed DAG for request-local resource composition.
- Support `llm`, `mcp`, and registered local `transform` steps.
- Preserve the existing execution authority of LLM routes and MCP services.
- Support bounded fan-out/fan-in, concurrency, cancellation, timeout, and
  fail-closed dependency handling.
- Preserve protocol-native streaming when an LLM route invokes a Pipeline.
- Give one Pipeline Execution and its steps stable correlation identifiers and child
  usage spans.
- Keep definitions declarative, revisioned, governable, and safe to bind to an
  ingress route.

### 3.2 Non-Goals

- No durable or caller-independent Gateway Pipeline Execution.
- No `agent` step type. External Workers invoke an Agent through AgentRoute.
- No schedule, delayed start, backfill, overlap policy, or timer service.
- No human task, approval queue, suspend/resume, or Project/Team model.
- No restart recovery, event replay, multi-process task claiming, lease, or
  leader election.
- No model-authored or model-mutated graph.
- No replacement for an Agent runtime's internal tool/reasoning loop.
- No arbitrary Go, shell, JavaScript, template, or expression execution from a
  stored definition.
- No implicit HTTP loopback to gateway routes; step handlers call runtime
  managers in-process.
- No binary payload persistence in definition or run JSON.
- No nested Pipeline step in the first implementation.

Builtin Agent topologies remain eino ADK graphs internal to one Agent. Business
Workflows coordinate first-class Agents from outside the gateway. Neither is
converted into a Gateway Request Pipeline.

### 3.3 Selection Rules

| Scenario | Execution owner |
|---|---|
| MCP preprocessing → transform → streamed LLM reply | Gateway Request Pipeline |
| several bounded MCP calls joined inside one LLM request | Gateway Request Pipeline |
| one interactive Agent turn streamed to its caller | direct AgentRoute call |
| one Agent operation that must survive disconnect/restart | external Business Workflow Activity |
| scheduled Agent maintenance | external Business Workflow/Schedule |
| Agent A → human approval → Agent B | external Business Workflow |

The gateway must reject attempts to select a durable mode for a Request
Pipeline rather than silently degrading it to process-local execution.

## 4. Core Model

### 4.1 Request Pipeline Definition

A `RequestPipelineDefinition` is a versioned, static DAG:

```json
{
  "id": "vision-before-completion",
  "name": "Vision before completion",
  "disabled": false,
  "schema_version": "1",
  "input_type": "normalized_llm_request",
  "required_bindings": ["ingress.llm_target"],
  "steps": [
    {
      "id": "analyze-images",
      "type": "mcp",
      "target": {"service_id": "vision", "tool": "analyze_image"},
      "for_each": {"collection": {"ref": "execution.input.images"}},
      "input": {"arguments": {"object": {"image": {"ref": "item"}}}}
    },
    {
      "id": "rewrite",
      "type": "transform",
      "depends_on": ["analyze-images"],
      "target": {"handler": "replace_images_with_analysis"},
      "input": {
        "request": {"ref": "execution.input"},
        "analysis": {"ref": "steps.analyze-images.output"}
      }
    },
    {
      "id": "complete",
      "type": "llm",
      "depends_on": ["rewrite"],
      "target": {"binding": "ingress.llm_target"},
      "input": {"request": {"ref": "steps.rewrite.output"}},
      "terminal_output": true
    }
  ],
  "output": {"ref": "steps.complete.output"}
}
```

Definitions are configuration objects. Updates append a revision. An ingress
route pins one approved revision, so changing a definition cannot silently
expand the resources available through an existing route.

Revisions are addressed by `(pipeline_id, revision)`, where revision is
`sha256:` plus the SHA-256 of normalized canonical JSON. Canonicalization:

1. recursively sorts object keys;
2. preserves array order;
3. applies schema-versioned defaults;
4. excludes revision, timestamp, comments, and source-only annotations;
5. emits UTF-8 JSON without insignificant whitespace.

The store retains every revision referenced by an ingress route. Deletion
rejects a referenced revision. Bundle dry-run must surface canonical hash
changes, including changes caused only by reordering set-valued arrays.

### 4.2 Pipeline Execution

A Pipeline Execution exists only in memory and only for the lifetime of its
caller:

```json
{
  "id": "pex_...",
  "pipeline_id": "vision-before-completion",
  "pipeline_revision": "sha256:...",
  "status": "running"
}
```

Execution states are intentionally small:

```text
pending -> running -> succeeded
                   -> failed
                   -> cancelled
pending            -> cancelled
```

Caller disconnect, request timeout, or explicit cancellation moves the execution to
`cancelled`, cancels every active handler, and prevents queued steps from
starting. A Pipeline Execution never becomes `suspended` or `interrupted`, and it is
never recovered after restart.

Execution and step state are diagnostic runtime values, not Admin API
resources. There are no `/admin/pipeline-executions` endpoints and no durable
execution/event tables.

### 4.3 Pipeline Step

Common step fields include:

```json
{
  "id": "analyze",
  "type": "mcp",
  "depends_on": [],
  "condition": {
    "type": "input_has_media",
    "input": {"ref": "execution.input"},
    "media_type": "image"
  },
  "timeout_seconds": 30,
  "retry": {"max_attempts": 2, "backoff": "exponential"},
  "failure_policy": "fail_pipeline"
}
```

Step states are:

```text
pending -> ready -> running -> succeeded
                         |-> failed
                         |-> cancelled
pending/ready            -> skipped
pending/ready            -> cancelled
```

Retry is bounded inside the same request and is opt-in. The handler decides
whether an error is retryable. A side-effecting handler that cannot make a
repeat call safe rejects retry configuration. The runner never retries the
whole graph implicitly.

## 5. Step Types

### 5.1 LLM Step

An `llm` step performs exactly one model operation. It resolves a referenced
LLM route in-process and invokes `RoutedProvider`; it never calls a raw
provider. Route resolution remains authoritative for logical models,
capabilities, credential scheduling, candidate fallback, concrete upstream
model rewriting, and usage attribution.

The target is either a static resource route:

```json
{"target": {"llm_route_id": "coding-model"}}
```

or an ingress binding:

```json
{"target": {"binding": "ingress.llm_target"}}
```

Exactly one step owns the response stream for LLM-route invocation. It is the
`llm` step marked `terminal_output: true`. It must be the unique DAG sink, have
no condition or fan-out, depend transitively on every other step, and supply
the Pipeline output.

### 5.2 MCP Step

An `mcp` step invokes one tool on one gateway-managed MCP service. Its handler
calls `pkg/mcp/service.Manager` directly and does not issue a request through an
MCP ingress route.

A step may declare bounded `for_each` fan-out. Child results retain input order
and share the request concurrency limit. The handler also respects the target
service's concurrency capability. In particular, a reused stdio service is
serialized until its transport provides safe concurrent request dispatch.

### 5.3 Transform Step

A `transform` step performs deterministic gateway-local conversion through a
compiled-in registered handler. Stored definitions cannot contain executable
source, filesystem paths to load as code, or arbitrary expressions.

Transforms cover protocol normalization, artifact preparation, deterministic
mapping, and join operations that do not belong to LLM or MCP runtimes.

### 5.4 Why There Is No Agent Step

An Agent turn is already a first-class operation exposed by AgentRoute and
implemented by `agentruntime.Backend.ServeTurn`. Wrapping it in a Gateway
Pipeline would either remain request-bound, adding little value, or reintroduce
the durable scheduling and recovery engine rejected by this design.

An upper-layer Worker therefore treats an Agent turn as a Business Workflow
Activity and calls `POST /<agent-route>/turn`. Multi-Agent handoff is expressed
in the external Workflow, not as an Agent Gateway DAG.

## 6. Typed Data And Artifacts

Steps exchange typed values through explicit `BoundValue` objects. Initial
references are:

```text
execution.input
execution.input.<path>
execution.bindings.<name>
steps.<step-id>.output
steps.<step-id>.output.<path>
```

Inside `for_each`, `item`, `item.<path>`, and `index` are additionally valid.
The path grammar is typed field traversal, not JSONPath: it has no wildcard,
filter, arbitrary array index, or executable expression.

```text
BoundValue :=
  { "ref": SourceReference }
  { "value": JSONLiteral }
  { "object": map<string, BoundValue> }
  { "array": list<BoundValue> }
```

Every step handler publishes an input/output schema. Validation checks reference
scope, dependency order, and compatible types where known; runtime validation
fails closed for dynamically shaped JSON.

Binary media uses an `ArtifactRef`. Request artifacts live in request-scoped
memory plus permission-restricted temporary files where a local MCP process
requires a path. Every temporary file is removed when the request succeeds,
fails, or is cancelled. Gateway Request Pipelines never persist binary
artifacts for later continuation.

## 7. Runner And Package Boundary

```text
pkg/requestpipeline
  - definitions and validation
  - in-memory execution/step states
  - bounded DAG runner
  - step handler SPI
  - request artifact interfaces

pkg/dispatcher
  -> narrow RequestPipelineRunner interface

assembly
  - LLM handler       -> pkg/gateway
  - MCP handler       -> pkg/mcp/service
  - transform registry
```

`pkg/requestpipeline` must not import `pkg/agent`, `pkg/mcp`, `pkg/llm`,
`pkg/gateway`, or protocol handlers. Concrete handlers live in the assembly
layer and are injected through an SPI:

```go
type StepHandler interface {
    Type() StepType
    Validate(context.Context, StepDefinition) error
    Execute(context.Context, StepRequest, StepEventSink) (StepResult, error)
    Cancel(context.Context, StepHandle) error
}
```

The runner pins and validates the definition, creates one request context and
artifact scope, executes ready steps under a hard concurrency bound, records
diagnostic events, resolves the output, and cleans up. No handler may create a
goroutine that outlives the request context.

### 7.1 Protocol-Native Output

The dispatcher supplies a protocol-specific `ResponseStreamer` only to the
terminal-output LLM step. It closes over the live response writer and existing
OpenAI/Anthropic/CC serializer. The runner decides which step receives the
streamer but never parses or writes a wire protocol itself.

Before any response bytes are committed, all preprocessing steps have reached
an accepted terminal state and the LLM handler has validated that the
streamer's protocol matches the normalized request type.

## 8. LLM Route Integration

An LLM route gains an optional execution policy:

```json
{
  "execution_policy": {
    "type": "request_pipeline",
    "pipeline_id": "vision-before-completion",
    "pipeline_revision": "sha256:...",
    "timeout_seconds": 120,
    "max_concurrency": 4
  }
}
```

The default remains `{"type":"direct"}`.

Dispatcher order is:

```text
match route and validate VirtualKey/rate limit
  -> resolve pinned definition from the in-memory snapshot
  -> protocol handler prepares normalized request/media
  -> validate invocation bindings
  -> execute request-local steps
  -> terminal LLM step streams through the protocol handler
```

The ingress path never reads the config store per request. Definition manager
mutations refresh an immutable in-memory snapshot keyed by `(id, revision)`.

## 9. Cancellation, Retry, And Error Mapping

The caller context is the root cancellation authority. Cancellation closes LLM
streams, cancels MCP calls, stops transforms, prevents new steps from becoming
ready, and marks every unfinished in-memory step cancelled.

Step handlers return a closed error classification rather than arbitrary HTTP
status codes:

| Classification | Sync ingress status |
|---|---:|
| `client_invalid_input` | 400 |
| `client_payload_too_large` | 413 |
| `client_policy` | 400 |
| `upstream_unavailable` | 503 |
| `upstream_timeout` | 504 |
| `upstream_failure` | 502 |
| `internal` or unknown | 500 |

The terminal LLM step keeps the route's existing provider error mapping. A
non-terminal LLM failure is `upstream_failure`. No partial-success response is
synthesized, although completed child usage spans remain observable.

Bounded retry exists only inside the live request. A logical step identity
`(execution_id, step_id, fan_out_index)` is stable across attempts and can be used as
an idempotency key where a downstream operation supports it. There is no
requeue or retry after caller disconnect or process restart.

## 10. External Business Workflow Integration

### 10.1 Gateway Contract For Workers

An external Workflow Worker uses the gateway's normal data plane:

- Agent Activity: `POST /<agent-route>/turn` and its common SSE event envelope;
- LLM Activity: an authorized LLM route;
- MCP Activity: an authorized MCP route or an application-owned MCP client;
- cancellation: the exact Agent run cancellation capability where advertised;
- inspection: Agent capability/runtime and interaction Admin APIs where the
  Worker has administrative authority.

The integration must not depend on gateway-internal Go packages or database
tables. Temporal is one supported architecture, not a library that lower
gateway packages import.

### 10.2 Correlation And Authority

The upper layer supplies and persists its own immutable business identity:

```text
business_workflow_id
business_run_id
business_task_id
project_id (optional)
origin principal
```

Gateway execution returns `agent_id`, `run_id`, trace/span ids, and interaction
ids. The workbench stores the mapping. Trusted deployments may propagate
bounded correlation fields through a versioned, authenticated metadata
envelope; untrusted callers cannot stamp arbitrary principals, budgets, or
Agent identities.

Temporal retry does not make an Agent or external side effect exactly-once.
The Worker must use a stable Activity-level execution key where the selected
Agent backend supports it, and configure non-retryable/at-most-once behavior
where it does not. Capability discovery is authoritative.

### 10.3 Human Approval

Human-task lifecycle belongs to the workbench and external engine:

1. a Worker calls Agent Gateway and records the result;
2. the Workflow waits for an external decision;
3. the workbench renders and authorizes the approval task;
4. the decision is sent to the external Workflow through its signal/update
   mechanism;
5. the Workflow schedules the next Activity.

Gateway-native runtime permissions remain a different, narrower concern: they
authorize a tool operation inside one live Agent turn using that runtime's
advertised permission capability. They are not a replacement for a Project
approval node.

### 10.4 Streaming

Durable Activities should return bounded results or artifact references, not
put token deltas, large transcripts, or binary media into durable Workflow
history. A Worker may relay live Agent events to the workbench's event channel
while the gateway remains the source of detailed usage/interaction events.

An interactive chat request that must stream directly to one HTTP client may
call AgentRoute directly. If it becomes a durable business task, the upper
layer must define how live events are projected independently of the caller
connection.

### 10.5 Deployment

Temporal Service, its persistence, and Workflow Workers are deployed and
operated outside Agent Gateway. The gateway binary does not embed Temporal,
start a Temporal scheduler, or require Temporal for ordinary routing and
request pipelines. A product distribution may provide Compose/Helm examples,
Workers, or an SDK integration package without changing this ownership.

## 11. Governance And Authorization

Every Pipeline Execution inherits its ingress VirtualKey, route id, trace,
rate limit, and caller cancellation. A definition may reference resources only by static
id or a declared binding such as `ingress.llm_target`; it cannot reference
credentials, environment values, arbitrary URLs, or dynamically computed
resource ids.

Binding a route to an immutable definition revision is the explicit grant for
that route to use the revision's static resource manifest. Updating a
definition never moves existing route bindings. Adopting a revision whose
manifest changes requires route revalidation.

External Business Workflows are authorized independently by the workbench and
Temporal deployment. Each Worker call to Agent Gateway must still authenticate
through a scoped VirtualKey or equivalent gateway credential and remains
subject to route, Agent, resource, quota, and depth policy. Possession of a
Temporal Workflow id never grants gateway access.

## 12. Validation And Safety

Definition validation always rejects:

- duplicate, slash-containing, or reference-unsafe ids;
- unknown step, transform, or condition types;
- cycles, dangling dependencies, and references to non-dependencies;
- `agent` or nested Pipeline steps;
- missing, disabled, or incompatible resource targets;
- unbounded step count, fan-out, concurrency, timeout, retry, input, or output;
- invalid BoundValue discriminators or reference grammar;
- unsafe media types, decoded sizes, aggregate sizes, or remote-URL policy;
- retry on a handler that cannot safely repeat the operation inside a request.

Binding validation additionally requires exactly one terminal-output LLM step,
the unique sink/output rules from §5.1, all invocation bindings, compatible
route capabilities, and an enabled pinned revision.

Definitions live in a dedicated configuration store (suggested name:
`request_pipelines`) and participate in transactional bundle
apply/export/validate. They are applied after referenced MCP services and
resource LLM routes, and before ingress LLM routes that pin them. Executions, events,
Projects, schedules, and external engine objects never enter a gateway bundle.

## 13. Implementation Plan

### G0: In-Memory Runner

- add `pkg/requestpipeline` definitions, validation, bounded DAG runner, step
  SPI, in-memory state, cancellation cascade, and request artifact store;
- implement Transform and MCP handlers;
- implement closed step-error classification;
- test fan-out ordering, bounds, cancellation, cleanup, and retry safety.

### G1: Definition Management

- add the `request_pipelines` config store and immutable manager snapshot;
- add `/admin/request-pipelines` definition CRUD;
- add bundle apply/export/validation and revision pinning;
- keep execution state non-persistent and without an Execution Admin API.

### G2: LLM Route Profile

- add `execution_policy.type = request_pipeline` to LLM routes;
- add normalized protocol request/media hooks and `ResponseStreamer`;
- implement the LLM handler over `RoutedProvider`;
- implement route binding validation;
- ship the Zhipu vision pipeline as the first end-to-end consumer.

### G3: External Orchestration Contract

- document Worker-safe AgentRoute/LLM/MCP invocation patterns;
- define trusted correlation metadata and stable execution-key propagation;
- publish a Temporal reference Worker/sample outside the runtime core;
- test cancellation, duplicate Activity delivery, permission capability
  discovery, event relay, and trace correlation;
- do not add Temporal SDK dependencies to `pkg/requestpipeline`,
  `pkg/agent/runtime`, or protocol packages.

## 14. Rejected Alternatives

### Gateway-Owned Durable Workflow Engine

This would duplicate the hardest parts of Temporal-class systems: persisted
history, recovery, timers, schedules, task claiming, worker versioning,
visibility, and failure semantics. It would also pull Project and human-task
concerns into the AI gateway.

### Temporal For Every LLM Request

Request-bound preprocessing needs low latency, caller cancellation, in-process
resource managers, and one live protocol response stream. Requiring a durable
service for that path adds deployment and wire complexity without useful
durability.

### Single-Step-Only Request Pipeline

A single step is already an ordinary LLM, MCP, or Agent call. The useful
gateway abstraction is a small but genuinely multi-step, bounded resource
pipeline. The simplification comes from removing durability, not from removing
composition.

### Agent Step Inside Gateway Pipeline

AgentRoute already provides the stable execution boundary. Durable Agent
coordination belongs to the upper-layer Workflow Worker; request-local wrapping
would add a second lifecycle without providing recovery.

### Separate Input Processor Pipeline

A dedicated processor chain would duplicate typed dependency handling,
cancellation, fan-out, events, and validation already needed by the request DAG.

### Provider-Local MCP Calls Or Implicit Tool Injection

Providers own model wire behavior, not tool orchestration. Injecting a tool
schema does not execute it, and intercepting model tool calls would create a
hidden Agent loop that breaks protocol transparency.
