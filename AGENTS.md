# AGENTS.md

## Purpose

This repository builds a custom Caddy binary that acts as an AI gateway for LLM, MCP, and ACP traffic.
The current primary LLM path is:

1. `agent_gateway` app loads providers, routes, virtual keys, credentials, and CLI auth state
2. `agent_route_dispatcher` matches an incoming HTTP request to a route
3. the route's `protocol` selects the protocol adapter (`openai` or `anthropic`)
4. the gateway validates the VirtualKey
5. in logical-model routes, the model catalog resolves the logical model to one concrete `(provider_id, upstream_model)` binding
6. the selected provider executes `Generate` or `Stream`

MCP is also active now through `agent_route_dispatcher` with MCP enabled, `pkg/gateway/mcproute`, `pkg/mcp/service`, and MCP Admin APIs. ACP is being implemented natively through `pkg/acp`, `pkg/gateway/acproute`, dispatcher turn handling, and ACP Admin APIs. Metrics now persist LLM/MCP/ACP usage events (with optional `agent_id` attribution) and expose Admin summaries/events. The agent control plane is active through `pkg/agent`, the `agents` config store, and `/admin/agents` Admin APIs (P0 + P1). Memory is not shipped in v0.4.x; `/admin/memory/...` is reserved and returns `501 Not Implemented`.

## Change Policy

- by default, changes in this repository do not preserve backward compatibility
- do not keep legacy aliases, deprecated field names, old route shapes, old module IDs, old CLI flags, or old API-visible IDs unless the change request explicitly requires compatibility
- when renaming or reshaping behavior, update the code, tests, `README.md`, `docs/architecture/architecture-overview.md`, `Caddyfile.example`, and this file to describe only the current behavior unless compatibility is explicitly required

## Build & Run

```bash
# Build the main gateway binary, standalone daemon, and management CLI
make build

# Or build only the gateway binary
go build -o agw ./cmd/agw

# Or build only the standalone daemon
go build -o agwd ./cmd/agwd

# Or build only the management CLI
go build -o agwctl ./cmd/agwctl

# Run with a Caddyfile
./agw run --config ./Caddyfile.example

# Format
go fmt ./...

# Static analysis
go vet ./...

# Tests
go test ./...
go test ./path/to/package -run TestName -v
```

Notes:

- `make build` builds `agw` from `cmd/agw/main.go`, `agwd` from `cmd/agwd/main.go`, and `agwctl` from `cmd/agwctl/main.go`.
- The resulting binary is a standard Caddy binary with custom modules compiled in, so normal Caddy subcommands such as `run`, `reload`, `validate`, and `hash-password` work.

## Core Modules

### Caddy app

- Module ID: `agent_gateway`
- Package: `caddy/gateway/`
- Main entry: `caddy/gateway/app.go`

Responsibilities:

- initialize the config store
- load static providers from the Caddyfile
- create the shared credential manager and CLI auth refresher
- create the runtime `AgentGateway`

### HTTP middleware

- Module ID: `http.handlers.agent_route_dispatcher`
- Package: `caddy/dispatcher/`
- Main entry: `caddy/dispatcher/dispatcher.go`

Responsibilities:

- resolve the matching `AgentRoute`
- select the route's `protocol`
- rewrite the request path by removing the route `path_prefix`
- validate the VirtualKey
- prepare the provider request payload
- resolve the logical model or direct provider target
- rewrite the provider-facing request model when logical-model routing is used
- invoke the selected LLM protocol handler
- when `mcp` is configured, resolve `MCPRoute` requests, parse MCP JSON-RPC, and invoke `pkg/mcp/service`
- track in-flight MCP requests and progress through the shared runtime registry
- when `acp` is configured, resolve `ACPRoute` requests, parse the gateway ACP turn request, and invoke `pkg/acp/runtime`

### Protocol handler modules

- Module ID: `agent_route_dispatcher.llm_apis.openai`
  - Runtime package: `pkg/dispatcher/llmapi/openai/`
  - Caddy adapter: `caddy/dispatcher/llmapi/openai/`
- Module ID: `agent_route_dispatcher.llm_apis.anthropic`
  - Runtime package: `pkg/dispatcher/llmapi/anthropic/`
  - Caddy adapter: `caddy/dispatcher/llmapi/anthropic/`
- Module ID: `agent_route_dispatcher.llm_apis.cc`
  - Runtime package: `pkg/dispatcher/llmapi/cc/`
  - Caddy adapter: `caddy/dispatcher/llmapi/cc/`

Responsibilities:

- parse wire-format requests
- convert HTTP payloads into `provider.ChatRequest`
- convert provider responses back to protocol-specific JSON or SSE

The `cc` handler is the Claude Code CLI-compatible Anthropic Messages profile. Keep Claude Code CLI-specific protocol shims in this handler rather than in generic providers.

These modules are not standalone `http.handlers.*` modules. They are loaded by `agent_route_dispatcher`.

### Admin API

- Module ID: `http.handlers.agent_gateway_admin`
- Package: `caddy/admin/`

Responsibilities:

- expose Admin API handlers; authentication is delegated to the HTTP mount layer
  such as Caddy `basic_auth`, mTLS, a reverse proxy authenticator, or standalone
  `--admin-basic-auth-hash`
- CRUD for providers, routes, virtual keys, and credentials
- CRUD for `mcp_services` and MCP routes
- CRUD for `acp_services` and ACP routes
- MCP discovery, execution, and dispatcher runtime inspection
- list startup-enabled provider types and LLM API handler types
- configure and trigger CLI auth authenticators
- start CLI auth logins bound to one `provider_id` and optional credential scope
- expose metrics summaries (with pipeline health counters), per-protocol LLM/MCP/ACP timeseries and breakdowns, recent interaction events, and a Prometheus exposition endpoint
- expose stubbed memory and agent endpoints

## Key Packages

### `pkg/gateway/`

Important files:

- `agentgateway.go`: runtime route, VirtualKey, and provider resolution
- `providerresolver.go`: static and dynamic provider resolution

Dynamic provider configs are cached after first load and invalidated through
the manager's create/update/delete paths. Do not put config-store reads back in
the per-request provider resolution hot path.

`AgentGateway` is the main runtime object. It resolves routes, validates VirtualKeys, and selects providers. It does not own the HTTP protocol details.

### `caddy/gateway/`

Important files:

- `app.go`: Caddy app wiring and runtime bootstrap
- `caddyfile.go`: global `agent_gateway` Caddyfile parsing

### `pkg/gateway/llmroute/`

Defines the route model used by static config, the Admin API, and runtime resolution.

Important types:

- `AgentRoute`
- `RouteMatch`
- `RouteTargetPolicy`
- `DirectProviderTarget`
- `RoutePolicy`
- `SelectionPolicy`
- `RetryPolicy`

Current route modes:

- model-target mode: `target_policy.model_targets` with optional `default_model`
- direct-provider mode: `target_policy.provider_target.provider_id`

Runtime route matching uses the in-memory manager snapshot. Bootstrap and
route manager create/update/delete/refresh keep that snapshot populated; do not
reintroduce per-request config-store `List` calls for matching.

Static config restriction:

- Caddyfile routes and standalone `--static-config` bundle `llmRoutes` only support direct-provider mode
- logical-model routes remain supported through the Admin API and config-store-backed bundle workflows

The route model uses `protocol` and `require_virtual_key`. Do not reintroduce the old `local API key` naming in new code or docs.

### `pkg/gateway/modelcatalog/`

This package owns provider model discovery, managed model overlays, and runtime validation of concrete route candidates.

Important types:

- `ManagedModel`
- `ProviderModelSnapshot`

### `pkg/gateway/mcproute/`

Defines the MCP route config expansion and runtime route model.

Important types:

- `MCPRouteConfig`
- `MCPRoute`
- `RouteMatch`
- `RouteAuthPolicy`

Current shape:

- persisted/static MCP routes use `routecore.AgentRouteConfig`
- `MCPRouteConfig` is the expanded config form used by admin and config-adjacent layers that need direct `service_id` access
- `MCPRoute` is the runtime object created by `MCPRouteResolver` and used by dispatcher/runtime code
- prefer `*MCPRoute` at runtime rather than copying `MCPRoute` values
- route ids must be slash-free so they are addressable as a single Admin API path segment (`/admin/mcp/routes/{id}`); `Normalize` auto-generates the deterministic `mcp:<service_id>:<path-slug>` (slug = path prefix lowercased, non-alphanumeric runs collapsed to `-`, `/` → `root`) when `id` is empty, and `routecore.ValidateRouteID` rejects slash-bearing ids at create/validate time. The id is fully predictable, so other objects (e.g. `allowed_route_ids`) can reference it before apply; two routes whose paths slugify to the same value collide and surface as a duplicate-id error, at which point set an explicit id.

### `pkg/gateway/acproute/`

Defines the ACP route config expansion and runtime route model.

Important types:

- `ACPRouteConfig`
- `ACPRoute`
- `RouteMatch`
- `RouteAuthPolicy`

Current shape:

- persisted/static ACP routes use `routecore.AgentRouteConfig`
- `ACPRouteConfig` is the expanded config form used by admin and config-adjacent layers that need direct `service_id` access
- `ACPRoute` is the runtime object created by `ACPRouteResolver` and used by dispatcher/runtime code
- route ids must be slash-free so they are addressable as a single Admin API path segment (`/admin/acp/routes/{id}`); `Normalize` auto-generates the deterministic `acp:<service_id>:<path-slug>` when `id` is empty, and `routecore.ValidateRouteID` rejects slash-bearing ids at create/validate time. The id is fully predictable; two routes whose paths slugify to the same value collide and surface as a duplicate-id error, at which point set an explicit id.

### `pkg/acp/`

Owns native ACP service config and runtime integration. Do not add a dependency on `github.com/beyond5959/ngent`; ACP runtime support is implemented in this repository.

Scope:

- supported agent types: `codex`, `opencode`
- service config store: `acp_services`
- dispatcher endpoints: `POST /<acp-route>/turn` with SSE events (`session`, `delta`, `reasoning`, `content`, `plan`, `tool_call`, `usage`, `available_commands`, `session_info`, `mode`, `config_options`, `permission`, `done`, `error`), `POST /<acp-route>/permission` for interactive permission decisions, `GET /<acp-route>/sessions`, and `GET /<acp-route>/sessions/{session_id}/transcript`
- permission modes are `deny` (default, fail-closed), `auto_approve`, and `interactive`; permission replies use the nested ACP outcome shape, run off the transport read loop, and time out fail-closed
- `interactive` surfaces `session/request_permission` as a `permission` SSE event (`request_id` + raw ACP params with exact option ids) on the active turn stream and resolves through `POST /<acp-route>/permission` (or the admin escape hatch `POST /admin/acp/runtime/permissions/{request_id}`); no streaming turn client, a decision timeout, or transient connections (session/list, transcript) all fail closed
- runtime shape: one stdio JSON-RPC driver plus thin agent adapters registered through `pkg/acp/agentspi`; `session/update` parsing lives in `pkg/acp/runtime/acpupdate`
- process shapes: `opencode` uses fixed `opencode acp --cwd <cwd>`; `codex` uses fixed external ACP adapter binary `codex-acp` by default
- model selection and `config_overrides` go through `session/set_config_option` (ACP has no `session/set_model`); opencode model selection sets its `model` config option
- `session/list` is exposed both route-scoped as `GET /<acp-route>/sessions` and service-scoped for operators as `GET /admin/acp/services/{id}/sessions`, checks the initialized ACP capability before calling the method, and supports optional `cwd` and `cursor` query parameters; the `cwd` filter is applied gateway-side with both sides symlink-canonicalized (opencode stores canonical session cwds, codex-acp stores the session/new cwd verbatim — never forward the filter to the agent)
- transcript replay is exposed both route-scoped as `GET /<acp-route>/sessions/{session_id}/transcript` and service-scoped for operators as `GET /admin/acp/services/{id}/sessions/{session_id}/transcript`: a transient connection checks the `loadSession` capability, replays the session via `session/load`, and returns coalesced `{role, text}` messages; `agentspi.TranscriptLoader` is the agent-specific override seam
- both surfaces of session/list and transcript share one error-status contract: service-not-found is `404`, a client-correctable request (disabled service, `cwd` outside `allowed_roots`, missing session id) is `400` and surfaces through the `acpruntime.ErrInvalidRequest` sentinel, and an upstream agent/transport failure is `502`
- each pooled instance caches the latest session metadata (config options, slash commands, session info, mode, usage) from a lifetime updates subscription, replays it as snapshot events at every turn start, and exposes it through `GET /admin/acp/runtime`
- codex-acp (verified live against v0.16.0) does NOT push `session_info_update`, so the `session_info` SSE event never fires for codex and its cached session info stays empty; the session title for a codex session is only available through `session/list` (`GET /<acp-route>/sessions` or `GET /admin/acp/services/{id}/sessions` → `sessions[].title`, parsed by `parseListSessionsResponse` into `SessionInfo.Title`), which codex auto-derives from the first user message. The `session_info` snapshot/SSE path remains for agents that do emit `session_info_update`.
- pool lifecycle: idle janitor (`IdleTTL`), optional per-service `MaxInstances` cap, dead-instance eviction, `fresh_session`, setup-handshake timeout, `PATH` preflight, stderr capture, `CloseScope`/`CloseThread`, and scope rebind (a session-addressed turn adopts the thread's live instance bound to that session instead of spawning a second process)
- do not reintroduce a `model`/`modelId` field on `session/new`/`session/prompt`, and do not answer `session/request_permission` with a flat `approved`/`declined` outcome — both are non-conformant with the ACP v1 schema
- the prompt loop keeps draining after the session/prompt result until the update stream is quiet for a short grace period — the real opencode binary can deliver the final `agent_message_chunk` updates after the result, so a buffered-only drain drops the reply tail
- session ids: the driver splits the raw protocol id (wire calls) from the host-bound id (session events, scope adoption) with `StableSessionResolver`/`SessionLoadResolver` seams for agents whose ids differ; no built-in agent implements them — the real `codex-acp` adapter's raw ids are verified stable (listable and loadable from a fresh process after the first turn)
- agwctl coverage: `acpServices`/`acpRoutes` are gateway bundle objects (apply/export/validate), `pkg/adminclient` has the full ACP client surface, and `agwctl gateway acp-service|acp-route|acp-runtime` subcommands cover service/route CRUD reads, sessions, transcript, runtime view, in-flight turns, thread close, and permission resolution
- deferred: crash retry and the codex app-server bridge (v2; its stable-id/load-id driver seams are already wired)

### `pkg/gateway/virtualkey/`

This package owns VirtualKey extraction, validation, and storage-facing helpers.

Use this terminology consistently:

- `VirtualKey`
- `VirtualKeyManager`
- `VirtualKeyStore`

Current shape:

- `VirtualKey.ID` is required and is the management/storage primary key
- `VirtualKey.Key` is the bearer key value used at request time and is generated by the gateway
- Caddyfile and standalone static bundle config do not support static virtual keys; create them through the Admin API

The gateway accepts a VirtualKey from either:

- `Authorization: Bearer <key>`
- `x-api-key: <key>`

### `pkg/llm/provider/`

This package defines the provider interface and provider registry.

Important files:

- `provider.go`: provider request and response types
- `registry.go`: provider factory registration

Provider config `api_key` values are provider-local fallback configuration.
They do not register as managed credentials and do not participate in credential scheduling.

Built-in provider runtime packages:

- `openai`
- `anthropic`: delegates chat/streaming to the eino-ext `claude` component (official anthropic-sdk-go); the provider keeps thinking-budget normalization, request metadata, and ListModels. `anthropicbase` remains the hand-rolled Messages wire layer used by `claudecode`.
- `claudecode`
- `codex`
- `gemini`
- `ollama`
- `openrouter`
- `deepseek`: delegates chat/streaming to the eino-ext `deepseek` component
  (`deepseek-go` underneath); the provider retains request compatibility,
  thinking-mode defaults, and Responses-via-chat adaptation
- `zhipu`
- `qwen`: DashScope OpenAI-compatible mode via the eino-ext `qwen` component; optional `enable_thinking` provider option, per-request reasoning fields override it

eino bridge packages (standalone libraries, the PB0 prerequisites of the
builtin agent runtime — see `docs/design/agents-control-plane.md` §5.7):

- `pkg/llm/provider/einomodel`: presents a `provider.Provider` (preferably a
  `*gateway.RoutedProvider`, so credential scheduling, candidate fallback,
  and usage attribution apply) as an eino `model.ToolCallingChatModel`
- `pkg/mcp/einotool`: presents gateway-managed MCP service tools
  (`pkg/mcp/service`) as eino `InvokableTool`s; selecting tools by name is
  fail-closed (a missing tool is an error, not a silent skip)

Self-implemented chat providers (`codex`, `claudecode`) fire the eino
callback aspect functions through the `provider.OnChatStart/OnChatEnd/
OnChatError/OnChatStreamEnd` helpers (`callbackaspects.go`), so registered
callbacks handlers observe every chat provider uniformly.

Provider registration rules:

- implement the `provider.Provider` interface
- register the factory with `provider.RegisterProviderFactory(...)`
- add a blank import for the runtime provider package in `cmd/agw/main.go`, `cmd/agwd/main.go`, and `cmd/agwctl/cmd_gateway.go`; agwctl needs it because `gateway validate`/`apply` check `provider_type` against the locally linked provider registry

### `pkg/cliauth/`

This is a `pkg` runtime package, not `llm/cliauth/`.

Important files:

- `authenticator.go`: `Authenticator` interface and factory registration
- `manager.go`: runtime authenticator registry and state
- `autorefresher.go`: background refresh scheduling
- `types.go`: credential and status types

Built-in authenticators currently registered via `pkg/cliauth/authenticator/`:

- `codex`
- `claudecode`
- `gemini`

Authenticator registration rules:

- implement the `cliauth.Authenticator` interface
- register the factory with `cliauth.RegisterAuthenticatorFactory(...)`
- ensure the package is included through the blank import of `pkg/cliauth/authenticator` in `cmd/agw/main.go`

### `pkg/llm/credentialmgr/`

This package manages persisted upstream credentials and selection state. It is separate from the provider registry and separate from `cliauth`, though `cliauth` integrates with it through an adapter.

### `pkg/configstore/`

Important packages:

- `pkg/configstore/`: generic store/backend interfaces, shared schema primitives, backend factory, and backend registration
- `pkg/configstore/schema/`: store names and built-in business schemas for persisted config object families
- `pkg/configstore/sqlite/`: SQLite JSON backend implementation
- `caddy/configstore/sqlite/`: SQLite backend Caddy adapter

The top-level storage interface is `ConfigStoreBackend`, which registers and returns schema-bound generic stores:

- `Register(name string, schema StoreSchema) error`
- `Get(name string) (ConfigStore, error)`

Current store names:

- `providers`
- `credentials`
- `routes`
- `mcp_services`
- `acp_services`
- `agents`
- `virtual_keys`
- `managed_models`

Current persisted backend:

- `sqlite`

### `pkg/agent/`

The external control-plane layer that composes the LLM/MCP/ACP/metrics surfaces
around an operator-facing agent identity. It depends on the lower protocol
managers; those packages must not depend on `pkg/agent`.

Important files:

- `types.go`: the `Agent` model. Runtime is `acp` (gateway owns the lifecycle via
  an ACP `service_id`) or `http` (the agent owns its own lifecycle). LLM and MCP
  are `resources`, not runtime types. `policy` is runtime-agnostic; ACP
  operational config stays on the ACP service.
- `manager.go`: agent CRUD plus the in-memory route/service → agent index. It
  enforces P0 one-runtime-one-agent (a `service_id` is bound by at most one
  agent), route-binding uniqueness (any LLM/MCP/ACP `route_id` is owned by at
  most one agent, so the route → agent attribution mapping stays unambiguous),
  and `acp_route_ids` → runtime-service consistency, and implements
  `ResolveAgentID` (the `usage.AgentAttributor` seam). `Refresh` rebuilds the
  index defensively: a `service_id` or `route_id` that resolves to more than one
  agent is dropped from the map (and `ResolveAgentID` returns `ok=false`) rather
  than silently picking a last writer.

Agents are a first-class gateway-bundle object (apply/export/validate) and have
an `agwctl gateway agent` read surface; create/update flow through the bundle.
See `docs/design/agents-control-plane.md` (including the §11 implementation
status) for the full direction.

### `internal/observability/`

Owns durable usage events and query helpers.

Important packages:

- `internal/observability/usage`: event models (with an optional `agent_id` attribution tag and the LLM token detail fields `cached_tokens`/`reasoning_tokens`), observer/span interfaces, no-op observer, the `AgentAttributor` seam plus the settable `AgentAttribution` holder, usage service, Prometheus exposition rendering, and metrics config (`retention_days`, `max_agent_depth`)
- `internal/observability/pipeline`: buffered event pipeline, SQLite sink (with a background retention janitor), the in-process Prometheus counter sink, and an `OpenTelemetrySink` adapter seam (no exporter is wired; sinks are passed to `NewEventPipeline` in `caddy/gateway/app.go` and `standalone/server/server.go`, so wiring one is an in-tree change)
- `internal/observability/einotap`: the process-global eino callbacks handler. It folds chat-model component detail into the current interaction span on the synchronous `OnEnd` timing only — merge-never-emit, no stream timings, no stream copies — and `einotap.Register()` is called from both bootstrap paths under a `sync.Once`, so Caddy config reloads cannot double-register it

SQLite usage tables are typed event tables (`llm_usage_events`, `mcp_usage_events`, `acp_usage_events`) created by the metrics sink through the sqlite backend's `UsageDB()` capability. They carry a nullable `agent_id` attribution column, stamped at write time by the observer when the originating route resolves to exactly one agent. They are separate from generic JSON config stores. Time-series and breakdown queries scan these event tables directly; there are no internal rollup tables. Use the Prometheus exposition (`GET /admin/metrics/prometheus`) plus an external system (Prometheus/Grafana) for high-volume aggregation, trends, and alerting.

The `metrics` Caddyfile block and `agwd` flags configure usage retention cleanup and agent-depth enforcement. Retention is applied at startup and by a periodic janitor in the SQLite sink. The dispatcher rejects requests when inbound `X-Agent-Depth` reaches the configured `max_agent_depth`; `0` disables the gate.

## Runtime Request Flow

```text
HTTP request
  -> http.handlers.agent_route_dispatcher
  -> AgentGateway.ResolveRoute(...)
  -> pick route.protocol
  -> rewrite path using route.match.path_prefix
  -> AgentGateway.ResolveVirtualKey(...)
  -> protocol handler PrepareLLMApiRequest(...)
  -> if route uses model targets: resolve the requested route model name to one concrete binding and rewrite request model
  -> else: use route.target_policy.provider_target.provider_id
  -> resolve provider instance
  -> provider.Chat(...) or provider.StreamChat(...)
  -> protocol handler writes JSON or SSE response
```

Key detail: provider resolution still happens after protocol parsing, but the request `model` now means route target name in model-target mode and upstream model name in direct-provider mode.

## Caddyfile Shape

The main config lives in the global `agent_gateway` block.

Minimal example:

```caddy
{
    agent_gateway {
        config_store sqlite {
            path ./data/configstore.db
        }

        provider_types {
            openai
        }

        provider openai-main {
            provider_type openai
            api_key {$OPENAI_API_KEY}
            default_model gpt-4.1
        }

        route openai-chat {
            protocol openai
            path_prefix /
            require_virtual_key
            target provider openai-main
        }
    }
}

http://127.0.0.1:8080 {
    agent_route_dispatcher {
        llm_api openai
        llm_api anthropic
        llm_api cc
        mcp
        acp
    }
}
```

Important current directives:

- `provider_types` is startup-only provider type availability; when omitted all registered provider types are enabled
- providers use `provider_type <name>`
- LLM routes use `protocol <openai|anthropic|cc>`, MCP routes use `protocol mcp`, and ACP routes use `protocol acp`
- `agent_route_dispatcher` uses `llm_api <name>` for LLM protocol handlers, `mcp` to enable MCP protocol handling, and `acp` to enable ACP turn handling
- auth uses `virtualkey`, not `local_api_key`

## Admin API Notes

Implemented families:

- `/admin/llm/providers/...`
- `/admin/llm/provider_types` read-only listing
- `/admin/llm/api_handler_types`
- `/admin/llm/routes/...`
- `/admin/virtual_keys/...`
- `/admin/credentials/...`
- `/admin/llm/models/providers/{provider_id}/discovered`
- `/admin/llm/models/providers/{provider_id}/refresh`
- `/admin/llm/models/managed/...`
- `/admin/cliauth/authenticators/...`
- `/admin/cliauth/refresher/...`
- `/admin/cliauth/logins/...`

Implemented MCP families:

- `/admin/mcp/services/...`
- `/admin/mcp/routes/...`
- `/admin/mcp/runtime/...`

Implemented ACP families:

- `/admin/acp/services/...`
- `/admin/acp/routes/...`
- `/admin/acp/runtime/...`

Implemented agent families:

- `/admin/agents/...` (CRUD plus `/{id}/workspace`, `/{id}/activity`,
  `/{id}/usage`, `/{id}/interactions`, `/{id}/resources`, `/{id}/health`)

Stubbed families currently return `501 Not Implemented`:

- `/admin/memory/...` (reserved; memory is not shipped in v0.4.x)

## Files To Check Before Large Changes

- `README.md`: user-facing setup and API examples
- `docs/architecture/architecture-overview.md`: broader architecture and roadmap
- `Caddyfile.example`: working reference config
- `cmd/agw/main.go`: the definitive list of linked modules

If you change module IDs, route semantics, provider registration, or Admin API paths, update this file and `README.md` in the same change.
