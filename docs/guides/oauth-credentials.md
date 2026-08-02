# OAuth Credentials with agw-auth

Interactive CLI authentication is provided by the separate
[`agw-auth`](https://github.com/agent-guide/agw-auth) project. Agent Gateway
does not expose login sessions or authenticator configuration through its
Admin API.

## Login

Create the target provider in Agent Gateway first, then run `agw-auth` on the
machine where the browser or device flow should happen:

```bash
agw-auth login \
  --authenticator codex \
  --provider-id codex-main \
  --gateway-addr http://localhost:8019
```

The Gateway's current OAuth-consuming providers are `codex` and `claudecode`.
Add `--no-browser` for a headless flow; Codex also supports `--device-flow`.
The `gemini` provider currently uses a Google AI Studio API key and does not
consume a Gemini CLI OAuth credential.

The tool resolves the provider through the Gateway Admin API and creates a
managed `oauth_token` credential. The default scope is
`id:<provider_id>`; pass `--scope type:<provider_type>` when one credential
should be eligible for every provider of that type.

## Runtime Refresh

Expiry detection and refresh orchestration remain in Agent Gateway, but all
provider-specific token refresh behavior lives in `agw-auth`. A credential
produced by `agw-auth` includes `refresh_name`, expiry,
`refresh_expiry_delta`, and refresh-token metadata. `refresh_name` is opaque to
Agent Gateway and is interpreted by `agw-auth`. Before an upstream model
request, the credential manager invokes the configured argv, which defaults to:

```text
agw-auth refresh
```

It sends the stored credential as JSON on stdin, persists the updated JSON
returned on stdout, and then continues the request. Configure another static
argv with Caddyfile `credential_refresh_command`, or use the standalone
`--credential-refresh-command` flag plus repeatable `--credential-refresh-arg`
flags. Explicit configuration replaces the complete argv: write
`credential_refresh_command agw-auth refresh`, not only
`credential_refresh_command agw-auth`, because the gateway does not append the
`refresh` subcommand. The standalone equivalent needs
`--credential-refresh-command agw-auth --credential-refresh-arg refresh`.

When refresh requests require an explicit proxy or Claude Code's
browser-like TLS profile, set `AGW_AUTH_PROXY_URL` or
`AGW_AUTH_TRANSPORT_PROFILE=browser_like_tls` in the Gateway process
environment. The spawned `agw-auth` process inherits those settings. The same
settings are also available as `agw-auth refresh` flags for manual execution.

Refresh is request-driven rather than a background polling job. A credential
that has been idle may therefore add refresh latency to its first request.
Credentials without expiry metadata are not refreshed automatically. If
`agw-auth` receives a credential without `refresh_name`, it reports a refresh
error. A missing, invalid, or zero refresh delta uses the gateway's 30-second
safety window; a positive value selects a credential-specific window.
The refresh subprocess is limited to 30 seconds, 64 KiB of stdout, and 16 KiB
of captured stderr.

Install `agw-auth` on the gateway host, not only on the machine used for login.
If the executable is missing or refresh fails, the gateway rejects that
credential for the current attempt; the request fails when no alternate
credential or configured provider authentication is available. A failed
refresh also cools the credential for 30 seconds, so subsequent requests fail
over fast instead of each waiting on the failing refresh subprocess.

Inspect the stored credential with:

```bash
./agwctl gateway --admin-addr http://localhost:8019 \
  credential list --type oauth_token
```

## Responsibility Boundary

- `agw-auth` owns OAuth/PKCE, device flow, browsers, callback servers, initial
  credential registration, and provider-specific token refresh.
- Agent Gateway owns credential persistence, scheduling, expiry detection,
  external refresh invocation, and provider invocation.

## Related Docs

- [credentials.md](credentials.md)
- [../reference/admin-api-reference.md](../reference/admin-api-reference.md)
- [../reference/agwctl-reference.md](../reference/agwctl-reference.md)
