# Provider Option Reference

This page lists the current common provider config fields and notable provider-specific options.

## Common Provider Fields

These fields are parsed from provider blocks in the Caddyfile and are also reflected in provider config objects:

- `provider_type`
- `api_key`
- `base_url`
- `default_model`
- `request_timeout_seconds`
- `max_retries`
- `retry_delay_seconds`
- `max_idle_connections`
- `max_idle_connections_per_host`
- `idle_keep_alive_timeout_seconds`
- `proxy_url`
- `header <name> <value>`
- `option <key> <value>`

## Field Notes

`provider_type`

- required
- must match the actual mounted provider module type

`api_key`

- provider-local fallback API key
- does not register as a managed credential

`base_url`

- overrides the provider default base URL

`default_model`

- provider default upstream model
- useful as an operator default, but route behavior still determines how request models are interpreted

Network tuning fields:

- `request_timeout_seconds`
- `max_retries`
- `retry_delay_seconds`
- `max_idle_connections`
- `max_idle_connections_per_host`
- `idle_keep_alive_timeout_seconds`
- `proxy_url`

Extra outbound request shaping:

- `header <name> <value>` adds a persistent extra HTTP header
- `option <key> <value>` sets provider-specific options and is parsed as strings in the Caddyfile

## Built-In Provider Defaults

- `openai`: `https://api.openai.com/v1`
- `anthropic`: `https://api.anthropic.com`
- `codex`: `https://chatgpt.com/backend-api/codex`
- `openrouter`: `https://openrouter.ai/api/v1`
- `deepseek`: `https://api.deepseek.com`
- `zhipu`: `https://open.bigmodel.cn/api/paas/v4`

## Notable Provider-Specific Options

`compact` (compatibility mode selector)

- supported values are `cc`, `codex`, and `none`
- unsupported modes are ignored by providers that do not implement that compatibility profile
- `option compact cc` enables Claude Code CLI compatibility mode for OpenAI-compatible chat providers (`openai`, `deepseek`, `openrouter`, `zhipu`) by dropping the OpenAI-style `metadata` and `user` request fields
- Claude Code always sends `metadata.user_id`; some OpenAI-compatible upstreams (e.g. GLM) reject these fields with a generic 400
- default is `none`; `metadata`/`user` are forwarded unless `compact` is `cc`

`claudecode`

- `option api_key_header <authorization|x-api-key>`
  - controls which header carries a plain API key (provider `api_key` or a managed `api_key` credential)
  - default is `authorization`, which sends `Authorization: Bearer <key>`
  - `x-api-key` sends the key in the `x-api-key` header instead
  - a managed `oauth_token` and any `sk-ant-oat-` OAuth token always use `Authorization: Bearer` regardless of this option
  - invalid values are rejected at startup
- `option compact codex` enables Codex CLI compatibility mode for Claude-Code-gated upstreams
  - rewrites Codex tool names (e.g. `exec_command`) to their Claude Code equivalents (e.g. `Bash`) on the outbound request so an upstream that gates on Claude Code tool names accepts Codex traffic, then restores the original names on the response
  - the rewrite is applied to the freshly built wire request only and never mutates the inbound request, so it is safe across retries
  - default is `none`; tool names are forwarded unchanged unless `compact` is `codex`

`codex`

- uses OpenAI-compatible `POST /responses`
- custom `base_url` must match the upstream codex-compatible deployment
- `option compact cc` enables Claude Code CLI compatibility mode by filtering stateful Claude Code tools that Codex-compatible upstreams do not reliably sequence

`deepseek`

- `option path <path>`
- `option response_format_type <text|json_object>`
- `option thinking_type <disabled|enabled|none>`
  - DeepSeek v4 models run in thinking mode by default and then require the `reasoning_content` of every tool-calling assistant turn to be replayed on the next request; the cc/anthropic protocol does not round-trip reasoning content, so default is `disabled` to keep Claude Code CLI tool loops working
  - `none` omits the `thinking` field entirely (use the upstream default)
- tuning options such as `max_tokens`, `temperature`, `top_p`, `presence_penalty`, `frequency_penalty`, `log_probs`, and `top_log_probs`

`zhipu`

- `option api_profile <auto|standard|coding_plan>`
  - default is `auto`; the official `/api/coding/` URL selects `coding_plan`, otherwise `standard`
  - set `coding_plan` explicitly when the Coding Plan API is exposed through a proxy or a custom URL
- `option context_window <positive integer>` and `option max_output_tokens <positive integer>` override the provider-level capability defaults
- `option vision <true|false>` and `option embeddings <true|false>` override the provider-level feature defaults
- capability options are coarse provider defaults; use managed-model `capability_overrides` when models on one endpoint have different limits or features
- `option thinking_type <disabled|enabled|none>`
- missing or `none` omits the field and uses the upstream GLM default; an inbound request-level `thinking` value overrides the provider option
- normalizes Anthropic `thinking.type=adaptive` to GLM `enabled`; in the `coding_plan` profile, maps Codex/OpenAI reasoning efforts `minimal|low|medium|high` to GLM `high` and `xhigh|max` to GLM `max`
- preserves GLM `reasoning_content` in assistant tool-call history and emits it through OpenAI Chat, Responses reasoning-summary compatibility events, and Anthropic/cc thinking blocks
- streaming requests with tools automatically enable GLM `tool_stream` unless the inbound request explicitly sets it
- `option compact cc` — see the shared `compact` note above

## Current Built-In Provider Types

- `openai`
- `anthropic`
- `codex`
- `claudecode`
- `gemini`
- `ollama`
- `openrouter`
- `deepseek`
- `zhipu`

## Related Docs

- [../guides/providers.md](../guides/providers.md)
- [caddyfile-reference.md](caddyfile-reference.md)
