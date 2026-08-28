# Builtin Agent Runtime

## 1. Purpose

This document is the authoritative product and technical design for
`runtime.type = "builtin"`: agent definitions materialized and executed
inside the gateway process by the eino ADK host.

The builtin runtime is implemented through PB1 and PB1b. PB2 remains deferred.
This document owns the builtin definition schema, materialization and session
lifecycle, topology and middleware behavior, route protocol, observability,
interactive permissions, operator cancellation, implementation status, and
remaining builtin-specific questions.

Related documents deliberately own different concerns:

- [Agents Control Plane](agents-control-plane.md) defines the shared `Agent`
  identity, resources, policy, attribution, runtime-backend contract, and
  external Workflow Activity boundary across `acp`, `http`, and `builtin`.
- [Unified Agent Runtime and Routing](../plans/unified-agent-runtime.md)
  defines the turn-first `agentruntime.Backend` adapter, common capability/event
  plane, and breaking migration from BuiltinRoute/ACPRoute to AgentRoute.
- [Eino Capability Reuse](eino-reuse.md) records which eino/eino-ext
  capabilities the repository adopts, defers, or rejects and why.
- This document defines how the builtin runtime itself works. Other documents
  should summarize and link here rather than duplicate its schema or lifecycle.

## 2. Position in the agent control plane

The gateway is an external agent control plane first. Hosting in-process
agents is a bounded extension of that product position, not a return to the
removed `pkg/llm/agent` reasoning loop.

The gateway owns the builtin agent definition, lifecycle, governance,
resources, and observation, while eino ADK owns the reasoning loop,
interrupt/resume machinery, cancellation primitives, and parameterizable
multi-agent topologies. Builtin remains the third runtime type behind the same
`Agent` model:

- `acp`: the gateway owns an external process lifecycle.
- `http`: an external service owns its lifecycle.
- `builtin`: there is no separate process; the gateway materializes a
  persisted definition in-process.

LLM and MCP remain agent resources, not runtime types. The builtin definition
binds models through owned LLM routes and tools through declared MCP services.
Cross-runtime concepts and cardinality rules remain authoritative in
[Agents Control Plane](agents-control-plane.md).

## 3. Model: the agent is data, not a program

eino ADK is a library, not a runnable agent binary, so the builtin runtime
cannot "start" an agent the way the `acp` runtime spawns `codex-acp`. Instead
the gateway ships **one generic ADK host**, compiled into `agw`/`agwd`, and a
builtin agent degenerates into a persisted definition under
`runtime.builtin`. "Starting" the agent means the host materializes the ADK
object graph — `adk.ChatModelAgent` plus tool adapters plus topology, driven by
an `adk.Runner` — from that definition, in-process. No new process, no new
binary. This is the `provider_type` pattern lifted to the agent layer:
compile-time capability, runtime configuration.

## 4. Definition schema

```json
"runtime": {
  "type": "builtin",
  "builtin": {
    "model": { "llm_route_id": "chat-main", "model": "smart",
               "retry": { "max_retries": 2 } },
    "system_prompt": "You are a triage agent...",
    "generation": { "max_tokens": 8192, "temperature": 0.2 },
    "tools": [
      { "mcp_service_id": "filesystem-tools", "tools": ["read_file", "list_dir"] }
    ],
    "topology": {
      "kind": "single",
      "sub_agents": []
    },
    "middlewares": {
      "summarization": { "enabled": true },
      "agentsmd": {
        "enabled": true,
        "docs": [ { "path": "AGENTS.md", "content": "# Rules\n..." } ]
      },
      "reduction": { "enabled": true, "max_tokens_for_clear": 120000 },
      "toolsearch": { "enabled": true },
      "plantask": { "enabled": true },
      "skill": {
        "enabled": true,
        "skills": [ { "name": "pdf-report", "description": "...", "content": "# Steps\n..." } ]
      },
      "patchtoolcalls": { "enabled": true }
    },
    "limits": { "max_concurrent_turns": 4, "turn_timeout_seconds": 600 }
  }
}
```

Schema rules:

- **`model` resolves through an LLM route**, not a raw provider. The host
  enters ADK via the provider → `ToolCallingChatModel` adapter over the
  route's `RoutedProvider`, so credential scheduling, candidate fallback,
  retry classification, and LLM usage events apply unchanged. The referenced
  route must appear in `routes.llm_route_ids` (route-binding uniqueness then
  keeps attribution unambiguous, per [agent attribution](agents-control-plane.md#56-agent-attribution)). The
  optional `retry` block (`max_retries`, 1–5) adds node-level ADK retry on
  top: RoutedProvider's fallback advances between candidates within one
  call, and `retry` re-runs the whole call after that fallback is exhausted,
  with retryability mirroring the gateway's failure classification (429 and
  5xx only) and the ADK default backoff. Sub-agents inherit it with the
  model reference; planexecute role models reject it (the eino prebuilt
  exposes no retry seam — validated at apply time, backstopped at
  materialization for the inherited path).
- **`tools` reference gateway-managed MCP services** and enter ADK via the
  MCP → `InvokableTool` adapter over `pkg/mcp/service` — in-process, no HTTP
  loopback. MCP tool policy applies exactly as it does for route-dispatched
  MCP traffic. Referenced services must appear in
  `resources.mcp_service_ids`.
- **`topology.kind`** enumerates what ADK exposes as parameterizable
  structure: `single`, `sequential`, `parallel`, `loop`, `supervisor`,
  `planexecute`, `deep`. `topology.max_iterations` bounds the kind's
  iteration loop (loop rounds, planexecute execute-replan rounds, deep
  reasoning iterations); 0 uses the ADK default.
- **`planexecute` configures its roles through `topology.plan_execute`**
  (`planner`/`executor`/`replanner`), not `sub_agents`. Every role inherits
  the node's model unless it overrides `model`/`generation`; the executor is
  the only role that runs MCP tools (its `tools` replace the node's
  selection, and its `max_iterations` bounds the inner tool-call loop). The
  planner and replanner emit plans through forced tool calling, so their
  models always resolve with `RequireTools`. The whole block is optional.
- **`deep` reuses the node's own fields** — model, `system_prompt` as the
  head instruction, tools, and optional `sub_agents` the head fans out to
  through the prebuilt's task tool (the prebuilt adds a general-purpose
  sub-agent by default, so `sub_agents` may be empty). The head always
  resolves with `RequireTools` (it drives `write_todos` and the task tool);
  filesystem/shell backends stay unset — a builtin agent has no workspace to
  hand out.
- **Sub-agents are inline child definitions, not references to other
  `Agent` objects.** Entries in `topology.sub_agents` are nested definition
  objects (same schema, minus `limits`) that exist only as internal nodes of
  this one agent. They have no first-class identity, no separate usage
  attribution, and no admin surface. Referencing another `Agent` as a
  sub-agent would make one agent a fan-out container over other agents'
  runtimes — exactly what the [agent cardinality rules](agents-control-plane.md#51-agent) exclude;
  coordinating first-class agents belongs to an upper-layer Business Workflow,
  not builtin topology.
- **`middlewares`** toggles the ADK middlewares that are safe as
  configuration; each is off by default, and they apply to the root
  definition's chat-model nodes (a single node, the supervisor head, the
  deep head). Registration order is fixed — patchtoolcalls, reduction,
  summarization, skill, plantask, toolsearch, agentsmd — so dangling tool
  exchanges are completed before anything else reads the history,
  tool-output bloat is cleared before summarization counts tokens, tool
  visibility is derived after the context managers settle the history, and
  the agentsmd injection stays invisible to all of them.
  - `summarization` compacts the context with the agent's own model once
    `trigger_tokens` is exceeded.
  - `agentsmd` injects **inline virtual documents** (`docs`, each
    `path` + `content`, with `@import` between docs) transiently at
    model-call time; the injection never enters the session history. Docs
    are inline by design: a builtin agent has no workspace, and host
    filesystem paths would let a config-store object read arbitrary
    gateway-visible files into model context. A host-filesystem variant
    would need gateway-level allowed-roots gating first.
  - `reduction` runs **clear-only**: past `max_tokens_for_clear`
    (a chars/4 estimate), older tool-call outputs become placeholders,
    keeping the most recent `clear_retention_suffix_limit` exchanges and
    skipping `clear_exclude_tools`. Clearing is lossy — the
    truncation/offload phase needs a file backend plus a `read_file` tool
    the agent does not have, so it stays disabled until a workspace design
    lands.
  - `toolsearch` withholds the node's MCP tools from the model's tool list
    and exposes a `tool_search` meta-tool that loads them on demand — for
    definitions whose MCP services expose many tools. Requires the
    definition to declare `tools`. Client-side search only (the
    model-native variant needs deferred-tool support the gateway's
    providers do not expose), and the tool list changing between calls can
    invalidate the upstream prompt cache. When reduction is also enabled,
    `tool_search` results are auto-excluded from clearing — visibility of
    loaded tools is re-derived from those results on every call.
    Summarization compaction can still swallow them; the model then simply
    searches again.
  - `skill` gives the model a `skill` tool over **inline virtual skills**
    (`skills`, each `name` + `description` + `content`): the tool
    description advertises every skill, and invoking one returns its
    instructions as the tool result. Inline execution only — the schema
    exposes no context/agent/model frontmatter, so fork-mode sub-agent
    execution and per-skill model overrides are structurally unreachable.
    Skills are inline for the same workspace/security reason as agentsmd
    docs. A skill result is an ordinary tool output to reduction: clearing
    may replace it with a placeholder, and the model re-invokes the skill
    if it needs the instructions again (operators can pin skills with
    `clear_exclude_tools: ["skill"]`).
  - `patchtoolcalls` inserts a placeholder tool result for any tool call in
    the history that has no corresponding response, so a structurally
    incomplete history never makes a strict upstream (tool_use/tool_result
    pairing) reject the request. Purely defensive: the host only commits
    successful turn transcripts, but a model that emits tool calls on a
    node without tools produces exactly such a transcript.
  - `plantask` gives the model TaskCreate/TaskGet/TaskUpdate/TaskList tools
    for maintaining a structured task list. The task board is
    **session-scoped in-memory storage** riding on the session object — it
    is evicted with the session, shares the documented restart-loss
    semantics, never leaks between conversations, and is capped (256 files,
    256 KiB per file, 1 MiB total)
    so a runaway model cannot grow it without bound. The task tools stay
    statically visible even under toolsearch, which only gates the node's
    MCP tools.

## 5. Escape hatch: compiled-in custom agents

Definitions cover parameterizable structure. An agent that needs custom Go
logic (bespoke tools, custom graph nodes) uses the repository's established
extension pattern instead: implement a builtin-agent factory SPI, register it
(mirroring `provider.RegisterProviderFactory`), and blank-import the package in
`cmd/agw/main.go`, `cmd/agwd/main.go`, and `cmd/agwctl/cmd_gateway.go` (agwctl
needs it so bundle `validate`/`apply` can check the factory name). The
definition selects it with `topology.kind = "custom"` plus a `factory` name;
validation rejects a factory name absent from the linked registry — the same
contract `provider_type` has today.

## 6. Generic host and lifecycle

Package: `pkg/agent/builtin`. The host owns:

- **Materialization cache**: the ADK object graph is built lazily on first
  use and cached keyed by agent id + `updated_at`. A definition update
  invalidates the cache and re-materializes on the next turn — config-driven,
  no gateway restart. In-flight turns finish on the old graph (drain, not
  cancel).
- **Fault containment**: every turn runs under panic recovery — a panicking
  agent fails that turn with a diagnosable error and must never take down the
  gateway process. `limits.turn_timeout_seconds` bounds a turn;
  `limits.max_concurrent_turns` bounds parallelism per agent; both
  fail-closed (reject, not queue unboundedly).
- **Disabled semantics**: a disabled agent rejects turns with a
  client-correctable `400`, matching the disabled-ACP-service contract.
- **Session state**: eino v0.9.x Runner interrupt/checkpoint state is
  in-memory only; durable session persistence is a v0.10 alpha
  (`eino-reuse.md` §6.2). PB1 ships turns whose session state is
  in-memory with documented restart-loss semantics; durable checkpoints are
  deferred until eino v0.10 stabilizes and must not be hand-rolled before
  that.

Dependency direction stays intact: `pkg/agent/builtin` depends on `eino/adk`,
the two bridge adapters, and the runtime managers. The bridges themselves live
in neutral packages (suggested: `pkg/llm/provider/einomodel` and
`pkg/mcp/einotool`) so they are reusable without importing `pkg/agent`, and the
lower protocol layers still never import `pkg/agent`.

## 7. Execution surface

- **Data-plane ingress**: a builtin agent is exposed through the unified
  `POST /<agent-route>/turn` streaming SSE endpoint, so clients and the
  frontend keep one turn interaction language across runtimes. The event vocabulary is mapped from ADK
  Runner events onto the existing turn event names where semantics align
  (`delta`, `content`, `tool_call`, `usage`, `done`, `error`); whether the
  vocabulary is exactly the ACP set or a marked subset is an open question
  (see [§13](#13-open-questions)).
- **Unified turn adapter**: builtin is registered as a turn-first
  `agentruntime.Backend`; AgentRoute is its only public ingress.
- **External Workflow Activity**: an upper-layer Worker invokes the same
  AgentRoute/turn adapter as any other caller. Temporal or another external
  engine may own durable business state, but it does not make the builtin
  runtime's in-memory session/checkpoint state durable. PB2 is therefore a
  backend persistence capability, not a gateway Workflow layer.

## 8. Observability and attribution

- The dispatcher begins the turn interaction span and stamps the builtin
  route's target `AgentID` explicitly, so builtin attribution never depends
  on the route → agent index and is always exact.
- Inner LLM calls flow through the `RoutedProvider` and produce
  `llm_usage_events` as usual; inner MCP tool calls flow through
  `pkg/mcp/service` and produce `mcp_usage_events`; both inherit `agent_id`
  from the turn context.
- Turn-level events have their own additive event family (route kind
  `builtin`, a `builtin_usage_events` table following the same
  `InteractionEvent` + extension pattern). Do not overload the
  ACP event family: the dimensions differ (no service id, no permission
  flow, topology/sub-agent step counts instead).
- Depth governance: the host carries trace context and agent depth in the
  turn context; nested topology steps and any outbound agent-to-agent calls
  increment depth and are enforced against `policy.max_agent_depth`, the same
  gate the dispatcher applies to inbound `X-Agent-Depth`.

## 9. Interrupt and human-in-the-loop (tool permissions)

Status: implemented (PB1b). This section is the authoritative design; §12
records the implementation notes.

**Problem.** A builtin agent executes MCP tools with external side effects the
moment the model asks. The ACP runtime already has a permission policy
(`deny`/`auto_approve`/`interactive`); builtin currently has only the implicit
equivalent of `auto_approve` over its declared tools. Operators need an
interactive mode where a human approves or refuses individual tool executions.

**Mechanism: ADK checkpoint interrupt/resume, not a blocking wait.** ACP
`interactive` blocks the in-flight turn: the SSE stream stays open, the
decision arrives on a side channel, and the waiting turn holds its resources.
That shape is forced by the external agent process, which cannot be paused.
The builtin host has a better primitive available — the eino ADK Runner's
interrupt/checkpoint/resume cycle (verified against v0.9.12):

- the approval gate wraps the `einotool` bridge at materialization time (the
  same layer as `observedTool`), so it covers every node of every topology,
  including sub-agents and the planexecute executor. On an unapproved call
  the wrapper returns `compose.Interrupt(ctx, info)` (stateless — a rerun
  re-invokes the gate with the same arguments, so there is no internal state
  to restore); the ADK
  ToolsNode folds it into a `CompositeInterrupt`, and the Runner (configured
  with a `CheckPointStore` and a per-turn `adk.WithCheckPointID`) serializes
  the full run state (gob; custom info/state types registered via
  `schema.RegisterName`) and ends the run iterator with an
  `AgentAction.Interrupted` event carrying `InterruptCtx` ids.
- the host resumes with `Runner.ResumeWithParams(ctx, requestID,
  &adk.ResumeParams{Targets: ...})`; the gated tool re-runs, reads the
  decision through `compose.GetResumeContext`, and either executes (allow) or
  returns a plain refusal tool message (deny) — deny is a tool *result*, not
  an error, so the model sees the refusal and continues the turn.

Between interrupt and resume the turn consumes nothing: no goroutine, no open
SSE stream, no `max_concurrent_turns` slot — only a checkpoint blob in memory.
That is the decisive advantage over porting the ACP blocking-wait shape, and
it is why this feature adopts the Runner primitive.

**Definition schema.** A root-level `permissions` block on `runtime.builtin`
(applies to the whole topology; per-node modes are deliberately out of scope):

```json
"permissions": {
  "mode": "auto_approve",
  "timeout_seconds": 600,
  "max_pending": 32,
  "auto_approve_tools": ["<mcp_service_id>/<tool_name>", "..."]
}
```

- `mode`: `auto_approve` (default — exactly today's behavior) or
  `interactive`. There is no `deny` mode: builtin tools are operator-declared
  allowlists already; a fully denied toolset is a definition without tools.
- `timeout_seconds`: pending-decision TTL, fail-closed (default 600).
- `max_pending`: cap on simultaneously pending permissions per agent
  (default 32). Pending permissions hold no turn slots, so without a cap they
  could accumulate without bound.
- `auto_approve_tools`: bypass list in `interactive` mode, entries fully
  qualified as `service_id/tool_name` (bare names could collide across
  services). Validation requires every entry to resolve to a declared tool.
- Gateway-local middleware tools (`skill`, the plantask task tools,
  `tool_search`) are always exempt: they have no external side effects, and
  gating them would interrupt on bookkeeping.

**Turn protocol.** The event vocabulary gains `permission` (still a marked
subset of the ACP vocabulary). When a gated call interrupts:

1. every event in the streamed segment carries the stable logical `run_id`
   and `session_id`; the turn stream emits one `permission` event additionally
   carrying the top-level `request_id` (the checkpoint id, generated per turn),
   while its `data` contains `expires_at` and the pending tool calls — each
   with `call_id` (the model's tool-call id), `mcp_service_id`, `name`, and
   `arguments`; parallel tool calls in one assistant step interrupt together
   and appear as one list;
2. the stream then ends with `done`, `stop_reason: "permission_required"`
   (the turn is suspended, not failed);
3. the client resumes with `POST /<agent-route>/turn` carrying
   `session_id` and a `permission` field instead of `input`:

```json
{
  "session_id": "s-1",
  "permission": {
    "request_id": "...",
    "decisions": [
      {"call_id": "...", "outcome": "allow"},
      {"call_id": "...", "outcome": "deny"}
    ]
  }
}
```

   The response is a new SSE stream continuing the turn to completion.
   Outcomes are `allow` and `deny`; a pending call absent from `decisions`
   is denied (fail-closed). `outcome: "cancel"` on the request level discards
   the checkpoint and ends the turn with `done`, `stop_reason: "cancelled"`,
   committing nothing.

There is no separate builtin `POST /<agent-route>/permission` operation: for ACP that
endpoint delivers a decision into a still-open stream, but for builtin the
decision *is* the continuation request and produces the continuation stream.
Reusing `/turn` keeps one ingress per route; `input` and `permission` are
mutually exclusive in one request.

**Pending-permission lifecycle (all in-memory, all fail-closed).**

- The checkpoint store is an in-process `adk.CheckPointStore` (map keyed by
  `request_id`, with `CheckPointDeleter` for cleanup). This is transient
  interrupt state with the same restart-loss semantics as sessions — it is
  *not* the hand-rolled durable checkpointing that PB2 forbids; when eino
  v0.10 Runner persistence stabilizes, this store is the swap seam.
- Alongside the checkpoint the host keeps the turn's commit set (user
  message, partial transcript, event counts) so the resumed completion can
  commit the full exchange; an interrupted turn commits nothing.
- A session with a pending permission rejects new `input` turns with a
  client-correctable `permission_pending` error carrying the `request_id` —
  resume or cancel explicitly. (Silently discarding the pending work on new
  input was considered and rejected: fail-closed beats convenience here.)
- TTL expiry and agent definition updates both invalidate pending
  permissions: resume validates that the definition `updated_at` still
  matches the one the checkpoint was taken under, since a checkpoint must
  resume on the graph that produced it. An invalidated or unknown
  `request_id` is a client-correctable error; the session unlocks and the
  next `input` turn proceeds fresh.
- When `max_pending` is reached, a new interrupt fails its turn with
  `permission_capacity_exceeded` instead of storing a checkpoint.
- A resume request re-enters the normal turn pipeline: it acquires the
  session lock and a `max_concurrent_turns` slot exactly like an `input`
  turn, and runs under `turn_timeout_seconds`.

**Observability.** The interrupted segment finishes its builtin turn span as
`success` with `result_status: "interrupted"` and a `permission` entry in the
event counts; the resumed segment is a new span with `operation: "resume"` on
the same session. Inner LLM/MCP child spans attach to whichever segment
executed them. One stable `run_id` identifies every segment of the logical
turn, while `session_id` and `request_id` remain explicit on SSE events,
SQLite usage events, the Admin runtime view, and OTLP attributes. Because a
resume is a new asynchronous HTTP request and therefore starts a new trace,
its usage event persists the checkpoint-producing trace/span ids and the OTLP
exporter reconstructs them as an OpenTelemetry Span Link. Cancellation keeps
the same correlation ids; lazy TTL cleanup emits a linked
`permission_expire` builtin lifecycle event with `result_status: "expired"`.
The expiry event remains available in builtin event and interaction listings,
but request-oriented summaries exclude it so it cannot inflate request/success
counts or skew average request latency. The interaction listing projects the
link trace/span ids for direct operator inspection.

**Admin surface.** `GET /admin/builtin/runtime` (new, mirroring
`/admin/acp/runtime`) lists host entries and pending permissions (agent id,
session id, run id, request id, tool calls, expiry). An operator decision escape
hatch (`POST /admin/builtin/runtime/permissions/{request_id}`, which would
need headless continuation semantics — the resumed events go nowhere but the
session transcript and metrics) is deferred until a concrete operator need
appears; see §13.

**Non-goals.** Durable pending permissions (eino v0.10 rule); per-node
permission modes; approval of anything other than MCP tool executions (model
calls and topology transfers stay ungated); model-native deferred/approval
tool protocols.

## 10. Operator turn cancellation (force / graceful)

Status: implemented. This answers the §13 open question on stuck turns: PB1
only drained in-flight turns on the old graph after a definition update and
bounded each turn with `turn_timeout_seconds`, with no way to stop a specific
running (or stuck) turn sooner. The builtin host now adopts the eino ADK
Runner cancel primitive (`adk.WithCancel` → `AgentCancelFunc`, `CancelMode`;
`eino-reuse.md` §5) for operator-initiated cancellation.

**Mechanism.** Every turn — fresh (`ServeTurn`) or resumed
(`servePermissionTurn`) — passes `adk.WithCancel()` to the Runner and
registers the returned cancel func in an in-memory activity registry keyed by
`(agent_id, session_id)`. Because turns on one session are serialized, that
key uniquely identifies the running turn. The entry is removed when the run
returns. Two modes:

- **force** (`CancelImmediate`, default): abort now — the in-flight model or
  tool step is abandoned and the turn ends immediately. This is the answer for
  stuck turns; the operator does not wait for `turn_timeout_seconds`.
- **graceful** (`CancelAfterChatModel | CancelAfterToolCalls` with a grace
  timeout and recursive propagation): stop after the current model/tool step
  completes, including inside a nested agent, escalating to immediate if no
  safe point is reached within the grace period, so a graceful cancel of a
  genuinely stuck turn still terminates.

The Runner surfaces the cancel as an `adk.CancelError` on the event stream;
the host maps it to a `done` event with `stop_reason: "cancelled"` on the
turn's own SSE stream and discards the partial exchange (the session is
released, never committed, so history is untouched). A checkpoint saved on
cancel of an interactive turn is deleted along the existing cleanup path. The
turn span records `result_status: "cancelled"`.

**Admin surface.** `GET /admin/builtin/runtime` also carries an
`in_flight` slice (agent id, session id, operation, topology kind, started
at); `GET /admin/builtin/runtime/inflight` is the dedicated list (mirroring
`/admin/acp/runtime/inflight`). Logical cancellation uses
`DELETE /admin/agents/{agent_id}/runs/{run_id}` with the backend's advertised
force/graceful modes. The builtin runtime family remains diagnostic and does
not duplicate common run control. agwctl exposes `builtin-runtime inflight`
for diagnostics and `agent cancel` for logical cancellation.

**Non-goals.** Resumable pause (a paused-then-resumed turn) is not in scope:
graceful stop ends the turn, it does not suspend it for later continuation.
Cancelling a queued turn that has not yet acquired its run (still waiting on
the session serial or the concurrency slot) is not exposed — such a turn is
the caller's to abandon by disconnecting.

## 11. Implementation track

The builtin runtime is its own track. PB0/PB1/PB1b have no dependency on a
gateway Workflow roadmap. The turn-first builtin `agentruntime.Backend` adapter
belongs to the unified Agent runtime foundation; PB2 is only the later durable
builtin session/checkpoint capability used by direct callers or external
Workflow Workers.

**PB0 — bridge adapters (no agent-model change):** implemented.

- MCP → `InvokableTool` adapter over `pkg/mcp/service`: `pkg/mcp/einotool`
  (tool selection by name is fail-closed: a missing tool is an error, not a
  silent skip)
- `RoutedProvider` → `model.ToolCallingChatModel` adapter:
  `pkg/llm/provider/einomodel`
- both are standalone libraries with tests; they are also independently
  useful to any in-repo eino consumer

**PB1 — runtime type, host, and ingress:** implemented.

- `runtime.type = "builtin"` with the definition schema and validation from
  [§4](#4-definition-schema), including the compiled-in factory registry
  check for `topology.kind = "custom"`
- the generic ADK host (`pkg/agent/builtin`): materialization cache, panic
  containment, limits, disabled semantics
- route-dispatched turn ingress (`POST /<agent-route>/turn`, SSE) through
  `pkg/gateway/agentroute` and dispatcher `agent` enablement
- the `builtin` usage event family and explicit `AgentID` span stamping; inner
  model/tool calls get child spans (kinds `llm`/`mcp`) parented under the turn
- workspace view keyed off `runtime.type = "builtin"` (definition summary,
  materialization state, live turns — no ACP fields)
- bundle/`adminclient`/`agwctl` parity, same as every other config object

Scope notes of the landed slice: every topology kind materializes —
`single`, `sequential`, `parallel`, `loop`, `supervisor`, `planexecute`
(role models via `topology.plan_execute`, per [§4](#4-definition-schema)),
`deep`, and `custom`. Middleware toggles cover `summarization` (using the
agent's own model), `agentsmd` (over inline virtual documents served by an
in-memory backend — the file-backend gap that originally deferred it), and
`reduction` (clear-only; truncation/offload waits for a workspace design,
per [§4](#4-definition-schema)). Sessions are in-memory
per the PB1 restart-loss semantics. Dispatcher `agent` enablement is wired in
both Caddy and standalone bootstrap paths.

**PB1b — interrupt and human-in-the-loop tool permissions:** implemented
([§9](#9-interrupt-and-human-in-the-loop-tool-permissions)).

- root-level `permissions` block (`auto_approve` default / `interactive`),
  approval gate over the `einotool` bridge, ADK Runner
  checkpoint/interrupt/resume with an in-memory `CheckPointStore`
- `permission` turn event + resume via `POST /<agent-route>/turn` with a
  `permission` field; every lifecycle edge (TTL, definition update, capacity,
  unanswered calls) fails closed
- `GET /admin/builtin/runtime` pending-permission view

**PB2 — durable builtin sessions/checkpoints:**

- adopt Runner session/checkpoint persistence only after a stable eino
  persistence surface exists; do not hand-roll a competing durable state
  engine
- advertise resume and external execution-key capabilities only after they are
  supported end to end
- let upper-layer Workflow Workers choose retry/resume policy from those
  capabilities; no gateway-owned Workflow Agent task is introduced

## 12. Implementation notes: PB1b interactive tool permissions

- Implemented exactly per §9 on eino v0.9.12: the approval gate
  (`pkg/agent/builtin/permission.go`) interrupts through `compose.Interrupt`
  with the tool-call payload as the user-facing info, the ADK Runner
  checkpoints into an in-memory `CheckPointStore` shared across
  materializations (request id = checkpoint id), and resume goes through
  `Runner.ResumeWithParams` with every pending interrupt point targeted —
  unanswered calls carry an explicit deny payload, so no gate is left to
  re-interrupt on its own.
- The gate wraps the einotool bridge *outside* the observability wrapper:
  interrupted and denied calls never open an `mcp` child span, so usage
  events only record executions that actually reached the MCP service.
- A resumed run that hits another gated call re-suspends under the same
  request id (the runner rewrites the checkpoint in place); the pending entry
  is re-registered with accumulated transcript and refreshed expiry, and
  replacement never counts against `max_pending`.
- Deviation from none of the design decisions was needed. One addition the
  design left implicit: an interrupt that yields no permission calls (some
  non-gate component interrupting) fails the turn as unresumable rather than
  suspending — only the approval gate is a sanctioned interrupt source in a
  builtin graph.
- The `permission` payload types registered for checkpoint gob serialization
  go through `schema.RegisterName`, mirroring how ADK registers its own
  checkpoint types.
- Verified end to end against the real Runner (no mocked ADK): interrupt →
  checkpoint gob round-trip → targeted resume → allow executes / deny returns
  a refusal tool result the model sees; plus TTL expiry, definition-update
  invalidation, capacity rejection, suspended-session input rejection, and
  the `auto_approve_tools` bypass.

## 13. Open questions and deferred work

The following builtin-specific items remain open:

- **Durable sessions and checkpoints:** PB1 uses in-memory sessions and
  checkpoint state. Adopt Runner-managed persistence only after a stable eino
  release provides the required seam; do not hand-roll a competing durable
  state engine.
- **Budget enforcement:** builtin is a convenient enforcement point because
  the gateway is the caller, but it must use the shared control-plane budget
  model rather than introduce a runtime-specific token, cost, or turn budget.
- **Administrative HITL continuation:** the data-plane resume flow is
  implemented. An Admin API decision endpoint remains deferred because it
  requires headless continuation whose events have no streaming client.
- **Permission scope:** interactive permissions gate MCP executions only.
  Gating topology transfers or sub-agent routing remains deferred until a
  concrete operator need exists.
- **Workspace-backed middleware:** filesystem middleware and reduction
  offload require a gateway workspace and allowed-roots design. Until then,
  agentsmd documents and skills remain inline, and reduction remains
  clear-only.
- **External durable orchestration:** upper-layer Workers already use the
  shared AgentRoute turn contract. Durable builtin session/checkpoint support
  remains PB2 and is independent of the external engine's history.

The turn event vocabulary and explicit operator cancellation are decided:
builtin exposes the documented ACP-compatible subset, and force/graceful
cancellation is implemented. Definition updates continue to drain turns on
the old graph unless an operator explicitly cancels them.
