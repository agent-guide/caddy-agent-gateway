# pkg/agent — AGENTS.md

Scope: the agent control plane under `pkg/agent/`, excluding the builtin host's
implementation details. Paths are repository-root relative; the root
`AGENTS.md` global rules apply. For `pkg/agent/builtin/`, also read its nested
`AGENTS.md`.

## Control plane

The external control-plane layer that composes the LLM/MCP/ACP/metrics surfaces
around an operator-facing agent identity. It depends on the lower protocol
managers; those packages must not depend on `pkg/agent`.

Important files:

- `runtimeapi/`: the runtime-neutral, turn-first Agent execution contracts,
  optional capability interfaces, normalized errors, and backend registry.
  It may depend on the `Agent` definition but must not import ACP, LLM, MCP, or
  other runtime implementations; gateway-owned adapters sit above both sides.
  `AgentGateway` registers the shipping ACP and builtin adapters during
  bootstrap; Agent-bound legacy route turns share its run sequencer and
  identity bridge, while unbound ACP remains the sole temporary native path.
  The gateway-owned run registry and permission broker provide M3 exact-run
  cancellation, bounded terminal tombstones, and one-shot opaque continuation
  claims. Claimed permission ids retain short-lived owner/runtime routing
  metadata so concurrent legacy and Agent entry points keep converging on the
  broker after the winner removes continuation state. Agent Admin
  capability/run/permission/session/transcript reads must
  resolve through these interfaces and fail with the normalized capability
  error when unsupported. Builtin Admin permission decisions are two-phase:
  the response advertises `resume_required`, and the decided continuation
  retains the original permission expiry until the next turn consumes it.
  The common broker owns expiry scheduling and shutdown drain; backend stores
  never run independent expiry sweeps. Public pending-permission records expose
  only allowlisted action identity/display and ACP option identity/kind/display
  fields, never native payloads or tool arguments. Opaque continuation tokens
  are resolved only in backend-owned stores.
- `types.go`: the `Agent` model. Runtime is `acp` (gateway owns the lifecycle via
  an ACP `service_id`), `http` (the agent owns its own lifecycle), or `builtin`
  (no separate process — a persisted definition materialized by the in-process
  ADK host). LLM and MCP are `resources`, not runtime types. `policy` is
  runtime-agnostic; ACP operational config stays on the ACP service.
- `builtin_types.go`: the `runtime.builtin` definition schema — model resolved
  through an LLM route (must appear in `routes.llm_route_ids`) with an
  optional `retry` block (`max_retries` 1–5, node-level ADK retry over the
  route's own candidate fallback; 429/5xx only; rejected on planexecute
  roles, which expose no retry seam), tools referencing
  MCP services (must appear in `resources.mcp_service_ids`), topology kinds
  `single`/`sequential`/`parallel`/`loop`/`supervisor`/`planexecute`/`deep`/
  `custom` (`planexecute` configures roles through the optional
  `topology.plan_execute` block — `planner`/`executor`/`replanner` inherit the
  node's model unless overridden, and only the executor carries tools; `deep`
  reuses the node's own fields with optional `sub_agents`; `custom`
  requires a factory name registered in the linked binary and is root-only —
  a factory receives the whole `BuiltinRuntime` definition, so a nested custom
  node is rejected at validation and again at materialization), inline-only
  sub-agent definitions, middleware toggles (`summarization`; `agentsmd` over
  inline virtual docs — never host filesystem paths; clear-only `reduction`
  with no truncation/offload phase; `toolsearch` gating the node's MCP tools
  behind a `tool_search` meta-tool, requiring declared tools; `plantask`
  task tools over a session-scoped in-memory board; `skill` over inline
  virtual skills, inline execution only — no fork/model frontmatter;
  defensive `patchtoolcalls` completing dangling tool exchanges),
  root-level `permissions` (HITL tool gating,
  `docs/design/builtin-agent-runtime.md` §9: mode
  `auto_approve`/`interactive`, `timeout_seconds`, `max_pending`, and
  fully-qualified `auto_approve_tools` validated against tools declared
  anywhere in the topology), and fail-closed `limits`
  (`max_concurrent_turns`, `turn_timeout_seconds`).
- `manager.go`: agent CRUD plus the in-memory route/service → agent index. It
  enforces P0 one-runtime-one-agent (a `service_id` is bound by at most one
  agent), route-binding uniqueness (any LLM/MCP/ACP/builtin `route_id` is owned
  by at most one agent, so the route → agent attribution mapping stays
  unambiguous), `acp_route_ids` → runtime-service and `builtin_route_ids` →
  target-agent consistency, and implements `ResolveAgentID` (the
  `usage.AgentAttributor` seam). The index is derived from the definition
  generation on every commit, defensively: a `service_id` or `route_id` that
  resolves to more than one agent is dropped from the map (and
  `ResolveAgentID` returns `ok=false`) rather than silently picking a last
  writer.
- `snapshot.go`: the deep-cloned, generation-swapped definition snapshot
  (unified-agent-runtime plan §11 decision 3). `GetSnapshot(id)`, `Snapshot()`,
  and `HasAgent(id)` read only the immutable current generation and never
  touch the config store — they are the required lookups for per-request
  dispatch. Create/Update/Delete build and validate a complete prospective
  generation before the store write, then commit the infallible swap after
  the store operation succeeds; `Refresh` decodes and deep-clones the complete
  store result before its swap and is serialized with CRUD across the complete
  List-to-commit window. `Recommit` republishes the loaded generation without a
  store read when external runtime records change. Returned values are deep clones (no pointer,
  slice, map, or topology field aliases the generation). Definition listeners
  (`AddDefinitionListener`) are three-stage: prepare runs before the snapshot
  write lock, commit runs under it, and cleanup runs afterward with a detached
  bounded context. Commit callbacks must be bounded and in-memory only and
  must never call `GetSnapshot`, `Snapshot`, `HasAgent`, or
  `SnapshotGeneration` (the snapshot mutex is not reentrant). Safety-sensitive
  state publication/retirement marks belong in commit so they precede new
  dispatch; store/process/transport I/O belongs in prepare or cleanup.

Agents are a first-class gateway-bundle object (apply/export/validate) and have
an `agwctl gateway agent` read surface; create/update flow through the bundle.
See `docs/design/agents-control-plane.md` for the cross-runtime direction and
`docs/design/builtin-agent-runtime.md` for the builtin runtime design and
implementation status.
