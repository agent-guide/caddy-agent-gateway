# AGENTS.md

## Purpose

This repository builds a custom Caddy binary that acts as an AI gateway for LLM, MCP, and ACP traffic.
The current primary LLM path is:

1. `agent_gateway` app loads providers, routes, virtual keys, and credentials, and configures the external credential refresh command
2. `agent_route_dispatcher` matches an incoming HTTP request to a route
3. the route's `protocol` selects the protocol adapter (`openai` or `anthropic`)
4. the gateway validates the VirtualKey
5. in logical-model routes, the model catalog resolves the logical model to one concrete `(provider_id, upstream_model)` binding
6. the credential manager invokes the configured external refresh command for an expiring `oauth_token` credential when required
7. the selected provider executes `Generate` or `Stream`

MCP is active through `agent_route_dispatcher` with MCP enabled. ACP and builtin execution now enter through unified `kind=agent` routes (`pkg/gateway/agentroute`, dispatcher `agent` enablement, `POST /<agent-route>/turn` SSE); the target Agent's `runtime.type` selects the registered backend. ACP execution config is owned inline by `Agent.runtime.acp`, while builtin definitions are materialized by the in-process eino ADK host. The agent control plane is exposed through `/admin/agents` and `/admin/agents/routes`. Memory is not shipped in v0.4.x; `/admin/memory/...` is reserved and returns `501 Not Implemented`.

Future gateway Pipeline support is limited to synchronous, request-bound
`llm`/`mcp`/`transform` composition. Durable Project/Team workflows, Agent
handoffs, schedules, human Tasks, and restart recovery belong to an upper-layer
workbench and an external engine such as Temporal; its Workers invoke the
gateway through normal AgentRoute/LLM/MCP data-plane APIs. Agent Gateway does
not own a durable business Workflow state machine.

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
- when renaming or reshaping behavior, update the code, tests, `README.md`, `docs/architecture/architecture-overview.md`, `Caddyfile.example`, this file, and the nearest nested `AGENTS.md` to describe only the current behavior unless compatibility is explicitly required

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

Bootstraps the config store, loads static providers from the Caddyfile,
creates the shared credential manager and external request-time refresh transport, and creates the
runtime `AgentGateway`. Caddyfile parsing lives in `caddy/gateway/caddyfile.go`.

### HTTP middleware

- Module ID: `http.handlers.agent_route_dispatcher`
- Package: `caddy/dispatcher/`
- Main entry: `caddy/dispatcher/dispatcher.go`

Resolves the matching route, selects the route `protocol`, rewrites the path
by removing the route `path_prefix`, validates the VirtualKey, prepares the
provider payload, resolves the logical model or direct provider target
(rewriting the provider-facing model for logical-model routes), and invokes the
LLM protocol handler. The runtime `RoutedProvider` selects and, when required,
refreshes an OAuth-backed credential before provider execution. With `mcp` or
`agent` enabled the dispatcher also
resolves those route kinds and dispatches to `pkg/mcp/service` or the
registered `runtimeapi` ACP/builtin adapters, tracks in-flight turns, and
stamps Agent identity on the interaction span. Subsystem detail:
`pkg/gateway/AGENTS.md`, `pkg/acp/AGENTS.md`, `pkg/agent/AGENTS.md`.

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

The `anthropic` and `cc` runtime packages are thin ingress profiles over the
shared Anthropic Messages core in `pkg/dispatcher/llmapi/anthropicmsg/`. That
core owns parsing, conversion, response lifecycle, and SSE block state.
Protocol-native state uses the scoped, dialect-neutral
`provider.ProtocolState`; same-dialect responses can select validated native
body or stream relay from the `ResolvedExecution` returned by the enriched
`RoutedProvider` contract.

Responsibilities:

- parse wire-format requests
- convert HTTP payloads into `provider.ChatRequest`
- convert provider responses back to protocol-specific JSON or SSE

The `cc` handler is the Claude Code CLI-compatible Anthropic Messages profile.
Its currently declared ingress difference is the local `count_tokens` estimate
shim. Keep future Claude Code CLI-specific ingress validation/defaults in this
profile rather than forking the shared Messages lifecycle.

These modules are not standalone `http.handlers.*` modules. They are loaded by `agent_route_dispatcher`.

### Admin API

- Module ID: `http.handlers.agent_gateway_admin`
- Package: `caddy/admin/`

Exposes the Admin API families listed under "Admin API Notes" below;
authentication is delegated to the HTTP mount layer such as Caddy
`basic_auth`, mTLS, a reverse proxy authenticator, or standalone
`--admin-basic-auth-hash`.

## Key Packages

Deep per-subsystem rules live in nested `AGENTS.md` files next to the code.
Read the applicable subsystem file before changing either its runtime package
or a cross-cutting adapter for it (for example `caddy/admin`,
`caddy/dispatcher`, `standalone/server`, `pkg/gatewaybundle`, or `cmd/agwctl`):

- `pkg/gateway/` — runtime route/VirtualKey/provider resolution (`AgentGateway`) and the public route-model packages (`llmroute`, `modelcatalog`, `mcproute`, `agentroute`, `virtualkey`) → `pkg/gateway/AGENTS.md`
- `pkg/acp/` — native ACP runtime (`codex`, `opencode`): pooling, sessions, permissions, transcripts → `pkg/acp/AGENTS.md`
- `pkg/credential/` — cross-cutting credential model, persistence, scheduling, expiry detection, and external refresh transport → `pkg/credential/AGENTS.md`
- `pkg/llm/` — provider interface/registry, built-in providers, and the `einomodel` eino bridge → `pkg/llm/AGENTS.md`
- `pkg/mcp/` — MCP service runtime and the `einotool` eino bridge → `pkg/mcp/AGENTS.md`
- `pkg/configstore/` — generic config store/backends (persisted backend: `sqlite`; stores `providers`, `credentials`, `routes`, `mcp_services`, `agents`, `virtual_keys`, `managed_models`) → `pkg/configstore/AGENTS.md`
- `pkg/agent/` — agent control plane (`runtimeapi` contracts, `Agent` model, route/service → agent index) → `pkg/agent/AGENTS.md`; builtin eino ADK host → `pkg/agent/builtin/AGENTS.md`
- `internal/observability/` — usage events, event pipeline, OTLP export, `einotap` → `internal/observability/AGENTS.md`
- `pkg/dispatcher/llmapi/` — LLM protocol handler runtimes; see "Protocol handler modules" above
- `caddy/gateway/` — Caddy app wiring and global `agent_gateway` Caddyfile parsing; see "Caddy app" above

Cross-cutting invariants:

- dependency direction: `pkg/agent` composes the LLM/MCP/ACP/metrics surfaces; the lower protocol packages must not depend on `pkg/agent`
- no config-store reads in per-request hot paths (provider resolution, route matching); manager snapshots are refreshed on mutation
- factory registration (providers and builtin custom agents) requires blank imports in the binaries that link them; see each subtree file for the exact rule
- gateway request pipelines never add an `agent` step, durable execution/event store,
  scheduler, human-task model, or Temporal dependency to lower runtime packages

## Runtime Request Flow

```text
HTTP request
  -> http.handlers.agent_route_dispatcher
  -> AgentGateway.ResolveRoute(...)
  -> pick route.protocol
  -> rewrite path using route.match.path_prefix
  -> AgentGateway.ResolveVirtualKey(...)
  -> protocol handler PrepareLLMApiRequest(...)
  -> if execution is local: serve without provider/candidate/credential resolution
  -> otherwise: filter candidates by generic capabilities and AST-derived atomic protocol features
  -> if route uses model targets: resolve the requested route model name to one concrete binding and rewrite request model
  -> else: use route.target_policy.provider_target.provider_id
  -> select a credential and invoke the external refresher when close to expiry
  -> resolve provider instance
  -> provider.Chat(...) or provider.StreamChat(...)
  -> protocol handler writes JSON or SSE response
```

Key detail: provider resolution happens after protocol parsing only for
`execution=provider`; the request `model` means route target name in model-target
mode and upstream model name in direct-provider mode.

## Caddyfile Shape

The main config lives in the global `agent_gateway` block. `Caddyfile.example`
is the working reference config.

Important current directives:

- `provider_types` is startup-only provider type availability; when omitted all registered provider types are enabled
- providers use `provider_type <name>`
- LLM routes use `protocol <openai|anthropic|cc>`, MCP routes use `protocol mcp`, and Agent ingress routes use `protocol agent`
- `agent_route_dispatcher` uses `llm_api <name>`, `mcp`, and `agent`
- auth uses `virtualkey`, not `local_api_key`

## Admin API Notes

Implemented families:

- LLM: `/admin/llm/providers/...`, `/admin/llm/provider_types` (read-only listing), `/admin/llm/api_handler_types`, `/admin/llm/routes/...`, `/admin/llm/models/providers/{provider_id}/discovered`, `/admin/llm/models/providers/{provider_id}/refresh`, `/admin/llm/models/managed/...`
- `/admin/virtual_keys/...`, `/admin/credentials/...`
- MCP: `/admin/mcp/services/...`, `/admin/mcp/routes/...`, `/admin/mcp/runtime/...` (discovery, execution, dispatcher runtime inspection)
- ACP: `/admin/acp/runtime/...` (native diagnostics keyed by `agent_id`)
- builtin: `/admin/builtin/runtime/...`
- agents: `/admin/agents/...` plus unified route CRUD under `/admin/agents/routes`
- metrics: summaries (with pipeline health counters), per-protocol LLM/MCP/ACP timeseries and breakdowns, recent interaction events, and the Prometheus exposition endpoint `GET /admin/metrics/prometheus`

Stubbed families currently return `501 Not Implemented`:

- `/admin/memory/...` (reserved; memory is not shipped in v0.4.x)

## Files To Check Before Large Changes

- `README.md`: user-facing setup and API examples
- `docs/architecture/architecture-overview.md`: broader architecture and roadmap
- `Caddyfile.example`: working reference config
- `cmd/agw/main.go`: the definitive list of linked modules

If you change module IDs, route semantics, provider registration, or Admin API paths, update this file, the affected nested `AGENTS.md`, and `README.md` in the same change.
