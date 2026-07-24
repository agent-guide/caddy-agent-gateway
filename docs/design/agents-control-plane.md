# Agents Control Plane

## 1. Purpose

This document defines the product and technical direction for first-class
`agents` support in `agent-gateway`.

The project should not evolve into an agent framework that owns an agent's
internal reasoning loop. Instead, `agent-gateway` should provide the external
control plane for agents:

- manage agent identities and workspaces
- store each agent's runtime-specific configuration and dispatch it through a
  runtime backend
- govern the LLM and MCP resources an agent can use
- observe sessions, transcripts, permissions, usage, and call chains
- coordinate agent-level workflow tasks, policies, scheduling, and future
  handoffs through the shared workflow runtime

This is the layer that turns the existing LLM, MCP, ACP, and metrics surfaces
from separate protocol gateways into one agent gateway.

## 2. Product Positioning

The primary user problem is not "proxy one protocol." The primary user problem
is:

> I need to register agents, expose them safely, assign their resources, monitor
> what they are doing, resolve human approvals, and coordinate work across
> agents and tools.

The repository already contains the protocol-level building blocks:

- LLM routes and providers for model access
- MCP services and routes for tool access
- ACP runtime/process management for agent execution
- VirtualKeys, credentials, and CLI auth for access control
- metrics and interactions for usage, latency, errors, and call-chain traces

`pkg/agent` should be the product layer that composes these building blocks.
It should not replace `pkg/llm`, `pkg/mcp`, or `pkg/acp`; it should organize
them around agent management and orchestration.

The implemented P0/P1 ACP model still binds an Agent to a separately persisted
ACP service. The target breaking cutover in
[Unified Agent Runtime and Routing](../plans/unified-agent-runtime.md) removes
that product concept: `Agent.runtime.acp` owns the ACP execution config,
`agent_id` owns the runtime pool, and no `service_id` or `acp_services` family
remains. Implementation-status sections retain the old names only to describe
the current tree and migration source accurately.

## 3. Explicit Non-Goal

`agent-gateway` should not own general-purpose internal agent orchestration.

Remove the current `pkg/llm/agent` package rather than expanding it. That
package represents an LLM-native internal tool loop, including provider calls,
memory retrieval, tool execution, and iteration control. Those concerns are
better handled by dedicated agent frameworks and agent runtimes.

The gateway does, however, support an implemented **builtin runtime**
(see [5.7](#57-builtin-runtime-adk-hosted-agents)): agents hosted inside the
gateway process, built on eino ADK. This does not conflict with the non-goal,
because the gateway still does not implement a reasoning loop of its own — the
loop, interrupt/resume machinery, and multi-agent topologies are delegated
wholesale to eino ADK as building material, while the gateway owns exactly what
it owns for every other runtime: definition, lifecycle, governance, resources,
and observation. The builtin runtime is a third `runtime.type` behind the same
`agents` model; it must not reshape that model and must not block the external
control-plane roadmap.

This direction supersedes the earlier roadmap note in
`docs/architecture/architecture-overview.md` that described agent orchestration
as "an execution mode rather than a separate external service." The control
plane is the external service; there is no internal LLM-native orchestration
mode planned.

## 4. Architecture Boundary

### 4.1 Current Protocol Layers

```text
pkg/llm
  - providers
  - credentials
  - model catalog
  - LLM protocol adapters

pkg/mcp
  - MCP service config
  - discovery and execution
  - runtime request inspection

pkg/acp
  - ACP runtime config
  - ACP route handling
  - codex/opencode runtime process management
  - sessions, transcript replay, permissions, pooled instances

internal/observability
  - LLM/MCP/ACP usage events
  - interaction traces
  - Prometheus counters
```

### 4.2 New Agent Layer

```text
pkg/agent
  - agent management model
  - runtime-specific Agent config
  - agent config store manager
  - agent workspace aggregation
  - agent policies
  - task and scheduling model
  - orchestration metadata
  - observability aggregation
```

`pkg/agent` depends on the lower-level runtime managers and query services. The
lower-level protocol packages must not depend on `pkg/agent`.

```text
pkg/agent
  -> pkg/acp/runtime
  -> pkg/gateway/agentroute + pkg/gateway/llmroute + pkg/gateway/mcproute
  -> pkg/llm/provider + pkg/llm/credentialmgr + pkg/gateway/modelcatalog
  -> pkg/mcp/service + pkg/mcp/runtime
  -> pkg/gateway/virtualkey
  -> internal/observability/usage
```

## 5. Core Concepts

### 5.1 Agent

An `Agent` is a first-class management object. It represents an operator-facing
agent identity, not a protocol-specific service.

Target shape after the unified-runtime cutover:

```json
{
  "id": "coding-agent",
  "name": "Coding Agent",
  "description": "Codex-backed development agent",
  "runtime": {
    "type": "acp",
    "acp": {
      "agent_type": "codex",
      "cwd": "/workspace",
      "allowed_roots": ["/workspace"],
      "default_model": "gpt-5",
      "permission_mode": "interactive",
      "idle_ttl": "30m",
      "max_instances": 4,
      "codex": {
        "mode": "adapter",
        "adapter_command": "codex-acp"
      }
    }
  },
  "routes": {
    "llm_route_ids": [],
    "mcp_route_ids": []
  },
  "resources": {
    "provider_ids": ["openrouter-main"],
    "mcp_service_ids": ["filesystem-tools"],
    "virtual_key_ids": ["coding-agent-key"]
  },
  "policy": {
    "max_agent_depth": 3,
    "budget": {
      "max_turns_per_day": 500,
      "max_tokens_per_day": 2000000
    }
  },
  "disabled": false,
  "created_at": "2026-06-20T00:00:00Z",
  "updated_at": "2026-06-20T00:00:00Z"
}
```

The current P0/P1 stored shape instead contains
`runtime.acp.service_id`, `routes.acp_route_ids`, and optionally
`routes.builtin_route_ids`; those are migration-source fields, not the target
schema.

P0 initially implemented `runtime.type = "acp"`, but the model defines three
built-in runtime types, split by a single axis — **who owns the agent's
lifecycle, and whether there is a separate process at all**:

- `acp`: the **gateway** owns the lifecycle of an **external process**
  (process pool, sessions, permission flow, transcript). For local,
  embeddable agent executables the gateway should drive directly. A bespoke
  executable that needs this depth should be wrapped to speak ACP (the same
  way `codex-acp` bridges codex), not given a new runtime type.
- `http`: the **agent service** owns its own lifecycle; the gateway is only a
  client that hands it a task and observes the result. For business agents that
  expose a network endpoint and consume LLM/MCP through `resources`.
- `builtin` (implemented runtime, summarized in [5.7](#57-builtin-runtime-adk-hosted-agents)):
  there is **no separate process**. The agent is a persisted definition
  materialized inside the gateway process by an eino-ADK-based host. The
  gateway owns the agent's entire existence, not just a process around it.

A `runtime.type = "http"` agent carries an `http` block instead of `acp`:

```json
"runtime": {
  "type": "http",
  "http": {
    "endpoint": "https://agents.internal/coding-agent",
    "auth_ref": "agent-callback-key"
  }
}
```

A `runtime.type = "builtin"` agent carries a `builtin` block holding the agent
definition itself (model binding, prompt, tools, topology — see
[Builtin Agent Runtime](builtin-agent-runtime.md#4-definition-schema) for the
full schema). Like ACP and HTTP, its runtime-specific definition is Agent-owned;
none of the target runtime types introduces a second management identity.

Crucially, **LLM and MCP are resources, not runtime types**. An agent's ability
to use models and tools lives in `resources` (and is governed there), regardless
of runtime. The runtime field only describes how the gateway dispatches work and
observes it. There is intentionally no "native non-ACP external-process
lifecycle" runtime: for a separate executable, that need collapses into `acp`
(wrap it) or `http` (don't manage its lifecycle). `builtin` is not that case —
it has no executable at all. See [5.4](#54-runtime-backends) for the executor
contract and the SPI escape hatch.

#### Generic policy vs runtime-specific config

Keep `policy` for cross-runtime governance only. Fields whose meaning depends on
the runtime backend belong under `runtime.<type>`, not under `policy`:

- `policy` holds runtime-agnostic governance: `max_agent_depth`, `budget`, and
  later schedule enablement, retention, and transcript visibility.
- `runtime.acp` owns `agent_type`, cwd/allowed roots, default model, environment,
  config overrides, pool limits, permission mode, and agent-specific adapter
  config.
- `runtime.http` owns endpoint/auth/timeouts.
- `runtime.builtin` owns the in-process definition.

The Agent's top-level `id`, `name`, `description`, `disabled`, and timestamps
remain generic identity/lifecycle metadata and are not repeated inside
`runtime.acp`. Conversely, ACP execution fields do not move into `policy`.
This leaves one source of truth without erasing the runtime-specific schema.

The current `pkg/acp/service.ServiceConfig` is the migration source for the ACP
fields above. At the breaking cutover, reusable normalization and validation
move to a protocol-owned `acp.RuntimeConfig` without service management fields.
The Agent adapter converts `Agent.runtime.acp` to that type; `pkg/acp` does not
import `pkg/agent`.

#### Source of truth: `runtime` vs `routes`

`runtime` is authoritative for execution. For an ACP-backed Agent,
`runtime.acp` is the complete execution configuration and `agent_id` is the
owner key passed separately to the ACP runtime manager. The target AgentRoute
cutover in
[Unified Agent Runtime and Routing §6.2](../plans/unified-agent-runtime.md#62-ownership-is-one-way)
uses one ownership direction: `AgentRoute.agent_id` targets the Agent. There is
no persisted reverse `agent_route_ids` list. Workspace views derive ingress
routes from the in-memory AgentRoute snapshot. `routes.llm_route_ids` and
`routes.mcp_route_ids` remain because they describe resources the Agent may use,
not ingress ownership.

AgentRoute management validation requires the target Agent to exist, but does
not require it to be enabled or currently executable. Disabled Agents and HTTP
Agents whose backend has not shipped may be configured in advance; capability
and workspace views expose that state, while dispatch fails with
`agent_disabled` or `runtime_not_executable` before backend invocation.

In the current pre-unification tree, `runtime.acp.service_id` plus
`routes.acp_route_ids`/`routes.builtin_route_ids` implement this indirectly.
They are removed together with ACPRoute/BuiltinRoute and the ACP service store.

#### Cardinality: one agent, one runtime

An `Agent` selects **exactly one** runtime backend type and owns one runtime
configuration block: `runtime.acp`, `runtime.http`, or `runtime.builtin`. It is
not a fan-out container over several backends. AgentRoute ingress resolves this
one Agent and its one runtime; it is not a way to aggregate multiple runtimes
under one identity.

This is deliberate, for three reasons:

- **Attribution stays unique.** AgentRoute supplies `agent_id` directly and the
  runtime manager carries it through native events.
- **The agent does not become an internal orchestrator.** Selecting among or
  dispatching across several real backends is internal-loop behavior, which is an
  explicit non-goal (see [3](#3-explicit-non-goal)).
- **Lifecycle semantics stay clean.** An ACP session is pinned to an instance in
  the Agent-owned pool and cannot be freely moved across Agent identities.

Therefore:

- **Multiple real agents** are modeled as multiple `Agent` objects. Coordinating
  them is a layer *above* agents — an `agent` Workflow Task runs one agent and a
  Workflow DAG coordinates A→B handoff or a larger graph. The workflow is a
  separate gateway-owned object that *references* several agents; it does not
  live inside any one agent. The authoritative execution model is
  [Unified Workflow Runtime](workflow-runtime.md).
- **One logical agent over interchangeable backends** (failover / load balance /
  A-B) is a different need and is out of scope here; see Open Questions.

#### Cardinality: one Agent, one runtime owner

`agent_id` is the sole owner key for ACP pools, instances, sessions, pending
permissions, transcripts, native recovery operations, and usage. There is no
second shareable runtime object and therefore no service-to-Agent cardinality
rule or ambiguous shared-service mode. Multiple Agents may carry identical ACP
configuration values, but they still materialize isolated pools keyed by their
different Agent IDs.

An ACP config update changes the runtime fingerprint: in-flight work drains,
old instances accept no new turns, and subsequent turns use the new config.
Deleting the Agent or changing away from `runtime.type = acp` retires its pool
and fails pending work closed, so no runtime survives without an owning Agent.

#### Identity and resource enforcement

In P0/P1 an `Agent` is a management-plane grouping, not a data-plane principal.
End-user requests still authenticate with a `VirtualKey` against a route; the
agent object does not appear in the request path. Therefore:

- `resources` starts as a *management view* of what the agent is allowed to use,
  assembled and validated at the admin layer. It is not enforced inline on the
  data-plane request path in P0/P1.
- Data-plane enforcement continues to come from VirtualKey + route policy.
  Binding a `VirtualKey` to an agent means the operator has scoped that key to
  the agent's routes/services; the gateway does not introduce a separate
  per-request "agent principal" check yet.
- A dedicated agent-as-principal model (where the request path resolves an agent
  identity and enforces `resources` directly) is deferred until there is a
  concrete isolation requirement; see Open Questions.

The target unified `AgentRoute` does resolve `agent_id` on the request path for
runtime selection, attribution, common policy, and capabilities. That cutover
does not by itself make all external ACP/HTTP `resources` references enforced
entitlements; scoped callback identity and resource enforcement remain a
separate milestone in the unified runtime plan.

### 5.2 Agent Workspace

An `AgentWorkspace` is a read model for the UI. It aggregates the things an
operator needs on one agent detail page.

It is not a stored object. It is assembled from:

- the `Agent` object
- the Agent-owned runtime config
- AgentRoutes that target the Agent
- runtime pooled instances and in-flight turns keyed by `agent_id`
- pending ACP permissions
- session and transcript **references** (counts + links), not full content
- LLM/MCP resources linked by policy
- metrics events and interaction traces filtered by
  agent/route/run/session/trace

The workspace is a **summary/index**, not a content aggregator. It returns
summaries, counts, runtime state, and links/references that let the frontend call
the dedicated Agent capability endpoints (`GET /<agent-route>/sessions`,
`GET /<agent-route>/sessions/{id}/transcript`) when the operator drills in. It must
not eagerly pull session transcripts: doing so would make one workspace call
unbounded in size and would entangle pagination, permissions, and performance
into a single endpoint. Transcripts and full session lists stay behind their own
paginated endpoints; the workspace only points at them.

The list above is the target `acp` runtime view. Current P0 assembles the
analogous data by reading through the bound service and ACPRoute; the cutover
removes that join. The workspace is keyed off `runtime.type`: an `http`-runtime
agent has no gateway-owned pooled instances, sessions, transcripts, or ACP
permissions, so its workspace degrades to the runtime-agnostic parts (the
Agent object, linked resources, tasks, and metrics/interaction traces). Do not
hard-code ACP fields as required in the workspace shape.

### 5.3 Agent Task

This section defines the **product semantics** of an Agent Task only. Its
**execution model** — the Workflow Run/Task Run state machines, invocation
policies, scheduling, retry, and task-type definitions — lives in
[Unified Workflow Runtime](workflow-runtime.md). An Agent Task is not a
separate object, state machine, or API family; it is a Workflow Task whose
`type` is `agent`, so this document does not duplicate that machinery.

In product terms, an Agent Task is an external unit of work owned by the
gateway: run this agent on a prompt now, resume a session with new input,
schedule maintenance, or hand off from agent A to agent B. It is never the
agent's internal plan. Execution dispatches through a runtime backend
([5.4](#54-runtime-backends)), selected by the agent's `runtime.type`. A
one-off "run this agent" operation is a one-task Workflow Run (defaulting to
durable, per [workflow-runtime §1](workflow-runtime.md#1-status)); schedules
target Workflow Definitions and create durable runs.

### 5.4 Runtime Backends

A runtime backend is the turn-first seam between a stable Agent identity and
one native execution runtime. It is selected by `agent.runtime.type` and is
shared by every caller: the current runtime-specific routes during migration,
the target unified `AgentRoute`, and the Workflow Runtime's `agent` task. The
authoritative implementation sequence is
[Unified Agent Runtime and Routing](../plans/unified-agent-runtime.md).

The required contract lives in `pkg/agent/runtimeapi`:

```go
type Backend interface {
    RuntimeType() string
    Capabilities(context.Context, Agent) (Capabilities, error)
    ServeTurn(context.Context, Agent, TurnRequest, EventSink) error
}
```

`TurnRequest.Options` is not a flat union of backend fields. It uses the
versioned `v1` envelope defined by the unified-runtime plan: northbound input
contains an optional strict `runtime` JSON object, while trusted gateway-only
execution metadata is carried separately and is never decoded from AgentRoute
JSON. The selected backend strictly decodes its runtime object and rejects
unknown or foreign options with `unsupported_option`.

Optional capabilities are narrow interfaces such as `SessionLister`,
`TranscriptLoader`, `PermissionResolver`, `RunCanceller`, `RuntimeInspector`,
and `HealthChecker`. Unsupported capabilities fail closed; a backend never
silently emulates them or falls through to another runtime.

There is deliberately no task-first `StartTask` SPI. The Workflow `agent` task
handler adapts its typed input to `TurnRequest`, invokes `ServeTurn`, consumes
the common event envelope, and maps the terminal result into its Task Run. The
Workflow Runner remains the sole owner of durable state, scheduling, retry,
idempotency, permission suspension, and handoff. The backend owns one Agent
turn and its native capability behavior.

#### Runtime categories and adapters

The classification axis is **who owns the agent's lifecycle, and whether a
separate process exists**, which yields three Agent runtime categories. ACP and
builtin execution are implemented today behind runtime-specific dispatch;
their `runtimeapi.Backend` adapters land before the AgentRoute cutover. `http`
remains a defined runtime shape whose executable adapter lands only after its
wire/auth contract is implemented.

- **`acp`** — the gateway owns the agent's external process lifecycle. Its
  adapter translates the Agent-owned `runtime.acp` block into
  `acp.RuntimeConfig` and invokes the pool with `agent_id` as owner, reusing
  sessions, scope rebind, permission flow, and transcript. A turn ending does
  not tear down the process; the pool governs it by `IdleTTL`.
- **`http`** — the agent service owns its lifecycle. Its future adapter
  dispatches to `runtime.http.endpoint` over the versioned HTTP Agent contract.
  A remote stateful agent still fits here; its session is an id passed over
  HTTP, not a process owned by the gateway.
- **`builtin`** — there is no separate process. The adapter invokes the
  in-process ADK host (see [5.7](#57-builtin-runtime-adk-hosted-agents)),
  materializing or reusing the definition graph and translating Runner events
  into the common envelope.

#### No bespoke external-process backend

There is intentionally no bespoke "native, non-ACP, gateway-managed lifecycle"
backend **for external executables**. That combination is contradictory: needing
gateway-managed lifecycle for a separate process *is* what ACP is for, so the
answer is to wrap as ACP rather than reinvent it. Concretely:

- needs gateway-managed lifecycle for an executable (pool/sessions/permission)
  → wrap as `acp`
- does not need it (remote / self-managed / stateless) → `http`
- is not an executable at all, but a declarative definition the gateway can
  host → `builtin`

#### SPI extension point

The backend registry rejects duplicate runtime types at startup. An Agent whose
runtime backend is not linked remains manageable but is not executable and
fails with `runtime_not_executable`. A later runtime category can add another
adapter behind this registry without adding another route family or changing
the Workflow Runner.

#### Executor contract

Every backend must accept the runtime-neutral turn identity, emit the ordered
common event envelope, report exactly one terminal result, and expose
capabilities honestly. Opting into Workflow retry or `requeue` recovery
additionally requires the adapter to propagate or enforce the stable logical
execution key from
[workflow-runtime §11](workflow-runtime.md#11-cancellation-retry-and-idempotency).
Cancellation, permission, session, transcript, health, and inspection behavior
are exposed only when the corresponding optional capability is implemented.

The common run registry owns exact-run control identity. Active entries hold
backend cancellation bindings; completed entries become process-local,
10-minute terminal tombstones capped at 1,024 per Agent. Repeated cancellation
of a retained terminal run returns its terminal result without re-invoking the
backend. Durable Workflow state replaces this process-local history only for
Workflow-owned runs.

The common permission broker owns pending identity, expiry, atomic claim, and
audit. A broker record contains an unguessable opaque backend token; ACP waiter
state and builtin checkpoint/calls/transcript/trace state stay in backend-owned
stores and are resolved through the selected adapter. Decision, expiry,
cancellation, Agent deletion/runtime switch, adapter failure, and process
shutdown consume the common claim once and clean up fail-closed. A backend
store is never an independently claimable permission registry.

ACP advertises `resume_mode=active_stream` and delivers a claimed decision to
its live waiter. Builtin advertises `resume_mode=new_stream`: an Admin decision
stores a validated, decided continuation without running it, and a later
AgentRoute `POST /turn` consumes it while owning the continuation SSE stream.
A decision submitted on builtin `POST /turn` claims and consumes in that same
request. The Admin endpoint never starts a builtin continuation in the
background because there is no durable event sink before Workflow W2/M10.

During M2-M4 only, a legacy ACPRoute whose service is not bound to exactly one
Agent remains on the pre-unification native ACP path because it has no truthful
Agent identity for `Backend.ServeTurn`. It receives no synthetic `agent_id` and
no Agent-scoped controls. M5 rejects such migration input and removes this
temporary exception.

### 5.5 Agent Policy

Agent policy is external governance. It should control the resources and
operator boundaries around the agent, not the internal reasoning algorithm.

Runtime-agnostic policy areas (live under `policy`):

- max agent depth
- budget and quota
- schedule enablement
- retention and transcript visibility

Runtime-specific config areas (for `acp`, owned by `runtime.acp`; see
[5.1](#51-agent)):

- permission mode and approval routing
- cwd and allowed roots for ACP-backed agents

These are surfaced directly in the workspace subject to secret redaction and
updated only through Agent CRUD. The ACP runtime manager receives a validated
protocol-owned copy and never becomes a second configuration authority.

Resource-scoping references (live under `resources` and `routes`):

- exposed routes
- allowed VirtualKeys
- allowed MCP services and tools
- allowed LLM providers and models

See [5.1](#51-agent) for why runtime-specific governance is kept out of the
generic `policy` block.

### 5.6 Agent Attribution

P1 observability ("usage for this agent", "activity for this agent") needs a
reliable way to map durable usage/interaction events back to an Agent. The
implemented metrics tables carry a nullable `agent_id` alongside the legacy
route/service/session/trace dimensions; pre-P1 rows may leave it empty.

**Decision: stamp `agent_id` at write time, starting in P1.** Usage events are
append-only history and cannot be backfilled, so the durable attribution tag
must exist from the moment agents exist. Deferring it to P2 would permanently
leave P1-era events without reliable per-agent attribution. Concretely:

1. **Schema-additive tag (P1).** Add an optional, nullable `agent_id` to the
   three usage event models (`llm_usage_events`, `mcp_usage_events`,
   `acp_usage_events`). This is additive — existing rows and non-agent traffic
   simply leave it empty.

2. **Hot-path stamping (P1).** The dispatcher / ACP runtime stamps `agent_id`
   when the originating route or service resolves to exactly one agent. That
   resolution must use an in-memory route/service → agent index owned by the
   agent manager and kept current on agent create/update/delete — never a
   per-request config-store read, consistent with the existing provider/route
   hot-path rule.

   **Dependency direction.** `pkg/acp`, `pkg/mcp`, and `pkg/llm` must not import
   `pkg/agent` to do this — that would reverse the layering in [4.2](#42-new-agent-layer).
   Instead the shared gateway runtime holds a small resolver interface that
   `pkg/agent` *implements* and the lower layers only *consume*:

   ```text
   AgentAttributor:
     ResolveAgentID(routeID, serviceID, sessionID) -> (agentID, ok)
   ```

   The dispatcher and the metrics usage observer take an optional
   `AgentAttributor` (no-op when agents are absent) and stamp the returned id onto
   the usage event/span at write time. The interface lives in a neutral package
   the lower layers already depend on (alongside the usage observer seam), so the
   arrows still point `pkg/agent -> runtime`, never the reverse. When no attributor
   is wired or it returns `ok = false`, the field is simply left empty.

3. **Stamp only when unambiguous.** If the originating route/service maps to zero
   or more than one agent, leave `agent_id` empty rather than guess. An ambiguous
   mapping is precisely the signal that the deployment has outgrown unique
   service/route → agent binding.

4. **Query layer prefers the tag, falls back to mapping.** Per-agent usage and
   activity prefer the `agent_id` filter and fall back to service/route/session
   mapping for events with no tag (pre-P1 events, non-agent traffic later
   reassigned, or the ambiguous case). The fallback's ambiguity caveat is
   surfaced in the response/UI, not hidden.

Items 2-4 describe the implemented pre-unification fallback. After the
AgentRoute/service-removal cutover, Agent ingress stamps `agent_id` directly and
new ACP runtime events carry their owner `agent_id`; active queries no longer
infer ownership from `service_id`. Any retained SQL service column is
historical-only.

`origin_agent_id` for cross-agent handoff is deferred to P2/P3, because there is
nothing to stamp until handoff exists. It is added the same additive way when
handoff ships.

### 5.7 Builtin Runtime (ADK-Hosted Agents)

Status: PB1 and PB1b implemented; PB2 deferred.

Builtin is the third runtime behind the shared `Agent` model. It has no
separate executable: the gateway materializes a persisted
`runtime.builtin` definition into an eino ADK object graph and executes it
in-process. This remains consistent with the external-control-plane position
because the gateway owns definition, lifecycle, governance, resources, and
observation while eino ADK owns the reasoning loop and orchestration
primitives.

The control-plane contract is:

- models resolve through LLM routes listed in `routes.llm_route_ids`;
- tools resolve through MCP services listed in
  `resources.mcp_service_ids`;
- builtin routes are owned by one agent and target that same agent;
- generic agent policy and attribution rules remain shared with `acp` and
  `http`;
- future `agent` Workflow Tasks use the shared `runtimeapi.Backend` contract rather
  than a builtin-only task model.

After the AgentRoute cutover, the runtime-specific builtin route family is
removed and one `AgentRoute.agent_id` relationship covers builtin, ACP, and
eventually executable HTTP Agents. The current builtin route remains
authoritative until that breaking milestone lands.

The authoritative runtime design—including the definition schema, topology,
middleware order, host and session lifecycle, SSE protocol, observability,
interactive permissions, cancellation, implementation status, and deferred
work—lives in
[**Builtin Agent Runtime**](builtin-agent-runtime.md). The eino adoption
inventory and framework-level tradeoffs remain in
[**Eino Capability Reuse**](eino-reuse.md).

## 6. Admin API Direction

The implemented `/admin/agents` endpoints are the product-level API for agent
management and UI aggregation. The unified-runtime cutover expands them with
AgentRoute and common runtime capabilities while removing service CRUD.

These endpoints are management-plane APIs. They are not the primary data-plane
entrypoint for end-user chat or task execution. Today, end users and business
apps call route-dispatched ACP endpoints such as:

```text
POST /<acp-route>/turn
POST /<acp-route>/permission
GET  /<acp-route>/sessions
GET  /<acp-route>/sessions/{session_id}/transcript
```

After M5 the same operations live under `/<agent-route>/...`; their ACP-specific
optional fields and capabilities continue through the adapter. Likewise,
agents continue to access LLM and MCP resources through the existing LLM API
and MCP route surfaces. `/admin/agents` coordinates and observes those
surfaces; it does not replace them.

### 6.1 P0 Endpoints (Current Pre-unification Implementation)

P0 should make the frontend productive without introducing scheduling or
multi-agent workflows.

- `GET /admin/agents`
- `POST /admin/agents`
- `GET /admin/agents/{id}`
- `PUT /admin/agents/{id}`
- `DELETE /admin/agents/{id}`
- `GET /admin/agents/{id}/workspace`

P0 semantics:

- an agent binds to one pre-existing ACP service
- Agent create/update does not create or mutate the backing service
- route creation can remain explicit at first, but the workspace must list
  matching ACP routes
- the workspace response includes enough references for the frontend to call
  ACP management endpoints when it needs sessions, transcripts, runtime state,
  thread close, or permission resolution
- do not duplicate ACP runtime management actions under `/admin/agents`

P0 lifecycle and ownership semantics:

- the `Agent` does not own its ACP service or routes; it references them. The
  ACP service/route objects remain independently managed under `/admin/acp/...`.
  In P0 a given ACP service is bound by **at most one** agent (see the
  one-runtime-one-agent rule in [5.1](#51-agent)); create/update rejects a
  `service_id` already claimed by another agent.
- deleting an agent deletes only the `Agent` record. It must not cascade-delete
  the backing ACP service or routes. `OwnsService` exists in the current model,
  but no auto-create/provenance write path shipped.

ACP-backed agents should use existing ACP management endpoints for runtime
operations:

- `GET /admin/acp/services/{service_id}/sessions`
- `GET /admin/acp/services/{service_id}/sessions/{session_id}/transcript`
- `GET /admin/acp/runtime`
- `GET /admin/acp/runtime/inflight`
- `DELETE /admin/acp/runtime/threads/{service_id}/{thread_id}`
- `POST /admin/acp/runtime/permissions/{request_id}`

All service/route ownership semantics and endpoints in this subsection describe
the implemented migration source only. M4-M7 of the unified-runtime plan
replace them with:

- Agent CRUD storing ACP config directly under `runtime.acp`;
- AgentRoute CRUD under `/admin/agents/routes`;
- Agent-scoped session, transcript, permission, run, capability, workspace, and
  health APIs;
- `/admin/acp/runtime/...` only for native pool/process diagnosis and recovery,
  keyed by `agent_id`.

There is no `/admin/acp/services`, `service_id`, `OwnsService`, service cascade,
or shared-service mode in the target architecture.

### 6.2 P1 Endpoints

P1 should make the UI an agent console rather than a resource CRUD console.

- `GET /admin/agents/{id}/activity`
- `GET /admin/agents/{id}/usage`
- `GET /admin/agents/{id}/interactions`
- `GET /admin/agents/{id}/resources`
- `PUT /admin/agents/{id}/resources`
- `GET /admin/agents/{id}/health`

P1 semantics:

- activity is assembled from recent ACP events, LLM events, MCP events, and
  pending permissions
- usage is assembled from metrics breakdown and timeseries APIs
- interactions are currently filtered by route, service, session, and trace
  identifiers; after the cutover new ACP events use direct `agent_id`
  attribution and no service dimension
- resources show the LLM providers, routes, MCP services, and VirtualKeys the
  agent can use
- health is shallow at first: disabled state, runtime instances, in-flight
  turns, pending permissions, recent error rate, pipeline health

### 6.3 P2 Endpoints

P2 adds durable agent execution and scheduling through the shared Workflow
Definition/Run surface. The endpoint list, run/task state machines, and
invocation contracts are authoritative in
[Unified Workflow Runtime §10](workflow-runtime.md#10-durable-runs-scheduling-and-handoff);
they are not repeated here. From the agent control plane's perspective, P2 is
characterized by:

- Workflow Runs and Task Runs are gateway-owned external work items, executed
  through the agent's runtime backend ([5.4](#54-runtime-backends)):
  the Workflow `agent` task invokes the already-proven
  `runtimeapi.Backend`; ACP and builtin adapters arrive during the unified
  runtime foundation, while HTTP becomes executable only after its own backend
  milestone;
- schedules create durable Workflow Runs; they do not become hidden runtime
  loops, and cancellation is gateway-visible and auditable;
- agent-scoped UI views filter Workflow Runs/Task Runs by `agent_id`; they do
  not introduce a second AgentTask API or state machine.

**Why P2 is limited to one `agent` task per durable definition.** This is a
deliberate product rollout boundary, not a runner or config-store limitation:
P2 first proves one durable backend dispatch, cancellation, recovery, and audit
lifecycle before exposing handoff graphs. A definition containing even one
`agent` task can create the valid management-reference cycle described in
[workflow-runtime §15](workflow-runtime.md#15-validation-and-safety) when an
Agent's LLM resource route binds a workflow that targets the same Agent. The
one-way AgentRoute ingress model does not create that cycle, but it does not
remove this LLM-resource cycle either. The transactional staged-apply operation
must therefore land **before any** P2 Agent Task is enabled. Once it lands, it
safely covers both one-step and multi-step definitions. P3 then removes the
one-task product validation when the handoff, topology, and multi-agent
management surfaces are ready.

### 6.4 P3 Endpoints

P3 extends the same P2 surface and Runner to static multi-step DAGs. It adds no
parallel `/admin/agent-workflows` object family. The graph coordinates external
turns, resource tasks (`llm`/`mcp`/`transform`), and handoffs; it is static and
gateway-owned, and never becomes an agent's internal reasoning loop. The
authoritative API, storage, invocation-policy, and task contracts are in
[Unified Workflow Runtime](workflow-runtime.md).

## 7. Backend Implementation Plan

P0/P1 below record the service-backed implementation sequence that has already
landed. They are retained as implementation history, not as the target ACP
model. The breaking M4-M7 replacement is defined in
[Unified Agent Runtime and Routing](../plans/unified-agent-runtime.md#8-delivery-sequence)
and summarized in [§9.4](#94-remove-acp-service-as-a-product-object).

### P0: Agent Resource And ACP-Backed Workspace

P0 splits into two shippable milestones so the frontend can integrate agent
list/detail before the surrounding tooling is finished. P0a is the minimum that
makes an `Agent` a real object; P0b adds the aggregated view and config-object
parity.

**P0a — agent object and CRUD:**

- remove `pkg/llm/agent`
- create `pkg/agent` (`types.go`, `manager.go`, `service.go`)
- add an `agents` config store schema (plugs into the existing
  `pkg/configstore/schema` store-name + `RegisterDefaultStores` pattern, the same
  way `acp_services` is registered)
- implement agent CRUD, including the 1:1 `service_id` uniqueness validation and
  the `acp_route_ids` → runtime-service consistency check
- wire `/admin/agents` CRUD to real handlers

After P0a the frontend can already build the Agents list and a basic detail page
from the stored object plus the existing ACP endpoints.

**P0b — workspace and config-object parity:**

- implement ACP-backed workspace aggregation (`workspace.go`) as the
  summary/index read model in [5.2](#52-agent-workspace)
- wire `GET /admin/agents/{id}/workspace`
- make `agents` a first-class gateway-bundle object (apply/export/validate)
  with a complete `pkg/adminclient` CRUD surface and `agwctl gateway agent`
  CRUD subcommands (create/list/get/update/delete), matching the parity
  `acpServices`/`acpRoutes` already have

Suggested packages:

```text
pkg/agent/
  types.go
  manager.go
  workspace.go
  service.go
```

P0 should avoid deep orchestration. It should provide a stable API for the
frontend to stop treating ACP services as agents.

Declarative-config note: `agents` reference other bundle objects (ACP services,
routes, providers, MCP services, VirtualKeys) by id, so they round-trip through
the same bundle apply/export/validate path as the objects they reference rather
than being admin-API-only — this is required, not optional, because the bundle is
the project's reproducible-config mechanism and an admin-only agent could not be
version-controlled or applied. P0 ships full config-object parity (bundle CRUD +
`adminclient` + `agwctl gateway agent` CRUD). The only deferred CLI surface is
the *read* subcommands that depend on later endpoints — `workspace`, `activity`,
`usage`, `health` — which follow the same phasing as their admin endpoints
(P1+). Apply ordering: because agents are pure references over existing bundle
objects, the apply pass must create/resolve all referenced objects before the
agent, and `validate` must reject an agent with a dangling reference.

### P1: Agent Observability And Resource View

Goals:

- add agent activity feed
- add agent usage summary
- add interaction filtering by agent
- expose linked LLM/MCP/VirtualKey resources
- add shallow health summary

This phase should reuse `internal/observability/usage` and existing managers. It
should not introduce rollup tables unless query performance requires it.

### P2: External Tasks And Scheduling

The Workflow Runner, Definition/Run models, durable persistence, and scheduling
loop are built once in `pkg/workflow` per
[workflow-runtime §16 W2/W3](workflow-runtime.md#16-implementation-plan); this
section lists only what the *agent* control plane contributes on top of that
runner.

Agent-control-plane goals for P2:

- require the common runtime contracts, registry, identities, events, errors,
  and ACP/builtin adapters from
  [Unified Agent Runtime M0–M3](../plans/unified-agent-runtime.md#8-delivery-sequence)
  before durable Agent Tasks consume them
- add the `agent` task handler over `runtimeapi.Backend`, registered into the
  Workflow Runner at bootstrap
- enable a backend for durable retry/recovery only when its capabilities can
  propagate the stable logical execution key; builtin durable
  session/checkpoint recovery additionally waits for PB2
- require the transactional staged-apply operation from
  [workflow-runtime §15](workflow-runtime.md#15-validation-and-safety) before
  enabling any Agent Task
- after staged apply is available, enforce one `agent` task per durable
  definition as the explicit P2 product scope

Agent tasks call `runtimeapi.Backend` through the shared registry. They must not
implement an internal reasoning loop or hard-code ACP/builtin/HTTP behavior.

Scheduler placement constraint (agent-control-plane concern, not a workflow
concern):

- the runtime has two assembly paths, `agw` (Caddy app) and `agwd` (standalone
  daemon). The scheduler loop must be owned by the shared runtime core so both
  paths get identical behavior, and started exactly once per process by the
  assembly layer — not by an HTTP handler or per-request code.
- the design assumes a single active gateway process per config store. Running
  multiple processes against one store is out of scope for P2; if it is needed,
  task claiming must move to a store-backed lease/leader mechanism rather than an
  in-memory loop. P2 should make this assumption explicit rather than silently
  double-firing schedules.

### P3: Multi-Agent Workflow

P3 lifts the P2 one-`agent`-task product limit to static multi-step DAGs. The
transactional staged-apply prerequisite has already landed in P2; P3 adds
handoff and dependency semantics plus topology-view support. The runtime remains
a static external orchestration graph, never a model-authored reasoning loop;
the multi-step DAG mechanics themselves are the Workflow Runner's, already
exercised by the synchronous profile.

### PB: Builtin Runtime Track

PB is independent of the P2/P3 control-plane schedule except where builtin
durable session/checkpoint state integrates with Workflow Agent Tasks:

- **PB0 — bridge adapters:** implemented
  (`pkg/mcp/einotool`, `pkg/llm/provider/einomodel`).
- **PB1 — runtime type, host, routes, ingress, observability, management
  parity, sessions, topologies, and middleware:** implemented.
- **PB1b — interactive MCP tool permissions:** implemented.
- **PB2 — durable Workflow integration and Runner-managed sessions:**
  the turn-first builtin `runtimeapi.Backend` adapter is delivered by the
  unified runtime foundation; PB2 is the additional durable
  session/checkpoint integration, deferred until Workflow W2 and a stable eino
  persistence surface exist. The two prerequisites are stated authoritatively in
  [workflow-runtime §16.1](workflow-runtime.md#161-builtin-agent-durable-dependency-gate-authoritative);
  this document does not restate it.

The detailed scope and implementation notes are authoritative in
[**Builtin Agent Runtime §11–§12**](builtin-agent-runtime.md#11-implementation-track).

## 8. Frontend Iteration Plan

The frontend should shift from protocol-resource navigation to an agent console
without waiting for every backend phase.

### P0 Frontend: Agent List And Workspace

Use:

- `GET /admin/agents`
- `GET /admin/agents/{id}/workspace`
- existing ACP endpoints as fallback until P0 backend is complete

Screens:

- Agents list
- Agent detail page
- tabs: Overview, Runtime, Sessions, Routes, Configuration

The first version should show ACP-backed agents only. The UI should avoid
presenting `acp service` as the primary product concept.

### P1 Frontend: Activity And Observability

Use:

- `GET /admin/agents/{id}/activity`
- `GET /admin/agents/{id}/usage`
- `GET /admin/agents/{id}/interactions`
- `GET /admin/agents/{id}/health`

Screens and widgets:

- recent activity stream
- pending permission banner
- usage cards
- error and latency trend
- call-chain / interaction topology
- resource access panel

### P2/P3 Frontend: Workflows, Runs, And Scheduling

Once the Workflow Runtime ships durable runs (W2) and scheduling (W3), the
agent console adds task/run/schedule surfaces driven by the workflow Admin APIs
([workflow-runtime §10](workflow-runtime.md#10-durable-runs-scheduling-and-handoff))
and filtered by `agent_id`. The screen inventory (task queue, run detail,
schedule editor, workflow graph editor, run timeline, handoff visualization,
per-step logs and usage) belongs to the workflow UI and is not enumerated here;
from the agent console's perspective these are filtered projections of the
shared workflow surfaces, not agent-specific screens.

## 9. Migration Notes

### 9.1 Remove `pkg/llm/agent`

Remove the package and any references. Do not move it into `pkg/agent` unless a
future product requirement explicitly asks for a built-in LLM-native runtime.

### 9.2 Keep Protocol Packages Focused

Do not move `pkg/acp`, `pkg/mcp`, or `pkg/llm` into `pkg/agent`.

The protocol packages remain reusable lower-level subsystems. `pkg/agent`
coordinates them.

### 9.2a Memory Subsystem Is Out Of Scope For P0–P1

`pkg/llm/memory` is a separate half-built subsystem. Removing `pkg/llm/agent`
does not change memory. P0–P1 do not add memory to the agent `resources` model.
If memory becomes an agent-scoped resource later, it joins `resources` as
another referenced backend (for example `memory_store_ids`) under the same
"management view first, enforcement later" rule used for the other resources.
This is tracked in Open Questions, not assumed.

### 9.3 Update Documentation Together

When implementing P0, update:

- `README.md`
- `docs/architecture/architecture-overview.md`
- `docs/reference/admin-api-reference.md`
- `docs/reference/agwctl-reference.md` if CLI commands are added
- frontend docs in `agwmngr`

The docs should describe `agents` as the primary product surface and
LLM/MCP/ACP as resource/runtime layers.

### 9.4 Remove ACP Service As A Product Object

The unified-runtime cutover is deliberately breaking:

- move reusable ACP normalization/validation out of `pkg/acp/service` into an
  identity-free protocol runtime config;
- rename ACP-internal `Service` identifiers that represented that management
  object to runtime/owner terminology;
- copy each bound service's execution fields into its owning
  `Agent.runtime.acp`;
- use `agent_id` as the ACP pool, scope, session, permission, diagnostic, and
  usage owner key;
- remove `service_id`, `OwnsService`, the `acp_services` store/bundle family,
  service CRUD/Admin client/CLI, and service-to-Agent indexes;
- retain `/admin/acp/runtime` only as an Agent-keyed native diagnostic/recovery
  surface.

An unbound legacy service has no implicit target identity. Migration requires
the operator to create an Agent for it or delete it. The new binary rejects old
service-backed shapes with an actionable export/rewrite/apply error rather than
silently inventing Agent IDs.

## 10. Open Questions

The following are still genuinely open. Items the body now takes a position on
(Agent-owned ACP config, resource enforcement, runtime-vs-routes authority,
attribution timing, and bundle/CLI parity) are decided there and are not
repeated here.
- Which dedicated operational schema and retention policy should back
  `workflow_runs`, `workflow_task_runs`, `workflow_events`, and schedules?
  Workflow Definitions may use the generic config-store pattern, but
  high-churn run state must not.
- What is the first budget model: token budget, cost budget, turn budget, or
  all three? And is budget enforced (data-plane) or only observed (management)
  in its first version?
- Should schedules live under agents only, or should there also be global
  schedules that target multiple agents?
- Does multi-process (HA) gateway operation against one config store become a
  requirement? If so, the P2 scheduler needs a store-backed lease/leader
  mechanism rather than the single-process assumption stated in P2.
- If/when memory becomes an agent resource, what is its reference shape and is it
  enforced or observed first? (See 9.2a.)
- Beyond AgentRoute resolving the Agent for ingress, does the Agent gain scoped
  callback credentials that enforce `resources` directly, or does external
  ACP/HTTP resource enforcement stay on VirtualKey + route policy
  indefinitely? (See 5.1.)
- Should one logical agent ever front several interchangeable runtime backends
  (failover, load balance, A-B) — analogous to logical-model routing over
  multiple provider bindings? The body keeps agent:runtime at 1:1 (see 5.1). If
  this is needed, it is a specialized runtime selection policy, clean only for
  stateless `http` backends; ACP session affinity (a session is pinned to one
  service/instance) makes it hard for `acp` and would need an explicit
  session-routing model. This is distinct from coordinating *multiple distinct*
  agents, which is Workflows (P3), not multi-backend.
- Builtin-runtime-specific deferred work and decided questions are tracked in
  [Builtin Agent Runtime §13](builtin-agent-runtime.md#13-open-questions-and-deferred-work),
  not duplicated in this cross-runtime list.

## 11. Implementation Status

P0 (P0a + P0b) and P1 are **implemented**. P2 and P3 remain
design-only. The builtin PB0, PB1, and PB1b implementation status is maintained
in [Builtin Agent Runtime](builtin-agent-runtime.md); PB2 remains deferred.

### 11.1 Landed in P0a — agent object and CRUD

- removed the dead `pkg/llm/agent` package.
- added `pkg/agent` (`types.go`, `manager.go`, `manager_test.go`): the `Agent`
  model with `acp`/`http` runtimes, `Normalize`/`Validate`,
  `DecodeStoredAgentConfig`, and a `Manager` with CRUD plus the in-memory
  route/service → agent index (`Refresh`, `ResolveAgentID`).
- registered the `agents` config store (`StoreAgents` + `AgentSchema`, runtime
  type as the tag) through `RegisterDefaultStores`.
- the manager enforces the P0 rules: one ACP `service_id` is bound by at most one
  agent (uniqueness), and `acp_route_ids` must resolve to the agent's runtime
  service (consistency, via an injected `ACPRouteServiceLookup` adapter so
  `pkg/agent` does not import the route resolver type).
- wired `GET/POST/GET/PUT/DELETE /admin/agents` to real handlers, backed by
  `AgentGateway.AgentManager()`.

### 11.2 Landed in P0b — workspace and config-object parity

- `GET /admin/agents/{id}/workspace` returns the summary/index read model:
  bound ACP service (read-through), matching ACP routes, runtime pooled
  instances / in-flight turns / pending permissions filtered to the service, an
  ACP usage rollup, and links to the dedicated session/transcript endpoints. It
  never pulls transcripts.
- `agents` is a first-class gateway-bundle object: bundle `agents` field with
  validation (id uniqueness, `Validate`, in-bundle one-service-one-agent, and
  dangling-reference checks across every referenced family — providers, MCP
  services, VirtualKeys, LLM/MCP/ACP routes, and the runtime ACP service — plus
  intra-bundle `acp_route_id` → runtime-service consistency), apply (last, after
  referenced objects) and export, the `pkg/adminclient` agent surface, and
  `agwctl gateway agent` subcommands. Each cross-object check is guarded by
  "the referenced family is present in the bundle" so a partial bundle that
  references existing config-store objects still applies.

### 11.3 Landed in P1 — attribution and observability views

- additive nullable `agent_id` on the three usage event models, the SQLite
  tables (CREATE + idempotent `ALTER … ADD COLUMN` for existing DBs), partial
  indexes, the writers, and the query filter allowlists.
- the `AgentAttributor` seam lives in the neutral `internal/observability/usage`
  package (`attribution.go`); `pkg/agent.Manager` implements it structurally, so
  the lower layers never import `pkg/agent`. A settable `AgentAttribution`
  holder is injected after bootstrap
  (`UsageService.Attribution().Set(agentManager)` in the app), and the observer
  stamps `agent_id` at `Begin` from the route id, only when the mapping is
  unambiguous.
- `GET /admin/agents/{id}/{activity,usage,interactions,resources,health}` and
  `PUT …/resources` are wired. Per-agent metric reads use an `AttributionFilter`
  (`agent_id = ? OR route_id IN (…) OR service_id IN (…)`) so they prefer the
  durable `agent_id` tag but fall back to the agent's owned routes/services for
  untagged-but-mappable events (pre-P1 history, or events written before a
  route/service was reassigned to the agent). `GET …/resources` resolves the
  stored id lists into object summaries (provider type, MCP transport, VirtualKey
  tag, route protocol/path, `exists` flag) instead of echoing raw ids.

### 11.4 Decisions taken during implementation

Three points the design left open were resolved as follows; revisit if needed:

- **Auto-created backing service:** not implemented in P0. An agent references a
  pre-existing ACP service; the `OwnsService` provenance flag exists in the model
  but no auto-create/derived-service write path ships yet. (Open Question in §10
  stays open.)
- **Workspace session counts:** the workspace exposes *live* runtime counts
  (pooled instances, in-flight turns, pending permissions) and an ACP usage
  rollup, plus links to `…/sessions` and `…/sessions/{id}/transcript`. It does
  not compute a persisted distinct-session count (that would need a blocking
  agent `session/list` or a dedicated metrics aggregate); the frontend follows
  the links for the authoritative list.
- **Workspace assembly location:** the workspace read model is assembled in the
  admin layer (`pkg/admin/agents.go`), where every subsystem manager is already
  injected, rather than in `pkg/agent/workspace.go`. The result shape is the
  canonical workspace; only the assembly site differs from the §5.2 sketch.

### 11.5 Convention notes

- `agwctl gateway agent` provides `list`/`get`/`delete` plus the P0/P1 read
  surfaces `workspace`/`activity`/`usage`/`interactions`/`resources`/`health`.
  Agent create/update go through `agwctl gateway apply` (the gateway-bundle
  path), the same convention every other config object follows; there is no
  per-object create-from-file CLI. This deliberately supersedes the literal
  "`create`/`update`" subcommand wording in §6.2/P0b: aligning agents with the
  repo-wide read-only-CLI + apply-for-mutation convention was preferred over a
  divergent per-object mutation CLI. The Admin API still exposes
  `POST`/`PUT /admin/agents`, which the bundle apply path uses under the hood.
- attribution is stamped at the dispatcher's single primary span `Begin` via the
  route id, which covers LLM/MCP/ACP route-dispatched traffic. Service-only
  resolution remains available through the `AgentAttributor` interface for future
  call sites.

### 11.6 Review-driven hardening

A post-implementation review tightened the following:

- **Uniqueness under concurrency:** the manager serializes the
  validate → store-write → index-refresh sequence with a dedicated `writeMu`, so
  the one-runtime-one-agent invariant holds even when two creates race on the
  same `service_id`. The List-based uniqueness check was otherwise a
  check-then-write TOCTOU. Covered by `TestCreateConcurrentServiceBindingIsExclusive`
  (run under `-race`).
- **Attribution fallback (§5.6 / §6.2):** per-agent reads no longer filter on
  `agent_id` equality alone; the new `usage.AttributionFilter` OR-matches the
  tag, the agent's owned route ids, and its service ids, so untagged-but-mappable
  events surface. A nil filter means "no attribution filtering"; a non-nil but
  empty filter matches nothing (never widens to all rows).
  - **Service fallback is ACP-only and unambiguous:** only the ACP runtime
    `service_id` (bound by at most one agent under P0 one-runtime-one-agent) is
    used as a service-level fallback, carried in `AttributionFilter.ACPServiceIDs`.
    MCP service resources have no uniqueness constraint — two agents may list the
    same `mcp_service_id` — so attributing untagged MCP usage by service would
    double-count; MCP events are recovered via the agent's owned `mcp_route_ids`
    instead.
  - **The service arm is ACP-scoped in SQL, not a bare `service_id IN (…)`:**
    `acp_services` and `mcp_services` are separate config stores with no global id
    uniqueness, so a same-named MCP service must not be matched by the agent's ACP
    fallback. The query layer therefore emits the service arm only against the ACP
    event table (`acp_usage_events`, already all-ACP) and, in the mixed-protocol
    interactions UNION, guards it as `route_kind = 'acp' AND service_id IN (…)`.
    The MCP/LLM tables get no service arm at all. Route arms stay unconditional
    because route ids are globally unique (single routes store).
  - **Interactions union projects `service_id`:** the
    `llm/mcp/acp` UNION behind `ListInteractions` projects `service_id` (NULL for
    llm rows) and enables the service arm, so `/activity`, `/interactions`, and
    `/health` recover ACP events for an agent that binds only the service and did
    not enumerate `acp_route_ids`. (Write-time `agent_id` stamping still happens at
    span `Begin`, where only the route id is known — the service id arrives later
    via the ACP extension — so the read-side service fallback is what closes this
    gap for service-only agents.)
- **Route-binding uniqueness (§5.1 / §5.6):** uniqueness is no longer limited to
  the ACP `service_id`. Any LLM/MCP/ACP `route_id` is now owned by at most one
  agent, enforced in three places: `Manager.checkRouteUniqueness` on create/update
  (inside the same `writeMu` critical section as the service check), the gateway
  bundle's `validate` (rejecting two agents that bind the same route), and
  `Manager.Refresh`, which drops any `route_id`/`service_id` that resolves to more
  than one agent instead of picking a last writer (so `ResolveAgentID` returns
  `ok=false` for an ambiguous binding). This closes the read-side gap: because
  `AttributionFilter.RouteIDs` is an OR-match, a route shared by two agents would
  otherwise double-attribute its events to both — the §11.6 "route arms stay
  unconditional because route ids are globally unique" reasoning now holds at the
  binding layer, not just the routes store.
- **Bundle reference integrity (§7 declarative-config note):** `validate` now
  rejects dangling agent references across all referenced families and enforces
  intra-bundle ACP route→service consistency, not just the runtime service and
  ACP route ids. The `agwctl gateway apply` path mirrors this against live server
  state: before create/update, `applyAgents` loads every referenced family and
  rejects an agent with a dangling provider/service/route/key reference (agents
  apply last, after every referenced object is resolved).
- **Resource view depth (§6.2):** `GET …/resources` resolves linked objects into
  summaries with an `exists` flag rather than returning raw id lists.
- **Standalone write-time attribution:** the standalone daemon (`agwd`) now wires
  the agent manager into the usage `AgentAttribution` holder after bootstrap,
  matching `caddy/gateway/app.go`. Without it, write-time `agent_id` stamping was
  silently inert under `agwd` (the attributor was never installed).
