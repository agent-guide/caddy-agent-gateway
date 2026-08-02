# Route Target Policy Architecture And Technical Specification

## 1. Scope

This document defines the current route target policy architecture in `agent-gateway`.

It is a runtime design and technical specification document, not a proposal or migration plan. It describes the implemented route target policy model, validation rules, runtime resolution behavior, credential scheduling semantics, and fallback behavior used by the gateway today.

The current Go module path is:

- `github.com/agent-guide/agent-gateway`

## 2. Architectural Position

Route target policy is the control layer between route matching and provider execution.

The relevant package ownership is:

- `pkg/gateway/llmroute`
  - owns route data model, target policy normalization, validation, and target resolution
- `pkg/gateway`
  - owns `RoutedProvider`, provider resolution, credential scheduling, and fallback execution
- `pkg/gateway/modelcatalog`
  - owns managed model overlays and discovered provider model facts
- `pkg/credential`
  - owns credential persistence, expiry detection, external refresh transport, and scheduling primitives
- `pkg/dispatcher`
  - owns protocol parsing and protocol-specific response generation

The route target policy decides which concrete upstream binding may be used for a request. `RoutedProvider` executes that decision and handles bounded fallback when configured.

## 3. Design Overview

The current implementation uses a concrete route target policy struct:

- `route.AgentRoute`
- `route.RouteTargetPolicy`

It does not use a Go interface-backed policy object at runtime.

Current route target policy supports two effective modes:

- `direct-provider`
- `logical-model`

The effective mode is determined by `RouteTargetPolicy.PolicyKind()` after normalization.

## 4. Route Data Model

The route runtime type is `pkg/gateway/llmroute.AgentRoute`.

Target selection is represented by `pkg/gateway/llmroute.RouteTargetPolicy`.

The fields that define target behavior are:

- `type`
- `provider_id`
- `models`
- `default_model`
- `model_selector_strategy`
- `credential_selector`
- `credential_scope_order`
- `credential_type_order`
- `fallback`
- `model_targets`
- `provider_target`

One compatibility layer exists in the current implementation:

- `provider_id` and `provider_target.provider_id` normalize to the same effective provider target

New code and new documentation must use the normalized shape:

- `target_policy.provider_target.provider_id`
- `target_policy.model_targets`
- `target_policy.default_model`

## 5. Policy Kinds

Current route target policy kinds are:

- `direct-provider`
- `logical-model`

### 5.1 Direct-Provider Mode

Direct-provider mode is active when the normalized target policy contains a provider target.

Effective route behavior:

- one provider is selected explicitly
- the request `model` is treated as the upstream model name
- no route-local logical model resolution is performed
- credential scheduling still runs through the route target policy

This mode is the simple pass-through path for static forwarding.

### 5.2 Logical-Model Mode

Logical-model mode is active when the normalized target policy contains model targets and no effective direct provider target.

Effective route behavior:

- the request `model` is interpreted as a route-visible logical model name
- the route-visible logical model name maps to one candidate set
- one concrete `(provider_id, upstream_model)` candidate is selected from that set
- the upstream request model is rewritten before provider execution

If the request omits `model`, `target_policy.default_model` is used when configured.

## 6. Normalization Rules

`RouteTargetPolicy.Normalize()` applies the following runtime normalization:

- trims and synchronizes `provider_id` and `provider_target.provider_id`
- infers `type` when omitted

This means the route target policy keeps one normalized logical-model shape at runtime: `model_targets`.

## 7. Logical Model Target Structure

Logical-model routing uses `RouteModelTarget`.

`RouteModelTarget` contains:

- `name`
- `strategy`
- `default_candidate`
- `candidates`

Each `RouteModelCandidate` contains:

- `provider_id`
- `upstream_model`
- `weight`
- `priority`
- `default`

The route-visible model name is `RouteModelTarget.Name`. Each target name defines one candidate pool.

## 8. Credential Policy Structure

Credential selection is part of route target policy, not protocol handler behavior.

Current credential selector values:

- `round_robin`
- `fill_first`

Current credential scope values:

- `model_custom`
- `provider_id`

Current credential type values:

- `api_key`
- `oauth_token`

These values are stored on `RouteTargetPolicy` as:

- `credential_selector`
- `credential_scope_order`
- `credential_type_order`

## 9. Validation Rules

The validation entrypoint is `route.AgentRoute.ValidateDefinition()`.

### 9.1 Common Rules

Every route must satisfy these common rules:

- `id` must be set
- `llm_api` must be set
- `credential_selector`, when set, must be valid
- `credential_scope_order` must contain only supported scope values
- `credential_scope_order` must not contain duplicates
- `credential_type_order` must not be empty
- `credential_type_order` must contain only supported credential type values
- `credential_type_order` must not contain duplicates

### 9.2 Direct-Provider Rules

In direct-provider mode:

- `target_policy.provider_id` must be set after normalization
- `credential_scope_order` must not contain `model_custom`

The route validation path accepts direct-provider mode as the effective mode whenever a provider target is configured, even if model-target fields are also present in legacy or mixed input.

That precedence is part of the current compatibility behavior.

### 9.3 Logical-Model Rules

In logical-model mode:

- `target_policy.model_targets` must be non-empty after normalization
- every target must have a unique `name`
- every target must define at least one candidate
- every candidate must define both `provider_id` and `upstream_model`
- duplicate `(provider_id, upstream_model)` candidates within one target are rejected
- `default_model`, when set, must reference an existing target name
- `model_selector_strategy` must be one of:
  - `auto`
  - `weighted`
  - `priority`
- `fallback.max_num` must be between `0` and `5`

## 10. Runtime Resolution Contract

The target resolution entrypoint is `route.AgentRoute.ResolveTarget(...)`.

Request-side requirements are represented by `route.RequestRequirements`.

Current request requirement fields:

- `model`
- `require_streaming`
- `require_tools`
- `require_vision`
- `require_embeddings`
- `excluded_candidates`

Current `ResolvedTarget` fields:

- `logical_model`
- `model`
- `provider_id`
- `provider_type`
- `upstream_model`
- `credential_scope`
- `capabilities`

`ResolvedTarget` is concrete and execution-oriented. It represents one selected binding, not the full route policy.

## 11. Direct-Provider Resolution

In direct-provider mode, `ResolveTarget(...)`:

- resolves the configured provider ID
- derives `credential_scope` as `ProviderIDCredentialScope(provider_id)`
- treats the request `model` as the upstream model
- does not perform logical-model admission checks
- returns one concrete `ResolvedTarget`

This mode is provider-explicit and does not depend on managed model enablement.

## 12. Logical-Model Resolution

In logical-model mode, `ResolveTarget(...)`:

- interprets the request `model` as the route model name
- falls back to `target_policy.default_model` when the request omits `model`
- rejects the request if no route model is available
- rejects the request if the requested route model is not allowed on the matched route
- loads all configured candidates for that route model
- excludes request-scoped rejected candidates from consideration
- reads managed model state from the model catalog
- drops candidates whose managed model record is missing or disabled
- drops candidates whose provider config is unavailable or disabled
- drops candidates that do not satisfy request capability requirements
- selects one candidate according to `model_selector_strategy`

If no eligible candidate remains, the resolver returns a gateway error indicating that the route model has no eligible bindings.

## 13. Candidate Selection Semantics

The current candidate selector is implemented in `pkg/gateway/llmroute/resolve.go`.

Supported selector strategies:

- `priority`
- `weighted`
- `auto`

### 13.1 `priority`

Selection behavior:

- higher `priority` wins
- `weight` is ignored
- stable config order breaks ties

### 13.2 `weighted`

Selection behavior:

- all candidates participate in one weighted draw
- candidates with `weight <= 0` contribute zero weight
- if total weight is zero or negative, the first candidate in config order is selected

### 13.3 `auto`

Selection behavior:

- candidates are first filtered to the highest `priority` tier
- if that tier has any positive weights, weighted selection runs within the tier
- if the best tier has no positive weight, the first candidate in tier order is selected

## 14. Capability Filtering

Logical-model candidate filtering uses per-model capability requirements from `RequestRequirements`.

Current capability gates are:

- streaming
- tools
- vision
- embeddings

Capability evaluation is performed against the candidate's effective `provider.ModelCapabilities`, which come from the model catalog's resolved managed model view.

## 15. Credential Scope Expansion

`RoutedProvider` expands credential scopes from `ResolvedTarget` and route policy.

Current scope expansion rules:

- `model_custom`
  - uses `ResolvedTarget.CredentialScope` when present
- `provider_id`
  - uses `credential.ProviderIDCredentialScope(target.ProviderID)`

Scope expansion preserves the route-configured order. Empty scopes are skipped.

## 16. Credential Type Ordering

Within each expanded scope, `RoutedProvider` checks credential types in the configured `credential_type_order`.

Current credential type handling:

- `api_key`
- `oauth_token`

If an OAuth token credential is selected, the credential manager may invoke
the configured external refresh command before execution proceeds.

## 17. RoutedProvider Responsibilities

The runtime execution object is `pkg/gateway.RoutedProvider`.

Current responsibilities:

- resolve one concrete route target
- resolve the concrete provider instance
- select credentials through the scheduler
- inject selected credentials into request context
- rewrite the request model to the selected upstream model
- execute chat and responses API calls
- mark credential scheduling results
- apply bounded model fallback

`RoutedProvider` implements:

- `Chat(...)`
- `StreamChat(...)`
- `CreateResponses(...)`
- `StreamResponses(...)`
- `ListModels(...)`

## 18. Fallback Semantics

Fallback is request-scoped and bounded.

Current execution state tracks:

- tried candidates
- tried credentials
- model fallback count

Current fallback behavior:

- fallback is active only for logical-model routes with `target_policy.fallback.enabled`
- `fallback.max_num` defines the maximum number of model reselections
- missing credentials for one candidate reject that candidate for the current request
- credential rejection consumes candidate availability but does not change route policy
- provider execution failures may trigger model reselection when classified as retryable at the model level

The current implementation does not expose a separate credential-reselection action enum. It supports:

- stop
- reselect model

## 19. Failure Classification

`RoutedProvider.classifyFailure(...)` currently maps failures into two runtime actions:

- stop
- reselect model

Current retryable model-fallback statuses are:

- `429 Too Many Requests`
- `502 Bad Gateway`
- `503 Service Unavailable`
- `504 Gateway Timeout`
- any `5xx` status not otherwise handled

All other failures stop request execution immediately.

This is the current normative behavior for route-level target fallback.

## 20. Scheduler Result Marking

After each execution attempt, `RoutedProvider.markResult(...)` records a scheduler result when a concrete managed credential was used.

Recorded scheduler result fields include:

- credential ID
- upstream model
- success or failure
- HTTP-derived error metadata when execution failed

This keeps credential scheduling feedback in the gateway runtime, not in protocol adapters.

## 21. Caddyfile Configuration Shape

In the current Caddy-based runtime, route target policy is expressed through route-level target directives, not through a nested `target_policy { ... }` block.

Current supported directives:

- `target provider <provider-id>`
- `target model <route-model> <provider-id> <upstream-model> [weight <n>] [priority <n>] [strategy <auto|weighted|priority>] [default]`

Direct-provider example:

```caddy
route openai-direct {
    llm_api openai
    path_prefix /
    require_virtual_key
    target provider openai-main
}
```

Logical-model example:

```caddy
route openai-chat {
    llm_api openai
    path_prefix /
    require_virtual_key
    target model chat-default openai-main gpt-4.1 weight 100 priority 100 default
    target model chat-default openai-backup gpt-4.1 weight 20 priority 100
    target model chat-default openrouter-main openai/gpt-4.1 weight 10 priority 80
}
```

The Caddyfile adapter compiles these directives into the normalized `RouteTargetPolicy` shape.

## 22. JSON Storage Shape

Persisted route JSON stores target policy as a concrete object, not as an interface envelope.

Current stored route payloads use fields such as:

- `target_policy.type`
- `target_policy.provider_target.provider_id`
- `target_policy.model_targets`
- `target_policy.default_model`
- `target_policy.model_selector_strategy`
- `target_policy.credential_selector`
- `target_policy.credential_scope_order`
- `target_policy.credential_type_order`
- `target_policy.fallback`

Compatibility fields may still appear in stored route payloads, but runtime normalization defines the effective policy.

## 23. Naming Rules

Use the following terms consistently:

- `agent-gateway`
  - project and module name
- `RouteTargetPolicy`
  - concrete target policy struct
- `ResolvedTarget`
  - one concrete selected execution target
- `RoutedProvider`
  - route-aware provider execution runtime
- `direct-provider`
  - explicit provider forwarding mode
- `logical-model`
  - route model selection mode

Do not reintroduce:

- legacy repository naming
- interface-based target policy terminology unless the code actually adopts that model
- configuration examples that rely on a non-existent `target_policy { ... }` Caddyfile block

## 24. Summary

The current route target policy architecture in `agent-gateway` is a concrete, normalized route policy model with two effective target modes.

Its stable design rules are:

- route target policy is owned by `pkg/gateway/llmroute`
- execution and fallback are owned by `pkg/gateway.RoutedProvider`
- direct-provider mode forwards explicitly to one provider
- logical-model mode resolves one route-visible model name to one concrete provider/model binding
- credential scope ordering and source ordering are part of route target policy
- fallback is bounded and request-scoped

This is the current `agent-gateway` architecture and the normative technical specification for route target policy behavior in the repository.
