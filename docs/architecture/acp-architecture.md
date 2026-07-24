# ACP Architecture

This document describes the implemented ACP architecture in `agent-gateway`.
ACP is a first-class gateway route kind beside LLM and MCP. The implementation
is native to this repository: the gateway owns route resolution, service config,
VirtualKey auth, the HTTP turn API, Admin CRUD, runtime pooling, event
normalization, and permission handling. It does not import or vendor ngent code.

The service/route model below is the **current pre-unification implementation**.
The target breaking architecture is defined by
[Unified Agent Runtime and Routing](../plans/unified-agent-runtime.md):

```text
AgentRoute.agent_id
  -> Agent.runtime.acp
  -> runtimeapi ACP adapter
  -> pkg/acp/runtime.Manager(owner=agent_id, config=acp.RuntimeConfig)
  -> pooled ACP process/session
```

In that target:

- ACP execution config is stored directly on `Agent.runtime.acp`;
- `agent_id` keys pools, scopes, sessions, permissions, diagnostics, and usage;
- `pkg/acp` receives an identity-free runtime config and does not import
  `pkg/agent`;
- `acp_services`, `service_id`, ACPRoute, `/admin/acp/services`,
  `/admin/acp/routes`, and their CLI/bundle surfaces are removed;
- AgentRoute and Agent capability APIs own ingress, sessions, transcripts,
  permissions, and logical-run cancellation;
- `/admin/acp/runtime` remains only for native process/pool diagnosis and
  recovery, with owner fields expressed as `agent_id`.

The remainder of this document stays in present tense so it remains an accurate
reference for the code before M4-M7 land.

## Scope

Implemented agent types:

- `codex`: launched through the fixed external ACP adapter command `codex-acp`
  by default.
- `opencode`: launched as `opencode acp --cwd <cwd>`.

The gateway launches allowlisted per-agent process shapes. ACP service config
does not expose arbitrary stdio command execution.

Deferred work:

- in-repository Codex app-server bridge (`codex` mode `app_server`)
- crash retry for failed agent turns

## Runtime Layers

ACP is split into three runtime layers:

```text
HTTP dispatcher/admin
  -> pkg/gateway/acproute        route config expansion and runtime route model
  -> pkg/acp/runtime             Manager pool, Instance driver, permissions, transcript replay
  -> pkg/acp/agentspi            agent SPI and optional capabilities
  -> pkg/acp/agent/{codex,opencode}
  -> pkg/acp/transport           JSON-RPC over stdio transport
```

The core driver lives in `pkg/acp/runtime` and depends only on the agent SPI.
Agent packages register factories with `pkg/acp/agentspi` and are linked by the
main binaries through blank imports. This keeps the dependency direction one-way:

```text
runtime -> agentspi <- agent/*
runtime -> transport
agent/* -> transport
```

## Request Flow

```text
POST /<acp-route>/turn
  -> http.handlers.agent_route_dispatcher
  -> match AgentRouteConfig with protocol acp
  -> resolve ACPRoute and service_id
  -> validate VirtualKey when auth_policy.require_virtual_key is true
  -> rewrite route prefix; accept /turn
  -> pkg/acp/runtime.Manager.ServeTurn
  -> select or create a pooled Instance for service/thread/session scope
  -> Instance initialize/session/new or session/load
  -> apply model and config overrides through session/set_config_option
  -> call session/prompt
  -> normalize session/update notifications into SSE events
```

Permission decisions use the same route prefix:

```text
POST /<acp-route>/permission
  -> resolve pending interactive permission request by request_id
  -> answer the waiting ACP session/request_permission callback
```

The Admin API has an operator escape hatch at
`POST /admin/acp/runtime/permissions/{request_id}`.

## Config Model

ACP configuration is split into services and routes.

An ACP service describes one agent backend:

- stable management `id`
- `agent_type` (`codex` or `opencode`)
- absolute `cwd`
- optional `allowed_roots`
- optional `default_model`
- optional `config_overrides`
- optional `idle_ttl`
- optional `max_instances`
- `permission_mode` (`deny`, `auto_approve`, or `interactive`)
- optional `codex` adapter settings

An ACP route exposes one service through the dispatcher:

- `kind: acp`
- `protocol: acp`
- `match_policy.path_prefix`
- `auth_policy.require_virtual_key`
- target policy kind `acp-service`, expanded as top-level `service_id` in
  ACP route API and bundle objects

When omitted, ACP route IDs normalize to the deterministic, slash-free form
(the path prefix lowercased, non-alphanumeric runs collapsed to `-`, `/` → `root`):

```text
acp:<service_id>:<path-slug>
```

## Pooling And Sessions

The runtime manager keeps an in-memory pool of agent instances keyed by service,
thread, and session scope.

Important behavior:

- one active turn is allowed per scope
- `fresh_session: true` forces a new backend session
- `idle_ttl` reaps idle instances
- dead instances are evicted before reuse
- `DELETE /admin/acp/runtime/threads/{service_id}/{thread_id}` closes pooled
  instances for one thread
- when a later turn supplies the `session_id` emitted by the first turn, the
  manager can rebind the thread's live instance instead of spawning a second
  process

Session metadata is cached per pooled instance and replayed at turn start:

- config options
- available commands
- session info
- current mode
- usage

The same metadata is visible through `GET /admin/acp/runtime`.

## Events

ACP `session/update` notifications are parsed in
`pkg/acp/runtime/acpupdate` and emitted as SSE events. Implemented event names:

- `session`
- `delta`
- `reasoning`
- `content`
- `plan`
- `tool_call`
- `usage`
- `available_commands`
- `session_info`
- `mode`
- `config_options`
- `permission`
- `done`
- `error`

Structured updates carry the raw ACP update object in `data`. Text updates use
`text`. Prompt completion emits `done` with `stop_reason` when available.

The prompt loop drains briefly after the `session/prompt` result because real
agents can deliver final message chunks after the result frame.

## Permission Model

ACP permission behavior is fail-closed.

Modes:

- `deny`: default; permission requests are cancelled.
- `auto_approve`: the runtime selects an allow-style option when available.
- `interactive`: the active turn stream receives a `permission` SSE event with
  `request_id` and raw ACP params. The client answers through
  `POST /<acp-route>/permission`, or an operator answers through the Admin API.

Permission decisions use ACP's nested outcome shape internally:

- `outcome: "selected"` with `option_id`
- `outcome: "cancelled"`

If the stream is gone, no decision arrives before timeout, or the request is
unknown, the runtime cancels the permission.

## Admin And Operations

Admin families:

- `/admin/acp/services`
- `/admin/acp/routes`
- `/admin/acp/runtime`

Operational capabilities:

- service and route CRUD
- list agent-side sessions
- replay a transcript through `session/load`
- inspect in-flight turns
- inspect pooled instances and pending permissions
- close all pooled instances for one service/thread
- resolve an interactive permission request

`agwctl gateway acp-service`, `agwctl gateway acp-route`, and
`agwctl gateway acp-runtime` wrap these Admin APIs.
