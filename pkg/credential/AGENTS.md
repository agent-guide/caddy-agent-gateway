# pkg/credential — AGENTS.md

Scope: the cross-cutting managed-credential domain. Paths are repository-root
relative; the root `AGENTS.md` rules apply.

This package owns credential models, persistence, selection state, expiry
detection, and the generic external refresh-command protocol. It is not an LLM
package because credentials may also serve MCP, ACP, and future upstream
resources.

Current credential types are `api_key` and `oauth_token`. OAuth credentials
carry access/refresh token material and request-time refresh metadata.

Provider-specific OAuth endpoints, client IDs, token exchange rules, retries,
interactive browser flows, and device flows belong to the independent
`agw-auth` project. Do not add provider-specific refresh implementations to
Agent Gateway.

Request-time refresh uses `metadata.expired` and the optional
`metadata.refresh_expiry_delta`; a missing, invalid, or zero delta uses the
30-second safety window. The gateway executes the configured executable
and its static arguments exactly as configured, writes one credential JSON
object to stdin, reads one updated credential JSON object from stdout, then
persists the result before continuing the upstream request. Provider-specific
metadata such as `refresh_name` is opaque to the gateway and is interpreted by
the external tool. Refresh output metadata is merged over the stored metadata
so omitted, unchanged opaque fields remain available for later refreshes.

Keep refresh-token rotation serialized so concurrent requests cannot reuse a
rotating refresh token. A refresh failure is memoized per credential for 30
seconds so queued and immediately subsequent requests fail over without
re-running the failing external command.
