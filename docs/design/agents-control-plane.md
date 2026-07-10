# Agents Control Plane

## 1. Purpose

This document defines the product and technical direction for first-class
`agents` support in `agent-gateway`.

The project should not evolve into an agent framework that owns an agent's
internal reasoning loop. Instead, `agent-gateway` should provide the external
control plane for agents:

- manage agent identities and workspaces
- bind agents to runtime backends such as ACP services
- govern the LLM and MCP resources an agent can use
- observe sessions, transcripts, permissions, usage, and call chains
- coordinate agent-level tasks, policies, scheduling, and future handoffs

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
- ACP services and routes for agent execution
- VirtualKeys, credentials, and CLI auth for access control
- metrics and interactions for usage, latency, errors, and call-chain traces

`pkg/agent` should be the product layer that composes these building blocks.
It should not replace `pkg/llm`, `pkg/mcp`, or `pkg/acp`; it should organize
them around agent management and orchestration.

## 3. Explicit Non-Goal

`agent-gateway` should not own general-purpose internal agent orchestration.

Remove the current `pkg/llm/agent` package rather than expanding it. That
package represents an LLM-native internal tool loop, including provider calls,
memory retrieval, tool execution, and iteration control. Those concerns are
better handled by dedicated agent frameworks and agent runtimes.

The gateway may later support a built-in agent runtime if there is a concrete
product requirement, but it must not define the core `agents` model or block
the external control-plane roadmap.

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
  - ACP service config
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
  -> pkg/acp/service + pkg/acp/runtime
  -> pkg/gateway/acproute + pkg/gateway/llmroute + pkg/gateway/mcproute
  -> pkg/llm/provider + pkg/llm/credentialmgr + pkg/gateway/modelcatalog
  -> pkg/mcp/service + pkg/mcp/runtime
  -> pkg/gateway/virtualkey
  -> internal/observability/usage
```

## 5. Core Concepts

### 5.1 Agent

An `Agent` is a first-class management object. It represents an operator-facing
agent identity, not a protocol-specific service.

Initial shape:

```json
{
  "id": "coding-agent",
  "name": "Coding Agent",
  "description": "Codex-backed development agent",
  "runtime": {
    "type": "acp",
    "acp": {
      "service_id": "codex-main"
    }
  },
  "routes": {
    "acp_route_ids": ["codex-turns"],
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

P0 implements `runtime.type = "acp"` only, but the model defines exactly two
built-in runtime types, split by a single axis — **who owns the agent's process
lifecycle**:

- `acp`: the **gateway** owns the lifecycle (process pool, sessions, permission
  flow, transcript). For local, embeddable agents the gateway should drive
  directly. A bespoke agent that needs this depth should be wrapped to speak ACP
  (the same way `codex-acp` bridges codex), not given a new runtime type.
- `http`: the **agent service** owns its own lifecycle; the gateway is only a
  client that hands it a task and observes the result. For business agents that
  expose a network endpoint and consume LLM/MCP through `resources`.

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

Crucially, **LLM and MCP are resources, not runtime types**. An agent's ability
to use models and tools lives in `resources` (and is governed there), regardless
of runtime. The runtime field only describes how the gateway dispatches work and
observes it. There is intentionally no third "native non-ACP lifecycle" runtime:
that need collapses into `acp` (wrap it) or `http` (don't manage its lifecycle).
See [5.4](#54-runtime-backends) for the executor contract and the SPI escape
hatch.

#### Generic policy vs runtime-specific config

Keep `policy` for cross-runtime governance only. Fields whose meaning depends on
the runtime backend belong under `runtime.<type>`, not under `policy`:

- `policy` holds runtime-agnostic governance: `max_agent_depth`, `budget`, and
  later schedule enablement, retention, and transcript visibility.
- `runtime.acp` holds only the binding (`service_id`). It does **not** duplicate
  the ACP service's operational config.

ACP-specific operational config — `permission_mode`, `allowed_roots`,
`default_cwd` — is **owned by the ACP service** under `/admin/acp/services`, not
copied onto the `Agent`. The `Agent` references a service; duplicating the
service's runtime config on the agent would create two sources of truth that
drift on update. The workspace surfaces those values read-through from the
service so the operator still sees them on the agent page. The single exception
is the auto-created-and-owned service (see [6.1](#61-p0-endpoints)): there the
service is a *derived object* of the agent, so an agent update may push those
fields into the service it owns — but that is a generated-service write path, not
a second stored copy on a referenced service.

This separation is still the main extensibility guarantee of the model. A future
runtime whose config is genuinely *agent-level* (not a property of a shared
backing service) would carry it under its own `runtime.<type>` block; today the
`acp` backend's config is service-level, so it stays on the service. The second
built-in `runtime.type`, `http`, introduces its own `runtime.http` block
(endpoint + auth, which are agent-level) and does not reshape `policy` or the
top-level object. Do not flatten ACP fields back onto `policy` or back onto the
agent.

#### Source of truth: `runtime` vs `routes`

`runtime` is authoritative for execution. For an ACP-backed agent,
`runtime.acp.service_id` is the binding that turns and runtime operations
resolve against. `routes.acp_route_ids` is a management/display reference used
to surface the matching ingress routes in the workspace and to drive
attribution; it does not select the execution backend. The two must stay
consistent (the listed ACP routes should point at `runtime.acp.service_id`), and
P0 validation should reject an agent whose `acp_route_ids` reference a service
other than its runtime service.

#### Cardinality: one agent, one runtime

An `Agent` binds to **exactly one** runtime backend instance — one ACP
`service_id` for `acp`, one endpoint for `http`. It is not a fan-out container
over several backends. The plural `routes.*_route_ids` are ingress references
that must all resolve to that single runtime, not a way to aggregate multiple
runtimes under one agent.

This is deliberate, for three reasons:

- **Attribution stays unique.** Write-time `agent_id` stamping (see [5.6](#56-agent-attribution))
  depends on a route/service mapping to exactly one agent; aggregating several
  services under one agent breaks that.
- **The agent does not become an internal orchestrator.** Selecting among or
  dispatching across several real backends is internal-loop behavior, which is an
  explicit non-goal (see [3](#3-explicit-non-goal)).
- **Lifecycle semantics stay clean.** An ACP session is pinned to a specific
  service/instance and cannot be freely moved, so an agent spanning multiple ACP
  services would have no coherent session model.

Therefore:

- **Multiple real agents** are modeled as multiple `Agent` objects. Coordinating
  them is a layer *above* agents — `AgentTask` handoff (P2) for the minimal A→B
  primitive, and `Agent Workflow` (P3) for graph orchestration. The workflow is a
  separate gateway-owned object that *references* several agents; it does not live
  inside any one agent.
- **One logical agent over interchangeable backends** (failover / load balance /
  A-B) is a different need and is out of scope here; see Open Questions.

#### Cardinality: one runtime, one agent

The 1:1 binding is enforced in both directions in P0. Not only does an agent bind
exactly one runtime (above); a given runtime backing instance — a specific ACP
`runtime.acp.service_id` — is bound by **at most one** agent. P0 create/update
validation rejects an agent whose `service_id` is already claimed by another
agent.

This is what keeps the identity, session, permission, usage, and workspace
semantics unambiguous: if two agents fronted one ACP service, then sessions,
pending permissions, transcripts, and usage on that service could not be
attributed to a single agent, and write-time `agent_id` stamping (see
[5.6](#56-agent-attribution)) would have to give up on that service entirely.

A future *shared service* mode (several agents intentionally sharing one backing
service) is possible but must be opt-in and explicitly marked — e.g. a
`shared: true` flag on the service binding — and in that mode the gateway does
**not** do precise per-agent `agent_id` stamping for that service; its events
fall back to the route/service mapping with the ambiguity surfaced. P0 does not
ship shared mode; it is tracked in Open Questions.

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

### 5.2 Agent Workspace

An `AgentWorkspace` is a read model for the UI. It aggregates the things an
operator needs on one agent detail page.

It is not a stored object. It is assembled from:

- the `Agent` object
- the bound ACP service (including its read-through operational config:
  `permission_mode`, `allowed_roots`, `default_cwd`)
- ACP routes that point at the service
- ACP runtime pooled instances and in-flight turns
- pending ACP permissions
- session and transcript **references** (counts + links), not full content
- LLM/MCP resources linked by policy
- metrics events and interaction traces filtered by route/service/session

The workspace is a **summary/index**, not a content aggregator. It returns
summaries, counts, runtime state, and links/references that let the frontend call
the dedicated ACP endpoints (`GET /<acp-route>/sessions`,
`GET /<acp-route>/sessions/{id}/transcript`) when the operator drills in. It must
not eagerly pull session transcripts: doing so would make one workspace call
unbounded in size and would entangle pagination, permissions, and performance
into a single endpoint. Transcripts and full session lists stay behind their own
paginated endpoints; the workspace only points at them.

The list above is the `acp` runtime view, which is the only one P0 assembles. The
workspace is keyed off `runtime.type`: an `http`-runtime agent has no pooled
instances, sessions, transcripts, or ACP permissions, so its workspace degrades
to the runtime-agnostic parts (the `Agent` object, linked resources, tasks, and
metrics/interaction traces). Do not hard-code ACP fields as required in the
workspace shape.

### 5.3 Agent Task

An `AgentTask` is an external unit of work owned by the gateway. It is not the
agent's internal plan.

Examples:

- run this agent on a prompt now
- resume this session with new input
- schedule this maintenance task daily
- hand off from agent A to agent B

Task execution dispatches through a runtime backend (see [5.4](#54-runtime-backends)),
not directly against a fixed protocol. The task model itself stays
runtime-agnostic: it captures gateway-level state, scheduling, cancellation,
retry, ownership, and audit metadata, and delegates "how to actually run this"
to the agent's `runtime.type` backend.

### 5.4 Runtime Backends

A runtime backend is the seam that decouples `AgentTask` from any single
protocol. It is an SPI in `pkg/agent`, selected by the agent's `runtime.type`:

```text
RuntimeBackend:
  Type() string
  StartTask(binding, taskSpec) -> handle      // dispatch one task to the backend
  // stream backend output into task events (delta / reasoning / usage / ...)
  // report terminal status (succeeded / failed)
  Cancel(handle)                              // gateway-visible, auditable
  // optional: idempotency / retry hooks
```

Everything above the SPI — task state machine, scheduling, cancellation, retry,
`agent_id` attribution, audit — is shared and reused across backends. Only "hand
the work to the agent and collect the result" differs per backend.

#### Two built-in backends

The classification axis is **who owns the agent's lifecycle**, which yields
exactly two built-in backends:

- **`acp`** — the gateway owns the agent's process lifecycle. `StartTask` issues
  an ACP turn against the agent's `runtime.acp.service_id`, reusing the existing
  pool, sessions, scope rebind, permission flow, and transcript. A task ending
  does not tear down the process; the pool governs it by `IdleTTL`. This backend
  gives sessions/permission/transcript "for free." Bespoke local agents that want
  this should be **wrapped to speak ACP** (the `codex-acp` precedent), not given
  a new backend.
- **`http`** — the agent owns its own lifecycle. `StartTask` dispatches the task
  to `runtime.http.endpoint` over one conventional HTTP task contract (deliver
  task → stream/poll events → terminal status) and the gateway only holds the
  task record. There is no process pool and usually no process reuse: an HTTP
  task is a network call that runs to terminal state. A remote stateful agent
  still fits here — its "session" is just an id passed over HTTP; the gateway does
  not manage its process.

#### No third native backend

There is intentionally no bespoke "native, non-ACP, gateway-managed lifecycle"
backend. That combination is contradictory: needing gateway-managed lifecycle
*is* what ACP is for, so the answer is to wrap as ACP rather than reinvent it.
Concretely:

- needs gateway-managed lifecycle (pool/sessions/permission) → wrap as `acp`
- does not need it (remote / self-managed / stateless) → `http`

#### SPI escape hatch (kept, not shipped)

`RuntimeBackend` remains a documented extension point, but only `acp` and `http`
are built in. If a concrete future case fits neither, a new backend can be added
behind the SPI — a YAGNI-style deferral, not an architectural exclusion. We do
not pre-ship a third backend speculatively.

#### Executor contract

Whatever the backend, it must provide at least: start work from a task payload,
stream/collect output into task events with a terminal status, and cancellation
that is gateway-visible and auditable. Idempotency/retry is recommended. `acp`
satisfies all of these; an `http` agent must satisfy at least start + result +
cancel for the task lifecycle (cancel/retry/audit) to hold.

### 5.5 Agent Policy

Agent policy is external governance. It should control the resources and
operator boundaries around the agent, not the internal reasoning algorithm.

Runtime-agnostic policy areas (live under `policy`):

- max agent depth
- budget and quota
- schedule enablement
- retention and transcript visibility

Runtime-specific config areas (for `acp`, owned by the ACP service under
`/admin/acp/services`, not duplicated on the agent — see
[5.1](#51-agent)):

- permission mode and approval routing
- cwd and allowed roots for ACP-backed agents

These are surfaced read-through in the workspace but governed on the service. A
future runtime whose config is genuinely agent-level would instead carry it under
its own `runtime.<type>` block.

Resource-scoping references (live under `resources` and `routes`):

- exposed routes
- allowed VirtualKeys
- allowed MCP services and tools
- allowed LLM providers and models

See [5.1](#51-agent) for why runtime-specific governance is kept out of the
generic `policy` block.

### 5.6 Agent Attribution

P1 observability ("usage for this agent", "activity for this agent") needs a
reliable way to map durable usage/interaction events back to an agent. The
metrics event tables today carry route/service/session/trace dimensions but no
`agent_id`.

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

`origin_agent_id` for cross-agent handoff is deferred to P2/P3, because there is
nothing to stamp until handoff exists. It is added the same additive way when
handoff ships.

## 6. Admin API Direction

The existing `/admin/agents` endpoints are stubs. They should become the
product-level API for agent management and UI aggregation.

These endpoints are management-plane APIs. They are not the primary data-plane
entrypoint for end-user chat or task execution. End users and business apps
should continue to call route-dispatched ACP endpoints such as:

```text
POST /<acp-route>/turn
POST /<acp-route>/permission
GET  /<acp-route>/sessions
GET  /<acp-route>/sessions/{session_id}/transcript
```

Likewise, agents should continue to access LLM and MCP resources through the
existing LLM API and MCP route surfaces. `/admin/agents` coordinates and
observes those surfaces; it does not replace them.

### 6.1 P0 Endpoints

P0 should make the frontend productive without introducing scheduling or
multi-agent workflows.

- `GET /admin/agents`
- `POST /admin/agents`
- `GET /admin/agents/{id}`
- `PUT /admin/agents/{id}`
- `DELETE /admin/agents/{id}`
- `GET /admin/agents/{id}/workspace`

P0 semantics:

- an agent can bind to one ACP service
- create/update can optionally create or update the backing ACP service
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
  `service_id` already claimed by another agent. Intentional multi-agent sharing
  is a deferred opt-in `shared` mode, not the default.
- deleting an agent deletes only the `Agent` record. It must not cascade-delete
  the backing ACP service or routes, because they may be shared or independently
  operated. The response should report what was unbound, not silently destroy
  runtime backends.
- a convenience `cascade` flag may be offered to also delete an
  auto-created-and-unshared service/route, but the default is non-cascading, and
  cascade must refuse when the service is still referenced by another agent.
- when create/update auto-creates a service, the agent records that it was the
  creator (provenance) so the UI can distinguish "agent owns this service" from
  "agent references a pre-existing shared service"; enforcement of single-owner
  vs shared deletion uses this provenance plus a reference check.

ACP-backed agents should use existing ACP management endpoints for runtime
operations:

- `GET /admin/acp/services/{service_id}/sessions`
- `GET /admin/acp/services/{service_id}/sessions/{session_id}/transcript`
- `GET /admin/acp/runtime`
- `GET /admin/acp/runtime/inflight`
- `DELETE /admin/acp/runtime/threads/{service_id}/{thread_id}`
- `POST /admin/acp/runtime/permissions/{request_id}`

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
- interactions are filtered by route, service, session, and trace identifiers
- resources show the LLM providers, routes, MCP services, and VirtualKeys the
  agent can use
- health is shallow at first: disabled state, runtime instances, in-flight
  turns, pending permissions, recent error rate, pipeline health

### 6.3 P2 Endpoints

P2 adds external orchestration and scheduling.

Agent-scoped (nested under a concrete agent id):

- `POST /admin/agents/{id}/tasks`
- `GET /admin/agents/{id}/tasks`
- `GET /admin/agents/{id}/schedules`
- `POST /admin/agents/{id}/schedules`
- `PUT /admin/agents/{id}/schedules/{schedule_id}`
- `DELETE /admin/agents/{id}/schedules/{schedule_id}`

Global task collection (separate prefix to avoid colliding with
`/admin/agents/{id}`, where a literal segment like `tasks` is ambiguous with an
agent id):

- `GET /admin/agent-tasks`
- `GET /admin/agent-tasks/{task_id}`
- `POST /admin/agent-tasks/{task_id}/cancel`

P2 semantics:

- tasks are gateway-owned external work items
- tasks execute through the agent's runtime backend (see [5.4](#54-runtime-backends)):
  `acp` issues a turn against the agent's ACP service, `http` dispatches to the
  agent's endpoint; the task layer itself is backend-agnostic
- schedules create tasks; they do not become hidden runtime loops
- cancellation must be gateway-visible and auditable
- the same collision rule applies to P3: keep global workflow collections under
  `/admin/agent-workflows` and never under `/admin/agents/<literal>`

### 6.4 P3 Endpoints

P3 adds multi-agent coordination.

- `GET /admin/agent-workflows`
- `POST /admin/agent-workflows`
- `GET /admin/agent-workflows/{id}`
- `PUT /admin/agent-workflows/{id}`
- `DELETE /admin/agent-workflows/{id}`
- `POST /admin/agent-workflows/{id}/runs`
- `GET /admin/agent-workflows/{id}/runs/{run_id}`

P3 semantics:

- workflows coordinate external turns and handoffs
- workflow state belongs to the gateway
- each step calls a runtime backend or a resource route
- interaction traces remain the source for runtime topology

## 7. Backend Implementation Plan

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

Goals:

- add the `RuntimeBackend` SPI (see [5.4](#54-runtime-backends)) with the `acp`
  backend; add the `http` backend and `runtime.type = "http"` once a concrete
  HTTP-agent consumer exists (the SPI lands first so the task layer never calls a
  protocol directly)
- add `AgentTask`
- add task state transitions
- add task history and cancellation
- add schedule config
- add a scheduler loop in the gateway runtime

Tasks should call runtime backends through the SPI. They should not implement an
internal reasoning loop, and the task layer must not hard-code ACP.

Scheduler placement constraint:

- the runtime has two assembly paths, `agw` (Caddy app) and `agwd` (standalone
  daemon). The scheduler loop must be owned by the shared runtime core so both
  paths get identical behavior, and started exactly once per process by the
  assembly layer — not by an HTTP handler or per-request code.
- the design assumes a single active gateway process per config store. Running
  multiple processes against one store is out of scope for P2; if it is needed,
  task claiming must move to a store-backed lease/leader mechanism rather than an
  in-memory loop. P2 should make this assumption explicit rather than silently
  double-firing schedules.
- task and schedule state must be durable so an in-flight task survives a
  restart with a well-defined recovery state (for example, re-queued or marked
  interrupted); the storage choice is an open question below.

### P3: Multi-Agent Workflow

Goals:

- add workflow definitions
- add workflow runs
- add handoff and dependency semantics
- add topology view support

The runtime implementation should remain conservative until real workflow
needs are clear from UI and operator usage.

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

### P2 Frontend: Tasks And Scheduling

Use:

- task endpoints
- schedule endpoints

Screens and widgets:

- task queue
- task run detail
- schedule editor
- cancellation controls
- retry status

### P3 Frontend: Multi-Agent Orchestration

Use:

- workflow endpoints
- workflow run endpoints
- interaction traces

Screens and widgets:

- workflow graph editor
- run timeline
- agent handoff visualization
- per-step logs and usage

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

## 10. Open Questions

The following are still genuinely open. Items the body now takes a position on
(resource enforcement, deletion/cascade, runtime-vs-routes authority,
attribution timing, and bundle/CLI parity) are decided there and are not
repeated here.

- Should creating an agent automatically create the backing ACP service and
  route, or should that remain an explicit advanced option? (The body allows
  optional auto-create; the default is still undecided.)
- When should intentional **shared-service** mode ship (several agents binding one
  ACP service via an opt-in `shared` flag), and what is the degraded-attribution
  contract in that mode? P0 enforces one service → one agent (see 5.1); shared
  mode would relax that and drop precise `agent_id` stamping for the shared
  service. What concrete use case justifies it over modeling each consumer as its
  own agent?
- Should `AgentTask` and schedule state be persisted in the generic config store
  or in a dedicated operational table? (Tasks are high-churn operational state,
  unlike config objects.)
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
- Does the agent ever become a data-plane principal (request-path identity that
  enforces `resources` directly), or does enforcement stay on VirtualKey + route
  policy indefinitely? (See 5.1.)
- Should one logical agent ever front several interchangeable runtime backends
  (failover, load balance, A-B) — analogous to logical-model routing over
  multiple provider bindings? The body keeps agent:runtime at 1:1 (see 5.1). If
  this is needed, it is a specialized runtime selection policy, clean only for
  stateless `http` backends; ACP session affinity (a session is pinned to one
  service/instance) makes it hard for `acp` and would need an explicit
  session-routing model. This is distinct from coordinating *multiple distinct*
  agents, which is Workflows (P3), not multi-backend.

## 11. Implementation Status

P0 (P0a + P0b) and P1 are **implemented**. P2 and P3 remain design-only.

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
