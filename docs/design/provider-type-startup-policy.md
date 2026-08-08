# Provider Type Startup Policy

## Purpose

Provider types are process capabilities. They are registered by compiled Go packages and enabled or disabled only during gateway startup.

Gateway bundles manage provider instances and gateway business objects. They do not manage provider type availability.

## Concepts

Provider type registry:

- the provider types linked into the running binary
- examples: `openai`, `anthropic`, `zhipu`, `ollama`
- created by provider package `init` registration

Enabled provider types:

- the subset of registered provider types accepted by this gateway process
- configured at startup through the Caddyfile or standalone daemon flags
- visible through the Admin API
- not mutable through GatewayBundle apply

Provider instances:

- concrete provider configs such as `openai-main` or `zhipu-test`
- reference an enabled `provider_type`
- may be static startup config or dynamic Admin API config

## Caddy Runtime

The Caddy runtime configures provider type availability in the global `agent_gateway` block:

```caddy
{
    agent_gateway {
        provider_types {
            openai
            anthropic
            zhipu
        }

        provider openai-main {
            provider_type openai
            api_key {$OPENAI_API_KEY}
            default_model gpt-4.1
        }
    }
}
```

If `provider_types` is omitted, every registered provider type remains enabled. If it is present, only the listed provider types are enabled; every registered type that is not listed is disabled.

## Standalone Runtime

The standalone daemon accepts provider type availability at startup:

```bash
agwd \
  --provider-type openai \
  --provider-type anthropic \
  --static-config ./gateway.yaml
```

If no `--provider-type` flag is provided, every registered provider type remains enabled. If any `--provider-type` flag is provided, only those provider types are enabled.

## GatewayBundle Boundary

GatewayBundle YAML does not contain `providerTypes`.

Valid:

```yaml
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle

providers:
  - id: openai-main
    provider_type: openai
    api_key: ${OPENAI_API_KEY}
    default_model: gpt-4.1
```

Invalid:

```yaml
providerTypes:
  - provider_type: openai
    enabled: true
```

`agwctl apply` validates that provider instances reference currently enabled provider types. It never enables or disables provider types.

## Admin API

`GET /admin/llm/provider_types` remains available as a read-only inspection endpoint.

Provider type mutation endpoints are not part of the current behavior:

- `POST /admin/llm/provider_types/{provider_type}/enable`
- `POST /admin/llm/provider_types/{provider_type}/disable`

Provider creation and update reject unknown or disabled provider types.
