# Admin API Reference

All endpoints below are under the path where `agent_gateway_admin` is mounted. The Admin API does not implement login or sessions; protect this mount with Caddy `basic_auth`, mTLS, a reverse proxy authenticator, or the standalone daemon's `--admin-basic-auth-hash` wrapper.

## Health

- `GET /admin/health`

## Providers

- `GET /admin/llm/provider_types`
- `GET /admin/llm/api_handler_types`
- `GET /admin/llm/providers`
- `POST /admin/llm/providers`
- `GET /admin/llm/providers/{id}`
- `PUT /admin/llm/providers/{id}`
- `POST /admin/llm/providers/{id}/enable`
- `POST /admin/llm/providers/{id}/disable`
- `DELETE /admin/llm/providers/{id}`

Create a provider:

```bash
curl -u admin:your-password -X POST http://localhost:8019/admin/llm/providers \
  -H 'Content-Type: application/json' \
  -d '{
    "id": "openrouter-main",
    "provider_type": "openrouter",
    "api_key": "sk-or-...",
    "base_url": "https://openrouter.ai/api/v1",
    "default_model": "openai/gpt-4o-mini",
    "network": {
      "request_timeout_seconds": 120,
      "max_retries": 3,
      "max_idle_connections": 100,
      "max_idle_connections_per_host": 20,
      "idle_keep_alive_timeout_seconds": 90
    }
  }'
```

Provider notes:

- `id` and `provider_type` are required
- `created_at` and `updated_at` are server-managed fields

## LLM Routes

- `GET /admin/llm/routes`
- `POST /admin/llm/routes`
- `GET /admin/llm/routes/{id}`
- `PUT /admin/llm/routes/{id}`
- `POST /admin/llm/routes/{id}/enable`
- `POST /admin/llm/routes/{id}/disable`
- `DELETE /admin/llm/routes/{id}`

Create a route:

```bash
curl -u admin:your-password -X POST http://localhost:8019/admin/llm/routes \
  -H 'Content-Type: application/json' \
  -d '{
    "id": "chat-prod",
    "protocol": "openai",
    "match": {
      "path_prefix": "/",
      "methods": ["POST"]
    },
    "target_policy": {
      "provider_target": {
        "provider_id": "openrouter-main"
      }
    },
    "auth_policy": {
      "require_virtual_key": true
    }
  }'
```

Route notes:

- `protocol` and `target_policy` are required
- `created_at` and `updated_at` are server-managed fields
- when `target_policy.provider_target.provider_id` is present, the route runs in direct-provider mode
- logical-model routes using `target_policy.model_targets` remain supported through dynamic route management and bundle workflows, but not in Caddyfile routes or `agwd --static-config`

## VirtualKeys

- `GET /admin/virtual_keys`
- `POST /admin/virtual_keys`
- `GET /admin/virtual_keys/{id}`
- `PUT /admin/virtual_keys/{id}`
- `POST /admin/virtual_keys/{id}/enable`
- `POST /admin/virtual_keys/{id}/disable`
- `DELETE /admin/virtual_keys/{id}`

Create a VirtualKey:

```bash
curl -X POST http://localhost:8019/admin/virtual_keys \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "id": "demo-key",
    "tag": "demo-user",
    "allowed_route_ids": ["chat-prod"]
  }'
```

Example response:

```json
{
  "id": "demo-key",
  "key": "vk-...",
  "tag": "demo-user",
  "allowed_route_ids": ["chat-prod"],
  "created_at": "2026-05-13T03:00:00Z",
  "updated_at": "2026-05-13T03:00:00Z",
  "source": "store",
  "read_only": false
}
```

VirtualKey notes:

- `id` is the stable management identifier
- `key` is the bearer credential value clients must send on requests
- `created_at` and `updated_at` are server-managed fields

## Upstream Credentials

- `GET /admin/credentials`
- `POST /admin/credentials`
- `GET /admin/credentials/{credential_id}`
- `PUT /admin/credentials/{credential_id}`
- `DELETE /admin/credentials/{credential_id}`

Create an API-key credential:

```bash
curl -X POST http://localhost:8019/admin/credentials \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "provider_id": "openai-main",
    "label": "primary",
    "attributes": {
      "api_key": "sk-...",
      "base_url": "https://api.openai.com/v1",
      "priority": "10"
    }
  }'
```

Credential notes:

- `created_at` and `updated_at` are server-managed response fields
- do not send them in `POST` or `PUT` bodies

## Models

- `GET /admin/llm/models/providers/{provider_id}/discovered`
- `POST /admin/llm/models/providers/{provider_id}/refresh`
- `GET /admin/llm/models/managed`
- `PUT /admin/llm/models/managed/{provider_id}/{upstream_model}`

## CLI Auth

- `GET /admin/cliauth/authenticators`
- `GET /admin/cliauth/authenticators/{authenticator_name}`
- `PUT /admin/cliauth/authenticators/{authenticator_name}`
- `POST /admin/cliauth/authenticators/{authenticator_name}/login`
- `GET /admin/cliauth/logins/{login_id}`

CLI auth behavior:

- login runs asynchronously and returns `202 Accepted`
- poll the login status endpoint for completion
- the login request body must include `provider_id`
- authenticator config set through the admin API is runtime-only
- disabling an authenticator or restarting the server resets it to factory defaults

Examples:

```bash
curl -X PUT http://localhost:8019/admin/cliauth/authenticators/codex \  -H 'Content-Type: application/json' \
  --data '{"enabled":true,"config":{}}'
```

```bash
curl -X PUT http://localhost:8019/admin/cliauth/authenticators/codex \  -H 'Content-Type: application/json' \
  --data '{"enabled":true,"config":{"callback_port":9002,"no_browser":true,"device_flow":true}}'
```

```bash
curl -X POST http://localhost:8019/admin/cliauth/authenticators/codex/login \  -H 'Content-Type: application/json' \
  --data '{"provider_id":"openai-main","scope":"type:openai"}'
```

## MCP Admin Families

Implemented MCP admin families include:

- MCP services
- MCP routes
- MCP discovery and execution endpoints
- MCP dispatcher runtime inspection endpoints

## ACP Admin Families

Implemented ACP admin families include:

- `GET /admin/acp/services`
- `POST /admin/acp/services`
- `GET /admin/acp/services/{id}`
- `PUT /admin/acp/services/{id}`
- `DELETE /admin/acp/services/{id}`
- `GET /admin/acp/services/{id}/sessions`
- `GET /admin/acp/services/{id}/sessions/{session_id}/transcript`
- `GET /admin/acp/routes`
- `POST /admin/acp/routes`
- `GET /admin/acp/routes/{id}`
- `PUT /admin/acp/routes/{id}`
- `DELETE /admin/acp/routes/{id}`
- `GET /admin/acp/runtime`
- `GET /admin/acp/runtime/inflight`
- `DELETE /admin/acp/runtime/threads/{service_id}/{thread_id}`
- `POST /admin/acp/runtime/permissions/{request_id}`

The dispatcher also exposes consumer-facing route-scoped ACP session endpoints
under `/<acp-route>/sessions` and
`/<acp-route>/sessions/{session_id}/transcript`; those are not Admin API
families because they resolve the service from the matched route and use the
route's VirtualKey policy.

See [acp-api.md](acp-api.md) for request and response shapes.

## Metrics

- `GET /admin/metrics`
- `GET /admin/metrics/prometheus`
- `GET /admin/metrics/llm/events`
- `GET /admin/metrics/llm/timeseries`
- `GET /admin/metrics/llm/breakdown`
- `GET /admin/metrics/mcp/events`
- `GET /admin/metrics/mcp/timeseries`
- `GET /admin/metrics/mcp/breakdown`
- `GET /admin/metrics/mcp/tools/summary`
- `GET /admin/metrics/acp/events`
- `GET /admin/metrics/acp/timeseries`
- `GET /admin/metrics/acp/breakdown`
- `GET /admin/metrics/acp/summary`
- `GET /admin/metrics/interactions`
- `GET /admin/metrics/interactions/summary`

Metrics are backed by SQLite usage event tables when the sqlite config store
backend is active. `GET /admin/metrics` returns per-kind summaries plus a
`pipeline` object with `dropped_events` and `write_failures` counters.
`GET /admin/metrics/prometheus` renders an O(1) in-process counter snapshot in
Prometheus text exposition format (request/failure/token totals by kind plus the
pipeline health counters); point an external Prometheus scrape at it (behind the
admin auth boundary) for trends, aggregation, and alerting. Event listing
endpoints support `from`, `to`, `limit`, `success`, and family-specific filters
such as route, provider/service, model, method, tool, operation, thread,
session, and trace identifiers. Aggregate endpoints support `from`, `to`,
`limit`, `group_by`, and endpoint-specific filters. The LLM, MCP, and ACP
`timeseries` and `breakdown` endpoints share the same shape; each accepts its
protocol's `group_by` dimensions and filter keys (LLM:
`route_id`/`provider_id`/`virtual_key_id`/`upstream_model`/`llm_api`; MCP:
`route_id`/`service_id`/`virtual_key_id`/`method`/`tool_name`/`result_status`;
ACP: `route_id`/`route_protocol`/`service_id`/`virtual_key_id`/`agent_type`/`operation`).
The ACP `route_protocol` filter separates data-plane turns (`route_protocol=acp`)
from the admin-plane audit spans the manager records when polling `/admin/acp`
(`route_protocol=admin`).

The LLM/MCP/ACP `breakdown`, `timeseries`, and `events` endpoints — and
`interactions`/`interactions/summary` — also accept an `agent_id` filter. Unlike a
plain column filter, `agent_id` resolves the agent server-side and scopes results
by its full attribution — the durable `agent_id` tag **OR** the routes/ACP service
the agent currently owns — matching `GET /admin/agents/{id}/usage`. This lets
untagged-but-mappable events (pre-tag history, or events written before a
route/service was assigned to the agent) still surface, so an `agent_id`-filtered
read is a strict superset of the per-agent rollup. An unknown `agent_id` returns
`404`. The durable-tag arm is indexed by `(agent_id, started_at)`.

`GET /admin/metrics/interactions` is the cross-protocol call-chain view (LLM +
MCP + ACP spans unified by `trace_id`/`span_id`/`parent_span_id`). Beyond the
common `from`/`to`/`limit`/`success`, it filters by `route_kind`,
`route_protocol`, `route_id`, `virtual_key_id`, `service_id`, `session_id`,
`trace_id`, `parent_span_id`, and `agent_depth`, plus the `agent_id` attribution
filter described above — pass `agent_id` to scope the whole call chain to a single
agent server-side. The durable `agent_id` tag, when present, is surfaced per span
in the response.
Timeseries supports `bucket=minute|hour|day` (plural and short forms such as
`minutes`/`min`/`m` are also accepted, as are Grafana-style durations such as
`3h`/`5m`/`30s`/`1d`; empty defaults to `hour`). `mcp/tools/summary` and
`acp/summary` remain as fixed-grouping convenience views. Usage-event retention is configured through the Caddyfile
`metrics` block or the `agwd` `--metrics-retention-days` flag and is enforced at
startup and by a periodic background janitor.

## Agents

- `GET /admin/agents`
- `POST /admin/agents`
- `GET /admin/agents/{id}`
- `PUT /admin/agents/{id}`
- `DELETE /admin/agents/{id}`
- `GET /admin/agents/{id}/workspace`
- `GET /admin/agents/{id}/activity`
- `GET /admin/agents/{id}/usage`
- `GET /admin/agents/{id}/interactions`
- `GET /admin/agents/{id}/resources`
- `PUT /admin/agents/{id}/resources`
- `GET /admin/agents/{id}/health`

Agents are first-class management objects that bind an operator-facing identity
to one runtime backend, currently an ACP service for managed local agents.
P0/P1 agents are management-plane groupings: data-plane requests still
authenticate through VirtualKeys and route policy. Agent usage and activity
views prefer durable `agent_id` attribution and fall back to the agent's owned
routes and ACP runtime service for older or untagged events.

## Stubbed Families

These families still contain `501 Not Implemented` endpoints:

- memory

## Caddy Integration Notes

The gateway admin handler does not expose Caddy server management endpoints.

- if you need `/admin/caddy/*` operations for a Caddy-managed deployment, run the standalone `caddymgr` service and point the Web UI at that service
- `agwctl caddy ...` talks to the Caddy admin API directly and does not use the gateway Admin API route table

## Related Docs

- [../guides/admin-auth.md](../guides/admin-auth.md)
- [runtime-modes.md](runtime-modes.md)
- [caddyfile-reference.md](caddyfile-reference.md)
