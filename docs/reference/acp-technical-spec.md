# ACP Runtime Technical Specification

ACP is an Agent runtime backend. It is not a public service or route family:
the Agent owns process configuration inline under `runtime.acp`, and a unified
AgentRoute targets the Agent by `agent_id`.

## Configuration Model

```yaml
agents:
  - id: codex-main
    runtime:
      type: acp
      acp:
        agent_type: codex
        cwd: /workspace
        allowed_roots: [/workspace]
        default_model: gpt-5
        max_instances: 4
        permission_mode: deny
    routes: {}
    resources: {}
    policy: {}

agentRoutes:
  - id: agent-codex
    agent_id: codex-main
    match_policy: {path_prefix: /agents/codex}
    auth_policy: {require_virtual_key: true}
```

The persisted stores are `agents` and the shared `routes` store. There is no
`acp_services` store and bundles do not accept `acpServices`, `acpRoutes`, or
`builtinRoutes`. Those legacy keys fail with `legacy_agent_runtime_config`.

Supported ACP fields include `agent_type`, `cwd`, `allowed_roots`,
`default_model`, `env`, `config_overrides`, `idle_ttl`, `max_instances`,
`permission_mode`, and Codex-specific configuration. An empty permission mode
normalizes to fail-closed `deny`.

## Dispatch

```text
POST /<agent-route>/turn
  -> resolve AgentRoute.agent_id
  -> read Agent from the in-memory definition snapshot
  -> select runtime.type = acp
  -> read the agent_id keyed ACP RuntimeConfig snapshot
  -> acquire/create a fingerprint-matching pooled process
  -> run the native ACP prompt
  -> emit the common Agent SSE envelope
```

The request uses the common turn envelope:

```json
{
  "input": "Fix the failing test",
  "options": {
    "version": "v1",
    "runtime": {
      "thread_id": "thread-1",
      "session_id": "optional-native-session",
      "cwd": "/workspace",
      "model": "optional-override",
      "fresh_session": false
    }
  }
}
```

Each event carries `agent_id`, `run_id`, and a monotonically increasing
`sequence`. ACP-specific session/thread values remain backend extension data.

## Pool And Snapshot Invariants

- Runtime configuration is snapshotted exclusively from `Agent.runtime.acp`.
- Pool ownership is keyed by `agent_id`, never a service ID.
- A configuration fingerprint change, disable, runtime switch, or Agent
  deletion retires stale instances; active turns drain before final cleanup.
- A request cannot repopulate an instance with an obsolete fingerprint after
  retirement.
- Session-addressed turns adopt an already-bound live instance when possible.
- Exact run cancellation sends native `session/cancel` for that run and does
  not close an unrelated thread or process.

## Permissions

ACP permission handling is fail-closed. Agent-bound requests are published to
the common permission broker and retain only an opaque backend continuation.
List and decide them through:

```text
GET  /admin/agents/{agent_id}/permissions
POST /admin/agents/{agent_id}/permissions/{request_id}
```

The data plane also accepts the common AgentRoute permission/resume flow. ACP
option IDs are preserved exactly; flat boolean approval payloads are invalid.

## Sessions And Transcripts

Consumer endpoints use the matched AgentRoute and its VirtualKey policy:

```text
GET /<agent-route>/sessions?cwd=...&cursor=...
GET /<agent-route>/sessions/{session_id}/transcript?cwd=...
```

Operator endpoints are capability-gated and Agent-scoped:

```text
GET /admin/agents/{agent_id}/sessions
GET /admin/agents/{agent_id}/sessions/{session_id}/transcript
```

The gateway canonicalizes the optional `cwd` filter and checks advertised ACP
capabilities before calling session list or load.

## Runtime Operations

```text
GET    /admin/acp/runtime
GET    /admin/acp/runtime/inflight
DELETE /admin/acp/runtime/agents/{agent_id}/threads/{thread_id}
```

Thread close is an ACP-specific destructive pool-recovery operation. Ordinary
cancellation uses `DELETE /admin/agents/{agent_id}/runs/{run_id}`.

## Errors

- malformed or unsupported common options: `400`
- missing AgentRoute or Agent: `404`
- disabled/non-executable Agent: normalized runtime error
- VirtualKey rate limit: `429` with `Retry-After`
- native process or transport failure: `502`
- unavailable runtime/backend: `503`

See [acp-api.md](acp-api.md) for endpoint examples and
[../architecture/acp-architecture.md](../architecture/acp-architecture.md) for
the runtime design.
