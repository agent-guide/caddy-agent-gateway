# Caddyfile Reference

This reference applies to the `agw` runtime. If you run `agwd`, use `--config-store` and optional `--static-config` bundle YAML instead of a Caddyfile.

## Top-Level Shape

The gateway is configured in the global `agent_gateway` block:

```caddy
{
	agent_gateway {
		config_store sqlite { ... }
		credential_refresh_command agw-auth refresh
		metrics { ... }
		provider <provider-id> { ... }
		route <route-id> { ... }
	}
}
```

Typical server block:

```caddy
http://127.0.0.1:8080 {
	route /admin/* {
		basic_auth {
			admin <hashed-password>
		}
		agent_gateway_admin
	}

	agent_route_dispatcher {
		llm_api openai
		llm_api anthropic
		mcp
	}
}
```

## `config_store sqlite`

```caddy
config_store sqlite {
	path ./data/configstore.db
}
```

- `path` sets the SQLite database path
- if `path` is omitted in `agw`, the store defaults to Caddy's app data directory under `agent-gateway/configstore.db`

## `credential_refresh_command`

`credential_refresh_command <executable> [args...]` selects the executable and
static arguments used when a managed credential reaches its request-time
refresh window. It defaults to `agw-auth refresh`. The gateway executes this
argv directly without a shell or variable expansion and does not append any
arguments. Dynamic refresh data is carried only by the credential JSON on
stdin. The command must implement the protocol documented in the OAuth
credential guide. Because an explicit directive replaces the complete argv,
use `credential_refresh_command agw-auth refresh`; an explicit
`credential_refresh_command agw-auth` runs `agw-auth` with no subcommand.

## `metrics`

```caddy
metrics {
	retention_days 30
	max_agent_depth 0
}
```

- `retention_days <days>` sets the usage-event retention window; omitted or `0` uses the 30-day default
- `max_agent_depth <depth>` rejects requests whose inbound `X-Agent-Depth` is greater than or equal to this value; `0` disables the depth gate

The gateway always records usage events through the configured SQLite store when the store exposes the metrics database capability. Metrics are queried through the Admin API under `/admin/metrics*`.

## `provider <provider-id>`

Provider type availability is startup-only configuration:

```caddy
provider_types {
	openai
	anthropic
}
```

If `provider_types` is omitted, all registered provider types are enabled. If it is present, only the listed provider types are enabled; every registered type that is not listed is disabled.

Common provider settings:

```caddy
provider openai-main {
	provider_type openai
	api_key {$OPENAI_API_KEY}
	base_url https://api.openai.com/v1
	default_model gpt-4.1
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

Important directives:

- `provider_type <name>`
- `api_key <value>`
- `base_url <url>`
- `default_model <model>`
- `request_timeout_seconds <seconds>`
- `max_retries <count>`
- `retry_delay_seconds <seconds>`
- `max_idle_connections <count>`
- `max_idle_connections_per_host <count>`
- `idle_keep_alive_timeout_seconds <seconds>`
- `proxy_url <url>`
- `header <name> <value>`
- `option <key> <value>`

Provider-specific notes:

- `openai` defaults to `https://api.openai.com/v1`
- `deepseek` defaults to `https://api.deepseek.com`
- `deepseek` accepts `option path <path>`, `option response_format_type <text|json_object>`, `option thinking_type <disabled|enabled|none>`, and tuning options such as `max_tokens`, `temperature`, `top_p`, `presence_penalty`, `frequency_penalty`, `log_probs`, and `top_log_probs`
- `zhipu` defaults to `https://open.bigmodel.cn/api/paas/v4`
- `zhipu` accepts `option thinking_type <disabled|enabled|none>`
- `ollama` can be used without an API key
- `option` values are parsed as strings in the Caddyfile

## `route <route-id>`

Static LLM route syntax:

```caddy
route openai-chat {
	protocol openai
	host api.example.com
	path_prefix /v1
	method POST
	require_virtual_key
	target provider openai-main
}
```

Supported subdirectives:

- `protocol <openai|anthropic|cc>`
- `host <host>`
- `path_prefix <prefix>`
- `method <method> [more-methods...]`
- `require_virtual_key [true|false]`
- `target provider <provider-id>`

Current static route behavior:

- Caddyfile routes only support direct-provider mode
- the request `model` is forwarded upstream as the provider model name
- logical-model routes are not accepted in Caddyfile config

## `agent_route_dispatcher`

Dispatcher example:

```caddy
agent_route_dispatcher {
	llm_api openai
	llm_api anthropic
	llm_api cc
	mcp
	acp
}
```

- `llm_api <name>` enables a protocol handler
- current LLM handler names are `openai`, `anthropic`, and `cc`
- `mcp` enables MCP protocol handling in the dispatcher
- `acp` enables ACP turn, permission, session-list, and transcript handling in the dispatcher

## `agent_gateway_admin`

Admin handler example:

```caddy
route /admin/* {
	@needauth {
		not path /admin/health
		not method OPTIONS
	}
	basic_auth @needauth {
		admin <hashed-password>
	}
	agent_gateway_admin
}
```

- `agent_gateway_admin` mounts the Admin API
- protect the mount with standard Caddy middleware such as `basic_auth`, mTLS, or an upstream auth gateway
- scope the auth matcher to skip `GET /admin/health` (a public probe) and `OPTIONS` (CORS preflight carries no credentials) so health checks and browser admin UIs keep working
- generate a Basic Auth hash with `./agw hash-password --plaintext 'your-password'`

## VirtualKey Notes

`agw` does not support static `virtualkey` declarations in the Caddyfile.

If a route sets `require_virtual_key`, create VirtualKeys through the Admin API after startup. The gateway persists them in the config store and generates the bearer `key` value at creation time.

## Related Docs

- [../getting-started/quickstart-llm.md](../getting-started/quickstart-llm.md)
- [../guides/admin-auth.md](../guides/admin-auth.md)
- [runtime-modes.md](runtime-modes.md)
- [admin-api-reference.md](admin-api-reference.md)
