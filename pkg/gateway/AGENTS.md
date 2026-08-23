# pkg/gateway — AGENTS.md

Scope: the runtime gateway core and the route-model packages under
`pkg/gateway/`. Paths are repository-root relative. The global rules in the
root `AGENTS.md` (change policy, terminology, build/test) apply here.

## Runtime core

Important files:

- `agentgateway.go`: runtime route, VirtualKey, and provider resolution
- `providerresolver.go`: static and dynamic provider resolution

Dynamic provider configs are cached after first load and invalidated through
the manager's create/update/delete paths. Do not put config-store reads back in
the per-request provider resolution hot path.

`AgentGateway` is the main runtime object. It resolves routes, validates VirtualKeys, and selects providers. It does not own the HTTP protocol details.

`RoutedProvider` selects managed credentials and invokes the credential
manager's request-time external refresh hook before attaching a
`oauth_token` to the provider context. Provider-specific login and token
refresh behavior are external to this repository and owned by `agw-auth`.
Its enriched `ExecuteChat` / `ExecuteStreamChat` methods return the one
`ResolvedExecution` that served the request, including the candidate's
provider, client/upstream model, protocol dialect/features, and credential
attribution. The ordinary provider methods are compatibility adapters. Never
mutate the caller's `ChatRequest.Model`; rewrite only an attempt-local clone.
Protocol response coordinators use the enriched path so mode selection reads
the served candidate and one lifecycle owner writes attribution plus usage.
The protocol handler attaches its immutable `ProtocolRequirementSet` to both
the route request and `ChatRequest.ProtocolState`; `RoutedProvider` asserts they
agree but never inspects message extras to derive stronger requirements.
Candidate filtering returns typed `RequirementGap` values and is generic set
inclusion over registered feature IDs.

Runtime route matching uses the in-memory manager snapshot. Bootstrap and
route manager create/update/delete/refresh keep that snapshot populated; do not
reintroduce per-request config-store `List` calls for matching.

## Route id convention (`llmroute`, `mcproute`, `agentroute`)

Route ids must be slash-free so they are addressable as a single Admin API path
segment (`/admin/<kind>/routes/{id}`). `Normalize` auto-generates the
deterministic `<kind>:<target>:<path-slug>` when `id` is empty (slug = path
prefix lowercased, non-alphanumeric runs collapsed to `-`, `/` → `root`), and
`routecore.ValidateRouteID` rejects slash-bearing ids at create/validate time.
The id is fully predictable, so other objects (e.g. `allowed_route_ids`) can
reference it before apply; two routes whose paths slugify to the same value
collide and surface as a duplicate-id error, at which point set an explicit id.

## `llmroute/`

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

Static config restriction:

- Caddyfile routes and standalone `--static-config` bundle `llmRoutes` only support direct-provider mode
- logical-model routes remain supported through the Admin API and config-store-backed bundle workflows

The route model uses `protocol` and `require_virtual_key`. Do not reintroduce the old `local API key` naming in new code or docs.

## `modelcatalog/`

This package owns provider model discovery, managed model overlays, and runtime validation of concrete route candidates.

Important types:

- `ManagedModel`
- `ProviderModelSnapshot`

## `mcproute/`

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
- route ids follow the shared convention above with the `mcp:<service_id>:<path-slug>` shape

## `agentroute/`

Defines the unified `kind=agent` ingress route model
(docs/plans/unified-agent-runtime.md §6): an AgentRoute targets a stable
`agent_id`, and the resolved Agent's `runtime.type` selects the execution
backend, so a runtime change never changes the route id, URL, or VirtualKey
allowlist. Route ids follow the shared convention with the
`agent:<agent_id>:<path-slug>` shape. `AgentRouteResolver.CreateConfig`/
`UpdateConfig` validate target existence through the optional `AgentLookup`
(wired to `agent.Manager.HasAgent`); disabled or currently non-executable
Agents remain valid targets and fail at dispatch with their normalized runtime
error. Its public surfaces are `/admin/agents/routes`, bundle `agentRoutes`,
CLI `agent-route`, and dispatcher `EnableAgent`/`agent`.

## ACP runtime-config snapshot (`runtime_backends.go`)

`ACPBackend` builds the canonical `agent_id -> acpruntime.RuntimeConfig`
snapshot solely from `Agent.runtime.acp` during the three-stage Agent
definition commit. A changed fingerprint, disabled state, runtime switch, or
deletion retires stale pools and drains pending permissions before the
replacement generation becomes dispatchable. There is no ACP service store or
fallback configuration source.

## `virtualkey/`

This package owns VirtualKey extraction, validation, and storage-facing helpers.

Use this terminology consistently:

- `VirtualKey`
- `VirtualKeyManager`
- `VirtualKeyStore`

Current shape:

- `VirtualKey.ID` is required and is the management/storage primary key
- `VirtualKey.Key` is the bearer key value used at request time and is generated by the gateway
- Caddyfile and standalone static bundle config do not support static virtual keys; create them through the Admin API
- optional `rate_limits` policies enforce in-memory request token buckets for
  LLM, MCP, and unified agent ingress. Admission runs centrally after VirtualKey
  validation and interaction-span setup, before protocol dispatch. Limiter
  state is process-local, keyed by VirtualKey ID plus route kind, and is removed
  on key update/delete/reset. Internal builtin LLM/MCP calls do not consume
  ingress buckets unless they re-enter the dispatcher.

The gateway accepts a VirtualKey from either:

- `Authorization: Bearer <key>`
- `x-api-key: <key>`
