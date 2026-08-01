# Runtime Modes

`agent-gateway` builds three binaries with different runtime roles.

## `agw`

- the main Caddy-based gateway runtime
- uses a Caddyfile plus the shared config store
- supports normal Caddy subcommands such as `run`, `reload`, `validate`, and `hash-password`

## `agwd`

- the standalone gateway daemon
- uses `--config-store`, optional `--static-config`, and optional repeated `--provider-type`
- does not use a Caddyfile runtime
- if any `--provider-type` flag is set, only those provider types are enabled for the process

Current static config restriction:

- `agwd --static-config` `llmRoutes` only support direct-provider targets
- `agwd --static-config` does not support `managedModels`
- create logical-model routes and managed models through the Admin API or bundle workflows

## `agwctl`

- the management CLI
- talks to the gateway Admin API, the Caddy admin API, or local CLI auth flows depending on the command group

Primary command families:

- `agwctl gateway ...`
- `agwctl caddy ...`
- `agwctl cliauth ...`

Recommended workflows:

- use `agwctl gateway apply/export/validate` for bundle-based configuration management
- use `agwctl gateway credential ...` for remote gateway credential management
- use `agwctl cliauth ...` for local login flows
- use `agwctl caddy ...` for direct Caddy admin API operations

Bundle YAML examples used by current workflows:

- `examples/gateway.bundle.llm.direct-provider.yaml`
- `examples/gateway.bundle.llm.logical-model.yaml`
- `examples/gateway.bundle.cliauth-authenticators.yaml`

## Related Docs

- [../getting-started/quickstart-llm.md](../getting-started/quickstart-llm.md)
- [agwctl-reference.md](agwctl-reference.md)
- [caddyfile-reference.md](caddyfile-reference.md)
