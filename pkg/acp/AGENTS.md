# pkg/acp — AGENTS.md

Scope: native ACP runtime configuration and protocol integration. The root
`AGENTS.md` rules apply. Architecture, endpoint inventories, and implementation
status belong in `docs/architecture/acp-architecture.md`.

## Boundaries

- ACP is implemented natively in this repository; do not add a dependency on
  `github.com/beyond5959/ngent`.
- Supported runtime adapters are registered through `pkg/acp/agentspi`; shared
  wire parsing belongs in `pkg/acp/runtime`, with `session/update` parsing in
  `pkg/acp/runtime/acpupdate`.
- At the gateway adapter boundary, pass the Agent-owned ACP config as an
  identity-free `pkg/acp/runtimeconfig.Config` and use `agent_id` as the
  runtime owner key. Keep Agent identity and control-plane dependencies out of
  `pkg/acp`.
- Pooled instances record the config content fingerprint they were created
  under (`RuntimeConfig.Fingerprint` / the internal `configFingerprint`). A
  turn never reuses an instance whose fingerprint differs from the current
  config, and `Manager.RetireOwner(ownerID, keepFingerprint)` closes idle
  stale instances immediately while active ones drain their in-flight turn
  and are reaped by the janitor once idle. The manager also records the current
  accepted owner fingerprint and rechecks it before inserting a newly created
  process, so a pre-retirement request cannot repopulate the pool afterward.
  Never bypass the fingerprint check when adding pool creation, reuse, or
  adoption paths.

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
- Preserve the shared HTTP error contract: missing Agent is `404`,
  client-correctable input represented by `acpruntime.ErrInvalidRequest` is
  `400`, and agent/transport failure is `502`.
- Pool changes must preserve dead-instance eviction, idle cleanup, instance
  caps, and scope rebind: a session-addressed turn adopts the live instance
  already bound to that session rather than spawning a second process.
- Agent-bound turns register their exact common `run_id` in the native runtime.
  Force cancellation must send ACP `session/cancel` through that run's live
  instance and cancel only its context; never implement ordinary run cancel by
  closing an owner scope, thread, or pooled process. Before the live instance
  and protocol session id are bound, cancellation must return a retryable error
  and leave the run context alive; it must never silently skip the native frame.
  Agent deletion/runtime retirement is distinct from an operator exact-run
  cancel: it records a fail-closed cancellation request even in the pre-bind
  window, and native bind must send `session/cancel` before allowing the prompt
  loop to start. The common bulk-cancel path uses bounded retry for backends
  that cannot persist this pending state.
- ACP Agent permission decisions are claimed by the common Agent permission
  broker before the ACP waiter is resolved. The native waiter is continuation
  state only. Common records retain only an opaque continuation token; raw ACP
  permission params and the native request id remain in the ACP-owned store.
