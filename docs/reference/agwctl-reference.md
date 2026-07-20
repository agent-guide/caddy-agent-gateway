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

## ACP Commands

ACP services and routes are gateway bundle objects (`acpServices` and
`acpRoutes`) and can also be inspected or operated through dedicated commands.

Service commands:

```bash
./agwctl gateway acp-service list
./agwctl gateway acp-service get <acp-service-id>
./agwctl gateway acp-service delete <acp-service-id>
./agwctl gateway acp-service sessions <acp-service-id> [--cwd <cwd>] [--cursor <cursor>]
./agwctl gateway acp-service transcript <acp-service-id> <session-id> [--cwd <cwd>]
```

Route commands:

```bash
./agwctl gateway acp-route list
./agwctl gateway acp-route get <acp-route-id>
./agwctl gateway acp-route delete <acp-route-id>
```

Runtime commands:

```bash
./agwctl gateway acp-runtime get
./agwctl gateway acp-runtime inflight
./agwctl gateway acp-runtime close-thread <acp-service-id> <thread-id>
./agwctl gateway acp-runtime resolve-permission <request-id> --outcome selected --option-id <option-id>
```

Use `--outcome cancelled` to deny an interactive permission request.

## Builtin Commands

Builtin routes are gateway bundle objects (`builtinRoutes`) and can also be
inspected through dedicated commands; the builtin ADK host runtime is operated
through `builtin-runtime`.

Route commands:

```bash
./agwctl gateway builtin-route list
./agwctl gateway builtin-route get <builtin-route-id>
```

Runtime commands:

```bash
./agwctl gateway builtin-runtime get
./agwctl gateway builtin-runtime inflight
./agwctl gateway builtin-runtime cancel-turn <agent-id> <session-id> [--mode force|graceful]
```

`cancel-turn` stops a running turn: `--mode force` (default) aborts
immediately — the answer for a stuck turn — and `--mode graceful` stops after
the current model/tool step, escalating to force after a grace period.

## Important Notes

- `agwctl gateway credential ...` manages remote gateway credentials through the Admin API
- `agwctl cliauth ...` runs local login flows
- `agwctl gateway cliauth ...` inspects remote gateway CLI auth authenticators and refresher state
- `agwctl gateway acp-service ...`, `acp-route ...`, and `acp-runtime ...` manage ACP config and runtime state through the Admin API
- `agwctl gateway apply/export ...` is the recommended CLI path for configuration objects instead of per-object JSON create or update workflows

## Related Docs

- [runtime-modes.md](runtime-modes.md)
- [admin-api-reference.md](admin-api-reference.md)
- [acp-api.md](acp-api.md)
- [../design/gateway-bundle-yaml.md](../design/gateway-bundle-yaml.md)
