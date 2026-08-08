# Runtime Modes

`agent-gateway` builds three binaries with different runtime roles.

## `agw`

- the main Caddy-based gateway runtime
- uses a Caddyfile plus the shared config store
- supports normal Caddy subcommands such as `run`, `reload`, `validate`, and `hash-password`

## `agwd`

- the standalone gateway daemon
- uses `--config-store`, optional `--static-config`, optional repeated
  `--provider-type`, `--credential-refresh-command`, and repeatable
  `--credential-refresh-arg` (together defaulting to `agw-auth refresh`)
- does not use a Caddyfile runtime
- if any `--provider-type` flag is set, only those provider types are enabled for the process

Current static config restriction:

- `agwd --static-config` `llmRoutes` only support direct-provider targets
- `agwd --static-config` does not support `managedModels`
- create logical-model routes and managed models through the Admin API or bundle workflows

## `agwctl`

- the management CLI
- talks to the gateway Admin API or the Caddy admin API

Primary command families:

- gateway bundle operations and resource commands are available directly under
  `agwctl`
- `agwctl caddy ...`

Recommended workflows:

- use `agwctl apply/export/validate` for bundle-based configuration management
- use `agwctl credential ...` for remote gateway credential management
- use the external `agw-auth` tool for interactive OAuth login flows
- use `agwctl caddy ...` for direct Caddy admin API operations

Bundle YAML examples used by current workflows:

- `examples/gateway.bundle.llm.direct-provider.yaml`
- `examples/gateway.bundle.llm.logical-model.yaml`

## Related Docs

- [../getting-started/quickstart-llm.md](../getting-started/quickstart-llm.md)
- [agwctl-reference.md](agwctl-reference.md)
- [caddyfile-reference.md](caddyfile-reference.md)
