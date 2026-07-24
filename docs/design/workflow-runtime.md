# Unified Workflow Runtime

## 1. Status

This document defines the proposed workflow architecture for `agent-gateway`.
It is not implemented.

Agent execution inside an `agent` task uses the turn-first capability layer
from [Unified Agent Runtime and Routing](../plans/unified-agent-runtime.md).
This document remains authoritative for Workflow Definition/Run/Task Run state,
durability, scheduling, retry, artifacts, and DAG semantics; the runtime plan is
authoritative for Agent backend selection, turn events, capabilities, and the
AgentRoute cutover.

The design supersedes the split roadmap in
[Agents Control Plane](agents-control-plane.md) where P2 `AgentTask` and P3
`Agent Workflow` were described as separate execution systems. There is one
workflow definition, one runner, one task SPI, and one run state machine.
Different callers select an invocation policy:

- an LLM route starts a synchronous, request-bound, non-durable run;
- an operator or schedule starts a durable run;
- an agent handoff is an `agent` task in a durable run;
- a one-off "run this agent" request is a one-task workflow run.

A one-off operator "run this agent now" request defaults to **durable**, not
synchronous, even though it is short and immediate. The reason is the
principal/origin model (§14): a gateway-owned agent turn needs an auditable,
cancellable, recoverable run record independent of whoever happens to be
holding an HTTP connection. A synchronous run dies with its caller; an operator
who kicks off an agent turn and then loses their terminal would lose the run.
Synchronous mode is reserved for the LLM-route ingress profile, where the run
is genuinely bound to one client response stream.

`AgentTask` remains useful product vocabulary, but it is not a second runtime
model. It is a workflow task whose `type` is `agent`.

## 2. Motivation

The gateway already has four independently useful execution surfaces:

- LLM routes resolve models, providers, credentials, fallback, and usage;
- MCP services discover and execute tools;
- ACP, HTTP, and builtin runtimes execute first-class agents;
- the agent control plane plans external tasks, schedules, and multi-agent
  handoffs.

Several gateway features need to compose these surfaces:

- preprocess images with an MCP vision tool before calling a text model;
- execute one agent immediately or on a schedule;
- hand work from agent A to agent B;
- run an MCP or LLM resource step between agent turns;
- expose one trace and cancellation boundary for the whole operation.

Implementing each feature as a separate pipeline would duplicate dependency
handling, cancellation, retry, events, persistence, and observability. The
workflow runtime provides those mechanics once without implementing an agent's
internal reasoning loop.

## 3. Goals

- Provide one static workflow DAG made of typed tasks.
- Support `llm`, `mcp`, `agent`, and registered local `transform` tasks.
- Let request-bound and durable runs use the same runner and task handlers.
- Preserve the existing execution authority of LLM routes, MCP services, and
  agent runtime backends.
- Support fan-out/fan-in, bounded concurrency, cancellation, timeout, retry,
  and fail-closed dependency handling.
- Preserve protocol-native streaming when an LLM route invokes a workflow.
- Give every run and task stable correlation identifiers and child usage spans.
- Make schedules create durable workflow runs rather than hidden loops.
- Cover the Agent Task and Agent Workflow requirements in
  `agents-control-plane.md`.

### 3.1 Agent Control Plane Coverage

The unified model covers the planned control-plane operations directly:

| Control-plane requirement | Workflow representation |
|---|---|
| Run an agent now | Durable or synchronous run with one `agent` task |
| Resume an agent session | `agent` task input carries the backend session id and new input |
| Schedule maintenance | Schedule creates a durable run of a stored definition |
| Cancel and audit work | Workflow Run cancellation and Task Run events |
| Retry external work | Task-local retry over `runtimeapi.Backend`, gated by idempotency capability |
| Agent A to Agent B handoff | B depends on A and consumes A's typed output |
| Multi-agent graph | Static DAG containing multiple `agent` tasks |
| Resource step between agents | `llm`, `mcp`, or `transform` task |
| Gateway-owned workflow state | Durable Workflow Run/Task Run/event tables |
| Runtime topology view | Workflow DAG plus trace/span parentage |

The Agent workspace may project runs involving one Agent, but that projection
does not create another task object or API lifecycle.

## 4. Non-Goals

- No model-authored or model-mutated workflow graph.
- No replacement for an agent runtime's internal tool/reasoning loop.
- No arbitrary Go, shell, JavaScript, template, or expression execution from
  stored workflow configuration.
- No distributed scheduler or multi-process task claiming in the first
  implementation.
- No implicit HTTP loopback to gateway routes. Task handlers call runtime
  managers in-process.
- No binary payload persistence in workflow JSON records.
- No compatibility requirement for the unimplemented P2/P3 API shapes in
  `agents-control-plane.md`.

Builtin agent topologies remain eino ADK graphs internal to one builtin agent.
They are not converted into gateway workflows. A workflow coordinates
first-class agents and resources from outside those agents.

## 5. Core Model

### 5.1 Workflow Definition

A `WorkflowDefinition` is a versioned, static DAG:

```json
{
  "id": "review-and-fix",
  "name": "Review and fix",
  "description": "Ask one agent to review and another to implement",
  "disabled": false,
  "schema_version": "1",
  "input_type": "object",
  "tasks": [
    {
      "id": "review",
      "type": "agent",
      "target": { "agent_id": "reviewer" },
      "input": { "prompt": { "ref": "run.input.prompt" } }
    },
    {
      "id": "fix",
      "type": "agent",
      "depends_on": ["review"],
      "target": { "agent_id": "implementer" },
      "input": {
        "prompt": { "ref": "run.input.prompt" },
        "context": { "ref": "tasks.review.output" }
      }
    }
  ],
  "output": { "ref": "tasks.fix.output" }
}
```

Definitions are configuration objects. Updates create a new definition
revision. A run pins the revision it started with; an update affects only new
runs. An ingress route also pins an approved definition revision, so updating a
definition cannot silently expand the resources available through an existing
route.

Revisions are immutable and addressable by `(workflow_id, revision)`. The
definition store retains a revision while any ingress route, durable run, or
schedule references it; update appends a revision rather than overwriting the
only stored payload. The in-memory manager snapshot is keyed the same way and
may additionally expose the latest revision for new administrative runs.
Deletion/garbage collection rejects a referenced revision.

The revision is `sha256:` plus the SHA-256 of the definition's normalized
canonical JSON. Canonicalization rules, in order:

1. sort object keys recursively (a stable, total ordering);
2. preserve array order exactly — arrays are ordered by the author and are
   **never** sorted or deduplicated;
3. apply one fixed, versioned set of schema defaults for missing optional
   fields (so an omitted field and its default produce the same bytes);
4. exclude revision/timestamp metadata, comments, and source-only annotations;
5. encode the result as UTF-8 with no insignificant whitespace.

This lets CLI, Admin API, and gateway runtime compute the same value. Two
consequences are explicit design choices:

- **Array order is authoritative for set-valued fields too.** Fields like
  `allowed_media_types` are validated as sets at apply time (duplicate-free,
  within an allowlist), but their canonical form is the author's order. Two
  bundles that list the same media types in a different order are *different
  revisions* and must be re-pinned deliberately. This keeps the hash a pure
  function of bytes rather than of an embedding-dependent set normalization.
- **Default values are versioned with the gateway.** If a future release
  changes the default for a field, the canonical form of an unchanged
  definition changes too, so its pinned revision no longer matches. Migration
  is therefore explicit: a new gateway release that changes defaults must
  either (a) keep the old default canonicalized for definitions whose schema
  version predates the change, or (b) re-compute and re-pin every affected
  route as part of the upgrade. A definition carries its schema version so the
  canonicalizer picks the right default table; "silently migrating" pinned
  revisions on upgrade is forbidden because it would grant a route a resource
  set it was never validated against.

**Operational consequence — set-valued field reorder is a silent revision bump.**
Because the resource manifest (§14) is computed from referenced resource ids
and tools, not from field byte order, reordering the entries of a set-valued
field such as `allowed_media_types` changes the canonical hash — and therefore
the pinned revision — **without changing the resource manifest**. The §14
"adopting a revision whose resource manifest changed requires revalidating
every affected route" guard therefore does not fire for a pure reorder, so an
operator who accidentally swaps two media types in source YAML silently
produces a new revision. An ingress route that pins the old revision keeps
serving the old definition (correct), but an operator who reapplies the
reordered bundle expecting it to be a no-op will instead get a revision
mismatch unless they deliberately repin. The mitigation is procedural, not
algorithmic: `agwctl apply --dry-run` (or a CI step) must compute and diff the
canonical hash against the currently pinned revision and surface a reorder as
an explicit, human-reviewed change rather than letting it pass as a no-op.
This tradeoff (a pure-bytes hash, at the cost of a CI discipline) is chosen
deliberately over the alternative of normalizing sets before hashing, which
would make the hash a function of an embedding-dependent canonicalization and
break the "same bytes → same revision" invariant across CLI, Admin API, and
runtime.

### 5.2 Workflow Run

A `WorkflowRun` is one execution of a pinned definition:

```json
{
  "id": "wfr_...",
  "workflow_id": "review-and-fix",
  "workflow_revision": "sha256:<definition-content-hash>",
  "mode": "durable",
  "status": "running",
  "input": {},
  "output": null,
  "error": null,
  "created_at": "...",
  "started_at": "...",
  "finished_at": null
}
```

Run states are:

```text
pending        -> running               (runner starts the run)
running        -> succeeded             (workflow output resolved + all required tasks terminal)
running        -> failed                (fail_workflow policy triggered)
running        -> cancelled             (caller/admin cancel, or caller disconnect on a sync run)
running        -> suspended             (durable run, a task requested permission suspend)
running        -> interrupted           (durable run lost ownership, e.g. process restart)
pending        -> cancelled             (cancelled before it ever started)
suspended      -> running               (allow/deny decision resumes — NOT recovery)
suspended      -> cancelled             (explicit request/admin cancel)
interrupted    -> running               (requeue recovery reclaims the run)
interrupted    -> cancelled             (admin cancel of an orphaned run)
```

For a synchronous LLM-route invocation, `running -> succeeded` additionally
requires the terminal-output LLM task to finish its protocol stream. A durable
workflow needs no terminal-output task; it succeeds when its declared workflow
output resolves and every required task has reached an accepted terminal state.

Each edge has a defined class of driving source; permission, recovery, and
cancellation transitions are not interchangeable:

- `-> running` from `pending` is driven by the **runner** (sync profile) or the
  **scheduler** (durable profile).
- `-> suspended` and `suspended -> running` are driven **only** by the
  **permission-decision path** (§12). A permission request suspends; either an
  allow or deny decision drives the return, with deny delivered to the backend
  as a refusal result. It does not cancel the run.
- `-> interrupted` is driven by **restart recovery** discovering a durable run
  whose owning process is gone (see startup semantics below). It is not a
  request-time transition.
- `interrupted -> running` is driven **only** by **`requeue` recovery**
  reclaiming the run; with `terminal` recovery, `interrupted` stays terminal.
- `-> cancelled` from any state is driven by **caller disconnect** (sync
  profile) or an **explicit request/admin cancel**, and it cascades to every
  unfinished task run (see §11). Gateway or scheduler shutdown is ownership
  loss for a durable run, not business cancellation: the live run/task is
  marked `interrupted` when shutdown can persist the transition, or is found as
  stale `running` by the next startup scan.

`suspended` is valid only for a durable run whose invocation policy allows
human-in-the-loop suspension. `interrupted` means the gateway lost ownership of
an in-flight durable run, for example after restart. A permission decision
resumes `suspended -> running`. Recovery with `requeue` reclaims the run through
`interrupted -> running`; with `terminal`, `interrupted` remains terminal. A
persisted suspended run stays `suspended` across restart and resumes only
through its permission decision; it is never converted to `interrupted`.

**Recovery startup semantics.** Recovery is a deterministic startup pass, not a
lazy on-access check. At gateway startup, after the durable run/task store is
loaded, the runner scans every persisted durable run whose status is `running`
and reclassifies it:

- a run whose owning process is no longer live (always true after a cold start,
  and true after a lease expiry once multi-process operation lands in W3) is
  moved `running -> interrupted`;
- a run already in `suspended` is left untouched (it resumes only via its
  permission decision, not via recovery);
- a run already `interrupted` or terminal is left as-is.

Then, for definitions with `durable.recovery = requeue`, the runner re-queues
every task run in `running` or `interrupted` through `interrupted -> ready` as a
new attempt (§5.3) and moves the parent run `interrupted -> running` once its
first re-queued task becomes ready. Succeeded task runs are never touched
(recovery invariant 1). For `durable.recovery = terminal`, the scan leaves
`interrupted` runs terminal for operator inspection. The scan runs to
completion before the scheduler begins firing new runs, so a recovered run is
not raced by a fresh schedule.

**Recovery policy.** Recovery is a property of the durable profile on the
`WorkflowDefinition` (`durable.recovery`), not of the per-call invocation
policy — the invocation policy is gone when the caller disappears, so it
cannot own what happens after a restart. The durable profile names one of:

- `requeue` (default): on restart the gateway reclaims `interrupted` runs,
  re-queues any `interrupted` task run, and resumes.
- `terminal`: an `interrupted` run is left in `interrupted` as a terminal
  state for operator inspection; nothing auto-resumes.

Two invariants govern recovery safety:

1. **Succeeded tasks are never re-executed.** A task run already in
   `succeeded` keeps its committed output; recovery only re-queues task runs
   in `running`/`interrupted`. Re-running a finished MCP or agent call would
   repeat side effects the operator already paid for.
2. **A re-queued task increments its attempt number but keeps its logical
   execution key.** The idempotency key derives from the stable logical task
   identity `(run_id, task_id, fan_out_index)`, not from the attempt. A
   downstream service that completed the lost attempt can therefore recognize
   the recovery call as the same operation. The runner does **not** assume the
   lost attempt had no effect; a task handler that cannot propagate or enforce
   that stable identity for a side-effecting operation must reject `requeue` at
   validation and force `terminal` recovery for that definition.

Recovery never applies to synchronous runs (they cannot be `interrupted`; when
the caller disappears the whole run is cancelled). Validation rejects
`requeue` when **any** reachable task cannot safely repeat an uncertain attempt
under the stable logical execution key; such a definition must use `terminal`.
`suspended` runs resume through the permission decision path (§12), not through
recovery.

### 5.3 Workflow Task

A `WorkflowTask` is one DAG node. Common fields are:

```json
{
  "id": "analyze",
  "type": "mcp",
  "depends_on": [],
  "condition": {
    "type": "input_has_media",
    "input": { "ref": "run.input" },
    "media_type": "image"
  },
  "timeout_seconds": 60,
  "retry": {
    "max_attempts": 2,
    "backoff": "exponential"
  },
  "failure_policy": "fail_workflow"
}
```

Rules:

- task ids are unique inside one definition;
- `depends_on` must reference tasks in the same definition;
- the graph must be acyclic;
- a task becomes ready only after all dependencies reach an accepted terminal
  state;
- default failure policy is `fail_workflow`;
- retry is opt-in and applies only to errors classified retryable by the task
  handler;
- a `condition`, when present, names a registered, typed predicate; the initial
  set is `input_has_media` and `output_not_empty`, and an unknown condition type
  or invalid predicate parameter is rejected at validation (see §15);
- skipped tasks do not execute and expose a typed skipped result to dependent
  join/transform tasks.

Task run states are:

```text
pending        -> ready                 (all dependencies reached an accepted terminal state)
ready          -> running               (runner dispatched it under max_concurrency)
running        -> succeeded             (handler returned a terminal result)
running        -> failed                (handler returned an error not classified retryable, or retries exhausted)
running        -> cancelled             (run cancelled — cascaded from the run's cancel)
running        -> suspended             (handler requested permission suspend, durable run only)
running        -> interrupted           (durable task lost its live handler, e.g. restart)
pending/ready  -> skipped               (condition predicate evaluated false)
pending/ready  -> cancelled             (run cancelled before dispatch)
suspended      -> running               (allow/deny decision resumes the same attempt — NOT recovery)
suspended      -> cancelled             (explicit request/admin cancel)
interrupted    -> ready                 (requeue recovery re-queues as a new attempt)
interrupted    -> cancelled             (explicit admin cancel of the parent run)
```

Driver sources mirror the run-level machine: `suspended -> running` is the
permission-decision path only; `interrupted -> ready` is `requeue` recovery
only; every `-> cancelled` edge is the run-cancel cascade from §11 (a task run
is never cancelled independently of its run).

`interrupted` is the task-run counterpart of a durable run losing ownership
(for example after restart): an in-flight task run left without a live handler
is marked `interrupted`, and the run's recovery policy decides whether it is
re-queued through `interrupted -> ready` as a new attempt or remains terminal.
A permission decision resumes the same attempt through `suspended -> running`.
Neither `suspended` nor `interrupted` is reachable in a synchronous run.

### 5.4 Invocation Policy

The caller selects an invocation policy; it is not a different workflow type.

```json
{
  "mode": "synchronous",
  "persistence": "none",
  "allow_suspend": false,
  "max_concurrency": 4,
  "timeout_seconds": 120
}
```

Supported modes:

| Property | Synchronous | Durable |
|---|---|---|
| Lifetime | Bound to caller context | Independent gateway-owned run |
| State store | In-memory | Persistent |
| Background continuation | No | Yes |
| Scheduling | No | Yes |
| Suspend/resume | No | Policy-controlled |
| Restart recovery | Not applicable | Required |
| Primary use | LLM route execution | Agent work and workflows |

Request cancellation immediately cancels every active task in a synchronous
run. A synchronous run never continues after its HTTP caller disappears.

## 6. Task Types

### 6.1 LLM Task

An `llm` task performs exactly one model operation:

```json
{
  "id": "complete",
  "type": "llm",
  "target": {
    "llm_route_id": "coding-model"
  },
  "input": {
    "request": { "ref": "tasks.rewrite.output" },
    "stream": { "value": true }
  },
  "terminal_output": true
}
```

The handler resolves the referenced LLM route in-process and invokes a
`RoutedProvider`. It must not call a raw provider because route resolution is
the authority for:

- logical model selection;
- model capability validation;
- credential scheduling;
- candidate fallback;
- concrete upstream model rewriting;
- usage attribution.

An LLM route may also supply its own target as an invocation binding:

```json
{
  "target": { "binding": "ingress.llm_target" }
}
```

This is useful for a protocol ingress route that retains its existing
`target_policy` but delegates execution to a workflow. The binding is present
only when the workflow is invoked by an LLM route. Definition validation must
declare required bindings, and run creation fails before starting tasks when a
binding is absent.

Only one task may own the protocol response stream of an LLM-route invocation.
It is the task marked `terminal_output: true`, which must be an `llm` task
(see §8 for how the runner routes the caller's `ResponseStreamer` to it). Earlier LLM
tasks are collected as ordinary task outputs. For an LLM-route invocation this
task must also be the graph's unique sink: it has no dependents, every other
task is its transitive dependency, the workflow output references its output,
and it is not a `for_each` task. Consequently all preprocessing has reached a
successful or accepted skipped state before response bytes can be committed.

A terminal-output task must not carry a `condition`. A skipped task executes
nothing and owns no response stream, so a conditional terminal task could
leave the LLM-route invocation with no task writing the client response — and
because `skipped` is an accepted terminal state, the `failure_policy` would
not catch it. Validation rejects a terminal-output task that declares a
condition (§15).

### 6.2 MCP Task

An `mcp` task invokes one tool on one gateway-managed MCP service:

```json
{
  "id": "analyze-image",
  "type": "mcp",
  "target": {
    "service_id": "zhipu-vision",
    "tool": "analyze_image"
  },
  "input": {
    "arguments": { "ref": "tasks.prepare-images.output.items" }
  }
}
```

The handler calls `pkg/mcp/service.Manager` directly. It does not issue a
request through an MCP ingress route. Service enablement, discovery, session
reuse, authentication, audit configuration, and runtime inspection remain
owned by the MCP service layer.

A task may declare bounded `for_each` fan-out. The runner creates child task
runs with stable indexes, enforces the run concurrency limit, and returns
results in input order rather than completion order. In a durable run, a
`for_each` task persists as one parent task run plus one child task run per
item, each keyed by a stable `(task_id, index)`; the parent's terminal state
aggregates its children.

Requested fan-out is also bounded by the target handler's concurrency
capability. The current MCP service manager reuses one transport session per
service, and its stdio transport is not safe for concurrent calls, so the
initial MCP task handler serializes calls to one stdio service. Parallel MCP
fan-out requires either a service process/session pool or a concurrency-safe
transport with unique request ids and one response dispatcher. Serial fan-out
means the wall-clock cost is the *sum* of the per-item calls, not the maximum:
N items at `max_concurrency = 1` cost N × (per-call latency). A synchronous
LLM-route ingress profile that fans out serially must set its
`timeout_seconds` to cover that sum, and the definition should bound the item
count — not just per-item and aggregate byte limits — so a pathological input
cannot push preprocessing latency past the client's own timeout and turn
client-disconnect cancellation into the common path.

### 6.3 Agent Task

This section defines the **execution model** of an `agent` task: its task-type
contract, target resolution, and how the runner drives it. The **product
semantics** of an Agent Task — what an Agent is, its runtime types, resources,
policy, and attribution — live in
[Agents Control Plane](agents-control-plane.md); an `agent` task is one task
type in this runtime, not a separate object or state machine.

An `agent` task submits external work to one first-class Agent:

```json
{
  "id": "implement",
  "type": "agent",
  "target": {
    "agent_id": "coding-agent"
  },
  "input": {
    "prompt": { "ref": "run.input.prompt" },
    "context": { "ref": "tasks.review.output" }
  }
}
```

The task resolves the Agent and invokes the turn-first
`runtimeapi.Backend` selected by `agent.runtime.type`:

- `acp` starts an ACP turn from the Agent-owned runtime config;
- `http` dispatches the conventional HTTP task contract;
- `builtin` runs the in-process ADK host.

The task handler owns no reasoning loop. It adapts the typed task input to a
`runtimeapi.TurnRequest`, invokes `ServeTurn`, collects the ordered common
events, and maps the terminal result. The Workflow Runner remains responsible
for durable state, retry, the stable logical execution key, scheduling, and
permission suspension; the backend owns native turn execution and advertises
which cancellation/permission/session capabilities it supports. The unified
turn contract and delivery sequence are defined in
[Unified Agent Runtime and Routing](../plans/unified-agent-runtime.md).
The stable logical execution/idempotency key travels in the trusted
internal-only `TurnOptions.Execution` block. It is not serialized into or
accepted from AgentRoute JSON; runtime-specific options use the separate
versioned v1 runtime envelope.

A one-off Agent Task is represented by an inline or stored workflow containing
one `agent` task. Schedules target workflow definitions and create durable
runs.

### 6.4 Transform Task

A `transform` task performs deterministic gateway-local data conversion:

```json
{
  "id": "rewrite",
  "type": "transform",
  "target": {
    "handler": "replace_images_with_analysis"
  },
  "input": {
    "request": { "ref": "run.input" },
    "analysis": { "ref": "tasks.analyze-images.output" }
  }
}
```

Transform handlers are compiled-in and registered by name. Validation rejects
an unknown handler. Stored definitions cannot contain executable source,
filesystem paths to load as code, or arbitrary expressions.

Transforms cover protocol normalization, artifact preparation, deterministic
mapping, and join operations that do not belong to LLM, MCP, or agent
runtimes.

## 7. Typed Data And Artifacts

Tasks exchange typed values through named ports and explicit `BoundValue`
objects rather than field-name conventions or an unrestricted expression
language.

Initial source references are:

```text
run.input
run.input.<path>
run.bindings.<name>
tasks.<task-id>.output
tasks.<task-id>.output.<path>
```

Inside a `for_each` task's per-item scope, two additional references are valid
and are rejected everywhere else:

```text
item
item.<path>
index
```

`<path>` is one or more dot-separated schema field or port segments. It may
traverse typed object output (for example `run.input.prompt` or
`tasks.prepare-images.output.images`) but is not a general JSONPath: array
indexing, wildcards, filters, and map keys containing dots are unsupported in
the initial grammar. Each traversed segment must exist in the source schema
where the shape is statically known.

A reference that resolves to a **typed array** may be consumed anywhere the
consumer's published schema accepts that array type: a task input port, a typed
condition operand such as `output_not_empty`, a `for_each.collection`, or the
workflow output. Only `for_each.collection` establishes per-item scope: the
runner binds each array element, in input order, to `item` (and its ordinal to
`index`). Passing an array to an ordinary task input passes the array as one
value and never implies iteration. Definition validation rejects an array only
when the destination schema is incompatible, not merely because the value is
an array.

Task ids use the reference-safe
`[A-Za-z0-9][A-Za-z0-9_-]*` form so the boundary before `.output` is
unambiguous. `<name>` is one complete declared binding id (including dots in a
name such as `ingress.llm_target`); binding subpaths are not supported
initially.

Every task input port, condition operand, `for_each` collection, and workflow
output is a `BoundValue` with exactly one discriminator:

```text
BoundValue :=
  { "ref": SourceReference }
  { "value": JSONLiteral }
  { "object": map<string, BoundValue> }
  { "array": list<BoundValue> }
```

Examples:

```yaml
input:
  request:
    ref: tasks.rewrite.output
  stream:
    value: true
  arguments:
    object:
      image_source:
        ref: item.local_path
      detail:
        value: high
```

Only the value of an explicit `ref` discriminator is interpreted as a source
reference. Literal payload fields may therefore be named `from`, `source`,
`ref`, or end in `_from` without changing their meaning:

```yaml
input:
  metadata:
    value:
      ref: this-is-literal-user-data
      copied_from: another-literal
```

A reference value is never an expression, template, or code — only a path in
the grammar above.

Each task handler publishes an input/output schema. Definition validation
checks source existence and compatible port types where known, and rejects a
`BoundValue` with zero or multiple discriminators, a reference outside this
grammar, or an `item`/`index` reference used in a non-`for_each` task. Runtime
validation remains fail-closed for dynamically shaped JSON.

Binary media is represented by an `ArtifactRef`, not embedded repeatedly in
task JSON:

```json
{
  "id": "art_...",
  "media_type": "image/png",
  "size_bytes": 12345,
  "sha256": "...",
  "scope": "run"
}
```

For synchronous runs, the artifact store is request-scoped memory plus
permission-restricted temporary files when a local MCP process requires a path.
All files are removed when the run finishes or is cancelled.

For durable runs, a durable artifact backend is required before binary
artifacts may outlive the process. The first durable implementation may reject
binary artifacts rather than silently persisting host-local temporary paths.

## 8. Runner

Package direction:

```text
pkg/workflow
  - definitions and validation
  - run/task state machines
  - DAG scheduler
  - task handler SPI
  - event and artifact interfaces

pkg/agent
  -> pkg/workflow
  - runtimeapi.Backend registry and native runtime adapters

pkg/dispatcher
  -> a narrow WorkflowRunner interface

assembly (task handlers live here, not in pkg/workflow)
  - LLM task handler    -> depends on pkg/gateway (builds a RoutedProvider)
  - agent task handler  -> depends on pkg/agent (calls runtimeapi.Backend)
  - MCP task handler    -> depends on pkg/mcp/service
  - transform handlers  -> compiled-in, depend only on pkg/workflow types
  - owns durable store and scheduler lifecycle
```

`pkg/workflow` must not import `pkg/agent`, `pkg/mcp`, `pkg/llm`, `pkg/gateway`,
or protocol handlers. Task execution is inverted through an SPI, and the
concrete handlers that *do* touch those packages live in the assembly layer
(registered into the runner at bootstrap, the same way providers register into
the provider registry). The LLM task handler is one of those: it needs
`AgentGateway.NewRoutedProvider` to resolve a route into a credentialed,
fallback-aware provider, so it depends on `pkg/gateway` — exactly as the agent
task handler depends on `pkg/agent`. The runner itself stays neutral.

```go
type TaskHandler interface {
    Type() TaskType
    Validate(context.Context, TaskDefinition) error
    Execute(context.Context, TaskRequest, TaskEventSink) (TaskResult, error)
    Cancel(context.Context, TaskHandle) error
}
```

Exactly one task may stream its result directly to a caller-owned response
target instead of buffering a `TaskResult`. The definition marks it with
`terminal_output: true`, and the invocation policy may supply a
`ResponseStreamer`:

```go
// ResponseStreamer adapts a provider's eino message/responses stream to the
// wire format the caller speaks. It is constructed by the dispatcher from the
// ingress protocol, never by pkg/workflow. The runner hands it to the
// terminal-output task opaquely; pkg/workflow imports only the interface, not
// any protocol type.
type ResponseStreamer interface {
    // Protocol identifies the wire shape (openai-chat / openai-responses /
    // anthropic / cc). The LLM task handler uses it to pick which provider
    // streaming method to call (StreamChat vs StreamResponses) and to validate
    // it matches the request's RequestType before any bytes are written.
    Protocol() ResponseProtocol

    // Stream consumes a provider stream and writes protocol-native SSE/JSON
    // bytes to the caller's response writer, flushing incrementally. It owns
    // the entire byte pipeline; the task handler never touches the raw writer.
    // It returns the resolved output metadata (concrete model, usage) that the
    // task surfaces through its TaskResult.
    Stream(ctx context.Context, providerStream any) (TaskResult, error)
}

type TaskRequest struct {
    // ... typed inputs, correlation ids, cancellation ...

    // ResponseStreamer is non-nil only for the task marked terminal_output,
    // and only when the caller supplied a streamer in the invocation policy.
    // Every other task returns its result through TaskResult.
    ResponseStreamer ResponseStreamer
}
```

This resolves the protocol-knowledge split that the current codebase has: today
the conversion from an eino `schema.StreamReader[*schema.Message]` (or a
Responses event stream) to wire bytes lives inside each protocol handler
(`pkg/dispatcher/llmapi/{openai,anthropic,cc}`), which directly holds the
`http.ResponseWriter`. The terminal-output task needs exactly that conversion
knowledge, but the runner must stay protocol-neutral. The seam is therefore a
**streamer object the dispatcher constructs from the live ingress protocol**,
not an opaque bag the handler introspects.

The flow is:

1. The dispatcher, knowing the matched route's protocol, builds a
   `ResponseStreamer` whose `Stream` method closes over the concrete protocol
   serializer (the same code path that today's `serveStream` /
   `writeProviderResponsesStream` uses) and the `http.ResponseWriter` with its
   flusher.
2. It passes the streamer into the invocation policy. The runner gives it only
   to the task marked `terminal_output`.
3. The LLM task handler resolves the route into a `RoutedProvider`, calls
   `StreamChat` or `StreamResponses` based on `streamer.Protocol()` and the
   request's `RequestType`, and hands the resulting provider stream to
   `streamer.Stream`. The handler validates the protocol/request-type match
   *before* any bytes are written; a mismatch fails with headers uncommitted.
4. `Stream` returns the `TaskResult` (concrete model, usage attribution); the
   response bytes themselves flow through the streamer, never through the
   buffered result.

The runner remains neutral: it decides *which* task receives the streamer, never
*how* the streamer serializes. The rule that no task but the terminal-output
task may write the client response is preserved, and the protocol serialization
stays owned by the protocol layer (it is merely re-entrant through the streamer
seam rather than through the handler's own `ServeHTTP`).

The Agent task adapter lives above the neutral runner and may depend on
`pkg/agent`. This preserves the control-plane dependency rule: lower protocol
packages never import `pkg/agent`.

The runner:

1. pins and validates the workflow revision;
2. validates invocation policy and required bindings;
3. creates a run context and artifact scope;
4. marks root tasks ready;
5. executes ready tasks up to `max_concurrency`;
6. records task events and releases dependents;
7. cancels siblings when fail-fast policy triggers;
8. resolves the workflow output;
9. closes artifacts and records the terminal run state.

No task may start a goroutine that outlives the run context unless it is owned
by a durable backend handle tracked by the runner.

## 9. LLM Route Integration

An LLM route gains an optional execution policy:

```json
{
  "execution_policy": {
    "type": "workflow",
    "workflow_id": "zhipu-coding-plan-vision",
    "workflow_revision": "sha256:<definition-content-hash>",
    "timeout_seconds": 120,
    "max_concurrency": 4
  }
}
```

The default remains:

```json
{
  "execution_policy": { "type": "direct" }
}
```

For workflow execution, dispatcher order is:

```text
match route
  -> authenticate VirtualKey and rate-limit
  -> resolve the route's workflow from the in-memory workflow snapshot
  -> protocol handler prepares a normalized request
  -> create synchronous workflow run (resolves required bindings, e.g. ingress.llm_target,
     from this route's target_policy; rejects if a declared binding is absent — §15)
  -> workflow executes resource tasks
  -> terminal-output LLM task streams through the protocol handler
```

The LLM route binding profile is deliberately restricted:

- mode is always synchronous and non-durable;
- the run cannot suspend;
- the workflow definition resolves from an in-memory manager snapshot on the
  ingress path by `(workflow_id, workflow_revision)`, never a per-request
  config-store read; workflow manager
  create/update/delete/refresh keep that snapshot populated, mirroring how route
  matching and provider resolution already avoid the config-store hot path;
- the graph must have exactly one task marked `terminal_output: true`, and it
  must be an `llm` task;
- that task must be the unique DAG sink, must not use `for_each`, must be the
  source of the workflow output, and every other task must be its transitive
  dependency;
- the graph must finish within the route timeout;
- request cancellation cancels the run;
- internal task events are observed but are not emitted as unknown OpenAI,
  Anthropic, or CC wire events;
- the terminal-output task is the only task allowed to write the client
  response.

The protocol handler must expose a normalized request/media view and a
`ResponseStreamer` (§8) constructed for its ingress protocol. Workflow code
must not parse protocol-specific HTTP bodies or hold the raw
`http.ResponseWriter`.

## 10. Durable Runs, Scheduling, And Handoff

Durable workflow definitions and schedules are control-plane objects:

```text
workflows
workflow_schedules
```

Runs and task runs are high-churn operational state:

```text
workflow_runs
workflow_task_runs
workflow_events
```

They should use dedicated indexed tables rather than generic configuration JSON
stores. Definitions may participate in gateway bundle apply/export/validate;
runs and schedules do not belong in configuration bundles.

Suggested APIs:

```text
GET    /admin/workflows
POST   /admin/workflows
GET    /admin/workflows/{id}
PUT    /admin/workflows/{id}
DELETE /admin/workflows/{id}

POST   /admin/workflows/{id}/runs
GET    /admin/workflow-runs
GET    /admin/workflow-runs/{run_id}
POST   /admin/workflow-runs/{run_id}/cancel
POST   /admin/workflow-runs/{run_id}/resume

GET    /admin/workflow-schedules
POST   /admin/workflow-schedules
PUT    /admin/workflow-schedules/{id}
DELETE /admin/workflow-schedules/{id}
```

Agent-filtered views query runs/task runs by `agent_id`; they are not a second
task API or state machine.

The first scheduler assumes one active gateway process per database. Before
supporting multiple processes, durable run claiming must use a database lease
with owner id, expiry, and compare-and-swap renewal. **Why this matters:** with
the single-process assumption violated and no lease, two gateway processes
would both read the same schedule/`interrupted` run and each drive it —
double-firing agent turns, double-charging budgets, and double-recording usage.
The lease is therefore a correctness requirement, not a performance one, and
the single-process assumption in W2/W3 exists precisely to defer it. Schedules
and `requeue` recovery must refuse to run (rather than best-effort fire) if the
lease store is unavailable, so a misconfiguration fails closed instead of
double-firing.

## 11. Cancellation, Retry, And Idempotency

- The run context is the root cancellation authority.
- Cancelling a run cancels all active task handlers and prevents new tasks
  from becoming ready.
- **Run cancellation cascades to every non-terminal task run.** When a run
  enters `cancelled`, the runner walks every unfinished task run (`pending`,
  `ready`, `running`, `suspended`, or `interrupted`) and moves it to
  `cancelled`, so the run and its task runs reach a consistent terminal set and
  every open child span is finished. Cancelling a suspended task also removes
  its pending permission correlation. An `interrupted` task is preserved only
  while its parent run remains `interrupted` for recovery or inspection; an
  explicit admin cancel of that parent moves both to `cancelled`. Without this
  sweep a sync run cancelled mid-`for_each` would leave
  queued-but-never-dispatched children unaccounted for, and the §13 span tree
  would show unfinished tasks.
- An Agent task delegates live native cancellation through the backend's
  optional `runtimeapi.RunCanceller`; unsupported modes fail closed.
- An MCP task cancels its tool call context.
- An LLM task closes its stream and cancels the provider context.
- A Transform task must honor context cancellation during bounded work.

### 11.1 Task Error To HTTP Status (Synchronous Ingress Profile)

A task handler returns a typed error, not an HTTP status. For the **synchronous
LLM-route ingress profile only**, the runner maps a failed task's error to the
HTTP status written to the client response (before any stream bytes are
committed, since preprocessing tasks finish before the terminal-output task
owns the stream — §8). This mapping exists precisely so that a `transform` task
rejecting an oversized image or an `mcp` task timing out can surface as the
protocol-correct status an LLM client expects (e.g. `400` / `413` / `502` /
`504`), as assumed by
[zhipu-vision §13](zhipu-vision-model-routing.md#13-failure-and-retry-semantics).
Durable and administrative runs have no HTTP caller and map the same errors to
run/task `failed` status plus an `error_type` dimension; they do not use this
table.

The handler signals status through a small, closed error classification, not an
arbitrary status code on the error value:

| Handler classification | Example | Sync ingress HTTP status |
|---|---|---|
| `client_invalid_input` | unsupported media type, malformed base64, too many images | `400` |
| `client_payload_too_large` | per-image or aggregate decoded byte limit exceeded | `413` |
| `client_policy` | remote URL fetching disallowed by route/media policy | `400` |
| `upstream_unavailable` | MCP service disabled or missing | `503` |
| `upstream_timeout` | task `timeout_seconds` exceeded | `504` |
| `upstream_failure` | MCP/tool failure, agent backend failure | `502` |
| `internal` | rewrite left image content; anything not classifiable | `500` |
| (handler returns retryable transient error, retries not exhausted) | transport blip | mapped after retries are exhausted, per the row above the error resolves to |

Rules:

- The classification is **closed**: a handler returns one of these or the runner
  defaults to `internal` / `500`. No handler may emit an arbitrary status.
- The LLM task handler does **not** use this table — a terminal-output LLM
  failure keeps the route's existing provider status mapping (it owns the
  client stream), and a non-terminal LLM task failure is `upstream_failure`.
- The mapping applies once, at the point the run fails; the runner does not
  synthesize a partial-success response.
- Partial success is intentionally invisible to the client: in a serial
  `for_each` where the Nth item fails after items 1..N-1 succeeded, the client
  sees only the mapped failure status. The successful items' usage events are
  still written to the usage tables with their correct `error_type = ""` and
  remain queryable through the metrics/Admin surfaces for debugging; only the
  client response omits them. This is a deliberate fail-closed choice, not a
  bug, and should be documented to operators on the run-detail view.

Retry is task-local. The runner never retries a whole workflow implicitly.
Retryability is decided by the handler, not by configuration: an MCP or Agent
task retries only failures the handler classifies as transport or transient
service errors, and never a successful-but-invalid result. A handler that
cannot make its side effects safe for re-execution rejects retry configuration
at validation. The runner exposes bounds and backoff only; it never reclassifies
a handler's terminal error as retryable.

Each task run has a stable logical execution identity. For an ordinary task it
derives from `(run_id, task_id)`; for a `for_each` child it additionally includes
the stable child index. The downstream idempotency key derives from that
identity and is reused across configured retries and `requeue` recovery. Each
actual execution still increments an attempt number for events, backoff, and
operator inspection, but the attempt number is never part of the idempotency
key. This is what lets a downstream service deduplicate a call whose previous
result was lost. A side-effecting handler that cannot propagate or enforce the
stable key must reject retry configuration and `requeue` recovery, forcing
`terminal` recovery for that definition.

## 12. Permissions

Permission behavior is an invocation-policy concern:

- synchronous LLM-route workflows set `allow_suspend = false`; an Agent or MCP
  operation that requires interactive approval fails closed;
- durable workflows may suspend the task and run, persist the permission
  correlation, and resume through an admin decision;
- both allow and deny decisions resume the suspended task: allow permits the
  operation, while deny is delivered to the backend as a refusal result so the
  agent may continue. Only an explicit request/admin cancel transitions the run
  and task to `cancelled`;
- auto-approved operations continue normally but remain auditable.

The workflow runtime does not invent a new permission schema. Agent tasks adapt
ACP/builtin permission events, and future MCP permission gating uses the shared
gateway permission vocabulary.

## 13. Observability

Every workflow run has a `run_id`. Every task attempt opens a child interaction
span:

```text
workflow run
├── transform: prepare-images
├── mcp: analyze-image[0]
├── mcp: analyze-image[1]
├── transform: rewrite-request
└── llm: completion
```

Task handlers preserve:

- trace, span, and parent span ids;
- workflow id, run id, task id, and attempt;
- route id and VirtualKey id for an ingress run;
- target agent id for an Agent task;
- MCP service/tool or LLM route/model dimensions.

LLM, MCP, ACP, and builtin usage events remain the source of resource usage.
Workflow events record orchestration lifecycle and must not duplicate token or
tool usage totals.

## 14. Governance And Authorization

Every run has an immutable invocation principal and origin:

```text
principal:
  - ingress VirtualKey
  - authenticated administrator
  - scheduler
  - parent Workflow Run

origin:
  - route id
  - schedule id
  - workflow run id
  - optional origin agent id
```

An LLM-route invocation is authorized by the route's VirtualKey and rate-limit
policy. The Workflow Definition itself is the authorization boundary for the
resources it composes: it may reference a resource only by static id or by a
declared invocation binding such as `ingress.llm_target`, never dynamically.
Binding a definition to a route is the explicit administrative grant that lets
that route's traffic use exactly the definition's statically declared resources
and nothing else.

The binding pins a deterministic workflow revision (the SHA-256 of the
canonical definition). Updating a Workflow Definition creates a new revision
but does not move existing route bindings. Adopting a revision whose resource
manifest changed requires updating and revalidating every affected ingress
route. A bundle that defines both the workflow and route may omit the literal
hash in source YAML only if bundle validation computes it and persists/exports
the expanded pinned revision.

Apply validation resolves the complete referenced graph for the pinned
revision — every LLM route, MCP service/tool, Agent, Transform handler, and
declared binding — and rejects the bundle if any reference is missing,
disabled, or supplied by anything other than a static id or a declared
binding. The route carries no second resource allowlist; the immutable pinned
revision's static resource manifest is that list.

A durable administrative run uses the admin surface's authentication. A
schedule pins its configured workflow id and revision and snapshots its input
and execution identity; it never stores or interpolates raw credentials into
task input.

Agent tasks additionally enforce:

- the target Agent exists, is enabled, and owns one valid runtime;
- the workflow may target that Agent;
- resources used inside the Agent remain governed by the Agent's own
  resources/routes and runtime policy;
- `agent_depth` increments across Agent Task and workflow handoff boundaries;
- `max_agent_depth`, budget, and quota reject work before dispatch;
- `origin_agent_id` and target `agent_id` are retained for handoff audit.

**Depth and budget across process boundaries.** Today `agent_depth` is a
single-process mechanism: the dispatcher increments an `X-Agent-Depth` header
on each agent/workflow hop and a process-local gate rejects over-depth
requests. That holds for synchronous runs, which live in one process for one
request. It does **not** hold for durable runs once W2/W3 land:

- a durable run that suspends or is recovered by the scheduler after a restart
  has no live `X-Agent-Depth` chain — the header was on an HTTP request that
  no longer exists;
- a scheduled agent task starts in the scheduler's process with no ingress
  header at all;
- multi-process operation (§10) means the child agent task may run in a
  different gateway process than the parent workflow.

So before W2 enables durable agent tasks and handoffs, `agent_depth` and budget
must be reconstructed from **persisted run state** — the parent run's depth and
budget ledger stamped into the durable run record, not from a transport header.
The depth a child agent task sees is `parent_run.depth + 1`, read from the run
record at dispatch; the same applies to budget/quota consumption. This is a W2
open question, tracked here rather than silently inherited from the in-process
gate.

Task definitions may reference resource ids and invocation bindings, never
credential ids, API keys, authorization headers, or environment values.
Handlers obtain credentials from the existing credential/service/runtime
managers.

Nested Workflow Tasks are not supported initially. If they are added later,
the runner must propagate the principal, trace, budget, cancellation, and depth
and reject direct or indirect workflow cycles.

## 15. Validation And Safety

Validation runs at two distinct times. **Definition validation** runs whenever a
definition is stored or exported (Admin API create/update, bundle apply/export)
and is independent of how the definition will later be invoked. **Binding
validation** runs when an LLM ingress route is bound to a workflow revision
(`execution_policy.type = workflow`), because the terminal-output/sink
constraints are a property of the *synchronous ingress profile*, not of the
definition itself — a multi-`agent`-task durable DAG is a legal definition that
has no terminal-output task at all. Keeping these two checks separate is what
lets one definition serve both durable and (when bound) ingress use without a
durable DAG being rejected at store time for lacking a stream sink.

Definition validation (always, fail-closed):

- unique slash-free workflow ids and reference-safe task ids matching
  `[A-Za-z0-9][A-Za-z0-9_-]*`;
- known task and transform handler types;
- known condition predicate types with valid typed parameters;
- acyclic dependencies;
- valid task targets;
- source references match the §7 grammar and point only to dependencies, run
  input/bindings, or (inside a `for_each` task) `item`/`index`;
- `ingress.*` bindings referenced by a task are *declared* (the definition
  names them in a `required_bindings` list), but their *resolution* — whether
  the bound route's target policy is valid for the terminal LLM task — is **not**
  checked at definition-store time, because the binding resolves against a route
  that may not exist yet (see apply-order note below);
- bounded task count, fan-out, concurrency, timeout, and retries;
- no recursive workflow invocation in the first version;
- no route-to-workflow-to-same-route cycle;
- disabled or missing referenced resources reject apply/run;
- durable definitions using unsupported ephemeral artifacts reject run.

Binding validation (only when an LLM ingress route pins a workflow revision,
checked at bundle apply and again at run creation against the in-memory
snapshot):

- the pinned revision exists and is enabled;
- exactly one task is marked `terminal_output: true`, it is an `llm` task, it
  is the unique non-`for_each` DAG sink, it depends transitively on every other
  task, it supplies the workflow output, and it carries no `condition` (a
  skipped terminal task would own no response stream);
- every `required_bindings` entry (e.g. `ingress.llm_target`) is actually
  supplied by the invocation policy the dispatcher builds for this route, and
  the bound route's `target_policy` resolves to a valid credentialed
  `RoutedProvider` with capabilities matching the terminal LLM task's
  `stream`/tools requirements after the image rewrite;
- the route carries no second resource allowlist; the pinned revision's static
  resource manifest is that list.

Media transforms additionally enforce MIME allowlists, decoded size limits,
total request limits, permission-restricted temporary files, cleanup, and
remote-URL policy. A remote URL must not be fetched by the gateway without
explicit SSRF controls.

W0/W1 bundle validation resolves the complete prospective object graph before
writing, then applies the acyclic W1 dependencies in order:

```text
providers and MCP services
  -> resource LLM routes referenced by static llm_route_id targets
  -> Workflow Definitions (declared ingress bindings are not resolved yet)
  -> ingress routes that pin Workflow revisions (binding resolution becomes possible)
```

An `ingress.llm_target` declaration is a late-bound contract, not a reference to
one concrete route and therefore not a definition↔route cycle. Definition
validation checks only that the binding is declared and used with a compatible
task target. When an ingress route later pins the revision, binding validation
checks that this route can supply the contract from its own `target_policy`;
run creation repeats that check against the in-memory snapshots (§9). Static
`llm_route_id` task targets are different: they name concrete resource routes,
so those routes must be applied before the Workflow Definition as shown above.

Agent tasks are not executable in W0/W1, so a definition containing one is
rejected rather than creating an early cross-object cycle.

W2 introduces valid management-reference cycles (for example, an Agent lists an
LLM resource route in `routes.llm_route_ids`, that LLM route binds a workflow,
and the workflow contains an `agent` task targeting the same Agent). This is
independent of the unified AgentRoute ingress relationship, which points only
from AgentRoute to Agent. The current `agwctl apply` sequence of independent
Admin API mutations and the current `ConfigStoreBackend` interface cannot
atomically commit such a prospective snapshot. Before W2 enables Agent tasks,
it must add a server-side transactional/staged bundle-apply operation with:

1. complete prospective-snapshot validation;
2. one atomic commit across affected config object families, or documented
   rollback that leaves no partial visible snapshot;
3. manager refresh only after the commit succeeds;
4. one failure result returned to `agwctl`, which no longer simulates this
   transaction with per-object calls.

This operation is required before enabling even a one-task Agent workflow: one
`agent` task is already enough to complete the reference cycle above. Once the
operation exists, it is also sufficient for definitions containing multiple
Agent tasks. Any W2 limit to one `agent` task is therefore a deliberate product
rollout boundary for the first durable backend integration, not a
config-store-safety property; W3 removes that product validation when handoff
and multi-agent management surfaces ship.

Workflow Definitions are stored in a `workflows` config store and participate
in config-store-backed bundle apply/export/validate. Caddyfile routes and
standalone `--static-config` do not support Workflow Definitions initially.
High-churn run/task/event state is never included in a gateway bundle.

The store and bundle treat `(id, revision)` as the definition identity. Source
bundle entries may omit the computed revision; validation canonicalizes the
payload and fills it before persistence. Export includes every revision
referenced by an exported route or schedule, even when it is not the latest
revision for that workflow id, so an exported route never points at a missing
definition.

## 16. Implementation Plan

Building this runtime is committed as the shared home for the P2/P3 control
plane; the synchronous LLM-route profile (W1) is its first consumer, not its
sole justification. The runtime is staged so that the Zhipu vision workflow can
exercise it end-to-end as early as possible: W0a delivers a runnable in-memory
DAG over Admin-API-created definitions, W0b adds config-store persistence and
bundle apply, and W1 wires the LLM-route ingress profile. W0 is split (rather
than landed as one batch) precisely so the generic machinery is validated by
real synchronous traffic before durable runs, scheduling, and agent tasks are
built on top of it. Treat the vision workflow as validation of the runtime, not
as a shortcut around it.

### W0a: In-Memory Runner And Handlers

- add `pkg/workflow` definition, definition-level validation, run/task state
  machines, DAG scheduler, task SPI, in-memory run state, and event sink;
- implement bounded static DAG scheduling, cancellation, and the run-cancel
  cascade (§11);
- implement registered Transform and MCP task handlers, including per-service
  serialization for the current reused stdio MCP transport;
- add request-scoped artifact handling;
- add a Workflow Definition Admin API (`/admin/workflows` create/update) backed
  by an in-memory manager (definitions are not yet persisted across restart);
- add the task-error → HTTP-status classification for the sync ingress profile
  (§11.1), even though no ingress route consumes it until W1 — it is a
  handler contract the MCP/Transform handlers must honor from day one.

W0a is independently testable: a definition created through the Admin API can
be run synchronously through a test harness, exercising DAG scheduling,
fan-out, cancellation, artifacts, and the task SPI without any ingress route,
config store, or bundle apply.

### W0b: Config Store And Bundle Apply

- add the `workflows` config store and an in-memory-snapshot-backed manager
  refreshed from it (no per-request config-store reads on any ingress path);
- add Workflow Definition bundle apply/export/validate in the acyclic W0/W1
  dependency order described in §15;
- definitions now survive restart and participate in gateway bundles; the
  Admin API create/update path from W0a becomes a thin wrapper over the store.

### W1: LLM Route Workflow Profile

- add `execution_policy` to LLM routes;
- add normalized protocol request/media hooks and the `ResponseStreamer` seam
  (§8);
- implement the LLM task handler over `RoutedProvider`;
- support one terminal protocol-native stream;
- implement binding validation (§15) at route-bind and run-creation time;
- implement the Zhipu Coding Plan vision workflow as the first end-to-end
  consumer.

### W2: Durable Runs And Agent Tasks

- require the common Agent runtime contracts, registry, identities, events, and
  ACP/builtin turn adapters from
  [Unified Agent Runtime M0–M3](../plans/unified-agent-runtime.md#8-delivery-sequence);
- add the server-side transactional/staged bundle apply operation from §15
  before enabling Agent tasks and their valid cross-object cycles;
- add indexed run/task/event persistence;
- add Workflow Definition and Run Admin APIs;
- add the Agent task handler over `runtimeapi.Backend`, propagating the stable
  logical execution key through `TurnOptions` and rejecting retry/`requeue`
  when the backend cannot enforce it;
- add durable cancellation, recovery (startup scan + requeue/terminal, §5.2),
  admin APIs, and one-step Agent workflows;
  the one-`agent`-task definition limit is an explicit W2 product scope after
  staged apply is available, not a workaround for reference cycles.

### W3: Schedules And Multi-Agent Workflows

- add schedule definitions and the shared scheduler loop;
- add durable suspend/resume and permission correlation;
- lift the W2 one-`agent`-task product limit and add handoff patterns, topology
  view, and agent-filtered run views;
- add database leasing before supporting multiple active gateway processes.

### 16.1 Builtin Agent Durable Dependency Gate (Authoritative)

The builtin runtime, the Workflow Runtime, and eino persistence are referenced
mutually across three design docs. The authoritative prerequisites for
**builtin agents becoming durable Workflow Agent Tasks** are defined here and
the other docs link to them:

```text
W2 durable Workflow Runs ───────────────┐
                                        ├──> PB2 builtin durable Workflow integration
eino v0.10 checkpoint/persistence ──────┘
```

Concretely:

1. **W2** delivers durable Workflow Runs and the Agent task handler over the
   already-established `runtimeapi.Backend` registry. The ACP and builtin
   turn adapters are runtime-foundation prerequisites, not PB2 work.
2. **eino v0.10** must stabilize a checkpoint/persistence surface before the
   builtin adapter can adopt Runner-managed durable sessions. Hand-rolling
   durable agent state before that is explicitly forbidden
   (builtin-agent-runtime.md §11). This is an external, upstream gate, not a
   gateway work item, and it may become available independently of W2.
3. **PB2** adds builtin durable Workflow session/checkpoint integration after
   both 1 and 2. It does not add the turn-first builtin backend adapter, which
   already serves current/unified Agent turns. W2 and eino v0.10 are parallel
   prerequisites; neither depends on the other.

PB2 is blocked by whichever prerequisite lands last.
`builtin-agent-runtime.md` §11 and `agents-control-plane.md` §5.7/§PB2
reference this gate rather than re-stating a conflicting order.

## 17. Rejected Alternatives

### Separate `input_processors`

A dedicated request processor pipeline would duplicate DAG execution,
cancellation, retry, and observability. Route-facing processor configuration is
unnecessary once an LLM route can invoke a synchronous workflow.

### Separate AgentTask State Machine

It would duplicate WorkflowRun lifecycle and make schedules/handoffs cross two
execution systems. A one-step workflow covers the same product operation.

### Provider-Local MCP Calls

Providers own model wire behavior, not tool orchestration. Provider-local MCP
calls bypass route governance and couple one provider to the MCP runtime.

### Implicit Tool Injection

Adding an MCP tool schema to an LLM request does not execute it. Intercepting the
resulting tool call would create a hidden agent loop and break protocol
transparency.

### Reusing Builtin Agent Topologies

Builtin topologies are reasoning graphs inside one Agent and are owned by eino
ADK. Gateway workflows coordinate first-class resources and agents outside that
reasoning loop.
