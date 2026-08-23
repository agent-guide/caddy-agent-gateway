# pkg/llm — AGENTS.md

Scope: the LLM provider layer (`pkg/llm/provider/` plus the built-in provider
runtimes). Paths are repository-root relative;
the root `AGENTS.md` global rules apply.

## `provider/`

This package defines the provider interface and provider registry.

Important files:

- `provider.go`: provider request and response types
- `registry.go`: provider factory registration

Provider config `api_key` values are provider-local fallback configuration.
They do not register as managed credentials and do not participate in credential scheduling.

Built-in provider runtime packages:

- `openai`
- `anthropic`: delegates chat/streaming to the eino-ext `claude` component (official anthropic-sdk-go); the provider keeps thinking-budget normalization, request metadata, signed-thinking replay adaptation, and ListModels. The adapter captures the raw non-streaming response to restore every signed/redacted thinking block that eino flattens or discards. The pinned eino streaming parser still discards upstream `redacted_thinking`, so streaming can replay redacted blocks already present in client history but cannot capture new ones. `anthropicbase` remains the hand-rolled Messages wire layer used by `claudecode` and the exact signed/redacted replay patch used by the eino adapter; `claudecode` captures and replays both block types directly from both wire modes. Upgrading `eino-ext/components/model/claude` requires running all tests under `pkg/llm/provider/anthropic`; those tests pin the component's private Extra keys and streaming block-boundary behavior.
- `claudecode`: Anthropic Messages wire provider with Claude Code fingerprint,
  signed/redacted thinking-block replay, native Anthropic server-tool request,
  response, citation, and history replay, and generic capability/default-token
  overrides for compatible endpoints. Provider-type capability metadata filters
  native Anthropic tools and opaque content history before credential selection;
  the eino-backed `anthropic` provider does not currently preserve web-search
  response blocks and is therefore ineligible for that state.
- `codex`
- `gemini`
- `ollama`
- `openrouter`
- `deepseek`: delegates chat/streaming to the eino-ext `deepseek` component
  (`deepseek-go` underneath); the provider retains request compatibility,
  thinking-mode defaults, and Responses-via-chat adaptation
- `zhipu`
- `qwen`: DashScope OpenAI-compatible mode via the eino-ext `qwen` component; optional `enable_thinking` provider option, per-request reasoning fields override it

eino bridge (a standalone library, one of the PB0 prerequisites of the
builtin agent runtime — see `docs/design/builtin-agent-runtime.md` §11):

- `pkg/llm/provider/einomodel`: presents a `provider.Provider` (preferably a
  `*gateway.RoutedProvider`, so credential scheduling, candidate fallback,
  and usage attribution apply) as an eino `model.ToolCallingChatModel`

The sibling bridge `pkg/mcp/einotool` presents gateway-managed MCP tools as
eino `InvokableTool`s — see `pkg/mcp/AGENTS.md`.

Self-implemented chat providers (`codex`, `claudecode`) fire the eino
callback aspect functions through the `provider.OnChatStart/OnChatEnd/
OnChatError/OnChatStreamEnd` helpers (`callbackaspects.go`), so registered
callbacks handlers observe every chat provider uniformly.

Provider registration rules:

- implement the `provider.Provider` interface
- register the factory with `provider.RegisterProviderFactory(...)`
- register stable protocol-fidelity metadata with
  `provider.RegisterProviderTypeCapabilities(...)` when route candidate
  filtering must happen before provider construction or credential selection
- add a blank import for the runtime provider package in `cmd/agw/main.go`, `cmd/agwd/main.go`, and `cmd/agwctl/cmd_gateway.go`; agwctl needs it because `validate`/`apply` check `provider_type` against the locally linked provider registry

## Native protocol state

Anthropic server tools, citations, and other content blocks that the generic
eino message model cannot express are carried verbatim: `ChatExtraFields.
AnthropicTools`/`AnthropicToolChoice` on the request, `provider.Attach
AnthropicContentBlocks` / `AttachAnthropicStreamEvent` on messages, and the
`RequiredNativeDialects` / `NativeDialects` capability sets for route filtering.
Authentic signed/encrypted reasoning uses the narrower
`RequiredReasoningDialects` / `ReasoningDialects` sets because the eino-backed
`anthropic` provider can replay it without supporting every native server-tool
content shape. Static provider-type capability registration is the single
source of truth; do not add matching runtime marker interfaces.

Rules for this state:

- populate `AnthropicTools` only when at least one declaration is a server tool
  or carries non-null fields that the generic tool model cannot express; when
  raw replay is required, retain the entire ordered tool array and tool choice
- attach native content only when the generic model would lose information
  (unknown block type or unmodeled fields). Attaching it for ordinary text or
  `tool_use` answers pins every later turn of that conversation to a
  native-capable provider and disables incompatible candidate fallback; the
  routed provider logs the active dialect requirements at Debug level
- authentic Anthropic thinking signatures and encrypted/redacted reasoning
  require an Anthropic reasoning-dialect-capable provider even when `ReasoningParts`
  can represent them structurally; representation does not make opaque upstream
  state portable across providers or models
- never drop a block or tool declaration that fails to decode; replay the raw
  bytes (`anthropicbase.OpaqueContentBlock`, `anthropicbase.OpaqueToolDef`)
- the eino-backed `anthropic` provider captures non-stream responses only to
  restore signed/redacted reasoning; it must not expose citations or other raw
  native blocks that its request and streaming paths cannot preserve

The routing capability model is dialect-neutral: add future codex / OpenAI
Responses fidelity requirements to the existing dialect sets rather than adding
parallel `RequireOpenAINative` fields. The message extras are still a
single-dialect solution; the second dialect that needs raw block replay should
generalize them into one envelope carrying `{dialect, blocks}`.

Signed reasoning is opaque upstream state, not a portable gateway token. Routes
that replay it across tool turns must keep the same upstream provider and model;
candidate switching requires a route/session-level affinity design before the
signature can be forwarded safely.
