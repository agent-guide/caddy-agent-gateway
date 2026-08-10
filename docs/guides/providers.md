# Providers

This guide covers how provider objects are used in `agent-gateway`, how to define them in `agw`, and how they relate to routes and credentials.

## What A Provider Does

A provider defines one upstream LLM backend configuration. Routes select providers either:

- directly, with `target_policy.provider_target.provider_id`
- indirectly, through logical-model routing that resolves to one concrete provider and upstream model binding

Providers do not define request matching. They only define upstream execution settings such as provider type, base URL, network settings, default model, and provider-specific options.

## Built-In Provider Types

Current built-in provider types linked into `agw` include:

- `openai`
- `anthropic`
- `codex`
- `claudecode`
- `gemini`
- `ollama`
- `openrouter`
- `deepseek`
- `zhipu`

## Minimal Caddyfile Example

```caddy
provider openai-main {
	provider_type openai
	api_key {$OPENAI_API_KEY}
	default_model gpt-4.1
}
```

The provider block name is the `provider_id` used by routes and Admin API objects.

## Common Configuration Pattern

```caddy
provider openrouter-main {
	provider_type openrouter
	api_key {$OPENROUTER_API_KEY}
	base_url https://openrouter.ai/api/v1
	default_model openai/gpt-4o-mini
	request_timeout_seconds 120
	max_retries 3
	retry_delay_seconds 1
	max_idle_connections 100
	max_idle_connections_per_host 20
	idle_keep_alive_timeout_seconds 90
	proxy_url http://127.0.0.1:7890
	header X-Custom value
	option organization org_...
}
```

## Provider Lifecycle

Providers can come from two sources:

- static Caddyfile config in `agw`
- persisted config-store records managed through the Admin API

Static providers are loaded at startup. Persisted providers can be created or updated through the Admin API without rebuilding binaries.

## Provider And Credential Relationship

Provider `api_key` is a provider-local fallback setting. It is not the same thing as a managed credential in the credential manager.

Use managed credentials when you need:

- multiple upstream credentials for one provider
- scheduling or rotation behavior
- OAuth-backed upstream tokens provisioned by `agw-auth`

## Current Defaults And Notes

- `openai` defaults to `https://api.openai.com/v1`
- `anthropic` defaults to `https://api.anthropic.com`
- `codex` defaults to `https://chatgpt.com/backend-api/codex` and sends OpenAI-compatible `POST /responses` requests; custom `base_url` values depend on the upstream codex-compatible deployment. Use `option compact cc` when routing Claude Code CLI traffic through a Codex-compatible upstream that cannot reliably sequence Claude Code stateful tools.
- `claudecode` accepts either an Anthropic-style `api_key` or a managed `oauth_token`. A managed `oauth_token` and any `sk-ant-oat-` OAuth token always use `Authorization: Bearer`. A plain API key follows `option api_key_header`, which defaults to `authorization` (`Authorization: Bearer`); set it to `x-api-key` to send the key in the `x-api-key` header instead. Use `option compact codex` when routing Codex CLI traffic through an upstream that gates on Claude Code tool names: it rewrites Codex tool names (e.g. `exec_command`) to their Claude Code equivalents (e.g. `Bash`) on the outbound request and restores the original names on the response. The `claudecode` provider also sends the full Claude Code CLI client fingerprint (`User-Agent`, `X-App`, `X-Stainless-*`, `anthropic-version`, `anthropic-beta`) by default, so a config that omits these still presents as the standard CLI; set `network.extra_headers` only to override individual values when matching a newer CLI release.
- `openrouter` defaults to `https://openrouter.ai/api/v1`
- `deepseek` defaults to `https://api.deepseek.com`
- `zhipu` defaults to `https://open.bigmodel.cn/api/paas/v4`
- `ollama` can be used without an API key

The OpenAI-compatible chat providers (`openai`, `deepseek`, `openrouter`, `zhipu`) accept `option compact cc`, which drops the OpenAI-style `metadata` and `user` request fields that Claude Code CLI always sends but some upstreams (e.g. GLM) reject. Providers ignore compact modes they do not support.

`deepseek` and `zhipu` also accept `option thinking_type <disabled|enabled|none>`. DeepSeek defaults to `disabled` because its thinking-mode tool loops require reasoning replay that the cc/anthropic path does not provide for that provider. Zhipu omits `thinking` by default so current GLM models use their upstream default; request-level `thinking` takes precedence, and `reasoning_content` is preserved across OpenAI Chat, Responses compatibility, and cc/Anthropic tool loops. Zhipu streaming tool requests automatically send `tool_stream=true`. Use an explicit provider option only to force one mode for every request. Zhipu's `option api_profile <auto|standard|coding_plan>` controls endpoint-specific defaults; set `coding_plan` behind custom proxies. Provider-level `context_window`, `max_output_tokens`, `vision`, and `embeddings` options can override those defaults, while per-model differences belong in managed-model `capability_overrides`.

## Related Docs

- [../reference/provider-option-reference.md](../reference/provider-option-reference.md)
- [routes.md](routes.md)
- [../reference/caddyfile-reference.md](../reference/caddyfile-reference.md)
