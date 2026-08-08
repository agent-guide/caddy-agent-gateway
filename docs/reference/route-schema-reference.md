# Route Schema Reference

This page summarizes the current LLM, MCP, and unified Agent route shapes used
by static config, Admin API objects, and runtime resolution.

## Core Route Fields

The current route object includes:

- `id`
- `kind`
- `protocol`
- `description`
- `disabled`
- `match_policy`
- `auth_policy`
- `target_policy`
- `created_at` and `updated_at` (server-managed response fields)

Current route kinds:

- `llm`
- `mcp`
- `agent`

Current route protocols:

- `openai`
- `anthropic`
- `cc`
- `mcp`
- `agent`

## `match_policy`

Current fields:

- `host`
- `path_prefix`
- `methods`

These fields control request matching only.

## `auth_policy`

Current fields:

- `require_virtual_key`

If enabled, the gateway accepts a VirtualKey from:

- `Authorization: Bearer <key>`
- `x-api-key: <key>`

## `target_policy`

LLM routes support two valid target modes:

- direct-provider mode
- logical-model mode

### Direct-Provider Mode

Current shape:

```json
{
  "provider_target": {
    "provider_id": "openai-main"
  }
}
```

Behavior:

- request `model` is treated as the upstream model name
- supported in dynamic routes, Caddyfile routes, and `agwd --static-config`

### Logical-Model Mode

Current shape includes concepts such as:

- `default_model`
- `model_selector_strategy`
- `fallback`
- `model_targets`

Each `model_target` can contain:

- `name`
- `strategy`
- `default_candidate`
- `candidates`

Each candidate can contain:

- `provider_id`
- `upstream_model`
- `weight`
- `priority`
- `default`

Behavior:

- request `model` is treated as the route model name
- the gateway resolves it to one concrete provider and upstream model binding
- supported through dynamic route management and bundle workflows
- rejected in Caddyfile routes and `agwd --static-config`

### Agent Mode

An AgentRoute targets a stable Agent identity. The Agent's `runtime.type`
selects ACP, builtin, or a future executable backend without changing the
route URL, ID, or VirtualKey allowlist:

```json
{
  "id": "agent:codex-main:agents-codex",
  "kind": "agent",
  "protocol": "agent",
  "agent_id": "codex-main",
  "match_policy": {
    "path_prefix": "/agents/codex"
  },
  "auth_policy": {
    "require_virtual_key": true
  }
}
```

When `id` is omitted, it defaults to the deterministic, slash-free
`agent:<agent_id>:<path-slug>`
(the path prefix lowercased, non-alphanumeric runs collapsed to `-`, `/` →
`root`). Route ids must be slash-free so they are addressable as a single Admin
API path segment. Manage these routes through bundle `agentRoutes`,
`/admin/agents/routes`, or `agwctl agent-route`.

## Static Config Restrictions

Current static restrictions:

- Caddyfile LLM routes only support direct-provider mode
- `agwd --static-config` `llmRoutes` only support direct-provider mode
- `agwd --static-config` does not support `managedModels`
- Caddyfile and standalone static bundles accept AgentRoutes targeting Agents

## Related Docs

- [../guides/routes.md](../guides/routes.md)
- [../design/model-first-routing.md](../design/model-first-routing.md)
- [../design/route-target-policy.md](../design/route-target-policy.md)
