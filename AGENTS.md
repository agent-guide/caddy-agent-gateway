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

MCP is also active now through `agent_route_dispatcher` with MCP enabled, `pkg/gateway/mcproute`, `pkg/mcp/service`, and MCP Admin APIs. ACP is being implemented natively through `pkg/acp`, `pkg/gateway/acproute`, dispatcher turn handling, and ACP Admin APIs. The builtin agent runtime is active: agents with `runtime.type = "builtin"` are persisted definitions materialized by the in-process eino ADK host (`pkg/agent/builtin`) and exposed through builtin routes (`pkg/gateway/builtinroute`, dispatcher `builtin` enablement, `POST /<builtin-route>/turn` SSE). Metrics now persist LLM/MCP/ACP/builtin usage events (with optional `agent_id` attribution) and expose Admin summaries/events. The agent control plane is active through `pkg/agent`, the `agents` config store, and `/admin/agents` Admin APIs (P0 + P1 + PB1). Memory is not shipped in v0.4.x; `/admin/memory/...` is reserved and returns `501 Not Implemented`.

## Product Site

`website/` holds the static product marketing site for https://agentguide.online
(plain HTML + one shared stylesheet `website/assets/site.css`, no build step).
It is not part of the gateway binary or its tests. Pages: `index.html`,
`platform.html`, `agents.html`, `solutions.html`, `observability.html`,
`why.html`, each with a Chinese translation under `website/zh/` (same file
names, `lang="zh-CN"`, `hreflang` alternates, nav language switcher). A content
change to any page must be applied to both language versions. Keep marketing
claims in sync with actual capabilities; features that are not implemented yet
must be labeled as roadmap on the pages.

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
- when `builtin` is configured, resolve `BuiltinRoute` requests, stamp the route's target agent id on the interaction span, and invoke the in-process ADK host (`pkg/agent/builtin`) for `POST /<builtin-route>/turn` SSE

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
builtin agent runtime — see `docs/design/builtin-agent-runtime.md` §11):

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
  `usage.AgentAttributor` seam). `Refresh` rebuilds the index defensively: a
  `service_id` or `route_id` that resolves to more than one agent is dropped
  from the map (and `ResolveAgentID` returns `ok=false`) rather than silently
  picking a last writer.

Agents are a first-class gateway-bundle object (apply/export/validate) and have
an `agwctl gateway agent` read surface; create/update flow through the bundle.
See `docs/design/agents-control-plane.md` for the cross-runtime direction and
`docs/design/builtin-agent-runtime.md` for the builtin runtime design and
implementation status.

### `pkg/agent/builtin/`

The generic eino ADK host for builtin-runtime agents
(`docs/design/builtin-agent-runtime.md`). One host instance (owned by
`AgentGateway`) serves
every builtin agent:

- materialization cache keyed by agent id + `updated_at`; a definition update
  re-materializes on the next turn while in-flight turns drain on the old graph
- the agent's model resolves through its LLM route into a `RoutedProvider`
  wrapped by the `einomodel` bridge, so credential scheduling, candidate
  fallback, and LLM usage events apply unchanged; a node that carries tools
  (and every tool-calling head: the supervisor head, the deep head, and the
  planexecute planner/replanner) resolves with `RequireTools`, so
  logical-model routes only bind tool-capable candidates; MCP tools come
  through the `einotool` bridge (fail-closed name selection); a `model.retry`
  block maps onto `ChatModelAgentConfig.ModelRetryConfig` (and `deep.Config`),
  retrying the whole routed call — including its internal candidate fallback —
  on 429/5xx with the ADK default backoff, mirroring
  `RoutedProvider.classifyFailure` via `internal/statuserr`
- every turn runs under panic recovery; `max_concurrent_turns` and
  `turn_timeout_seconds` are fail-closed (reject, not queue); a disabled agent
  rejects turns as a client-correctable error
- operator turn cancellation (`builtin-agent-runtime.md` §10): every turn
  (fresh or resumed) runs
  under an `adk.WithCancel` handle registered in an in-flight registry keyed by
  `(agent_id, session_id)`; `force` (`CancelImmediate`, the answer for stuck
  turns) or `graceful` (safe-point stop propagated through nested agents and
  escalating to immediate) cancel maps
  the resulting `adk.CancelError` to a `done` event with
  `stop_reason: "cancelled"` and discards the uncommitted partial exchange
- sessions are in-memory conversation histories (idle TTL, per-agent cap,
  message cap) with documented restart-loss semantics; durable checkpoints wait
  for eino v0.10 and must not be hand-rolled before that. Turns on one session
  are serialized through a context-aware wait that happens before the
  concurrency semaphore (waiting same-session turns never occupy
  `max_concurrent_turns` slots and abort with `session_busy` on turn timeout
  or client disconnect); a new session beyond the cap with nothing evictable
  is rejected (`session_limit_exceeded`), never queued; cap eviction runs only
  when a new session is created and never touches the session a turn is
  reusing (a continued conversation cannot lose its history to the evictor)
- observability: the dispatcher stamps the route's target agent id on the turn
  span; the host opens child spans for inner model calls (kind `llm`) and tool
  executions (kind `mcp`), parented under the turn span and carrying the agent
  id, so inner traffic produces `llm_usage_events`/`mcp_usage_events` as usual;
  every builtin logical turn has a stable `run_id`, and an HITL resume starts a
  fresh trace with an OpenTelemetry Span Link to the checkpoint-producing span
- turn events use the shared vocabulary subset `session`, `delta`, `content`,
  `tool_call`, `usage`, `permission`, `done`, `error`
- interactive tool permissions (`builtin-agent-runtime.md` §9): with
  `permissions.mode = "interactive"` every non-allowlisted MCP tool call
  interrupts the turn
  through the ADK Runner checkpoint cycle — the approval gate wraps the
  einotool bridge outermost (denied/interrupted calls never open an MCP child
  span), the run state checkpoints into an in-memory store, and the turn ends
  with a `permission` event plus `done`/`permission_required` while holding
  no turn slot, stream, or goroutine. The client resumes through
  `POST /<builtin-route>/turn` with a `permission` field (per-call
  allow/deny; unanswered calls denied; request-level `cancel` discards),
  which streams the continuation and commits the full exchange on
  completion. Everything fails closed: decision TTL expiry, definition
  updates (a checkpoint only resumes on the graph that produced it),
  `max_pending` capacity, and new input on a suspended session (rejected
  with the pending request id). SSE events, builtin usage events, and the
  Admin runtime view expose the stable run/session/request correlation ids;
  TTL cleanup emits a linked `permission_expire` lifecycle event. Pending
  permissions are one-shot and listed in `GET /admin/builtin/runtime`;
  lifecycle expiry events remain queryable but are excluded from request
  summaries so they do not skew counts or latency;
  middleware tools (skill, plantask,
  tool_search) are never gated
- middlewares attach to the root definition's chat-model nodes (single,
  supervisor head, deep head) in the fixed order patchtoolcalls → reduction →
  summarization → skill → plantask → toolsearch → agentsmd: patchtoolcalls
  completes dangling tool exchanges before anything else reads the history,
  reduction clears tool-output bloat before summarization counts tokens,
  toolsearch derives tool visibility after the context managers settle the
  history, and the agentsmd injection (inline docs served by an in-memory
  backend, transient per model call, never persisted to the session) stays
  invisible to all of them; reduction is clear-only because there is no file
  backend to offload to and no `read_file` tool to hand the model; with
  toolsearch enabled the node's MCP tools become the middleware's dynamic
  tools instead of static `ToolsConfig` entries (client-side search only), and
  reduction auto-excludes `tool_search` results from clearing since
  loaded-tool visibility is re-derived from them every call; skill and
  plantask tools stay statically visible under toolsearch — the plantask board
  is session-scoped in-memory state riding on the session object (evicted with
  the session, capped at 256 files, 256 KiB per file, and 1 MiB total,
  stateless backend reading the board from the turn context), and skills are
  inline virtual documents whose backend never returns fork/model frontmatter,
  so fork-mode execution is structurally unreachable
- custom Go agents register through `builtin.RegisterFactory` (mirroring
  provider registration) and are selected with `topology.kind = "custom"`;
  factory packages must be blank-imported in `cmd/agw/main.go`,
  `cmd/agwd/main.go`, and `cmd/agwctl/cmd_gateway.go`

### `pkg/gateway/builtinroute/`

Defines the builtin route config expansion and runtime route model, mirroring
`acproute` with an `agent_id` target instead of a service. Route ids are
slash-free and deterministic (`builtin:<agent_id>:<path-slug>` when `id` is
empty). The dispatcher serves `POST /<builtin-route>/turn` when `builtin` is
enabled.

### `internal/observability/`

Owns durable usage events and query helpers.

Important packages:

- `internal/observability/usage`: event models (with an optional `agent_id` attribution tag and the LLM token detail fields `cached_tokens`/`reasoning_tokens`), observer/span interfaces, no-op observer, the `AgentAttributor` seam plus the settable `AgentAttribution` holder, usage service, Prometheus exposition rendering, and metrics config (`retention_days`, `max_agent_depth`, and the `otlp` export block). A failed span without an explicit `error_type` falls back by status class (`client_error` for 4xx, `internal_error` otherwise), and `InteractionSpan.Discard` ends a span without emitting — the dispatcher discards the span whenever it passes a request through to the next handler, so unhandled requests never appear in usage events
- `internal/observability/pipeline`: buffered event pipeline, SQLite sink (with a background retention janitor), the in-process Prometheus counter sink, and the `OpenTelemetrySink` adapter (sinks are passed to `NewEventPipeline` in `caddy/gateway/app.go` and `standalone/server/server.go`)
- `internal/observability/otelexport`: the OTLP exporter behind `OpenTelemetrySink`. Usage events already carry W3C trace/span/parent ids, so the exporter reconstructs the interaction span tree post-hoc — each event becomes one OTel span (via `tracetest.SpanStub.Snapshot()`, the only supported ReadOnlySpan constructor outside the SDK) behind a `BatchSpanProcessor` shipping OTLP gRPC or HTTP. Builtin-internal LLM/MCP child events export as `internal`-kind spans; everything else is `server`. Enabled only when `metrics.otlp.endpoint` is set; an exporter setup failure logs a warning and disables export without affecting request serving. The optional `components` toggle additionally exports one client-kind span per eino chat-model component call through a process-global callbacks tap (registered once, exporter swapped atomically on reload; streaming spans end at stream-return and the tap closes its stream copy unread), parented under the interaction span from the request context — orphan calls without one export nothing
- `internal/observability/einotap`: the process-global eino callbacks handler. It folds chat-model component detail into the current interaction span on the synchronous `OnEnd` timing only — merge-never-emit, no stream timings, no stream copies — and `einotap.Register()` is called from both bootstrap paths under a `sync.Once`, so Caddy config reloads cannot double-register it

SQLite usage tables are typed event tables (`llm_usage_events`, `mcp_usage_events`, `acp_usage_events`, `builtin_usage_events`) created by the metrics sink through the sqlite backend's `UsageDB()` capability. They carry a nullable `agent_id` attribution column, stamped at write time by the observer when the originating route resolves to exactly one agent. They are separate from generic JSON config stores. Time-series and breakdown queries scan these event tables directly; there are no internal rollup tables. Use the Prometheus exposition (`GET /admin/metrics/prometheus`) plus an external system (Prometheus/Grafana) for high-volume aggregation, trends, and alerting.

The `metrics` Caddyfile block and `agwd` flags configure usage retention cleanup, agent-depth enforcement, and optional OTLP span export (`otlp { endpoint ... }` / `--metrics-otlp-*`). Retention is applied at startup and by a periodic janitor in the SQLite sink. The dispatcher rejects requests when inbound `X-Agent-Depth` reaches the configured `max_agent_depth`; `0` disables the gate.

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
- LLM routes use `protocol <openai|anthropic|cc>`, MCP routes use `protocol mcp`, ACP routes use `protocol acp`, and builtin routes use `protocol builtin`
- `agent_route_dispatcher` uses `llm_api <name>` for LLM protocol handlers, `mcp` to enable MCP protocol handling, `acp` to enable ACP turn handling, and `builtin` to enable builtin-agent turn handling
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

Implemented builtin families:

- `/admin/builtin/routes/...`
- `/admin/builtin/runtime` (host materializations, pending interactive
  tool permissions, and in-flight turns; permission decisions flow through the
  data-plane resume)
- `/admin/builtin/runtime/inflight` (in-flight builtin turns) and
  `DELETE /admin/builtin/runtime/turns/{agent_id}/{session_id}?mode=force|graceful`
  (operator force/graceful cancel of a running or stuck turn;
  `docs/design/builtin-agent-runtime.md` §10)

Implemented agent families:

- `/admin/agents/...` (CRUD plus `/{id}/workspace`, `/{id}/activity`,
  `/{id}/usage`, `/{id}/interactions`, `/{id}/resources`, `/{id}/health`;
  the workspace view carries a builtin slice — definition summary, host
  materialization state, builtin routes — for `runtime.type = "builtin"`)

Stubbed families currently return `501 Not Implemented`:

- `/admin/memory/...` (reserved; memory is not shipped in v0.4.x)

## Files To Check Before Large Changes

- `README.md`: user-facing setup and API examples
- `docs/architecture/architecture-overview.md`: broader architecture and roadmap
- `Caddyfile.example`: working reference config
- `cmd/agw/main.go`: the definitive list of linked modules

If you change module IDs, route semantics, provider registration, or Admin API paths, update this file and `README.md` in the same change.
