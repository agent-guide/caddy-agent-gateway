# Logical-Model Routing

This guide covers when to use logical-model routing, how it differs from direct-provider routing, and the minimum objects required to make it work.

## What Logical-Model Routing Is

In logical-model mode, the client sends a route model name in the request `model`, and the gateway resolves that to one concrete provider and upstream model binding.

This gives you:

- stable client-facing model names
- provider indirection
- candidate selection at the gateway layer
- fallback and future rerouting flexibility

## Direct-Provider Versus Logical-Model

Direct-provider mode:

- one route points at one provider
- request `model` is treated as the upstream model name
- supported in Caddyfile routes and static bundles

Logical-model mode:

- one route exposes one or more route model names
- request `model` is treated as the route model name
- the gateway resolves it to one concrete provider and upstream model
- supported through dynamic routes and config-store bundle workflows

## Minimum Required Objects

To use logical-model routing you typically need:

- providers
- managed models
- one route with `target_policy.model_targets`
- a VirtualKey if the route requires caller auth

## Minimal Bundle Example

```yaml
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle

providers:
  - id: openai-main
    provider_type: openai
    api_key: ${OPENAI_API_KEY}
  - id: anthropic-main
    provider_type: anthropic
    api_key: ${ANTHROPIC_API_KEY}

managedModels:
  - provider_id: openai-main
    upstream_model: gpt-4.1
    enabled: true
  - provider_id: anthropic-main
    upstream_model: claude-sonnet-4-5
    enabled: true

llmRoutes:
  - id: chat-prod
    protocol: openai
    match_policy:
      path_prefix: /
      methods:
        - POST
    auth_policy:
      require_virtual_key: true
    target_policy:
      default_model: chat-default
      model_targets:
        - name: chat-default
          candidates:
            - provider_id: openai-main
              upstream_model: gpt-4.1
              weight: 100
        - name: reasoning-default
          candidates:
            - provider_id: anthropic-main
              upstream_model: claude-sonnet-4-5
              weight: 100
```

## Important Route Fields

Common logical-model route fields:

- `default_model`
- `model_targets`
- `model_selector_strategy`
- `fallback`
- `credential_selector`
- `credential_scope_order`
- `credential_type_order`

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

## Request Behavior

Current runtime behavior:

- if the request omits `model`, `target_policy.default_model` is used when present
- if the request `model` does not match one of the route's `model_targets`, the route rejects it
- once one candidate is selected, the gateway rewrites the upstream request model before invoking the provider

## Current Restrictions

- logical-model routes are not accepted in Caddyfile routes
- logical-model routes are rejected in `agwd --static-config`
- use the Admin API or `agwctl apply` for logical-model route creation

## Credential Selection Notes

Logical-model routes can use model-specific credential scope first, then fall back to provider scope.

With the default provider-scope-only setting, runtime expansion becomes:

- `model_custom`
- `provider_id`

## Related Docs

- [routes.md](routes.md)
- [bundle-yaml.md](bundle-yaml.md)
- [../design/model-first-routing.md](../design/model-first-routing.md)
- [../design/route-target-policy.md](../design/route-target-policy.md)
