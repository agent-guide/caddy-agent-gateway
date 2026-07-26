# pkg/acp — AGENTS.md

Scope: native ACP service configuration and runtime integration. The root
`AGENTS.md` rules apply. Architecture, endpoint inventories, and implementation
status belong in `docs/architecture/acp-architecture.md`.

## Boundaries

- ACP is implemented natively in this repository; do not add a dependency on
  `github.com/beyond5959/ngent`.
- Supported runtime adapters are registered through `pkg/acp/agentspi`; shared
  wire parsing belongs in `pkg/acp/runtime`, with `session/update` parsing in
  `pkg/acp/runtime/acpupdate`.
- At the gateway adapter boundary, snapshot an Agent-bound service into an
  identity-free `pkg/acp/runtime.RuntimeConfig` and use `agent_id` as the
  runtime owner key. Keep Agent identity and control-plane dependencies out of
  `pkg/acp`.

## Protocol invariants

- ACP permission handling is fail-closed. Interactive replies use the nested
  ACP outcome shape and preserve the exact option ids supplied by the agent;
  never reply with a flat `approved`/`declined` outcome.
- Do not add `model` or `modelId` to `session/new` or `session/prompt`. Apply
  model selection and other overrides through `session/set_config_option`.
- Keep permission replies off the transport read loop. Missing streaming
  clients, timeouts, and transient session-list/transcript connections must
  not auto-approve requests.
- Continue draining prompt updates for the configured quiet grace period after
  the prompt result; real adapters may deliver final message chunks after the
  result.
- Keep raw wire session ids distinct from host-bound ids. Use the existing
  `StableSessionResolver`/`SessionLoadResolver` seams for adapters whose ids
  differ instead of rewriting shared driver semantics.

## Sessions and errors

- Check advertised ACP capabilities before calling session list or load.
- Apply the optional session-list `cwd` filter in the gateway after
  symlink-canonicalizing both sides; never forward that filter to the agent.
- Preserve the shared HTTP error contract: missing service is `404`,
  client-correctable input represented by `acpruntime.ErrInvalidRequest` is
  `400`, and agent/transport failure is `502`.
- Pool changes must preserve dead-instance eviction, idle cleanup, instance
  caps, and scope rebind: a session-addressed turn adopts the live instance
  already bound to that session rather than spawning a second process.
