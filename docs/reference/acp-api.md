# ACP Agent API Reference

ACP executes through unified AgentRoutes. Replace `<agent-route>` with the
configured route prefix and authenticate according to its VirtualKey policy.

## Stream A Turn

```text
POST /<agent-route>/turn
Content-Type: application/json
Authorization: Bearer <virtual-key>
```

```json
{
  "input": "Reply with one sentence",
  "options": {
    "version": "v1",
    "runtime": {
      "thread_id": "thread-1",
      "session_id": "optional-session-id",
      "cwd": "/workspace",
      "model": "optional-model",
      "fresh_session": false
    }
  }
}
```

The response is SSE. Event data uses the common envelope:

```json
{
  "agent_id": "codex-main",
  "run_id": "run-...",
  "sequence": 1,
  "event": "session",
  "session_id": "..."
}
```

Subsequent events can include `content`, `tool_call`, `permission`, `usage`,
`done`, or `error` payloads. Use the returned `run_id` for exact cancellation.

## Sessions And Transcript

```text
GET /<agent-route>/sessions?cwd=/workspace&cursor=...
GET /<agent-route>/sessions/{session_id}/transcript?cwd=/workspace
```

Both requests use the AgentRoute's VirtualKey policy. Availability depends on
the native adapter's advertised session capabilities.

## Agent Admin Operations

```text
GET    /admin/agents/{agent_id}/capabilities
GET    /admin/agents/{agent_id}/runs
DELETE /admin/agents/{agent_id}/runs/{run_id}?mode=force
GET    /admin/agents/{agent_id}/permissions
POST   /admin/agents/{agent_id}/permissions/{request_id}
GET    /admin/agents/{agent_id}/sessions
GET    /admin/agents/{agent_id}/sessions/{session_id}/transcript
```

An ACP permission decision preserves the native option ID:

```json
{
  "outcome": "selected",
  "option_id": "allow-once"
}
```

Use the outcome supported by the pending request; denying/cancelling remains
fail-closed.

## ACP Runtime Diagnostics

```text
GET    /admin/acp/runtime
GET    /admin/acp/runtime/inflight
DELETE /admin/acp/runtime/agents/{agent_id}/threads/{thread_id}
```

Thread close is for ACP pool recovery, not ordinary run cancellation.

## CLI Equivalents

```bash
./agwctl gateway agent get <agent-id>
./agwctl gateway agent-route get <agent-route-id>
./agwctl gateway agent capabilities <agent-id>
./agwctl gateway agent runs <agent-id>
./agwctl gateway agent cancel <agent-id> <run-id> --mode force
./agwctl gateway agent permissions <agent-id>
./agwctl gateway agent decide <agent-id> <request-id> --outcome selected --option-id <option-id>
./agwctl gateway agent sessions <agent-id> [--cwd <cwd>] [--cursor <cursor>]
./agwctl gateway agent transcript <agent-id> <session-id> [--cwd <cwd>]
./agwctl gateway acp-runtime get
./agwctl gateway acp-runtime inflight
./agwctl gateway acp-runtime close-thread <agent-id> <thread-id>
```

Agent definitions and AgentRoutes are created or updated through bundle apply,
or through `/admin/agents` and `/admin/agents/routes`.
