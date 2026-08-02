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
- `anthropic`: delegates chat/streaming to the eino-ext `claude` component (official anthropic-sdk-go); the provider keeps thinking-budget normalization, request metadata, and ListModels. `anthropicbase` remains the hand-rolled Messages wire layer used by `claudecode`.
- `claudecode`
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
- add a blank import for the runtime provider package in `cmd/agw/main.go`, `cmd/agwd/main.go`, and `cmd/agwctl/cmd_gateway.go`; agwctl needs it because `gateway validate`/`apply` check `provider_type` against the locally linked provider registry
