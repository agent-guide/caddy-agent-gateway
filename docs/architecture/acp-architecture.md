# ACP Runtime Architecture

## Scope

ACP is the gateway-owned process runtime for Agents such as Codex and
OpenCode. Since the unified Agent runtime cutover, ACP has no public service or
route object: process configuration lives inline in `Agent.runtime.acp`, and a
unified AgentRoute targets the Agent's stable `agent_id`.

The removed surfaces include `acp_services`, `service_id`, ACPRoute,
`/admin/acp/services`, `/admin/acp/routes`, `acpServices`, `acpRoutes`, and the
matching CLI commands. Legacy source packages remain only until the planned M7
cleanup and are not registered publicly.

## Components

- `pkg/agent.Manager` owns persisted Agent definitions and the immutable
  in-memory definition snapshot.
- `pkg/gateway/agentroute` owns unified ingress matching and `agent_id`
  targeting.
- `pkg/gateway.ACPBackend` adapts common Agent execution to the native ACP
  runtime and maintains the canonical `agent_id -> RuntimeConfig` snapshot.
- `pkg/acp/runtime.Manager` owns process pools, native sessions, permissions,
  transcripts, and exact-run cancellation.
- `pkg/agent/runtimeapi` owns common run IDs, event envelopes, capabilities,
  permission brokering, and normalized errors.

The dependency direction remains one-way: gateway adapters compose Agent and
ACP; `pkg/acp` does not depend on `pkg/agent`.

## Configuration And Publication

Agent create/update/refresh uses the three-stage definition listener protocol:

1. prepare validates and builds prospective runtime state outside the snapshot
   lock;
2. commit atomically publishes the Agent generation and accepted ACP
   fingerprint;
3. cleanup retires stale pools and drains obsolete continuation state with a
   bounded detached context.

Any change to `runtime.acp`, disablement, runtime switch, or deletion retires
instances whose fingerprint no longer matches. Active instances drain; idle
ones close immediately. The native manager rechecks the accepted owner
fingerprint before inserting a newly created process, preventing an old
request from repopulating retired state.

## Request Flow

```text
POST /<agent-route>/turn
  -> dispatcher matches kind=agent route
  -> validate VirtualKey and Agent rate limit
  -> resolve AgentRoute.agent_id from memory
  -> resolve Agent definition from immutable snapshot
  -> runtime registry selects ACPBackend
  -> ACPBackend reads the agent_id keyed RuntimeConfig snapshot
  -> native manager reuses or creates a fingerprint-matching process
  -> native ACP session/new or session/load + session/prompt
  -> common Agent SSE envelope
```

The route is independent of the runtime backend. Switching an Agent between
ACP and builtin does not change its route ID, URL, or VirtualKey allowlist.

## Pooling And Sessions

Pools are keyed by Agent owner, thread/session scope, adapter, and accepted
configuration fingerprint. The manager preserves:

- dead-instance eviction and idle cleanup;
- per-Agent instance caps;
- session rebind to the already-live instance;
- setup handshake timeouts and stderr capture;
- `fresh_session` isolation;
- bounded retirement of stale generations.

The common `run_id` is bound to the native ACP instance and protocol session.
Exact cancellation sends `session/cancel` to only that run. ACP thread close is
a separate destructive recovery action and must not substitute for ordinary
run cancellation.

## Permissions

Interactive permissions are fail-closed. Agent-bound requests are registered
with the common permission broker before being exposed publicly; the ACP
runtime retains native payloads and waiter state behind an opaque continuation
token. Decisions preserve the exact ACP option IDs and are claimed atomically
before the native waiter is resolved.

Public operations are Agent-scoped:

```text
GET  /admin/agents/{agent_id}/permissions
POST /admin/agents/{agent_id}/permissions/{request_id}
```

## Sessions And Transcripts

Consumer reads resolve through the AgentRoute and use its VirtualKey policy:

```text
GET /<agent-route>/sessions
GET /<agent-route>/sessions/{session_id}/transcript
```

Operator reads resolve through the Agent capability layer:

```text
GET /admin/agents/{agent_id}/sessions
GET /admin/agents/{agent_id}/sessions/{session_id}/transcript
```

The gateway checks advertised native capabilities before list/load and applies
the optional `cwd` filter only after symlink-canonicalizing both sides.

## Runtime Diagnostics

ACP-specific diagnostics and pool recovery remain available without restoring
a separate config object:

```text
GET    /admin/acp/runtime
GET    /admin/acp/runtime/inflight
DELETE /admin/acp/runtime/agents/{agent_id}/threads/{thread_id}
```

Common run inspection and cancellation use:

```text
GET    /admin/agents/{agent_id}/runs
DELETE /admin/agents/{agent_id}/runs/{run_id}
```

## Observability

The dispatcher opens one interaction span after route and Agent resolution.
Usage records include the unified route ID/kind, `agent_id`, `runtime_type`,
common `run_id`, native thread/session dimensions when available, latency,
outcome, and token/event counts. Provider and MCP activity nested inside a
builtin Agent follows the same Agent attribution rules.

## Migration Boundary

New binaries run a read-only SQLite preflight before normal open. Legacy ACP
service rows, `kind=acp|builtin` routes, and Agent JSON containing service
ownership fields fail with `legacy_agent_runtime_config`. Operators export
with the old binary, run `scripts/migrate-unified-agent-runtime`, apply the
converted bundle to a clean store, and then switch binaries. The helper embeds
ACP service config into Agents, converts runtime-specific routes to
AgentRoutes, rewrites VirtualKey references, removes server timestamps, and
checks route-ID collisions across all families.
