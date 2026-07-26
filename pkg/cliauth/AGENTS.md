# pkg/cliauth — AGENTS.md

Scope: CLI auth authenticators and credential refresh. Paths are
repository-root relative; the root `AGENTS.md` global rules apply.

This is a `pkg` runtime package, not `llm/cliauth/`.

Important files:

- `authenticator.go`: `Authenticator` interface and factory registration
- `manager.go`: runtime authenticator registry and state
- `autorefresher.go`: background refresh scheduling
- `types.go`: credential and status types

Built-in authenticators currently registered via `pkg/cliauth/authenticator/`:

- `codex`
- `claudecode`
- `gemini`

Authenticator registration rules:

- implement the `cliauth.Authenticator` interface
- register the factory with `cliauth.RegisterAuthenticatorFactory(...)`
- expose built-in authenticators through `pkg/cliauth/authenticator`
- keep that aggregate package blank-imported by `cmd/agw/main.go`,
  `cmd/agwd/main.go`, `cmd/agwctl/cmd_gateway.go`, and
  `cmd/agwctl/cmd_cliauth.go`; the daemon needs the runtime factories, while
  agwctl needs them for gateway validation/apply and local login flows
