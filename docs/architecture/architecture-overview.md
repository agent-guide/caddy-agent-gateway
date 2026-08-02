# Architecture Overview

## 1. Scope

This document describes the current architecture of `agent-gateway` as it exists in the repository today, plus the intended extension points that are already visible in the codebase.

It is not a pure future-state blueprint anymore. Where the implementation is partial, this document calls that out explicitly.

## 2. Design Goals

The project is built around four practical goals:

- Reuse Caddy's module system and config model where they fit, while keeping the core runtime reusable by the standalone daemon
- Expose familiar LLM-compatible HTTP APIs to agent clients
- Centralize provider configuration, upstream credentials, and gateway-side API keys
- Leave room for richer agent runtime features such as MCP, ACP, memory, and orchestration without forcing them into every caller

The current Go module path is `github.com/agent-guide/agent-gateway`.

Related extension design notes live in `docs/` when a topic needs more detail than this architecture overview. The ConfigStore architecture and technical specification is documented in [configstore-architecture.md](configstore-architecture.md). The gateway bundle YAML proposal is documented in [../design/gateway-bundle-yaml.md](../design/gateway-bundle-yaml.md).

## 3. Top-Level Architecture

The request path runs through four layers; memory remains a reserved subsystem:

```text
Client
  |
  v
HTTP handlers
  - Caddy adapters: http.handlers.agent_route_dispatcher, http.handlers.agent_gateway_admin
  - Standalone server: net/http handlers assembled by standalone/server
Dispatcher / protocol modules
  - agent_route_dispatcher.llm_apis.openai
  - agent_route_dispatcher.llm_apis.anthropic
  - agent_route_dispatcher.llm_apis.cc
  - agent_route_dispatcher with MCP enabled
  - agent_route_dispatcher with unified Agent ingress enabled
  |
  v
Shared gateway runtime
  - provider loading and resolution
  - credential loading and refresh-strategy registration
  - config store loading
  - llmroute, mcproute, and agentroute registries
  - virtual key lookup
  - credential manager
  - MCP runtime registry
  - Agent-owned ACP runtime/process manager
  - agent manager (Agent CRUD + immutable definition snapshot)
  - builtin ADK host (in-process materialization of builtin-runtime agents)
  - usage event pipeline (typed llm/mcp/acp/builtin events, spans, optional OTLP export)
  |
  v
External systems
  - upstream LLM providers (OpenAI / Anthropic / Gemini / DeepSeek / Qwen / Zhipu / OpenRouter / Ollama / Codex / Claude Code)
  - upstream MCP services
  - local ACP agent or adapter processes (codex, opencode)
  - SQLite config database and usage event tables
  - optional OpenTelemetry collector (metrics.otlp span export)
  - future memory backends
```

Builtin-runtime agents live inside the shared runtime layer (the builtin ADK
host), not in the external systems row: their inner model and tool calls
resolve through the same route and service registries as any other traffic, so
no external agent process is involved. See
[Builtin Agent Runtime](../design/builtin-agent-runtime.md) for the
authoritative runtime design.

## 4. Main Components

### 4.1 `caddy/gateway/`, `standalone/server/`, And `pkg/gateway/`: Runtime Assembly And Backbone

The `caddy/gateway.App` type is the root Caddy app module with module ID `agent_gateway`. The standalone daemon in `standalone/server/` assembles the same core runtime services without depending on a Caddy app lifecycle.

Its responsibilities are:

- Provision the configured config store
- Load static provider configs and instantiate runtime providers through the provider registry
- Initialize the credential manager
- Configure the external request-time credential refresh command
- Restore persisted credentials from storage
- Build route loading and provider resolution dependencies
- Construct the shared `pkg/gateway.AgentGateway` runtime used by HTTP handlers

The app owns both:

- statically configured routes from the Caddyfile
- dynamically persisted route and provider records from the config store

Static route limitation:

- Caddyfile routes only support direct-provider targets
- logical-model routes are configured through the Admin API and config-store-backed workflows

This is the key design choice in the project: transport adapters are intentionally thin, runtime assembly is allowed to differ between `agw` and `agwd`, and `pkg/gateway` owns the reusable gateway services.

### 4.2 `caddy/dispatcher/` And `pkg/dispatcher/`: Compatible LLM Ingress

The `caddy/dispatcher/` package registers the `agent_route_dispatcher` Caddyfile directive. It adapts the reusable `pkg/dispatcher` runtime and accepts dispatcher-local LLM API protocol modules:

```caddy
agent_route_dispatcher {
    llm_api openai
    llm_api anthropic
    llm_api cc
    mcp
    acp
}
```

The HTTP handler is `http.handlers.agent_route_dispatcher`, and it loads LLM protocol handlers from:

- `agent_route_dispatcher.llm_apis.openai`
- `agent_route_dispatcher.llm_apis.anthropic`
- `agent_route_dispatcher.llm_apis.cc`

The `cc` handler is the Claude Code CLI-compatible ingress profile. It uses the Anthropic Messages wire format and keeps Claude Code-specific behavior out of the generic Anthropic handler and provider implementations.

MCP handling is enabled with the dispatcher-local `mcp` option instead of a separate HTTP handler module. ACP handling is enabled the same way with `acp`; it uses gateway-owned route endpoints for turns, permission decisions, route-scoped session listing, and transcript replay, then routes to `pkg/acp` instead of the LLM provider interface.

The runtime dispatcher in `pkg/dispatcher` does not define route policy inline. Instead, it asks the shared gateway route manager to match the HTTP request against `AgentRoute.match`, strips the matched route path prefix, selects the route's `protocol`, and resolves the matched route and target provider.

This separation is deliberate:

- API compatibility stays transport-focused
- route policy stays centralized
- provider selection can evolve independently from HTTP parsing

### 4.3 `caddy/admin/` And `pkg/admin/`: Operational Control Surface

The `caddy/admin/` package registers `agent_gateway_admin` with module ID `http.handlers.agent_gateway_admin`, and delegates request handling to the reusable `pkg/admin` runtime package.

Today it exposes working endpoints for:

- health
- provider CRUD
- LLM route CRUD
- MCP service CRUD
- MCP route CRUD
- unified Agent route CRUD
- virtual key CRUD
- credential list/get/delete
- async CLI login and login status
- MCP service discovery and execution endpoints
- MCP dispatcher runtime inspection endpoints
- ACP runtime inspection and operator escape-hatch endpoints
- metrics summary, event, timeseries, breakdown, and Prometheus exposition endpoints

The `/admin/agents` family is implemented (CRUD; unified AgentRoute CRUD; workspace/activity/usage/interactions/resources/health; runtime capabilities; exact-run list/cancel; one-shot permission list/decision; and capability-gated session/transcript reads). ACP and builtin both enter through `kind=agent`, `protocol=agent` routes. The same route table still defines memory endpoints that are not yet implemented.

This means the admin package is now the active control-plane entrypoint for LLM, MCP, ACP, agents, and metrics inspection, while the memory admin family remains future work. ACP consumer runtime APIs that should be scoped by route and VirtualKey, such as turns, permission decisions, session listing, and transcript replay, stay under the dispatcher route prefix rather than under `/admin/acp`.

### 4.4 `pkg/llm/provider/`: Provider Abstraction

Providers implement a shared interface:

```go
type Provider interface {
    Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
    StreamChat(ctx context.Context, req *ChatRequest) (*schema.StreamReader[*schema.Message], error)
    ListModels(ctx context.Context) ([]ModelInfo, error)
    Capabilities() ProviderCapabilities
    Config() ProviderConfig
}
```

Important characteristics:

- the interface is intentionally small
- chat and stream are first-class
- model listing is supported
- embeddings are optional through `EmbeddingProvider`
- providers expose capability metadata and runtime config

Built-in providers:

- `openai`
- `anthropic`
- `claudecode`
- `codex`
- `gemini`
- `ollama`
- `openrouter`
- `deepseek`
- `zhipu`
- `qwen`

The provider layer uses shared helpers for HTTP client construction, auth/header injection, and OpenAI-compatible behavior. The design keeps provider implementations narrow while still allowing provider-specific behavior.

### 4.5 Credential Lifecycle

`pkg/credential` owns registration, persistence, selection state, expiry
detection, and the generic external refresh-command transport. Agent Gateway
contains no provider-specific token refresh implementation.

Interactive OAuth login is deliberately outside the gateway binary. The
separate `agw-auth` tool performs browser, PKCE, callback, or device flows and
registers the resulting `oauth_token` through `/admin/credentials`. Before
an upstream request, the gateway invokes the configured external refresh argv
when a selected credential is close to expiry and persists the returned token.

Upstream provider credentials and gateway caller VirtualKeys remain separate concerns.

### 4.6 `pkg/configstore/` And `caddy/configstore/`: Persistent Control Data

The default config store backend is `agent_gateway.config_store_backends.sqlite`.

It persists:

- provider configs
- route definitions
- virtual keys
- upstream provider credentials
- managed model overlays
- MCP service definitions
- MCP route definitions
- Agent definitions (including inline ACP runtime config)
- unified Agent route definitions

SQLite is the only storage backend that is provisioned end-to-end today.

The runtime storage API is schema-bound. `ConfigStoreBackend.Register(name, schema)` validates a schema, prepares storage, creates a schema-bound generic `ConfigStore`, and caches it. `ConfigStoreBackend.Get(name)` returns the cached store. The gateway registers the canonical schemas for providers, credentials, routes, virtual keys, and managed models during startup. Generic store interfaces and schema primitives live under `pkg/configstore/`; built-in business schemas live under `pkg/configstore/schema/`.

The config store is important for one reason beyond persistence: it allows some route and provider updates to take effect dynamically without rewriting the entire Caddy config.

### 4.7 `pkg/mcp/`, `pkg/acp/`, `pkg/agent/`, `pkg/llm/memory/`

These packages are present because the gateway is intended to grow beyond plain API proxying.

Current status:

- `pkg/mcp/`
  - protocol types, transport clients, service runtime, and runtime registry are active
  - `pkg/mcp/service` manages `mcp_services`, discovery, execution, and session reuse
  - `pkg/mcp/runtime` tracks in-flight requests and progress for the MCP dispatcher
  - `streamable_http` is the active upstream transport path today
  - `stdio` and `sse` code exist but are not yet equally integrated
- `pkg/acp/`
  - Agent-owned runtime config, turn request/event types, agent SPI, stdio JSON-RPC transport, activity tracking, and Admin/dispatcher integration are active
  - first-version service config allows only `codex` and `opencode`
  - `opencode` uses the fixed `opencode acp --cwd <cwd>` stdio process shape
  - `codex` uses the fixed external ACP adapter binary `codex-acp` by default; it does not launch `codex acp`
  - the runtime driver handles `initialize`, `session/new`, `session/load`, `session/prompt`, full `session/update` parsing (`pkg/acp/runtime/acpupdate`: text, reasoning, tool calls, plan, usage, available commands, session info, mode, config options), route-scoped and Admin `session/list` and transcript replay (`session/load` over a transient connection) after ACP capability checking, model selection and `config_overrides` via `session/set_config_option`, and spec-correct fail-closed permission replies with an off-loop timeout
  - each pooled instance caches the latest session metadata (config options, slash commands, title, mode, usage) from a lifetime updates subscription; the cache is replayed as snapshot events at every turn start and exposed through the runtime Admin inspection
  - runtime hardening: `PATH` preflight, stderr capture, a setup-handshake timeout, an idle janitor, dead-instance eviction, `fresh_session`, scope rebind (a session-addressed turn adopts the thread's live instance instead of spawning a second process), and `CloseScope`/`CloseThread` teardown
  - permission modes `deny`/`auto_approve`/`interactive`: interactive requests follow the runtime capability's advertised continuation mode and common Agent permission controls
  - verified end to end against the real `opencode acp` and `codex-acp` binaries (deterministic full-lifecycle and interactive-permission integration tests plus gated real-agent handshake, session-lifecycle, and prompt-level real-model smokes); crash retry and the codex app-server bridge (v2) are deferred, and codex stable-session id resolution is a verified non-gap for v1 (the driver seams for v2 are wired)
- `pkg/agent/`
  - the external agent control plane: the `Agent` model, `agents` store, unified `AgentRoute.agent_id` ingress, and runtime-neutral capability APIs
  - `pkg/agent/builtin/` is the in-process eino ADK host for `runtime.type = "builtin"` agents and executes behind the same AgentRoute contract as ACP
  - composes the protocol subsystems and observes them; the protocol packages do not depend on it
  - the legacy `pkg/llm/agent` LLM-native orchestrator has been removed, per the external-control-plane direction; see [../design/agents-control-plane.md](../design/agents-control-plane.md)
- `pkg/llm/memory/`
  - interfaces exist
  - SQLite and Mem0-related code exists
  - not yet fully active in normal request execution

Architecturally, MCP and ACP are active native runtime subsystems, and `pkg/agent` is the active external control plane that composes them (it does not own an agent's internal reasoning loop). Memory is still an extension subsystem.

## 5. Configuration Model

### 5.1 Static App Configuration

Static configuration lives in the global `agent_gateway` Caddyfile block:

```caddy
{
    agent_gateway {
        provider_types {
            openai
        }

        provider openai-main {
            provider_type openai
            ...
        }
        config_store sqlite { ... }
        route chat { ... }
    }
}
```

The parser currently supports:

- `provider <provider-id>`
- `config_store <name>`
- `route <id>`

Static route parsing is intentionally small right now. Supported route subdirectives are:

- `require_virtual_key`
- `target provider <provider-id> [weight]`

The Go route model is richer than the current static config grammar. That mismatch is intentional: the runtime and Admin API support logical-model routes, while Caddyfile and standalone startup config only accept direct-provider routes to keep static bootstrap simpler.

### 5.2 Dynamic Persisted Configuration

The config store also holds:

- provider records keyed by ID and tag
- LLM route objects keyed by ID
- MCP route objects keyed by ID
- MCP service objects keyed by ID
- virtual key objects keyed by key string

When an API handler receives a request for a given `route_id`, the runtime can reload the latest stored route definition for that ID. Provider references can also resolve through persisted provider config.

This produces a hybrid model:

- Caddy owns the long-lived process and module graph
- the config store owns mutable operational records

That is one of the core architectural decisions in the project.

## 6. Request Routing Design

### 6.1 Route Object

The primary routing configuration is `pkg/gateway/llmroute.AgentRoute`.

Important fields include:

- `ID`
- `Match`
- `Protocol`
- `TargetPolicy`
- `Policy`
- timestamps and disabled state

The richer route model already supports ideas such as:

- logical-model and direct-provider routing
- route-level auth
- allowed model restrictions
- timeout, retry, fallback, quota, and rate-limit policy
- caller-specific policy overrides through `VirtualKey`

Only part of this model is enforced today, but the shape of the runtime data model is already defined.

Current runtime resolution treats `TargetPolicy.ProviderTarget.ProviderID` as the direct-provider switch. If that field is set, the route resolves in direct-provider mode; otherwise it resolves through `TargetPolicy.ModelTargets`.

### 6.2 Selection and Resolution

At startup, the runtime assembly layer builds:

- a route loader
- a provider resolver
- a virtual key store binding

Provider resolution currently combines:

- statically provisioned provider instances from the active runtime assembly
- dynamically decoded provider configs from the config store

This allows the request path to resolve a named target provider without hard-coding the source of truth to either the Caddyfile or the database alone.

Provider config `options.compact` is the current compatibility-mode selector. Supported values are `cc`, `codex`, and `none`; providers ignore compact modes they do not support.

## 7. Data Flows

### 7.1 LLM API Request

The standard request path is:

```text
HTTP request
  -> agent_route_dispatcher
  -> match AgentRoute by host/path prefix/method
  -> validate virtual key if required
  -> apply the VirtualKey's in-memory route-kind rate limit if configured
  -> strip matched path prefix
  -> select route protocol handler
  -> resolve target provider
  -> convert request into provider.Chat/StreamChat input
  -> call upstream provider
  -> translate provider response back to dialect response
```

The important design property here is that compatible ingress is separated from route policy and from provider implementation.

### 7.2 MCP Request

The MCP request path is now:

```text
HTTP request
  -> agent_route_dispatcher with mcp enabled
  -> match MCPRoute by host/path prefix/method
  -> validate virtual key if required
  -> apply the VirtualKey's in-memory MCP rate limit if configured
  -> decode JSON-RPC request
  -> register in-flight request in pkg/mcp/runtime
  -> resolve target MCP service
  -> initialize or reuse upstream Streamable HTTP session
  -> invoke discovery or execution method on upstream MCP service
  -> map notifications/cancelled and notifications/progress into runtime state
  -> translate upstream result into JSON-RPC response
```

### 7.3 Admin Mutation

For a route or provider change:

```text
HTTP admin request
  -> agent_gateway_admin
  -> config store CRUD
  -> later request path reloads latest stored record
```

This is why the project can support operational changes without treating the Caddyfile as the only mutable state.

### 7.4 External CLI Login

```text
agw-auth login
  -> resolve the target provider through the Gateway Admin API
  -> run the interactive provider OAuth flow locally
  -> POST the resulting oauth_token to /admin/credentials
  -> Gateway selects and refreshes that credential on model requests
```

The Gateway has no authenticator configuration or login-session endpoints.

## 8. Current Implementation Boundaries

The following are implemented enough to be production-shape code, even if still early:

- Caddy app provisioning
- standalone server assembly
- provider module loading
- request-time OAuth credential refresh
- SQLite config persistence
- provider CRUD
- route CRUD
- MCP service CRUD
- MCP route CRUD
- virtual key CRUD
- credential inspection and deletion
- CLI login orchestration
- OpenAI-compatible and Anthropic-compatible ingress handlers
- MCP dispatcher, upstream discovery, upstream execution, and runtime inspection
- SQLite-backed usage metrics summaries and recent interaction event inspection,
  including unified AgentRoute dimensions and typed ACP/builtin persistence;
  Agent ingress carries direct `agent_id`, `run_id`, and `runtime_type`
- bounded-label Prometheus counters grouped by `route_kind` and `runtime_type`
- OTLP span export of usage events to an OpenTelemetry collector (opt-in via the `metrics.otlp` config)

The following are partial or placeholder:

- memory admin APIs
- agent admin APIs
- first-class non-HTTP MCP transports such as stdio in the active request path
- full upstream progress relay back to MCP clients
- full memory retrieval and writeback in request path
- the `agents` control plane, ACP/builtin runtime adapters, unified AgentRoute,
  Agent-owned ACP configuration, common capability plane, and observability
  cutover are implemented through M6; physical legacy-source deletion and the
  HTTP execution backend remain follow-up work. See
  [Unified Agent Runtime and Routing](../plans/unified-agent-runtime.md).
- the Gateway Request Pipeline for synchronous, request-bound LLM/MCP/transform
  composition remains future work. Durable Project/Team/Agent workflows,
  scheduling, human approval, and multi-Agent DAGs belong to an upper-layer
  workbench and an external engine such as Temporal; its Workers call the
  gateway data plane. See
  [Gateway Request Pipeline And External Orchestration](../design/request-pipeline.md).
- the legacy `pkg/llm/agent` orchestrator has been removed
- richer static Caddyfile route syntax for all route fields

## 9. Extension Points

The codebase is designed to be extended in a few stable ways:

### 9.1 New Provider

Implement `provider.Provider` in `pkg/llm/provider/<name>`. If the provider should also be available in the gateway binaries, link the runtime package from `cmd/agw/main.go` and `cmd/agwd/main.go`.

This is the most mature extension path in the project today.

### 9.2 New OAuth Credential Source

Add both interactive login and provider-specific refresh support to the
external `agw-auth` project. The login result declares `refresh_name` for the
external tool's internal dispatch plus expiry and `refresh_expiry_delta`
metadata for request-time renewal.

### 9.3 New Config Store

Add a store creator factory through `pkg/configstore.RegisterConfigStoreFactory(...)`. If the backend should be available in Caddy config, add a Caddy adapter under `agent_gateway.config_store_backends.<name>`.

A backend-specific creator should implement `pkg/configstore.ConfigStoreCreator`. The shared `pkg/configstore.Backend` implements `pkg/configstore.ConfigStoreBackend`: `Register(name, schema)` validates and caches a schema-bound store from the creator, and `Get(name)` returns the cached store.

This path exists architecturally, but SQLite is the only end-to-end store currently exercised by the main runtime.

### 9.4 Future MCP / Memory / Agent Runtime Extensions

The MCP and memory packages are structured as internal subsystem boundaries. The intended direction is:

- MCP expands from the current Streamable HTTP gateway path into broader transport coverage and richer runtime semantics
- memory becomes retrieval and persistence around model calls

The agent direction is different and is intentionally **not** an internal execution mode inside `pkg/llm`. A first-class `agents` layer (`pkg/agent`) becomes an **external control plane** that composes the LLM, MCP, ACP, and metrics subsystems: it manages agent identities and their runtime-specific configuration, governs the resources they may use, and observes their sessions, usage, and call chains. It does not own an agent's internal reasoning loop. The legacy `pkg/llm/agent` orchestrator is removed rather than expanded. This supersedes the earlier "agent orchestration becomes an execution mode" direction. See [Agent Control Plane](../design/agents-control-plane.md).

The execution boundary for ACP and builtin turns is one
turn-first `runtimeapi.Backend` layer registered by `AgentGateway`. The
gateway-owned adapters execute through one run sequencer behind a unified
`AgentRoute.agent_id` relationship. There is no unbound ACP ingress or
runtime-specific public route family. HTTP remains non-executable. Upper-layer
Workflow Workers call the same AgentRoute/turn boundary while their external
engine owns durable business state, retry, scheduling, approval, and DAG
semantics. Gateway Request Pipelines deliberately exclude an `agent` step. See
[Unified Agent Runtime and Routing](../plans/unified-agent-runtime.md) and
[Gateway Request Pipeline And External Orchestration](../design/request-pipeline.md).

ACP service is no longer a first-class product/config object.
An ACP Agent owns its execution config under `Agent.runtime.acp`, and
`agent_id` directly owns the ACP process pool, sessions, permissions, runtime
diagnostics, and attribution. Native `/admin/acp/runtime` diagnostics remain
Agent-keyed.

Those boundaries are already visible in code, but they should still be treated as evolving.

## 10. Design Tradeoffs

### 10.1 Why Support Both a Caddy App and a Standalone Gateway Server

Using a Caddy app gives the project:

- a mature module graph
- shared provisioning lifecycle
- established HTTP pipeline integration
- existing config loading and deployment patterns

The standalone daemon avoids coupling everything to Caddy's lifecycle and makes it easier to run the gateway as a conventional service. The downside is that the project must maintain two assembly paths over the same runtime core.

### 10.2 Why Hybrid Static + Dynamic Config

Only static config would make operational updates clumsy. Only dynamic config would weaken the value of reproducible startup composition, especially in the Caddy-based runtime.

The hybrid model keeps:

- static infra wiring in the Caddyfile or standalone bundle
- mutable provider and route records in SQLite

This is slightly more complex, but it matches how the gateway is meant to be operated.

### 10.3 Why Keep the Route Model Ahead of the Caddyfile Grammar

The repository already needs a richer route object for admin APIs and internal policy evaluation. Shipping the richer data model first allows the runtime and storage layers to settle before the public Caddyfile grammar is expanded.

That means some fields are representable in JSON and Go types before they are representable in the Caddyfile.

## 11. Near-Term Evolution

The most coherent next steps for the architecture are:

- extend MCP runtime beyond the current Streamable HTTP and request-scoped cancellation model
- include MCP objects in bundle/export/apply flows
- finish the missing admin handlers for memory
- complete the legacy source-deletion follow-up after the unified
  AgentRoute/Agent-owned ACP configuration and observability cutover
- implement the
  [Gateway Request Pipeline](../design/request-pipeline.md) for request-bound
  LLM/MCP/transform composition, and harden AgentRoute as the Activity boundary
  used by upper-layer Temporal Workers; do not add gateway-owned durable Agent
  Tasks, schedules, or multi-Agent DAG state
- expand enforcement of route policy beyond the currently active subset
- integrate memory into the request path
- expand Caddyfile route syntax to cover more of the existing route data model
- decide how the separate web UI becomes a first-class operator surface

Until then, the project should be understood primarily as a route-based LLM gateway with both Caddy-based and standalone deployment modes, and with a broader agent-runtime architecture still under active construction.
