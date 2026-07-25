# Unified Agent Runtime and Routing Plan

Status: implementation in progress — M0-M1 complete

Source branch: `feature/unified-agent-runtime` working tree based on `bc4e739`

Reconciled baseline: `dev` / `4f89fac` on 2026-07-25

Scope: unify the runtime contract, capability control plane, and
route-dispatched ingress for `runtime.type = acp`, `http`, and `builtin`, and
fold ACP execution config into the owning Agent

## 1. Decision Summary

The implementation is broader than replacing ACPRoute and BuiltinRoute. It
first introduces one Agent runtime capability layer, moves ACP and builtin
behind that layer while their existing routes still work, then cuts ingress
over to one AgentRoute and folds ACP service config into the owning Agent. HTTP
execution and durable Workflow Agent Tasks build on the same contract after the
two shipping runtimes prove it.

The target stack is:

```text
Agent APIs / AgentRoute / Workflow `agent` task
  -> runtime-neutral ids, events, errors, capabilities, policy
  -> runtimeapi.Backend registry
       acp     -> ACP adapter -> agent-owned ACP runtime/process pool
       builtin -> builtin adapter -> in-process ADK host
       http    -> HTTP adapter -> external endpoint
```

The delivery rule is **contract before route cutover**. ACPRoute and
BuiltinRoute temporarily call the common runtime layer during implementation;
they are removed, not retained as compatibility aliases, when AgentRoute
becomes public.

### 1.1 Relationship to the Workflow Runtime

There is one Agent execution SPI: the turn-first
`runtimeapi.Backend.ServeTurn` contract defined here and in
[`agents-control-plane.md` §5.4](../design/agents-control-plane.md#54-runtime-backends).
It is introduced before route cutover and registers ACP and builtin as the
first executable backends; HTTP follows after its wire/auth contract is real.

There is also one durable task/DAG state machine, owned by
[`workflow-runtime.md`](../design/workflow-runtime.md). Its `agent` task handler
resolves the target Agent, invokes the same `runtimeapi.Backend`, consumes the
common event stream, and maps its terminal result into the Workflow Task Run.
The Workflow Runner owns persistence, scheduling, retry, idempotency,
permission suspension, and handoff. The runtime backend owns one Agent turn and
its native capabilities. No second task-first `StartTask` SPI or separate
`AgentTask` state machine is introduced.

M10 is therefore the integration point with Workflow Runtime W2/W3, not a
competing durable task implementation.

The route end state replaces the runtime-specific `ACPRoute` and
`BuiltinRoute` ingress objects with one `AgentRoute` that targets an
`agent_id`:

```yaml
agentRoutes:
  - id: agent:reviewer:review
    match_policy:
      path_prefix: /agents/reviewer
    auth_policy:
      require_virtual_key: true
    agent_id: reviewer
```

The persisted route normalizes to:

```json
{
  "id": "agent:reviewer:review",
  "kind": "agent",
  "protocol": "agent",
  "match_policy": {"path_prefix": "/agents/reviewer"},
  "auth_policy": {"require_virtual_key": true},
  "target_policy": {"kind": "agent", "agent_id": "reviewer"}
}
```

The route resolves the stable product identity first. The resolved Agent then
selects the execution backend through `runtime.type`:

```text
HTTP request
  -> shared route match + VirtualKey validation
  -> AgentRoute.agent_id
  -> AgentManager.Get(agent_id)
  -> runtime.type
       acp     -> ACP backend -> Agent.runtime.acp -> ACP process pool
       builtin -> builtin backend -> in-process ADK host
       http    -> HTTP backend -> external endpoint
```

Changing an Agent from one runtime to another must not require changing its
public route, VirtualKey allowlist, URL, or attribution identity.

In the target model an ACP Agent owns its runtime configuration directly:

```yaml
id: reviewer
runtime:
  type: acp
  acp:
    agent_type: codex
    cwd: /workspace
    allowed_roots: [/workspace]
    default_model: gpt-5
    permission_mode: interactive
    idle_ttl: 30m
    max_instances: 4
    codex:
      mode: adapter
      adapter_command: codex-acp
```

There is no persisted ACP service object, `service_id`, `owns_service`, or
`acp_services` bundle/store family after the cutover. The Agent supplies the
stable identity and lifecycle metadata; `runtime.acp` supplies only
ACP-specific execution config. The ACP runtime manager keys pools, scopes,
sessions, permissions, and diagnostics by `agent_id`.

This unifies northbound capability contracts and governance, not backend
mechanics. The three runtimes do not share process lifecycle, session storage,
permission transport, or runtime-specific operator details.

## 2. Baseline Alignment

This proposal originated from the pre-release v0.5 development tree and was
reconciled with the current `dev` documentation after the Unified Workflow
Runtime design landed. The implementation, product website, and permanent
documentation currently agree on the following facts.

### 2.1 Code

- `agent.Agent` supports three identity/runtime types: `acp`, `http`, and
  `builtin`.
- ACP and builtin are executable through the dispatcher today.
- HTTP stores and validates `endpoint` and `auth_ref`, but has no data-plane
  dispatcher or backend yet.
- Today's runtime route objects are `acproute.ACPRoute`, which targets
  `service_id`, and `builtinroute.BuiltinRoute`, which targets `agent_id`;
  HTTP has no route kind.
- Those runtime objects expand the shared `routecore.AgentRouteConfig` persisted
  shape and use the `routes` config store, one in-memory matcher, common
  match/auth fields, and the VirtualKey path. The shared config type name does
  not mean the target `pkg/gateway/agentroute.AgentRoute` runtime object exists
  today; M4 introduces it.
- Public management surfaces are still split across `acpRoutes`,
  `builtinRoutes`, `/admin/acp/routes`, `/admin/builtin/routes`, and separate
  `agwctl` commands.
- `agent.Routes` currently has four fields: `ACPRouteIDs`, `LLMRouteIDs`,
  `MCPRouteIDs`, and `BuiltinRouteIDs`. The ingress fields
  `acp_route_ids`/`builtin_route_ids` duplicate ownership, so the manager must
  cross-check them against route targets and rebuild a reverse attribution
  index.
- The Caddy dispatcher has separate `acp` and `builtin` enablement flags.
- The standalone server constructs the shared dispatcher with MCP enabled but
  does not currently enable ACP or builtin ingress.

Source-of-truth implementation anchors:

- `pkg/agent/types.go`
- `pkg/gateway/routecore/types.go`
- `pkg/gateway/{acproute,builtinroute}/`
- `pkg/gateway/agentgateway.go`
- `pkg/dispatcher/{handler,acp_handler,builtin_handler}.go`
- `pkg/agent/manager.go`
- `pkg/gatewaybundle/bundle.go`
- `cmd/agwctl/gateway_bundle_{apply,export}.go`
- `pkg/admin/{acp,builtin,agents,routes}.go`
- `caddy/dispatcher/{dispatcher,caddyfile}.go`
- `standalone/server/server.go`

### 2.2 Website

The English and Chinese website correctly describe the current pre-cutover
product as two execution runtimes plus one external identity model:

- builtin executes in-process;
- ACP executes Codex/OpenCode through managed processes;
- HTTP identity is persisted, but direct HTTP task dispatch is deferred;
- cross-runtime workflows remain roadmap work.

The proposal must not change those statements to “three execution runtimes”
until the HTTP backend is implemented and tested. Product-site updates always
apply to both `website/*.html` and `website/zh/*.html`.

Relevant pages are `index.html`, `agents.html`, `platform.html`, `why.html`,
`solutions.html`, and `observability.html` in both languages.

### 2.3 Documentation

Permanent docs correctly describe the current runtime-specific surfaces:

- builtin: `POST /<builtin-route>/turn`;
- ACP: `POST /<acp-route>/turn`, `/permission`, `/sessions`, and transcript;
- HTTP: identity/configuration only;
- ACP and builtin usage have different typed event families and dimensions.

This proposal does not retroactively describe planned behavior as shipped.
`agents-control-plane.md` §5.4 and `workflow-runtime.md` now share the
turn-first SPI boundary described in §1.1; all current-behavior ACP/builtin docs
remain authoritative until each migration milestone lands.

The current architecture diagram and product website must be updated in the
same implementation change that switches the public route surface. Until then,
they remain accurate and should not be edited to present tense.

## 3. Problem Statement

The current route model leaks runtime implementation into the public ingress:

```text
ACP agent     -> ACPRoute(service_id) -> find owning Agent indirectly
builtin agent -> BuiltinRoute(agent_id) -> invoke Agent directly
HTTP agent    -> no route
```

That causes six avoidable problems:

1. An Agent runtime change also changes its route family and management API.
2. ACP attribution depends on duplicated service/route-to-Agent indexes, while
   builtin attribution is direct.
3. Agent and route objects contain reciprocal ownership data that can disagree.
4. Bundle validation and apply must understand each runtime-specific route
   family and its ordering constraints.
5. A third execution backend would add another route kind, resolver, Admin API,
   CLI command, dispatcher flag, and documentation branch.
6. ACP exposes a second product identity (`service_id`) even though one service
   can bind to at most one Agent, forcing users to create and correlate two
   objects for one executable Agent.

The common route/store/matcher foundation means this duplication is no longer
necessary.

## 4. Goals and Non-goals

### 4.1 Goals

- One runtime backend contract for every Agent runtime type.
- Common run/session/request correlation ids and a common event envelope.
- Common capability discovery, error vocabulary, cancellation, permission
  lifecycle, runtime summary, and health surfaces.
- Runtime-agnostic governance semantics with fail-closed backend enforcement.
- One ingress route object for all Agent runtime types.
- Stable public routes when `runtime.type` changes.
- Stable route URL, authentication, common `/turn` input, and core event
  envelope when `runtime.type` changes. Optional capability wire behavior is
  not assumed interchangeable; clients re-read capabilities after a runtime
  change.
- Route target and attribution resolve directly to `agent_id`.
- ACP runtime config is owned directly by `Agent.runtime.acp`; `agent_id` is
  also the ACP pool/runtime identity.
- Remove standalone ACP service CRUD, storage, bundle objects, CLI commands,
  ownership provenance, and service-to-Agent reverse indexes.
- One Admin route CRUD family, one bundle field, and one CLI route family.
- One dispatcher enablement switch and one top-level Agent dispatch branch.
- Backend-specific capabilities remain fail-closed.
- Preserve the existing ACP and builtin runtime semantics behind adapters.
- Provide an explicit place to add HTTP execution without another route kind.
- Keep route matching on the existing in-memory snapshot; no per-request store
  reads for route matching.

### 4.2 Non-goals

- Do not merge LLMRoute, MCPRoute, and AgentRoute.
- Do not flatten ACP-specific process, cwd, permission, or adapter config into
  generic Agent policy; it remains under `runtime.acp`.
- Do not merge builtin materialization/runtime diagnostics into ACP runtime
  diagnostics.
- Do not make ACP and builtin sessions durable or behaviorally identical.
- Do not add cross-agent scheduling, hand-offs, or DAG execution in the runtime
  and route foundation. Those belong to the separately designed Workflow
  Runtime; its durable W2/W3 implementation consumes the finished backend
  contract.
- Do not claim HTTP execution before its outbound auth and wire contract ship.
- Do not preserve the old ACPRoute/BuiltinRoute public shapes or aliases. The
  repository change policy treats this as a breaking schema change.

### 4.3 Known limitation: permission wire portability

The first route cutover does not make permission continuation wire-compatible
between runtimes. ACP resolves a pending native request while the original turn
stream remains active through `POST /permission`; builtin ends the stream at an
ADK checkpoint and resumes through a later `POST /turn` carrying a permission
decision. An Agent changed between those runtimes keeps its route, URL,
VirtualKey, common turn fields, and event envelope, but a permission-aware
client must re-read capabilities and follow the advertised `resume_mode`.

Therefore “runtime changes preserve the public contract” in this plan means the
common route/turn core, not every optional capability operation. Full permission
wire portability requires a later explicit contract decision (§11); it is not
an M5 acceptance criterion and must not be advertised before it exists.

## 5. Unified Capability Plane

The capability plane is implemented before AgentRoute. Existing ACPRoute and
BuiltinRoute handlers first become callers of this plane, proving that the
abstraction preserves shipping behavior before the public schema changes.

### 5.1 Capability matrix

The common contract exposes capabilities rather than pretending every backend
has the same implementation:

| Capability | ACP today | builtin today | HTTP today | Target |
|---|---|---|---|---|
| turn execution | yes | yes | no | required backend method |
| SSE events | rich ACP vocabulary | shared subset | no | common envelope |
| session resume | yes | in-memory | identity only | optional capability |
| session list | yes | no public API | no | optional capability |
| transcript | yes | internal history only | no | optional capability |
| interactive permission | live ACP request | ADK checkpoint | no | common lifecycle, native decision data |
| cancellation | thread/transport close | force/graceful turn cancel | no | common run cancel; ACP force-only initially |
| runtime state | pool/inflight/pending | materialization/inflight/pending | none | common summary plus details |
| health | service/process-derived | host/materialization-derived | identity only | common health result |
| typed usage | ACP events | builtin events | none | common dimensions, typed extensions |
| durable Workflow Agent Task | no | no | no | Workflow Runtime W2 integration |

Capability discovery is authoritative. Clients and Admin/UI code do not infer
support from `runtime.type`, and the dispatcher never silently emulates an
unsupported operation.

### 5.2 Execution and correlation identities

Define runtime-neutral identities with one meaning across every backend:

- `agent_id`: stable managed Agent identity;
- `run_id`: one logical execution, including any permission-resume segments;
- `session_id`: optional conversation/session identity returned by a backend;
- `request_id`: one control interaction such as a permission request;
- `trace_id`/`span_id`: one transport/execution segment;
- `parent_span_id` and Span Links: causal relationships between segments.

The gateway generates `run_id` before invoking a backend. A backend may return
or bind its native session id, but it cannot replace the gateway run id. ACP
`thread_id` becomes backend binding data rather than the common execution
identity. Builtin HITL resumes keep the current stable run id and trace-link
behavior.

Every emitted event, in-flight view, pending permission, cancellation target,
usage event, and structured log carries the applicable common identities.

### 5.3 Turn request and event envelope

The common request owns only semantics shared at the Agent boundary:

```go
type TurnRequest struct {
    RunID      string
    Input      string
    SessionID  string
    Permission *PermissionDecision
    Options    TurnOptions
}
```

`TurnOptions` uses one versioned envelope; the implementation does not use a
flat runtime superset:

```go
type TurnOptions struct {
    Version   string           `json:"version"`
    Runtime   json.RawMessage  `json:"runtime,omitempty"`
    Execution ExecutionOptions `json:"-"`
}
```

The northbound JSON form is:

```json
{
  "input": "review this change",
  "session_id": "optional",
  "options": {
    "version": "v1",
    "runtime": {}
  }
}
```

`version` is required whenever `options` is present. M1 implements only `v1`.
`runtime` is an opaque JSON object to the common decoder and is decoded
strictly by the selected backend with unknown-field rejection. An empty or
absent runtime object is valid. Supplying options for another runtime is not a
fallback mechanism: the selected backend rejects them with
`unsupported_option`.

`ExecutionOptions` is internal-only metadata used by trusted gateway callers,
including the future Workflow logical execution/idempotency key. It is never
decoded from AgentRoute JSON. The M2 legacy ACPRoute/BuiltinRoute decoders keep
their existing public request bodies and translate them once into this
container; the M5 AgentRoute surface publishes only the versioned envelope.
The decoder therefore distinguishes absent, supported, and unsupported fields,
and no backend silently ignores input.

The event envelope is stable even when `Data` is runtime-specific:

```go
type TurnEvent struct {
    Event        string          `json:"-"`
    AgentID      string          `json:"agent_id"`
    RunID        string          `json:"run_id"`
    SessionID    string          `json:"session_id,omitempty"`
    RequestID    string          `json:"request_id,omitempty"`
    Sequence     uint64          `json:"sequence"`
    SegmentIndex uint32          `json:"segment_index"`
    Text         string          `json:"text,omitempty"`
    Data         json.RawMessage `json:"data,omitempty"`
}
```

`sequence` is monotonically increasing across the entire logical `run_id`, not
merely within one HTTP/SSE stream. `segment_index` starts at zero and increments
when a logical run continues on a new transport stream, such as builtin HITL
resume. Ordering is the composite `(run_id, sequence)`; segment index exists for
transport diagnostics and is not needed to recover the run order.

The common sink serializes concurrent producers and allocates sequence numbers.
Before M10, a suspended run keeps the next sequence and segment index in its
in-memory checkpoint/pending-run record. M1 therefore guarantees monotonicity
and uniqueness across resume segments only within one process-lifetime logical
run. M10 persists the cursor transactionally with task events, enforces
uniqueness on `(run_id, sequence)`, and extends the same contract across
durable recovery and process restart. A resumed run within its advertised
durability boundary must never restart sequence numbering at one.

Core event names are:

```text
session delta reasoning content plan tool_call usage permission done error
```

A backend advertises the subset it can emit. ACP metadata events such as
`available_commands`, `session_info`, `mode`, and `config_options` remain
registered optional event names; builtin is not required to synthesize them.

### 5.4 Runtime backend and capability interfaces

The required backend surface is intentionally small:

```go
type Backend interface {
    RuntimeType() string
    Capabilities(ctx context.Context, a agent.Agent) (Capabilities, error)
    ServeTurn(ctx context.Context, a agent.Agent, req TurnRequest, emit EventSink) error
}
```

Optional operations are narrow interfaces rather than required no-op methods:

```go
type SessionLister interface { ListSessions(...) (..., error) }
type TranscriptLoader interface { LoadTranscript(...) (..., error) }
type PermissionResolver interface { ResolvePermission(...) error }
type RunCanceller interface { CancelRun(...) (CancelResult, error) }
type RuntimeInspector interface { RuntimeSummary(...) (RuntimeSummary, error) }
type HealthChecker interface { Health(...) (Health, error) }
```

The required contracts live in `pkg/agent/runtimeapi`; the registry is owned by
`AgentGateway`. Protocol packages stay unaware of `pkg/agent`; thin
higher-level adapters perform translation.

Registry rules:

- duplicate `RuntimeType()` registration is a startup error;
- an Agent whose backend is not registered remains manageable but is not
  executable;
- execution fails with `runtime_not_executable`, never another backend;
- capability results may depend on Agent/runtime configuration but must be
  deterministic for one definition version;
- definition/runtime changes invalidate cached capabilities.

### 5.5 Capability discovery

Add a runtime-neutral read surface:

```text
GET /admin/agents/{id}/capabilities
```

The same summary is embedded in Agent workspace and health responses:

```json
{
  "executable": true,
  "turn": {"streaming": true},
  "sessions": {
    "resume": true,
    "list": true,
    "transcript": true,
    "durable": true
  },
  "permissions": {
    "interactive": true,
    "resume_mode": "active_stream"
  },
  "cancellation": {
    "force": true,
    "graceful": false
  },
  "events": ["session", "delta", "reasoning", "content", "usage", "done", "error"]
}
```

`resume_mode` describes externally relevant behavior without claiming the
underlying mechanism is shared. The initial closed values are
`active_stream` for ACP and `new_stream` for builtin.

### 5.6 Common error contract

Every adapter maps native errors to a stable Agent error code:

```text
agent_not_found
agent_disabled
runtime_not_executable
capability_not_supported
invalid_request
unsupported_option
session_not_found
session_busy
session_limit_exceeded
run_not_found
permission_required
permission_not_found
permission_expired
turn_limit_exceeded
turn_cancelled
backend_unavailable
backend_timeout
turn_failed
```

The error type is available through `errors.Is`/a typed Go error internally and
as a stable `error_type` externally. Native details remain in structured logs
and typed usage extensions; public responses do not leak commands, credentials,
environment values, or unapproved filesystem paths.

Pre-stream errors return real HTTP status codes. Mid-stream failures emit one
terminal SSE `error`; cancellation emits `done` with
`stop_reason=cancelled`. A stream has exactly one terminal event.
`RunSequencer.ServeSegment` returns a `SegmentResult` with explicit `Started`
and `Terminal` flags: callers map a returned error to HTTP only when `Started`
is false, and never guess stream state from the error or backend behavior.

### 5.7 Run inspection and cancellation

Unify the Agent-facing operator model around `run_id`:

```text
GET    /admin/agents/{id}/runs
DELETE /admin/agents/{id}/runs/{run_id}?mode=force|graceful
```

The adapter maps the common request to native behavior:

- builtin invokes its activity registry and `CancelTurn` primitive;
- ACP advertises **force only**, but this is a new ACP runtime primitive rather
  than a mapping to an existing Manager method. `Manager.ServeTurn` (or its
  immediate execution owner) must register each active `run_id` with that
  turn's exact context/cancel handle, remove it at terminal completion, and
  make force cancellation drive the prompt loop's native ACP
  `session/cancel`. The current `CloseScope`/`CloseThread`/`CloseService`
  operations and their pool teardown are not sufficient implementations.
  ACP does not advertise graceful because it has no verified
  safe-point/grace-period primitive;
- HTTP eventually invokes the versioned remote cancellation contract.

Supported modes come from capabilities. Requesting an unsupported mode returns
`capability_not_supported`; graceful never silently becomes force. The
operation is idempotent: an already terminal run reports its terminal state,
while an unknown run returns `run_not_found`.

That distinction requires a bounded common run registry rather than deleting
all knowledge at completion:

- active entries hold the exact backend cancel binding and current state;
- terminal completion removes the cancel binding and moves the normalized
  result into an in-memory tombstone;
- tombstones are retained for 10 minutes, capped at 1,024 entries per Agent,
  with oldest-completed eviction when the cap is exceeded;
- `GET /admin/agents/{id}/runs` returns active entries and retained tombstones;
- cancelling a retained terminal run returns its terminal result without
  calling the backend again;
- an id absent from both sets returns `run_not_found`.

The registry is process-local through M7 and capabilities report that run
history is not durable. M10 replaces the tombstone source for Workflow-owned
runs with durable task/run state; it does not change the exact-run cancellation
contract.

ACP `CloseScope`/`CloseThread` are not mappings for graceful or ordinary force
cancel: they tear down pooled instances and may affect more than one logical
run. They remain runtime recovery escape hatches. The ordinary Agent console
uses the common exact-run API. M3 is not complete until real `codex-acp` and
`opencode acp` integration tests demonstrate that cancelling one active run
sends/observes native `session/cancel`, reaches the normalized cancelled
terminal outcome promptly, and does not terminate an unrelated run sharing the
same runtime owner or pool. Fake-backend cancellation tests alone are
insufficient.

The two operator layers are intentional, not aliases:

- `/admin/agents/{id}/runs/...` targets a logical Agent run by `run_id`, has
  normalized outcomes, and is the normal product/API surface;
- `/admin/acp/runtime/...` and `/admin/builtin/runtime` expose backend-native
  process, pool, materialization, or forensic state. ACP runtime entries are
  keyed by `agent_id`; no unbound ACP runtime exists after the cutover.

Where an old backend endpoint performs the same logical operation rather than
backend recovery, M7 removes it. In particular the builtin
`DELETE /admin/builtin/runtime/turns/{agent_id}/{session_id}` endpoint is
replaced by the Agent run cancel API; it is not retained as a permanent second
cancel path. ACP thread close remains because its destructive pool-recovery
semantics are deliberately different.

### 5.8 Permission lifecycle

Unify pending permission identity, expiry, listing, decision audit, and
fail-closed behavior:

```go
type PendingPermission struct {
    RequestID string
    AgentID   string
    RunID     string
    SessionID string
    CreatedAt time.Time
    ExpiresAt time.Time
    Actions   []PermissionAction
    Native    json.RawMessage
}
```

Common operator APIs are:

```text
GET  /admin/agents/{id}/permissions
POST /admin/agents/{id}/permissions/{request_id}
```

These are the only operator decision APIs after M7. Once M3 lands and until
M7, every decision entry point -- the legacy ACP route/Admin endpoints, builtin
checkpoint resume on `POST /turn`, and the common Agent endpoint -- resolves
through the same one-shot broker. The first valid decision wins and every
later decision attempt receives `permission_not_found`; every attempt is
audited with its control-plane source.

The common broker owns pending identity, expiry, atomic claim, and audit for
both runtimes. ACP's live waiter and builtin's ADK checkpoint store remain
backend continuation mechanisms, not competing pending-request registries.
Each backend registers one continuation binding with the broker. A decision
entry point first atomically claims and removes the common record under the
in-process broker lock, then dispatches that binding through the owning
adapter. Expiry similarly claims once and invokes the backend's
fail-closed path. A future durable or multi-process broker must provide the
equivalent compare-and-set transaction. M3 must move both backends and all
decision entry points to this broker in one change; an adapter-local fallback
registry is not allowed.

The continuation binding is fixed as an opaque backend token. The common
broker record stores `runtime_type` plus an unguessable process-local token;
the selected adapter resolves that token in its backend-owned continuation
store. The broker never stores, copies, or decodes ACP waiter state or
builtin's ADK checkpoint/calls/transcript/trace-link payload.

Lifecycle is fail-closed and one-shot:

- registration first stores backend state, then publishes exactly one common
  broker record; publication failure deletes the backend state;
- decision or expiry atomically claims the common record before invoking the
  adapter; no second caller can resolve the token;
- a claimed decision that encounters an adapter error is terminally failed and
  audited, not restored for retry;
- expiry invokes the adapter's deny/cancel cleanup and then deletes its state;
- exact-run cancellation claims every related permission before cancelling the
  run and invokes the same backend cleanup;
- Agent deletion, runtime switch, config-fingerprint retirement, and process
  shutdown drain matching records through the fail-closed cleanup path;
- an adapter that cannot resolve a claimed token returns
  `permission_not_found` internally and records `continuation_lost`; it never
  falls back to a native independently claimable registry.

Backend stores may index opaque state by token and native identity for cleanup,
but only the common broker owns whether a request is pending and claimable.
This preserves builtin's complete continuation payload and ACP's live waiter
without exposing either in the common schema.

Claiming a permission and executing its continuation are distinct for
`resume_mode = "new_stream"`:

- ACP uses `resume_mode = "active_stream"`: a claimed decision is delivered to
  its live waiter and the original turn stream continues;
- builtin uses `resume_mode = "new_stream"`: the adapter validates and stores
  the claimed decision beside the opaque checkpoint state, marks the run
  `suspended_decided`, and returns
  `{status:"accepted", resume_required:true, request_id}` from the Admin
  decision API without executing the graph;
- a later `POST /<agent-route>/turn` carrying only the decided `request_id`
  atomically consumes that decided continuation and owns the new SSE segment;
- when the decision itself arrives on builtin `POST /turn`, that request claims
  the common broker record and consumes the continuation in one operation;
- the decided continuation keeps the original permission expiry, run sequence,
  segment cursor, definition fingerprint, transcript, and trace-link state. If
  it is not resumed before expiry it is cleaned up fail-closed;
- a repeated decision after claim receives `permission_not_found`; a repeated
  resume after consumption receives `permission_not_found`. Resume is not a
  second permission decision and never recreates the common pending record.

This two-phase behavior is advertised by capabilities and run state. The Admin
permission endpoint never starts a builtin continuation in the background:
before M10 there is no durable event sink to own such an execution.

The common layer does not flatten native decision semantics:

- ACP preserves the exact advertised option IDs and nested ACP outcome;
- builtin preserves per-tool-call allow/deny plus request cancel;
- unanswered/expired decisions fail closed for every backend.

ACP may keep the original turn stream alive while builtin resumes a checkpoint
on a new stream. The capability result exposes that difference. Unifying the
operator lifecycle does not require falsely identical transport behavior.

### 5.9 Sessions and transcripts

Sessions are optional backend capabilities with common list/load envelopes:

```text
GET /<agent-route>/sessions
GET /<agent-route>/sessions/{session_id}/transcript
```

Operator reads use the same capability implementations without a route lookup:

```text
GET /admin/agents/{id}/sessions
GET /admin/agents/{id}/sessions/{session_id}/transcript
```

Common session metadata is limited to `session_id`, title, update time, and
optional backend details. Durability, cwd filtering, pagination, load support,
and transcript roles are advertised capabilities.

ACP continues to use its capability handshake, allowed-root validation,
canonicalized cwd filtering, and transient load connection. Builtin does not
advertise list/transcript merely because it holds in-memory history internally;
those operations require explicit APIs, pagination, visibility rules, and
tests. HTTP exposes only what its remote contract supports.

### 5.10 Runtime summary and health

Agent workspace should expose a common summary and an additive backend detail:

```json
{
  "runtime": {
    "type": "builtin",
    "executable": true,
    "healthy": true,
    "state": "ready",
    "active_runs": 2,
    "pending_permissions": 1,
    "session_count": 8,
    "last_activity_at": "2026-07-21T12:00:00Z"
  },
  "runtime_details": {}
}
```

Common state values are `unknown`, `disabled`, `not_executable`, `starting`,
`ready`, `degraded`, and `unhealthy`. They summarize rather than replace ACP
pool entries, builtin materialization state, or future HTTP probe details.

Health is side-effect-free and bounded. It must not create an ACP process,
materialize a builtin graph, start a session, or execute an HTTP turn merely to
answer a list/workspace request.

### 5.11 Governance and resource enforcement

Runtime-neutral policy describes outcomes:

- disabled execution;
- maximum Agent depth;
- turn timeout and maximum concurrent runs;
- turn/token/cost budgets;
- transcript/session visibility and retention;
- schedule enablement when durable Workflow scheduling exists;
- default fail-closed permission posture.

Backend-specific operational config stays under the owning Agent runtime block:
ACP process pool, cwd/allowed roots, permission and adapter config under
`runtime.acp`; builtin topology/middleware/materializer under
`runtime.builtin`; HTTP endpoint/auth/timeouts under `runtime.http`.

The common backend wrapper enforces policies that can be enforced at the Agent
boundary. Resource declarations should become effective entitlements in a
separate milestone: builtin already enforces selected LLM/MCP resources, while
external ACP/HTTP Agents need scoped gateway credentials or equivalent callback
identity before the same claim is true. Until then, docs continue describing
external resource references as a management view.

### 5.12 Observability contract

Every Agent execution records common dimensions:

```text
agent_id runtime_type run_id session_id operation result_status
route_id virtual_key_id trace_id span_id parent_span_id
```

Typed ACP and builtin extensions remain. Runtime unification must not erase
service/thread/config metadata or builtin topology/checkpoint metadata. Common
interaction queries, logs, Prometheus counters, and OTLP traces use the shared
identities; backend event tables retain runtime fidelity.

M1 includes an additive SQLite usage-schema migration in
`pkg/configstore/sqlite/usage_schema.go`: add nullable `run_id` and
`runtime_type` to `llm_usage_events`, `mcp_usage_events`, and
`acp_usage_events`, and nullable `runtime_type` to `builtin_usage_events`
(`run_id` already exists there). Add run indexes where query paths use them.
Direct non-Agent LLM/MCP/ACP traffic leaves the new columns null. The migration
uses the file's existing idempotent `ALTER TABLE ... ADD COLUMN` pattern and
must be tested from a pre-migration schema as well as a fresh database.
The existing builtin column is nullable `TEXT`, and its populated values
already identify one logical builtin run across HITL resume segments. M1 moves
fresh-run ID generation from the builtin Host to the common Agent execution
boundary; the adapter/Host must preserve the supplied ID, while historical
null values remain null and require no backfill.

M1 also audits every existing usage summary, interaction union, Agent
attribution filter, scan destination, and index that touches these tables.
Tests must retain direct non-Agent rows with null common identity, keep existing
`agent_id`-filtered results unchanged, and add mixed legacy/new-row coverage.
For SQLite query paths that add `run_id` or `runtime_type` filters, inspect
`EXPLAIN QUERY PLAN` in representative fixtures and add or adjust indexes only
where the actual access path uses them; do not assume the new nullable columns
automatically replace ACP `thread_id`/`session_id` lookup semantics.

### 5.13 Durable Workflow Agent Task boundary

The Workflow Runtime's `agent` task is the eventual durable consumer of the
same backend contract:

```text
Workflow Task Run -> invoke backend turn -> persist ordered events
                  -> permission/cancel/retry -> terminal result
```

The Workflow Runner owns durable state, scheduling, retry, handoff, artifacts,
and audit. It is not introduced during the route cutover. The runtime
foundation must avoid coupling `Backend` exclusively to an HTTP response so the
Workflow Agent task handler can invoke the same backend and consume the same
events. The authoritative run/task states, idempotency rules, permissions, and
W2/W3 delivery gates live in
[`workflow-runtime.md`](../design/workflow-runtime.md).

## 6. Target Route and Control-plane Model

### 6.1 Route core

Add the following route-core values:

```go
const RouteKindAgent RouteKind = "agent"
const RouteProtocolAgent RouteProtocol = "agent"
const RouteTargetPolicyKindAgent RouteTargetPolicyKind = "agent"
```

Remove the public/runtime use of:

```text
RouteKindACP
RouteKindBuiltin
RouteProtocolACP
RouteProtocolBuiltin
RouteTargetPolicyKindACPService
RouteTargetPolicyKindBuiltinAgent
```

The shared `routes` store remains unchanged. `AgentRouteConfig` is an expanded
Admin/bundle representation over `routecore.AgentRouteConfig`, following the
same pattern used by current route packages:

```go
type AgentRouteConfig struct {
    routecore.AgentRouteConfig
    AgentID string `json:"agent_id"`
}

type AgentRoute struct {
    routecore.AgentRouteConfig
    AgentID string `json:"agent_id"`
}
```

Normalization sets `kind = agent`, `protocol = agent`, trims `agent_id`, and
generates `agent:<agent_id>:<path-slug>` when the ID is empty. IDs stay
slash-free and globally unique across every route family.

The package should be `pkg/gateway/agentroute`. Delete
`pkg/gateway/acproute` and `pkg/gateway/builtinroute` after all callers move.

### 6.2 Ownership is one-way

`AgentRoute.agent_id` is the authoritative ingress ownership relationship.

Remove these persisted Agent fields:

```text
routes.acp_route_ids
routes.builtin_route_ids
```

Do not replace them with a persisted `routes.agent_route_ids`. A reverse list
would duplicate the target already stored on AgentRoute and recreate the same
consistency problem.

Agent workspace/read APIs derive ingress routes by listing the in-memory route
snapshot and filtering `target_policy.agent_id`. `routes.llm_route_ids` and
`routes.mcp_route_ids` remain because they describe resources used by an Agent,
not ingress ownership. Renaming those fields is outside this change.

Consequences:

- one Agent may have zero or many AgentRoutes;
- one AgentRoute targets exactly one Agent;
- creating/updating an AgentRoute requires its target Agent to exist;
- a disabled Agent or an Agent whose backend is not currently executable may
  still be targeted and persisted; management validity is separate from
  execution availability;
- deleting an Agent is rejected while AgentRoutes target it, unless an existing
  explicitly requested cascade operation owns deletion of those exact routes;
- changing `runtime.type` leaves AgentRoutes untouched;
- usage attribution reads `agent_id` directly from the matched route and no
  longer needs route ownership inference for Agent ingress.

ACP has no second persisted identity after the cutover. Its runtime manager
uses `agent_id` directly for pool keys, scopes, sessions, pending permissions,
native recovery operations, and typed usage. The old
`service_id -> agent_id` index and `OwnsService` provenance are deleted.

### 6.3 Runtime backend integration

Use the backend registry defined in §5.4 above. It owns runtime selection, not
HTTP route matching. Its adapters must preserve these dependency rules:

- `pkg/acp` does not import `pkg/agent`;
- lower LLM/MCP packages do not import `pkg/agent`;
- the adapter translates `Agent.runtime.acp` into a protocol-owned
  `acp.RuntimeConfig` and passes `agent_id` as the runtime owner key;
- route matching and VirtualKey validation stay in `pkg/dispatcher`;
- runtime process/session logic stays in its current owner.

Required placement and ownership:

- shared request/event/backend contracts: `pkg/agent/runtimeapi`;
- ACP adapter: a higher-level Agent adapter over `pkg/acp/runtime.Manager`;
- builtin adapter: a higher-level Agent adapter over `pkg/agent/builtin.Host`;
- HTTP adapter: a higher-level outbound client with explicit auth resolution.

The final `pkg/acp` surface contains a runtime config type without management
identity fields (`id`, `name`, `description`, timestamps, or `disabled`).
Those fields belong to `Agent`. The runtime manager accepts the owner
`agent_id` separately and never reads the Agent store itself. Updating an
Agent's ACP config retires the prior config fingerprint: in-flight turns drain,
old pooled instances accept no new turns, and subsequent turns materialize from
the new config. Safety-sensitive changes such as `allowed_roots` and
`permission_mode` therefore cannot reuse an instance created under stale
policy.

Deleting an ACP Agent (after its AgentRoutes are removed) or changing its
runtime type retires the entire Agent-keyed pool, cancels in-flight/pending
permission work fail-closed, and removes its runtime registry state. No orphan
ACP process pool may survive without an owning Agent.

`AgentGateway` owns the registry and exposes one AgentRoute resolver. At
startup it registers only linked/configured backends. A missing backend fails
closed; it never falls through to another runtime.

### 6.4 Dispatch

The shared dispatcher branch becomes:

```go
case routecore.RouteKindAgent:
    return h.dispatchAgent(...)
```

`dispatchAgent` performs, in order:

1. resolve `AgentRoute` from the matched config;
2. resolve the target Agent from the in-memory Agent manager/cache;
3. stamp `agent_id` before opening the usage span;
4. reject a disabled Agent before starting a stream;
5. match the Agent endpoint operation;
6. resolve the backend by `agent.Runtime.Type`;
7. check the backend capability for that operation;
8. translate and execute through the backend adapter.

The matched request must not perform a config-store list. If Agent lookup is not
already snapshot-backed at implementation time, extend `agent.Manager` with the
deep-cloned, generation-swapped definition snapshot fixed in §11 rather than
putting store access in the per-request hot path. Store-free Agent lookup is a
hard M4 gate.

### 6.5 Northbound endpoint contract

Route unification and wire-format unification are separate decisions. The first
implementation should share the route and dispatcher while preserving proven
runtime behavior behind adapters.

The common guaranteed operation is:

```text
POST /<agent-route>/turn -> SSE
```

The stable common request subset is:

```json
{
  "input": "review this change",
  "session_id": "optional existing session"
}
```

Runtime-specific optional input must be validated rather than silently ignored:

- ACP's v1 `options.runtime`: `thread_id`, `cwd`, `model`, `fresh_session`,
  `config_overrides`;
- builtin: the common top-level `permission` continuation plus any future
  builtin v1 runtime options;
- HTTP: only fields supported by the eventual Agent HTTP contract.

The AgentRoute decoder accepts only the common fields and the §5.3 versioned
options envelope. Unknown top-level, envelope, or selected-runtime fields
return `400 unsupported_option`. Only the temporary ACPRoute/BuiltinRoute
decoders accept their proven legacy bodies and translate them internally. A
later wire-contract design may normalize thread/session and permission
behavior; that is not required to remove runtime-specific routes.

Additional route-scoped operations are capabilities:

```text
POST /<agent-route>/permission
GET  /<agent-route>/sessions
GET  /<agent-route>/sessions/{session_id}/transcript
```

ACP supports these today. A backend that does not implement an operation
returns `501 capability_not_supported`; it must not proxy or emulate it
silently. Builtin permission continuation remains on `POST /turn` until a
separate permission-contract decision changes it.

All SSE implementations should use the existing lazy-header rule: synchronous
validation failures return a real non-200 HTTP response; only mid-stream
failures become terminal SSE error events.

### 6.6 HTTP runtime

An AgentRoute may target an HTTP Agent as soon as the unified model lands, but
the HTTP backend is not considered executable until all of the following exist:

1. a versioned outbound turn request/SSE response contract;
2. fail-closed `auth_ref` resolution with no secret material in Agent objects;
3. connect, response-header, idle-stream, and total-turn timeouts;
4. trace and agent-depth propagation;
5. response body limits and strict content-type/event validation;
6. retry semantics that cannot duplicate a non-idempotent turn;
7. health and usage instrumentation;
8. integration tests with a real streaming test server.

Before those gates pass, dispatch to an HTTP Agent returns
`501 runtime_not_executable`. The website continues to label HTTP execution as
roadmap. Route unification alone is not permission to advertise a third
execution runtime.

This creates an intentional interim operator experience: an HTTP Agent and its
AgentRoute can validate, persist, appear in workspace, accept VirtualKey
assignment, and report `capabilities.executable=false`, while an attempted
`POST /turn` returns the stable 501 above. Admin/CLI views must show the
non-executable state prominently so an existing-but-inactive route is not
misdiagnosed as a matcher or authentication defect. Route creation does not
imply backend availability.

### 6.7 Runtime Admin APIs remain separate

Unify ingress route CRUD:

```text
GET    /admin/agents/routes
POST   /admin/agents/routes
GET    /admin/agents/routes/{id}
PUT    /admin/agents/routes/{id}
DELETE /admin/agents/routes/{id}
```

Retain runtime diagnostic surfaces because they expose different native state:

```text
/admin/acp/runtime/...
/admin/builtin/runtime/...
```

This is not permanent duplication of the logical Agent controls. Agent APIs use
`agent_id`/`run_id` and normalized capability semantics. Runtime APIs use native
scope/process/materialization identities for diagnosis and recovery. ACP
runtime paths and response objects identify their owner by `agent_id`; there is
no `/admin/acp/services` family or unbound service. M7 removes backend endpoints
whose only behavior is an alias for an Agent-level operation while preserving
genuinely lower-level ACP pool/process and builtin host inspection.

For example, destructive ACP thread recovery becomes:

```text
DELETE /admin/acp/runtime/agents/{agent_id}/threads/{thread_id}
```

It is intentionally separate from exact logical-run cancellation.

The HTTP backend may later add an operator view under `/admin/http/runtime` or
an Agent health view, but it must not introduce `/admin/http/routes`.

CLI changes:

```text
agwctl gateway agent-route list|get|create|update|delete
```

Remove `acp-route`, `builtin-route`, and `acp-service`. Keep `acp-runtime`, with
all owner arguments and output expressed as `agent_id`. Builtin runtime
operations remain under the existing Agent/builtin runtime commands until
separately redesigned.

### 6.8 Bundle model and apply order

Replace:

```yaml
acpServices: [...]
acpRoutes: [...]
builtinRoutes: [...]
```

with:

```yaml
agentRoutes: [...]
```

[`workflow-runtime.md` §15](../design/workflow-runtime.md#15-validation-and-safety)
defines the relative W0/W1 workflow order only. This plan newly places
`agents`, `AgentRoutes`, and `VirtualKeys` around that existing subsequence; it
does not claim that §15 already defines those placements. The W2 staged-apply
requirement is unchanged. For the W0/W1 workflow profile (which rejects
`agent` tasks), the resulting safe apply order is:

```text
providers
managed models
MCP services and routes
resource LLM routes referenced by workflows
Workflow Definitions
LLM ingress routes that pin Workflow revisions
agents
Agent routes
VirtualKeys
CLI auth configuration
```

This order works because:

- Workflow Definitions can validate concrete MCP services and resource LLM
  routes before an ingress route pins their revision;
- builtin Agents can validate their referenced LLM routes and MCP services;
- ACP Agents validate their inline `runtime.acp` config without a cross-object
  service reference;
- HTTP Agents can validate their own configuration;
- AgentRoutes can then validate `agent_id`;
- VirtualKeys can finally validate every `allowed_route_id`.

Workflow W2 introduces valid Agent/resource-route/workflow reference cycles.
At that point this linear apply order is replaced by the transactional staged
prospective-snapshot validation defined in
[`workflow-runtime.md` §15](../design/workflow-runtime.md#15-validation-and-safety).
The one-way `AgentRoute.agent_id` ownership relationship itself remains
acyclic.

Bundle-local validation rejects an AgentRoute whose `agent_id` is absent when
the bundle contains an `agents` section that is authoritative for the apply.
Partial bundles may resolve an existing Agent through the Admin API at apply
time, following the existing partial-reference convention.

Export emits Agent-owned ACP config and `agentRoutes`; it never reconstructs
`acpServices` or old runtime-specific route families.

### 6.9 Caddy and standalone enablement

Replace dispatcher subdirectives:

```caddy
agent_route_dispatcher {
    acp
    builtin
}
```

with:

```caddy
agent_route_dispatcher {
    agent
}
```

The Caddy module field becomes `EnableAgent`. `HandlerOptions` follows the same
shape. The final validator error names `llm_api`, `mcp`, and `agent` only.

The standalone server must explicitly enable Agent dispatch once its bootstrap
wires the same Agent manager, route resolver, and available backend registry as
the Caddy app. Standalone parity is part of completion, not a follow-up.

### 6.10 Route observability cutover

Route dimensions become runtime-neutral:

```text
route_kind     = agent
route_protocol = agent
agent_id       = AgentRoute.agent_id
runtime_type   = acp | builtin | http
```

Do not collapse runtime-specific event payloads merely because routes unify:

- ACP extensions retain agent type, thread/session, config, and
  permission dimensions;
- builtin extensions retain run/checkpoint/topology/materialization dimensions;
- HTTP adds outbound endpoint/operation dimensions without recording secrets.

The observer can choose the typed event family from a runtime extension rather
than `route_kind`. Existing `acp_usage_events` and `builtin_usage_events` remain
valid runtime event stores. The cross-protocol interaction query remains the
place where callers see a uniform Agent interaction stream.

ACP Admin audit events may continue using their ACP/admin dimensions because
they are runtime operations, not AgentRoute ingress.

Prometheus and OTLP add `runtime_type` only where cardinality is bounded. Never
use Agent endpoint URLs, session IDs, or request IDs as Prometheus labels.

## 7. Validation and Failure Contract

The unified surface fails closed with stable errors:

| Condition | Status | Error type |
|---|---:|---|
| no route match | pass through / 404 | unchanged |
| route disabled | 403 | `route_disabled` |
| VirtualKey rejected | 401/403 | `virtual_key_rejected` |
| target Agent missing | 404 | `agent_not_found` |
| Agent disabled | 400 | `agent_disabled` |
| runtime backend not linked | 501 | `runtime_not_executable` |
| operation unsupported | 501 | `capability_not_supported` |
| invalid/unsupported request field | 400 | `invalid_request` / `unsupported_option` |
| backend saturated | 429 | backend-specific normalized limit error |
| pre-stream upstream failure | 502/504 | normalized backend error |
| mid-stream failure | HTTP 200 + terminal SSE `error` | span marked failed |

Runtime adapters translate native errors into this common envelope without
discarding diagnosable backend detail from logs and typed usage extensions.

## 8. Delivery Sequence

Each milestone must leave the tree compiling and its touched package tests
passing. M0-M3 intentionally preserve the current public route surface while
moving Agent-bound execution behind the new contract. A legacy ACP service and
route that are not bound to exactly one Agent remain on the existing native
ACP dispatch path until the M5 breaking cutover; they have no Agent identity
that can truthfully satisfy `Backend.ServeTurn`. M4-M7 perform the breaking
public cutover. Temporary internal adapters are not public compatibility
promises.

### M-1 — Implementation readiness gate

M0 does not begin until the following repository preparation is complete:

- the `TurnOptions` v1 envelope and internal-only `ExecutionOptions` split in
  §5.3 are reflected in `agents-control-plane.md`;
- the opaque-token permission continuation lifecycle in §5.8 is reflected in
  permanent design documentation;
- the common run registry/tombstone bounds in §5.7 are accepted as the M3
  process-lifetime idempotency contract;
- Agent lookup is assigned to a deep-cloned definition snapshot owned by
  `agent.Manager`, refreshed atomically on create/update/delete/refresh;
- the M2 unbound-ACP exception is covered by explicit parity tests and marked
  for mandatory removal in M5;
- the SQLite legacy-shape detector runs before every schema registration,
  migration, Agent decode, snapshot refresh, or other database write;
- the release test manifest schema below is accepted; each native
  cancellation/permission/session test run will record the exact `codex-acp`
  and `opencode` binaries, their versions or immutable artifact digests, and
  the required opt-in environment;
- the plan header is reconciled to the implementation branch and `dev`
  baseline.

Verification:

- a contract test matrix exists for turn decoding, terminal event ownership,
  permission decision/expiry/cancel races, run tombstone idempotency, bound and
  unbound legacy ACP dispatch, and disabled/non-executable AgentRoute targets;
- `go test ./...` and `go vet ./...` pass on the reconciled baseline;
- `git diff --check` passes.

Every native ACP release run archives a machine-readable manifest with at least:

```yaml
gateway_commit: <full sha>
go_version: <go version>
os_arch: <goos/goarch>
codex_acp:
  command: codex-acp
  version: <reported version or unknown>
  sha256: <binary digest>
opencode:
  command: opencode
  version: <reported version>
  sha256: <binary digest>
environment:
  AGW_ACP_SMOKE: "1"
  AGW_ACP_SMOKE_PROMPT: "<0-or-1>"
tests:
  - name: <test name>
    result: pass
    duration: <duration>
```

If a binary cannot report a version, its SHA-256 digest is mandatory and is
the immutable identity. Secrets, tokens, prompts containing private data, and
environment values other than the allowlisted test toggles are never written
to the manifest.

The implementation contract-test matrix is:

| Contract | First gate | Required fixtures/assertions |
|---|---|---|
| Turn decoding | M1/M2 | absent options, v1 empty runtime, unknown version, unknown runtime field, foreign-runtime field, legacy ACP/builtin translation |
| Event ownership | M1/M2 | concurrent producer serialization, one terminal per segment, duplicate terminal suppression, builtin HITL sequence continuation |
| Error mapping | M1/M2 | pre-stream HTTP status, mid-stream terminal SSE error, cancellation as terminal `done`, redaction |
| Legacy ACP transition | M2/M5 | uniquely bound route uses adapter; unbound route preserves native wire; multiply-bound state fails closed; M5 has no native exception |
| Run control | M3 | exact active cancel, unrelated-run isolation, repeated terminal cancel, unknown run, TTL/cap eviction, ACP force and builtin force/graceful |
| Permission broker | M3 | decision/decision, decision/expiry, expiry/cancel, adapter failure, definition update, runtime switch, shutdown, lost opaque token, ACP active-stream delivery, builtin Admin-decide/new-stream resume |
| Agent snapshot | M4 | no store read on dispatch, deep-clone mutation isolation, generation swap, update/delete invalidation |
| AgentRoute target | M4/M5 | missing target rejected; disabled and non-executable target persisted; dispatch fails before backend |
| Legacy store | M5/M7 | every legacy family and mixed fixture detected before writes; database unchanged; remediation complete |
| Migration helper | M5/M7 | explicit/generated IDs, VirtualKey rewrites, collision, orphan/multi-bind, unrelated-object byte preservation, no partial output |

### M0 — Runtime API contracts and registry

Implementation status: complete.

- add `pkg/agent/runtimeapi` with backend, optional capability, request, event,
  capability, runtime-summary, health, and normalized error types;
- add a backend registry with duplicate/missing registration validation;
- wire the registry into `AgentGateway` without changing dispatch behavior;
- add a fake backend test kit used by dispatcher/Admin contract tests;
- document dependency boundaries in package comments and architecture tests.

Verification:

- backend registry unit tests;
- duplicate and unknown runtime types fail closed;
- optional capability interface detection tests;
- package dependency check proves `pkg/acp`, LLM, and MCP do not import
  `pkg/agent`;
- `go test ./pkg/agent/... ./pkg/gateway/...`.

### M1 — Common identities, events, and errors

Implementation status: complete.

M1 is foundational runtime work, not only shared type extraction. The common
sink/sequencer must outlive one SSE segment, and the builtin checkpoint record
must carry its cursor into resume. ACP and builtin adapters must not number
events independently. M1 proves this with fake multi-segment backends; M2
proves it against the real builtin HITL path. Cross-process durability remains
an M10 responsibility as stated in §5.3.

- generate stable `run_id` at the Agent execution boundary;
- define the common TurnRequest and TurnEvent envelope;
- assign run-scoped event sequence numbers, segment indexes, and
  exactly-one-terminal-per-stream semantics;
- propagate Agent/run/session/request and trace identities through context;
- define normalized typed errors and HTTP/SSE mappings;
- extend usage dimensions and structured logging with `runtime_type` and
  `run_id` without changing route kind yet;
- migrate all four SQLite usage tables additively as specified in §5.12 and
  update writers, query filters, scans, indexes, and migration fixtures;

Verification:

- ID format/uniqueness and context propagation tests;
- event ordering, sequence, and terminal-event property tests;
- fake-backend multi-segment tests prove sequence continues across resume and
  that `(run_id, sequence)` is unique within one process-lifetime run;
- pre-stream versus mid-stream error mapping tests;
- secret/path redaction tests;
- fresh-schema and old-schema SQLite migration tests cover nullable
  `run_id`/`runtime_type` columns and indexes;
- mixed direct/Agent and legacy/new usage fixtures prove null identity columns
  do not drop or misattribute rows, all interaction-union scan destinations
  accept the new columns, existing `agent_id` filters retain their result sets,
  and representative run/runtime filters use the intended SQLite query plans.

### M2 — ACP and builtin backends on the unified runtime SPI

The backend adapters introduced here are the permanent boundary between the
common Agent runtime contract and each native runtime. Only their temporary
invocation through ACPRoute/BuiltinRoute is removed during M4-M7; the adapters
remain behind AgentRoute.

This is not only a thin `/turn` wrapper. The two native `ServeTurn` methods are
similar, but ACP also has `/permission`, `/sessions`, and
`/sessions/{id}/transcript`; builtin has checkpoint-based permission
continuation instead. M2 must unify the turn types and event production, while
M3 must move those asymmetric operations onto capability interfaces and the
common control plane. Estimate and review those as explicit workstreams.

- replace `acpruntime.TurnRequest`/`TurnEvent` and
  `builtinhost.TurnRequest`/`TurnEvent` at the adapter boundary with the common
  `runtimeapi` request/event types; keep native-only data behind validated
  options and typed event payloads;
- implement ACP and builtin turn adapters over their existing managers/hosts;
- bridge `runtimeapi.Identities` and the existing observability
  `usage.InteractionDimensions` once at each adapter boundary so Agent/run/
  session/request and trace identities cannot diverge between common backend
  context and nested usage spans;
- make current `dispatchBuiltin` and every Agent-bound `dispatchACP` call the
  common turn contract, including one shared event sink/sequencer rather than
  independent backend sequence allocation;
- while old routes remain, have the ACP adapter resolve the current
  `service_id` once at the migration boundary and translate its service record
  into the future identity-free `acp.RuntimeConfig`; the common SPI and event
  model never expose `service_id`;
- when a legacy ACPRoute's service is not bound to exactly one Agent, preserve
  its current native ACP turn/permission/session/transcript path through M4.
  It emits no synthetic `agent_id` and is not exposed through Agent-scoped
  capabilities or controls. This is the sole temporary execution exception;
  M5 rejects such input during migration and removes the path;
- preserve all current turn bodies, SSE events, permissions, cancellation,
  usage extensions, and status codes;
- inventory and contract-test the ACP permission/session/transcript subroutes
  before M3 moves them onto optional capability interfaces; they are not
  treated as incidental dispatcher cleanup;
- keep ACPRoute/BuiltinRoute public shapes unchanged during this milestone;
- add Agent manager snapshot/cache lookup if required for a store-free hot path.

Verification:

- existing ACP and builtin dispatcher suites pass without weakened assertions;
- bound ACP routes use the backend adapter, while unbound ACP routes retain
  byte/status/event parity with the pre-M2 native path and never receive a
  synthetic Agent identity;
- adapter parity tests compare old native fixtures with common events/errors;
- builtin HITL resume retains run id, continues run sequence, increments
  segment index, and preserves the Span Link through the common sequencer. This
  is a hard M2 release gate on the real builtin checkpoint/resume path and
  may not be replaced by or deferred to the M1 fake-backend proof;
- ACP native thread/session ids remain correctly bound to the gateway run id;
- request/event compile-time and fixture tests prove native adapters cannot
  bypass the common envelope or allocate their own run sequence;
- the ACP permission/session/transcript inventory pins existing authentication,
  validation, capability checks, status mapping, and transient-connection
  behavior for the M3 migration;
- no config-store `List` occurs per turn;
- disabled, saturated, timeout, permission, and cancellation regressions;
- Caddy and standalone bootstrap tests register the same available backends.

### M3 — Common capability and operator control plane

- implement the fixed opaque-token continuation-binding lifecycle from §5.8;
- implement `GET /admin/agents/{id}/capabilities`;
- add common runtime summary/health to workspace and Agent health responses;
- add Agent-scoped run listing/cancellation;
- add exact `run_id -> native cancel handle` tracking; for ACP this requires a
  new per-turn in-flight registry and native `session/cancel` path inside
  `pkg/acp/runtime`, not delegation to `CloseScope`/`CloseThread` or inference
  from a later I/O failure. ACP advertises force only, while builtin advertises
  force and graceful;
- add Agent-scoped pending permission listing/decision;
- route optional sessions/transcript through capability interfaces while
  preserving only genuinely native ACP runtime diagnostics during migration;
- replace both ACP's pending-request registry path and builtin's checkpoint
  permission registry path with the common broker ownership defined in §5.8;
  native waiters/checkpoints remain opaque continuation state behind adapters;
- route every legacy and common permission decision transport through the same
  atomic broker claim in this milestone, with no dual-write or fallback lookup;
- make unsupported operations return the normalized capability error;
- expose equivalent `agwctl gateway agent` read/control commands.

Verification:

- capability snapshots for ACP, builtin, HTTP-not-executable, disabled, and
  unknown backend cases;
- health reads cause no process creation, materialization, or remote turn;
- cancellation matrix tests pin ACP `force=true/graceful=false`, builtin
  `force=true/graceful=true`, exact-run targeting, idempotency, and no silent
  graceful-to-force downgrade;
- terminal-run tombstone tests cover repeated cancel, active-to-terminal races,
  10-minute expiry, per-Agent cap eviction, and the distinction between a
  retained terminal run and `run_not_found`;
- real `codex-acp` and `opencode acp` end-to-end tests prove ACP force
  cancellation reaches native `session/cancel`, produces the normalized
  cancelled terminal outcome, removes the active cancel binding, retains the
  bounded terminal tombstone, and leaves unrelated active runs/pool entries
  intact; fake backends remain useful for race injection but are not the
  acceptance evidence for native cancellation;
- ACP and builtin permission decision/expiry audit tests;
- concurrent decisions across ACP route/Admin/Agent entry points and builtin
  resume/Agent entry points prove exactly one atomic winner and decision losers
  receive `permission_not_found`; expiry races have the same atomic-winner
  property and a decision losing to expiry receives `permission_expired`;
- sessions/transcript capability tests preserve ACP's authentication,
  validation, status, and transient-connection semantics, while builtin
  returns `capability_not_supported`;
- workspace contains common summary plus intact runtime details;
- unsupported capability requests never fall through or silently no-op.

M0-M3 are the first mergeable foundation boundary. At this point the product
still documents ACPRoute/BuiltinRoute. Agent-bound execution no longer depends
on their native turn implementations below the dispatcher adapter; the
explicit unbound legacy ACP exception remains until M5.

### M4 — AgentRoute model and internal dispatch

- add `pkg/gateway/agentroute` and routecore Agent constants;
- introduce protocol-owned `acp.RuntimeConfig` without service management
  identity fields and key the runtime manager/pool by `agent_id`;
- make the preloaded legacy bound-service snapshot the **only** ACP execution
  config source during M4. Agent inline ACP config is not accepted yet, and
  turn dispatch never merges or races legacy and target config sources;
- keep the temporary legacy-record translator thin: translate once at
  definition refresh into the same canonical `acp.RuntimeConfig` shape that M5
  will populate from Agent definitions, then discard the service-shaped input.
  Pool keys, config fingerprints, retirement, and turn execution consume only
  the canonical shape and contain no source-specific branch;
- add target encode/decode, normalization, validation, deterministic IDs, and
  resolver tests;
- wire one AgentRoute resolver and `dispatchAgent` into `AgentGateway` and the
  dispatcher;
- resolve Agent, capability, backend, identity, policy, and attribution before
  execution;
- initially exercise AgentRoute in tests/internal fixtures before exposing its
  Admin/bundle surface.

Verification:

- route normalization/round-trip and deterministic ID tests;
- matcher collision and priority tests across every route family;
- the same AgentRoute schema executes ACP and builtin Agents;
- the Agent snapshot supplies identity/runtime selection, while ACP execution
  obtains configuration exclusively from the canonical preloaded snapshot
  produced from the legacy bound-service record during M4; these are not two
  competing ACP configuration sources, and no per-turn config-store read
  occurs;
- tests prove no second inline source is accepted during M4 and that one
  definition refresh atomically replaces the Agent-to-runtime-config snapshot;
- an ACP config update retires the prior fingerprint and prevents stale pooled
  instances from accepting new turns;
- a runtime-type change preserves route ID, URL, and VirtualKey allowlist;
- AgentRoute create/update rejects a missing target, permits disabled and
  currently non-executable targets, and dispatch returns the normalized
  `agent_disabled`/`runtime_not_executable` error without invoking a backend.

### M5 — Public control-plane and bundle cutover

- expose AgentRoute Admin handlers, admin client, and CLI commands;
- remove the temporary unbound legacy ACP native dispatch exception; after this
  point every Agent turn ingress resolves an Agent and a registered backend;
- replace `Agent.runtime.acp.service_id` with Agent-owned ACP runtime config and
  remove `OwnsService` from the persisted/public Agent schema;
- atomically switch the ACP adapter's sole snapshot input from legacy bound
  services to `Agent.runtime.acp`; remove the legacy snapshot builder in the
  same change, including the temporary legacy-record translator. The canonical
  `acp.RuntimeConfig` consumer, fingerprint, and retirement path remain
  unchanged; the runtime never overlays, prefers, or falls back between two
  config sources;
- replace bundle fields, validation, apply/export, summaries, and examples,
  removing `acpServices` as well as `acpRoutes`/`builtinRoutes`;
- ship the versioned offline bundle migration helper defined in §9; it consumes
  an old-binary export, emits the new bundle plus an old-to-new route ID map,
  and rewrites every VirtualKey `allowed_route_ids` reference atomically;
- remove `/admin/acp/services`, `agwctl gateway acp-service`, and the
  `acp_services` config-store registration; Agent CRUD is the only write
  surface for ACP runtime config;
- remove ingress route IDs from Agent persistence and validation;
- derive workspace ingress routes from AgentRoute targets;
- reject Agent deletion while targeted;
- change bundle apply order;
- replace Caddy dispatcher flags with `agent` and enable standalone parity;
- switch all data-plane Agent ingress to `kind=agent`.

Verification:

- Admin CRUD and target-reference tests;
- full and partial bundle validate/apply/export tests;
- migration-helper golden tests cover explicit and generated route IDs,
  VirtualKeys referencing multiple route families, orphan/multiply-bound ACP
  services, collisions, and byte-for-byte preservation of unrelated objects;
- Agent ACP config CRUD/validation and absence of every ACP service surface;
- ACP execution resolves entirely from the Agent snapshot after the public
  schema cutover;
- source-isolation tests prove legacy service mutations cannot affect execution
  after cutover and inline Agent config could not affect execution before it;
- ACP Agent update/delete/runtime-switch tests prove config-fingerprint
  retirement and no orphan pool/pending permission survives;
- delete-reference and runtime-change tests;
- Caddyfile adaptation tests;
- Caddy and standalone end-to-end turn/capability tests;
- explicit old-store detection returns the documented actionable error;
- starting or validating against a fixture database containing
  `acp_services`, `Agent.runtime.acp.service_id`, and `kind=acp`/`kind=builtin`
  rows fails with `legacy_agent_runtime_config` and the export/migrate/apply
  remediation from §9; no old row is modified or deleted.

### M6 — Observability route cutover

- emit `route_kind=agent`, `route_protocol=agent`, and `runtime_type`;
- retain typed ACP/builtin extensions and storage;
- update summary/filter/query behavior;
- update Prometheus and OTLP mappings;
- remove route-to-Agent inference from Agent ingress;
- stop emitting or querying active ACP identity by `service_id`; new ACP events
  use `agent_id` directly, while any retained SQL column is historical-only;
- correlate run, permission, cancellation, and lifecycle events.

Verification:

- typed event persistence for both executable backends;
- Agent/run-filtered workspace/activity/usage queries;
- nested builtin LLM/MCP parentage and HITL span-link regression tests;
- ACP Admin audit event regression tests;
- bounded-label Prometheus assertions;
- OTLP spans contain common identities without secrets.

### M7 — Remove old route surface and align released documentation

- M5 has already made the legacy API/store/dispatch surfaces unreachable and
  removed their registration; M7 owns physical source deletion, stale naming
  removal, negative source assertions, and released documentation;
- delete `pkg/gateway/acproute` and `pkg/gateway/builtinroute`;
- delete `pkg/acp/service`, service managers/schemas/Admin/client/CLI/bundle
  surfaces, and service-to-Agent indexes after reusable validation/config types
  move to a protocol-owned runtime-config package;
- rename remaining ACP-internal identifiers whose `Service` name meant the old
  management object (for example adapter open requests and pool-count helpers)
  to runtime/owner terminology; this does not rename MCP services;
- delete runtime-specific dispatch entrypoints after reusable logic lives only
  in adapters;
- remove old constants, Admin paths, CLI commands, bundle fields, dispatcher
  flags, Agent fields, examples, and tests;
- explicitly remove
  `DELETE /admin/builtin/runtime/turns/{agent_id}/{session_id}` as the legacy
  logical-run cancel path replaced by
  `DELETE /admin/agents/{id}/runs/{run_id}`; retain builtin runtime inspection
  only where it exposes host/materialization diagnostics rather than a second
  logical control operation;
- ensure no old public identifier remains in current-behavior docs.

Removal verification:

```bash
rg 'ACPRoute|BuiltinRoute|acpRoutes|builtinRoutes|acpServices|acp_services|acp-service|/admin/acp/services|runtime\\.acp\\.service_id|pkg/acp/service|acpservice|ACPServiceID|OwnsService|acp_route_ids|builtin_route_ids|EnableACP|EnableBuiltin'
go test ./...
go vet ./...
make build
```

Expected `rg` matches are limited to historical context explicitly labeled
pre-unification, the old-database rejection fixture, and the versioned offline
migration helper. Current API/reference text and runtime compatibility paths
have no matches.

The release gate also performs the reverse enablement check: positive
configuration/bootstrap tests and targeted source assertions must show
`EnableAgent` and the `agent` directive wired through the dispatcher, Caddyfile
adapter, and standalone server. Absence of `EnableACP`/`EnableBuiltin` alone
does not prove that Agent dispatch is reachable.

The full M5 old-store fixture test remains in the M7 release gate. Run it
against the built release binary, not only a schema helper, and assert the
documented `legacy_agent_runtime_config` error plus export/migrate/apply
remediation. Also assert that the database is unchanged after the failed
startup.

Update permanent documentation in the same change that removes the old route
surface:

- `README.md`;
- `AGENTS.md`;
- `docs/architecture/architecture-overview.md` and its SVG;
- `docs/design/agents-control-plane.md`;
- `docs/design/builtin-agent-runtime.md`;
- `docs/design/workflow-runtime.md`;
- `docs/architecture/acp-architecture.md`;
- `docs/reference/{route-schema-reference,admin-api-reference,agwctl-reference}.md`;
- `docs/reference/{acp-api,acp-technical-spec}.md`;
- `docs/getting-started/quickstart-acp.md`;
- relevant bundle/routing guides;
- `examples/Caddyfile.example`;
- `examples/gateway.bundle.{acp,builtin}.yaml`, replaced by Agent-owned runtime
  config plus AgentRoute examples.

Website release edits must be paired English/Chinese changes:

- describe one stable Agent route across runtimes;
- update diagrams and Caddy/config snippets;
- keep “two execution runtimes plus HTTP identity” until M8 ships;
- keep cross-Agent workflow claims on the roadmap;
- update observability wording from runtime-specific route ownership to direct
  AgentRoute attribution.

Documentation verification:

- every example uses `agentRoutes`, `kind: agent`, and `protocol: agent`;
- every ACP Agent example stores config under `runtime.acp` and contains no
  `service_id` or `acpServices`;
- no current guide instructs users to create ACPRoute or BuiltinRoute;
- all English website claims have matching Chinese claims;
- snippets match test-backed bundle/Caddy examples.

### M8 — HTTP execution backend

This milestone completes three-runtime execution. The common runtime and route
foundation remains complete and releasable without it.

- finalize the outbound HTTP Agent protocol and `auth_ref` resolver;
- implement the backend with streaming/timeouts/limits/trace propagation;
- add health and typed usage instrumentation;
- run contract and integration tests;
- only then change website wording from two to three execution runtimes.

Verification includes a real streaming test server, cancellation and timeout
tests, auth-ref redaction, response size/content-type enforcement, trace/depth
propagation, and retry/idempotency assertions.

### M9 — Common policy and external resource enforcement

- move runtime-neutral concurrency/timeout/budget policy enforcement into the
  common backend wrapper without duplicating backend operational config;
- define scoped callback identity for ACP/HTTP Agents;
- turn external Agent LLM/MCP/VirtualKey resource references into enforced
  entitlements;
- preserve builtin's stricter definition-time resource validation;
- update website claims only after external enforcement is real.

### M10 — Durable Workflow Agent Task integration

This milestone maps to Workflow Runtime W2; it does not add another state
machine:

- implement the Workflow `agent` task handler over `runtimeapi.Backend`;
- persist Workflow Run/Task Run state and ordered task events, including the
  next run-sequence cursor transactionally and unique `(run_id, sequence)`;
- propagate the Workflow logical execution key through `TurnOptions`; reject
  retry or `requeue` when a backend cannot enforce idempotency;
- link Workflow cancellation and permission suspension/resume by `run_id`;
- retain scheduling and multi-agent handoff as Workflow Runtime W3.

Verification:

- durable cursor recovery continues sequence and segment indexes across a
  simulated process restart;
- the persisted uniqueness constraint rejects duplicate
  `(run_id, sequence)` task events.

M8-M10 are post-route capabilities. They may ship after the M7 foundation in
separate reviewable changes without changing the contracts defined here.

## 9. Breaking Cutover

No runtime aliases, dual Admin endpoints, dual bundle fields, or legacy route
kinds are retained.

Existing v0.4.x ACP config stores can contain persisted ACP services,
`Agent.runtime.acp.service_id`, and `kind=acp` route rows. Pre-release v0.5
development stores may additionally contain `kind=builtin` route rows. The new
binary must detect those shapes during startup or validation and return an
actionable error; it must not silently ignore, delete, or reinterpret them.
Operators export with the old binary, mechanically merge each bound service
record into its Agent's `runtime.acp`, rewrite ingress as `agentRoutes`, and
apply to a clean/new-version store.

Detection is a read-only bootstrap phase, not an ordinary schema migration.
For the current SQLite backend it uses a dedicated read-only/query-only
connection before the normal write-capable backend is opened and before
config-store schema registration, usage-schema migration, Agent decode,
manager refresh, static provisioning, retention cleanup, or any other
write-capable startup component. It inspects `sqlite_master`, the relevant
config tables, and stored JSON using read-only queries. The detector first
collects every legacy family and offending id, closes the connection, and only
then returns the normalized error. A detector error also aborts startup before
writes. Tests compare the logical dump plus hashes/metadata of every
pre-existing database/WAL file before and after failed startup; checking only
the rows returned through the new schema is insufficient.

If the configured database file does not exist, the detector records a clean
new-store result without creating it; the normal backend may create it only
after the preflight returns successfully.

The current product has no other persisted config backend. Any future backend
must implement an equivalent pre-registration read-only detector before it can
support this release transition; absence of that detector fails closed rather
than assuming the store is clean.

The stable error code is `legacy_agent_runtime_config`. Its message must name
every detected legacy object family, state that the store was left unchanged,
and point to the release migration procedure. Detection happens before any
new-version config write.

The release notes must provide the mechanical mapping:

```text
Agent.runtime.acp.service_id + matching ACP service config
  -> Agent.runtime.acp.<inline runtime config>

ACPRoute.service_id -> owning Agent.id
  -> AgentRoute.agent_id

BuiltinRoute.agent_id
  -> AgentRoute.agent_id
```

An ACP service that is not bound to exactly one Agent cannot be migrated
implicitly: the operator must create a target Agent or delete the orphan before
apply. Conflicting or multiply-bound service references fail migration. Service
metadata maps as follows: Agent `id`/`name`/`description`/`disabled`/timestamps
remain Agent-owned, except migration sets target `Agent.disabled` to
`agent.disabled || service.disabled` fail-closed. Service `name`,
`description`, and timestamps are discarded. Only execution fields
(`agent_type`, cwd/roots, model, environment, overrides, pool, permission, and
agent-specific adapter config) move under `runtime.acp`.

The release notes and migration-helper error output must list each offending
service ID and the Agent IDs, if any, that reference it. They must include both
of these old-binary remediation recipes rather than only stating the rule:

```bash
# Inspect an orphan and every route that may still reference it.
old-agwctl gateway acp-service get <service-id>
old-agwctl gateway acp-route list

# To delete it, export first. Remove the affected route IDs from every
# VirtualKey allowed_route_ids, then validate/apply those VirtualKey updates.
# Bundle apply has no prune semantics, so it does not delete the route/service.
old-agwctl gateway export -f legacy.bundle.yaml
old-agwctl gateway validate -f legacy.bundle.yaml
old-agwctl gateway apply -f legacy.bundle.yaml

# Explicitly delete every ACPRoute whose service_id is <service-id>, then the
# service itself. Repeat the route command for every matching route ID.
old-agwctl gateway acp-route delete <route-id>
old-agwctl gateway acp-service delete <service-id>

# Or bind it to exactly one Agent in an exported old-schema bundle.
old-agwctl gateway export -f legacy.bundle.yaml
# Edit legacy.bundle.yaml: add or choose exactly one Agent whose
# runtime.type is acp and runtime.acp.service_id is <service-id>; remove that
# service_id from every competing Agent, deleting or assigning those Agents a
# valid different runtime as appropriate.
old-agwctl gateway validate -f legacy.bundle.yaml
old-agwctl gateway apply -f legacy.bundle.yaml
```

The operator must re-export after either repair and feed that fresh export to
the migration helper. For a multiply-bound historical record, the helper never
chooses a winner. For an `OwnsService=false` reference, binding is still
determined by the unique `runtime.acp.service_id`; provenance does not make the
service an orphan or authorize deletion.

VirtualKey `allowed_route_ids` can remain unchanged when explicit route IDs are
kept. Auto-generated route IDs change from `acp:`/`builtin:` to `agent:`, so
their VirtualKey references must change with them.

The cutover release ships a versioned offline helper at
`scripts/migrate-unified-agent-runtime`. The operator workflow is explicit and
scriptable:

```bash
# Run with the old binary while the old store is still authoritative.
old-agwctl gateway export -f legacy.bundle.yaml

# Run from the cutover release; it never connects to or mutates either store.
scripts/migrate-unified-agent-runtime \
  --input legacy.bundle.yaml \
  --output unified.bundle.yaml \
  --route-map unified.route-map.yaml

# Run against a clean/new-version store with the new binary.
agwctl gateway validate -f unified.bundle.yaml
agwctl gateway apply -f unified.bundle.yaml
```

The helper computes each resulting AgentRoute ID before writing output,
records every `old_route_id -> new_route_id` pair in the route-map file, and
rewrites all matching VirtualKey `allowed_route_ids` entries in the same
operation. Explicit IDs normally map to themselves. Generated IDs map from
`acp:`/`builtin:` to `agent:`. Missing references, duplicate target IDs,
unbound/multiply-bound ACP services, or a VirtualKey reference that cannot be
resolved are fatal; the helper emits no partial output and reports the
offending IDs plus the applicable inspect/delete or export/edit/validate/apply
recipe above. This helper is a one-shot migration artifact, not a legacy
runtime/API alias.

## 10. Acceptance Criteria

The common runtime foundation is complete when:

- ACP and builtin execution both go through registered runtime backends;
- common run/session/request identities propagate through events, controls,
  logs, usage, and traces;
- event streams are ordered and have exactly one terminal event;
- one logical run has a globally unique monotonic sequence across every resume
  segment within its advertised durability boundary (process lifetime through
  M7, including builtin HITL resume; process restart after M10);
- normalized errors preserve pre-stream HTTP and mid-stream SSE behavior;
- capability discovery is authoritative and unsupported operations fail closed;
- workspace and health expose common summaries without creating runtime work;
- Agent-scoped run cancellation and permission operations work for every
  capability-advertising backend;
- runtime-specific session, transcript, permission, process, topology, and
  observability details remain intact.

The routing cutover is complete when:

- only `kind=agent` publishes Agent turn ingress;
- ACP and builtin Agents are both callable through the same AgentRoute schema;
- ACP runtime configuration is stored only on `Agent.runtime.acp`, every pool
  and diagnostic entry is owned by `agent_id`, and no ACP service
  store/API/bundle/CLI concept remains;
- an Agent runtime change preserves route ID, URL, and VirtualKey allowlist;
- an Agent runtime change preserves the common `/turn` core but requires clients
  to refresh optional capabilities; permission transport is not claimed to be
  portable before the §11 decision lands;
- Agent ingress attribution always has the target `agent_id` without reverse
  ownership inference;
- bundle/Admin/CLI/Caddy expose one Agent route family;
- standalone and Caddy have equivalent Agent ingress behavior;
- ACP-specific and builtin-specific capabilities still behave as documented;
- old route shapes and aliases are absent from code and current docs;
- website English and Chinese pages describe only shipped behavior;
- `go test ./...`, `go vet ./...`, and `make build` pass.

HTTP joins the executable acceptance criteria only after M8. Before that, all
three identity types share AgentRoute, while the product truth remains two
execution runtimes plus one external identity model.

External resource enforcement has its own M9 gate; until then ACP/HTTP resource
references remain a management view. Durable task execution remains deferred
until M10 / Workflow W2, and scheduling plus handoff remain deferred until
Workflow W3.

## 11. Decision Status Before Implementation

The decisions required by M0-M7 are closed:

1. The common turn decoder uses the versioned `TurnOptions` v1 envelope in
   §5.3. Legacy route bodies are translated at their dispatcher boundary; the
   AgentRoute wire does not publish a flat runtime superset.
2. The common permission broker uses the opaque backend token and lifecycle in
   §5.8. There is no registered callback variant or native claim fallback.
3. M4 extends `agent.Manager` to own a full, deep-cloned definition snapshot.
   `GetSnapshot(id)` and `Snapshot()` read only that immutable generation;
   create/update/delete build and validate a complete prospective generation
   before the store write, then commit the already-infallible local generation
   swap under one lock after the store operation succeeds. `Refresh` decodes
   and clones the complete store result before its swap. Pointer, slice, map,
   topology, middleware, environment, and credential-reference fields must not
   alias caller or config-store objects. Runtime adapters receive a value from
   one generation for the whole operation. Definition lifecycle listeners run
   while new snapshot reads are excluded: safety-sensitive ACP changes retire
   the old fingerprint/pending state before the new generation becomes
   dispatchable, and their local retirement path must not fail.
4. M2 preserves the explicit unbound legacy ACP native path described in §8.
   It is removed, not generalized, at M5.
5. The process-lifetime terminal-run idempotency boundary is the bounded common
   tombstone registry in §5.7.

The following decisions are deliberately deferred because they do not affect
M0-M7:

1. M8 defines the generic secret source and exact meaning of HTTP `auth_ref`.
2. M8 decides whether HTTP usage receives a new typed table or a common Agent
   event table; ACP and builtin typed tables remain unchanged either way.
3. Permission-aware clients continue following capability-advertised
   runtime-specific `resume_mode` through M7. A later design may introduce one
   asynchronous portable continuation protocol before the product claims
   drop-in permission-wire substitution.

Reopening a closed M0-M7 decision requires updating this plan, the corresponding
permanent design contract, and the affected milestone acceptance tests before
implementation continues.
