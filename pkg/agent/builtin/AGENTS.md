# pkg/agent/builtin — AGENTS.md

Scope: the in-process eino ADK host for agents with
`runtime.type = "builtin"`. The root and parent `AGENTS.md` files apply. The
design source of truth is `docs/design/builtin-agent-runtime.md`; keep detailed
feature inventories and implementation status there.

## Runtime invariants

- One host owned by `AgentGateway` serves all builtin agents. Materializations
  are cached by agent id and definition version; updates affect the next turn
  while existing turns drain on their current graph.
- Resolve models through the agent's LLM route and wrap the resulting
  `RoutedProvider` with `einomodel`. Tool-calling nodes must request
  tool-capable route candidates. Resolve MCP tools through `einotool`, with
  missing named tools failing closed.
- Enforce disabled-agent, concurrency, timeout, session-cap, and panic handling
  fail-closed. Same-session turns serialize before taking a concurrency slot;
  never let a waiting turn consume `max_concurrent_turns` capacity.
- Sessions and permission checkpoints are intentionally in memory. Do not add
  an ad-hoc durable checkpoint format; follow the design document's persistence
  plan.
- Every fresh or resumed turn must register cancellation by
  `(agent_id, session_id)`. Cancellation discards the uncommitted partial
  exchange and emits `done` with `stop_reason: "cancelled"`.

## Permissions and middleware

- Interactive MCP tool permission uses the ADK checkpoint/resume cycle. It
  releases the stream, goroutine, and turn slot while awaiting a decision.
  Expiry, definition mismatch, capacity exhaustion, omitted decisions, and new
  input on a suspended session all fail closed.
- The common Agent permission broker is the sole expiry scheduler and atomic
  claim owner. The builtin permission registry is an opaque continuation store;
  it must not independently sweep expired checkpoints.
- The permission gate wraps the MCP tool bridge outside observability, so
  denied or interrupted calls do not open MCP child spans. Builtin middleware
  tools (`skill`, `plantask`, `tool_search`) are never permission-gated.
- Preserve middleware order: `patchtoolcalls` → `reduction` → `summarization`
  → `skill` → `plantask` → `toolsearch` → `agentsmd`.
- `agentsmd` and skills use inline virtual documents, never host filesystem
  paths. Reduction remains clear-only until a supported offload/read path
  exists. Plantask state is session-scoped and in memory.

## Observability and registration

- Parent inner LLM and MCP spans under the builtin turn span and retain
  `agent_id`/`run_id` correlation. Permission resume starts a new trace linked
  to the checkpoint-producing span.
- Turn events use the shared vocabulary subset: `session`, `delta`, `content`,
  `tool_call`, `usage`, `permission`, `done`, `error`.
- Register custom Go agents with `builtin.RegisterFactory`. Blank-import each
  factory package in `cmd/agw/main.go`, `cmd/agwd/main.go`, and
  `cmd/agwctl/cmd_gateway.go`.
