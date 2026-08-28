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
- `anthropic`: delegates chat/streaming to the eino-ext `claude` component (official anthropic-sdk-go); the provider keeps thinking-budget normalization, request metadata, signed-thinking replay adaptation, and ListModels. The adapter captures the raw non-streaming response to restore every signed/redacted thinking block that eino flattens or discards. The pinned eino streaming parser still discards upstream `redacted_thinking`, so streaming can replay redacted blocks already present in client history but cannot capture new ones. `anthropicbase` remains the hand-rolled Messages wire layer used by `claudecode` and the exact signed/redacted replay patch used by the eino adapter; `claudecode` captures and replays both block types directly from both wire modes. The official v0.1.25 component has a known race in `ChatModel.Stream` when it concatenates buffered empty messages; the repository temporarily accepts that upstream issue and excludes only this provider package from the CI race run. Upgrading `eino-ext/components/model/claude` requires running all tests under `pkg/llm/provider/anthropic`; those tests pin the component's private Extra keys and streaming block-boundary behavior.
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
- register stable protocol-fidelity metadata with an explicit single dialect
  and its atomic feature set through
  `provider.RegisterProviderTypeCapabilities(...)` when route candidate
  filtering must happen before provider construction or credential selection
- add a blank import for the runtime provider package in `cmd/agw/main.go`, `cmd/agwd/main.go`, and `cmd/agwctl/cmd_gateway.go`; agwctl needs it because `validate`/`apply` check `provider_type` against the locally linked provider registry

## Native protocol state

Protocol-native values that the generic eino message model cannot express use
the scoped, dialect-neutral `provider.ProtocolState`. Request tools and tool
choice live on `ChatRequest.ProtocolState`; history blocks, ephemeral response
bodies, and stream events use the one neutral message Extra carrier. The
registered dialect codec owns raw capture, presence-aware baseline digests,
differential overlay, stream folding, and duplicate-fragment validation.
`einomodel` carries request state through one impl-specific option and folds
response/stream transport state into persistent history before returning it.

The Messages AST derives one immutable `ProtocolRequirementSet`, including
bounded reasons, before generic conversion. Route selection performs only set
inclusion against the provider type's registered atomic feature set. Server-tool
request, native response, native history replay, and reasoning replay are
separate requirement-class features; body and stream relay are mode-selection
features and cannot enter request filtering. Static provider-type registration
is the single source of truth; do not add matching dialect sets or runtime
marker interfaces.

Rules for this state:

- populate request-scoped native envelopes only when at least one declaration
  is a server tool or carries non-null fields that the generic tool model cannot
  express; when raw replay is required, retain the entire ordered tool array and
  tool choice
- attach native content only when the generic model would lose information
  (unknown block type or unmodeled fields). Attaching it for ordinary text or
  `tool_use` answers pins every later turn of that conversation to a
  native-capable provider and disables incompatible candidate fallback; the
  routed provider logs the active protocol feature requirements at Debug level
- authentic Anthropic thinking signatures and encrypted/redacted reasoning
  require the Anthropic reasoning-replay feature even when `ReasoningParts`
  can represent them structurally; representation does not make opaque upstream
  state portable across providers or models
- never drop a block or tool declaration that fails to decode; replay the raw
  bytes (`anthropicbase.OpaqueContentBlock`, `anthropicbase.OpaqueToolDef`)
- the eino-backed `anthropic` provider captures successful non-stream response
  bodies for validated body relay and restores signed/redacted reasoning, but
  does not declare native stream relay or native-history replay

The routing capability model is dialect-neutral: add future Codex/OpenAI
Responses fidelity requirements as registered atomic features rather than
parallel `RequireOpenAINative` fields. Dialect-specific JSON inspection stays
inside its registered codec; neutral routing and eino packages only carry
envelopes and feature IDs.

Signed reasoning is opaque upstream state, not a portable gateway token. Routes
that replay it across tool turns must keep the same upstream provider and model;
candidate switching requires a route/session-level affinity design before the
signature can be forwarded safely.
