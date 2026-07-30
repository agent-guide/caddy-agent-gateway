# `agwctl` Reference

`agwctl` is the management CLI for the gateway Admin API, direct Caddy admin API operations, and local CLI auth login flows.

## Main Command Families

- `agwctl gateway ...`
- `agwctl caddy ...`
- `agwctl cliauth ...`

## Common Patterns

Show available commands:

```bash
./agwctl --help
```

List gateway LLM routes through the gateway Admin API:

```bash
./agwctl gateway --admin-addr http://localhost:8019 \
  --admin-basic-auth admin:your-password \
  llm-route list
```

List Caddy HTTP servers through the Caddy admin API directly:

```bash
./agwctl caddy --addr http://127.0.0.1:2019 server list
```

Start a local CLI auth login flow and list gateway-stored CLI auth credentials:

```bash
./agwctl cliauth login --authenticator codex --provider-id openai-main

./agwctl gateway --admin-addr http://localhost:8019 \
  --admin-basic-auth admin:your-password \
  credential list \
  --type cliauth_token
```

List remote gateway CLI auth authenticators and refresher status:

```bash
./agwctl gateway --admin-addr http://localhost:8019 \
  --admin-basic-auth admin:your-password \
  cliauth authenticators list
```

```bash
./agwctl gateway --admin-addr http://localhost:8019 \
  --admin-basic-auth admin:your-password \
  cliauth refresher status
```

Validate a gateway bundle YAML file locally:

```bash
./agwctl gateway validate -f ./examples/gateway.bundle.minimal.yaml
```

Apply a gateway bundle YAML file through the Admin API:

```bash
./agwctl gateway --admin-addr http://localhost:8019 \
  --admin-basic-auth admin:your-password \
  apply -f ./examples/gateway.bundle.minimal.yaml
```

Export remote gateway objects as bundle YAML:

```bash
./agwctl gateway --admin-addr http://localhost:8019 \
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
./agwctl gateway agent list
./agwctl gateway agent get <agent-id>
./agwctl gateway agent workspace <agent-id>
./agwctl gateway agent capabilities <agent-id>
./agwctl gateway agent runs <agent-id>
./agwctl gateway agent cancel <agent-id> <run-id> [--mode force|graceful]
./agwctl gateway agent permissions <agent-id>
./agwctl gateway agent decide <agent-id> <request-id> --outcome <outcome>
./agwctl gateway agent sessions <agent-id> [--cwd <cwd>] [--cursor <cursor>]
./agwctl gateway agent transcript <agent-id> <session-id> [--cwd <cwd>]
./agwctl gateway agent delete <agent-id>
```

The default `agent list` table includes `RUNTIME-STATE` and `EXECUTABLE`.
`not_executable` / `no` means the Agent and route can be configured but turns
will return `501 runtime_not_executable`.

Manage AgentRoutes with:

```bash
./agwctl gateway agent-route list
./agwctl gateway agent-route get <agent-route-id>
./agwctl gateway agent-route create -f <route.yaml>
./agwctl gateway agent-route update <agent-route-id> -f <route.yaml>
./agwctl gateway agent-route delete <agent-route-id>
```

Backend-specific runtime diagnostics and recovery remain available:

```bash
./agwctl gateway acp-runtime get
./agwctl gateway acp-runtime inflight
./agwctl gateway acp-runtime close-thread <agent-id> <thread-id>
./agwctl gateway builtin-runtime get
./agwctl gateway builtin-runtime inflight
```

Cancel logical runs with `gateway agent cancel <agent-id> <run-id>`; runtime
diagnostic commands do not provide a second session-keyed cancellation path.

## Important Notes

- `agwctl gateway credential ...` manages remote gateway credentials through the Admin API
- `agwctl cliauth ...` runs local login flows
- `agwctl gateway cliauth ...` inspects remote gateway CLI auth authenticators and refresher state
- `agwctl gateway agent ...` operates runtime-neutral Agent state; `agent-route ...` manages unified ingress
- `agwctl gateway acp-runtime ...` and `builtin-runtime ...` expose backend-specific diagnostics and recovery
- `agwctl gateway apply/export ...` is the recommended CLI path for configuration objects instead of per-object JSON create or update workflows

## Related Docs

- [runtime-modes.md](runtime-modes.md)
- [admin-api-reference.md](admin-api-reference.md)
- [acp-api.md](acp-api.md)
- [../design/gateway-bundle-yaml.md](../design/gateway-bundle-yaml.md)
