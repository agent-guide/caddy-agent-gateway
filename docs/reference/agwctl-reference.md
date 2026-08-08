# `agwctl` Reference

`agwctl` is the management CLI for the Gateway Admin API and direct Caddy Admin API operations. Interactive OAuth login is provided by the separate `agw-auth` project.

## Command Layout

- gateway bundle operations and resources are direct top-level commands such as
  `agwctl apply`, `agwctl agent`, `agwctl provider`, and `agwctl mcp-service`
- `agwctl caddy ...`

## Common Patterns

Show available commands:

```bash
./agwctl --help
```

List gateway LLM routes through the gateway Admin API:

```bash
./agwctl --admin-addr http://localhost:8019 \
  --admin-basic-auth admin:your-password \
  llm-route list
```

List Caddy HTTP servers through the Caddy admin API directly:

```bash
./agwctl caddy --addr http://127.0.0.1:2019 server list
```

Create an OAuth credential with `agw-auth`, then list the Gateway-stored credential:

```bash
agw-auth login --authenticator codex --provider-id openai-main

./agwctl --admin-addr http://localhost:8019 \
  --admin-basic-auth admin:your-password \
  credential list \
  --type oauth_token
```


Validate a gateway bundle YAML file locally:

```bash
./agwctl validate -f ./examples/gateway.bundle.llm.direct-provider.yaml
```

Apply a gateway bundle YAML file through the Admin API:

```bash
./agwctl --admin-addr http://localhost:8019 \
  --admin-basic-auth admin:your-password \
  apply -f ./examples/gateway.bundle.llm.direct-provider.yaml
```

Export remote gateway objects as bundle YAML:

```bash
./agwctl --admin-addr http://localhost:8019 \
  --admin-basic-auth admin:your-password \
  export -f ./gateway.bundle.yaml
```

The exported bundle does not include managed upstream credentials. Preserve
credentials separately through `/admin/credentials`; bundle export is not a
complete gateway backup.

## Agent Commands

Agents and unified ingress routes are bundle objects (`agents` and
`agentRoutes`). Common Agent operations include:

```bash
./agwctl agent list
./agwctl agent get <agent-id>
./agwctl agent workspace <agent-id>
./agwctl agent capabilities <agent-id>
./agwctl agent runs <agent-id>
./agwctl agent cancel <agent-id> <run-id> [--mode force|graceful]
./agwctl agent permissions <agent-id>
./agwctl agent decide <agent-id> <request-id> --outcome <outcome>
./agwctl agent sessions <agent-id> [--cwd <cwd>] [--cursor <cursor>]
./agwctl agent transcript <agent-id> <session-id> [--cwd <cwd>]
./agwctl agent delete <agent-id>
```

The default `agent list` table includes `RUNTIME-STATE` and `EXECUTABLE`.
`not_executable` / `no` means the Agent and route can be configured but turns
will return `501 runtime_not_executable`.

Manage AgentRoutes with:

```bash
./agwctl agent-route list
./agwctl agent-route get <agent-route-id>
./agwctl agent-route create -f <route.yaml>
./agwctl agent-route update <agent-route-id> -f <route.yaml>
./agwctl agent-route delete <agent-route-id>
```

Backend-specific runtime diagnostics and recovery remain available:

```bash
./agwctl acp-runtime get
./agwctl acp-runtime inflight
./agwctl acp-runtime close-thread <agent-id> <thread-id>
./agwctl builtin-runtime get
./agwctl builtin-runtime inflight
```

Cancel logical runs with `agent cancel <agent-id> <run-id>`; runtime
diagnostic commands do not provide a second session-keyed cancellation path.

## Important Notes

- `agwctl credential ...` manages remote gateway credentials through the Admin API
- `agw-auth login ...` runs interactive login flows and registers credentials
- `agwctl agent ...` operates runtime-neutral Agent state; `agent-route ...` manages unified ingress
- `agwctl acp-runtime ...` and `builtin-runtime ...` expose backend-specific diagnostics and recovery
- `agwctl apply/export ...` is the recommended CLI path for configuration objects instead of per-object JSON create or update workflows

## Related Docs

- [runtime-modes.md](runtime-modes.md)
- [admin-api-reference.md](admin-api-reference.md)
- [acp-api.md](acp-api.md)
- [../design/gateway-bundle-yaml.md](../design/gateway-bundle-yaml.md)
