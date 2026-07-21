# Eino Capability Reuse

## 1. Purpose

This document records which capabilities of the eino framework
(github.com/cloudwego/eino, currently v0.9.12) and its component ecosystem
(github.com/cloudwego/eino-ext) this project reuses directly, which
capabilities are planned for adoption, and which are deliberately not
adopted. The goal is to keep provider/protocol plumbing delegated to eino
wherever a maintained component exists, so this repository stays focused on
agent observation, management, scheduling, and builtin multi-agent
coordination.

Baseline facts in this document were verified against eino v0.9.12 and the
eino-ext component versions listed below.

## 2. Current Baseline

The gateway couples to eino through its core component interfaces, callbacks,
and the ADK orchestration stack, plus the eino-ext model components. The
builtin runtime uses `compose` only for ADK tool wiring and
interrupt/resume primitives; it does not depend on the legacy `flow/agent`
packages.

| Layer | What is used |
|---|---|
| `eino/schema` | `Message`, `ToolCall`, `ToolInfo`, `ResponseMeta`, `TokenUsage`, `StreamReader`/`Pipe`, tool-choice constants, multimodal input parts |
| `eino/components/model` | `ToolCallingChatModel`, `Option`, common options, `WrapImplSpecificOptFn`/`GetImplSpecificOptions` (carries gateway-specific request context inside option lists) |
| `eino/callbacks` | the observability tap (§4.1): a global handler in `internal/observability/einotap`, plus the aspect functions fired by the self-implemented providers |
| `eino/components/tool` | `InvokableTool`, implemented by the MCP bridge `pkg/mcp/einotool` (§5.3) |
| `eino/adk` | `Runner`, `ChatModelAgent`, sequential/parallel/loop workflows, checkpoint resume, and operator cancellation for the builtin runtime (§5) |
| `eino/adk/prebuilt` | supervisor, planexecute, and deep builtin topologies |
| `eino/adk/middlewares` | agentsmd, summarization, skill, plantask, dynamic tool search, reduction, and patchtoolcalls |
| `eino/compose` | ADK tool-node configuration plus interrupt/resume context for interactive tool permissions |

Provider delegation status:

| Provider | Basis | eino-ext component |
|---|---|---|
| openai | eino-ext (chat) + own Responses API transport | `model/openai` v0.1.13 |
| anthropic | eino-ext (official anthropic-sdk-go underneath) | `model/claude` v0.1.22 |
| gemini | eino-ext with injected `genai.Client` | `model/gemini` v0.1.33 |
| ollama | eino-ext | `model/ollama` v0.1.9 |
| openrouter | eino-ext | `model/openrouter` v0.1.10 |
| qwen | eino-ext (DashScope OpenAI-compatible mode) | `model/qwen` v0.1.9 |
| deepseek | eino-ext (deepseek-go underneath) | `model/deepseek` v0.1.7 |
| zhipu | eino-ext openai component (no zhipu/glm component exists) | — |
| codex | self-implemented (ChatGPT Codex backend: non-standard auth and Responses endpoint) | — |
| claudecode | self-implemented on `anthropicbase` (Claude Code fingerprint headers, beta flags, OAuth/CLI-auth token flows) | — |

deepseek uses its dedicated component so DeepSeek-specific thinking and
`reasoning_content` behavior stays owned by the maintained integration. The
gateway retains provider-level compatibility policy and request extra-field
forwarding around that component.

## 3. Reuse Principles

1. Delegate wire protocols (request building, SSE decoding, tool/usage
   mapping) to eino-ext components backed by official SDKs whenever one
   exists and is actively maintained.
2. Keep gateway-differentiating logic in this repository: credential
   scheduling and per-request credential override, CLI-auth token flows,
   client fingerprint/compat shims (claudecode, codex, `compact: cc`),
   route policies, VirtualKey, config store, Admin APIs, and MCP/ACP
   protocol serving.
3. Respect the agents-control-plane positioning: the gateway is an external
   control plane first. Hosting in-process agents through eino ADK (the
   `builtin` runtime, §5) is an implemented positioning extension; its
   authoritative runtime design lives in
   `docs/design/builtin-agent-runtime.md`.
4. Do not build on Beta eino surfaces for shipping paths. Track them and
   migrate when they graduate (see §6).

## 4. Directly Reusable Now (No Architecture Change)

### 4.1 `eino/callbacks` as an observability tap

Status: implemented. `internal/observability/einotap` registers the global
handler (both `agw` app provision and the standalone server call
`einotap.Register()`, guarded by a `sync.Once`); `cached_tokens` and
`reasoning_tokens` are captured end to end (extension → event → SQLite columns
with additive migration → summary/timeseries/breakdown); `codex` and
`claudecode` fire the callback aspect functions through the
`provider.OnChat*` helpers in `pkg/llm/provider/callbackaspects.go`.

Every eino-ext chat model already invokes `callbacks.OnStart` / `OnEnd` /
`OnError` / `OnEndWithStreamOutput` internally, carrying model name, token
usage, and timing — including visibility into component-internal behavior
the dispatcher cannot see from outside (e.g. timing across SDK-level
retries). Registering a global callbacks handler and forwarding events into
`internal/observability/usage` gives component-level call detail for free,
complementing the existing dispatcher-level spans. This is the highest
value-to-effort reuse item.

Verified against v0.9.12: global handlers fire on standalone component
calls too — `callbacks.EnsureRunInfo` initializes the callback manager from
`GlobalHandlers` when the context carries none, and every eino-ext chat
model calls it at the top of `Generate`/`Stream`. The handler receives the
same context the gateway passed to the provider, so
`usage.SpanFromContext(ctx)` resolves the current interaction span
directly; no new correlation mechanism is needed.

The main new signal is token detail the gateway previously dropped: cached
prompt tokens (`PromptTokenDetails.CachedTokens`) and reasoning tokens
(`CompletionTokensDetails.ReasoningTokens`). These now flow through
`usage.LLMExtension`, the LLM usage event, and the `cached_tokens` /
`reasoning_tokens` SQLite columns, from every capture point: the
non-streaming path via `provider.UsageFromMessage` in `RoutedProvider`, the
streaming paths via the protocol handlers, and component-internal calls via
the tap. The anthropic wire layers (`anthropicbase`, used by `claudecode`)
were aligned with the eino-ext claude accounting in the process: prompt
tokens are input + cache read + cache creation, with the cache-read subset
broken out as `cached_tokens`.

Design decisions as implemented:

- No stream timings, no race: `schema.TokenUsage` on `ResponseMeta.Usage`
  already carries the cached/reasoning detail on the primary stream (the
  acl/openai, claude, and gemini components all populate it), so protocol
  handlers capture streaming token detail synchronously before the span
  finishes. The tap's `TimingChecker` requests `OnEnd` only — the framework
  then never copies streams for it, and the once-feared late-usage race
  against `span.Finish` is eliminated structurally instead of synchronized
  around.
- Merge, never emit: the handler folds detail into the current span via
  `SpanFromContext(ctx).SetExtension(...)`. It never enqueues a second
  `LLMUsageEvent`, which would double-count every request in metrics and
  Prometheus counters.
- Stateless registration under Caddy reload: `AppendGlobalHandlers` is
  process-global, not thread-safe, and has no unregister, while a Caddy
  config reload re-runs app provision in the same process. `Register` is
  guarded by a `sync.Once`, and the handler holds no app reference at all —
  it resolves the span (which carries its own sink) from the request
  context.

Closing the coverage gap: the callback aspect functions are public API
intended for component implementers — eino-ext components invoke exactly
these. `codex` and `claudecode` call them in their own `Chat`/`StreamChat`
through the shared `provider.OnChatStart/OnChatEnd/OnChatError/
OnChatStreamEnd` helpers, so the tap and any vendor handler (§4.4) see all
chat providers uniformly. This is instrumentation in place — the wire layer
stays self-implemented; do not rewrite these providers as eino-ext-style
components, since their value is exactly the gateway-specific auth and
fingerprint behavior that no upstream component would carry. The raw
Responses passthrough (`openaibase.DoCreateResponses`/`DoStreamResponses`,
serving the `/v1/responses` protocol surface) is not a chat-model call and
stays dispatcher-metered; codex's chat path over that transport is
instrumented.

Trace positioning (context for the constraints above): the gateway keeps
one flat interaction event per request; `trace_id` / `span_id` /
`parent_span_id` / `agent_depth` are correlation dimensions on usage
events, not a span-tree subsystem. The tree view is delegated to external
tracing systems: `internal/observability/otelexport` (behind the
`pipeline.OpenTelemetrySink` seam) exports every usage event as one OTel
span carrying the event's own ids, so an OTLP collector reconstructs the
recorded span tree without the gateway owning a trace store. Detail finer
than the event grain (per provider attempt, per SDK retry) is still not
spanned; if ever needed, extend the exporter rather than the event
tables.

### 4.2 `schema.ConcatMessages` for stream aggregation

Status: adopt in new code as the need arises.

The official stream-chunk merge function used inside eino components. Any
gateway code that aggregates a streamed response into one final message
(e.g. persisting full content for interaction events) must use it instead
of hand-written merging — it correctly handles incremental tool-call
argument concatenation, `ReasoningContent`, and `ResponseMeta` merging.

### 4.3 ADK model retry / failover

Status: retry adopted for builtin nodes; failover not adopted.

In v0.9.12 both ship as `ChatModelAgentConfig` fields (`ModelRetryConfig` /
`ModelFailoverConfig`, also exposed by the `deep` prebuilt) rather than the
standalone wrappers of v0.9.7. The builtin definition's `model.retry` block
(`max_retries`) maps onto `ModelRetryConfig` with an `IsRetryAble` mirroring
`RoutedProvider.classifyFailure` (429/5xx only), so a node-level retry
re-runs the whole routed call — credential scheduling and candidate fallback
included — after the gateway's own fallback is exhausted. The planexecute
prebuilt exposes no retry seam, so `model.retry` is rejected there
(validation plus a materialization backstop for the inherited path) instead
of silently dropped.

Failover stays out: the gateway already has model-level failover:
logical-model routes advance to the next untried
`(provider_id, upstream_model)` candidate within the same request
(`executeWithFallback` in `pkg/gateway/routedprovider.go`), integrated with
credential scheduling, failure classification (4xx does not trigger a
switch), and per-attempt usage attribution. `ModelFailoverConfig` is a
static model list that knows none of that, so it is not a replacement and
stays unadopted.

### 4.4 eino-ext callbacks handlers for platform export

Status: the gateway-side OTel exit is implemented — usage events export as
OTLP spans through `internal/observability/otelexport` behind
`pipeline.OpenTelemetrySink`, enabled by the `metrics.otlp` config block.
The callbacks→OTLP component tap is also implemented (`metrics.otlp`
`components` toggle, off by default): a process-global callbacks handler
exports one client-kind span per eino chat-model component call, parented
under the interaction span resolved from the request context, so
component-internal detail (call setup, per-call token usage, errors) nests
inside the same trace tree. Registration follows the einotap pattern —
handler registered once, the exporter behind it swapped atomically on
config reload. For streaming calls the span ends when the component returns
the stream (the drain duration lives on the interaction span); the handler
closes its stream copy unread. Vendor callbacks handlers remain optional.

eino-ext ships ready-made callbacks handlers as standalone modules under
`eino-ext/callbacks/`: `apmplus`, `cozeloop`, `langfuse`, `langsmith`.
When LLM-call export to such a platform is wanted, register the vendor
handler globally instead of writing an exporter; global handlers are a
list, so it composes with the gateway's own tap handler (§4.1). Exported
coverage follows §4.1: full once the self-implemented providers are
instrumented, eino-backed providers only until then.

There is no vendor lock-in to these platforms; the OTel route is open:

- The `apmplus` handler is itself a standard OpenTelemetry implementation:
  it builds on the shared `eino-ext/libs/acl/opentelemetry` library and the
  otel-go SDK with OTLP gRPC exporters, merely defaulting to Volcengine's
  ingestion endpoint.
- `callbacks.Handler` is an open five-method interface. A custom handler
  bridging to the otel-go SDK (span per OnStart/OnEnd/OnError, OTLP
  export) is on the order of a hundred lines and reaches any
  OTel-compatible backend: Jaeger, Tempo/Grafana, SigNoz, Datadog, or a
  self-hosted collector. Langfuse also ingests OTLP natively, so even a
  Langfuse target does not require its proprietary handler.
- On the gateway side, `pipeline.OpenTelemetrySink` is the vendor-neutral
  usage-event exit, implemented by `internal/observability/otelexport`:
  each usage event becomes one OTel span reusing the event's own W3C ids
  (the events are correlation-complete, so no live tracer is needed),
  batched by a `BatchSpanProcessor` and shipped over OTLP gRPC or HTTP.
  Asynchronous builtin HITL resumes additionally carry the stable logical
  `run_id` and reconstruct a Span Link to the checkpoint-producing span.

Default recommendation when export is requested: prefer the OTel route
(custom handler or `libs/acl/opentelemetry`) → collector → backend of
choice. This converges with the trace positioning in §4.1: flat gateway
events for metering, OTel for fine-grained tracing, standard protocols at
every exit.

## 5. ADK Reuse for the Builtin Runtime

Status: implemented through PB1 and PB1b.

This document owns only the framework-adoption decision: the builtin runtime
uses eino ADK as its orchestration engine and does not reimplement ADK
behavior inside the gateway. The authoritative builtin schema, lifecycle,
protocol, permissions, cancellation, implementation track, and deferred work
live in [Builtin Agent Runtime](builtin-agent-runtime.md).

Adopted ADK capabilities:

- `adk.Runner` for event-stream execution;
- `adk.ChatModelAgent`, sequential/parallel/loop workflows, and
  `adk/prebuilt` supervisor, planexecute, and deep topologies;
- Runner checkpoint interrupt/resume for interactive MCP tool permissions;
- `adk.WithCancel` and `CancelMode` for operator force/graceful turn
  cancellation;
- `adk/middlewares` agentsmd, summarization, skill, plantask, dynamic tool
  search, reduction, and patchtoolcalls;
- `compose` only where ADK exposes tool-node configuration and
  interrupt/resume context through it.

The builtin runtime deliberately prefers ADK over lower-level compose graphs
and the legacy `flow/agent` packages. The gateway supplies one generic host:
a builtin agent is persisted data interpreted by compiled-in capability, not a
new executable or a per-agent Go program. Custom Go behavior uses the
compiled-in builtin factory SPI described in the runtime design.

Two neutral bridge libraries connect gateway resources to ADK:

- `pkg/llm/provider/einomodel` presents a gateway
  `provider.Provider`—preferably a `RoutedProvider`—as an eino
  `model.ToolCallingChatModel`, preserving credential scheduling, route
  candidate fallback, and usage attribution.
- `pkg/mcp/einotool` presents gateway-managed MCP service tools as eino
  `InvokableTool` values. Name selection is fail-closed: a missing selected
  tool is a materialization error.

Externally built eino/ADK agents are outside this track. They consume gateway
LLM and MCP routes as clients and fit the control plane's `http` runtime.


## 6. Deferred: Track Until Stable

### 6.1 AgenticMessage / AgenticModel (Beta)

`schema.AgenticMessage` (ordered content blocks: reasoning with signatures,
server tools, MCP tool calls with approval flow, multimodal) plus provider
extension packages `schema/openai`, `schema/claude`, `schema/gemini`, and
the eino-ext `agenticopenai` / `agenticclaude` / `agenticgemini`
components. `agenticopenai` supports the OpenAI Responses API on the
official openai-go/v3 SDK.

This is the correct future replacement for the gateway's largest
self-maintained protocol code: the hand-written Responses API transport in
`openaibase` and the chat↔Responses bridge in
`pkg/llm/provider/responses_compat.go`. Migrate the openai (and possibly
codex) Responses paths once the AgenticModel interface and the agentic*
components leave Beta. Until then the classic `model/openai` component is
Chat-Completions-only and the self-implemented Responses transport stays.

### 6.2 v0.10 alpha features

Runner-managed session persistence, auto-memory middleware, and permission
gates are previewed in v0.10 alphas and are not in v0.9.12. Do not depend
on them yet. The auto-memory middleware overlaps with this project's
reserved `/admin/memory` surface (501 in v0.4.x); when v0.10 stabilizes,
evaluate "gateway memory = ADK memory middleware + gateway-owned storage
and Admin API" before designing a separate memory engine.

## 7. Non-Goals

Not adopted from eino, by design: VirtualKey and credential scheduling,
config store, Admin APIs, MCP/ACP protocol serving, client-compat shims
(claudecode fingerprints, codex backend auth), route matching and policy.
Eino is a framework for building agents; these are the gateway's
infrastructure surfaces.

## 8. Adoption Sequence

1. ~~Wire `eino/callbacks` into the observability pipeline, including
   instrumenting the self-implemented providers with the callback aspect
   functions so coverage is uniform (§4.1).~~ Done.
2. Use `schema.ConcatMessages` for any new stream-aggregation code (§4.2).
   The builtin host's turn loop already does.
3. ~~Build the `builtin` agent runtime on ADK (§5).~~ Done (PB1): bridges,
   generic ADK host, definition schema, turn ingress, and management parity
   all landed; see `builtin-agent-runtime.md` §11 for the scope notes and
   the PB2 remainder (task backend, durable sessions after eino v0.10).
4. Migrate openai/codex Responses paths to agentic* components after they
   graduate from Beta (§6.1).
5. ~~Wire the gateway-side OTel exit (§4.4).~~ Done: usage events export
   as OTLP spans via `internal/observability/otelexport` when
   `metrics.otlp.endpoint` is configured. Vendor callbacks handlers stay
   on-request for platforms that need component-level detail beyond the
   usage-event grain.
6. ~~ADK model retry for builtin nodes (§4.3).~~ Done: the definition's
   `model.retry` block; failover stays unadopted by design.
7. ~~ADK Runner cancel for operator turn cancellation (§5).~~ Done:
   `adk.WithCancel`/`CancelMode` behind the Admin API force/graceful cancel of
   in-flight builtin turns (`builtin-agent-runtime.md` §10).

## 9. Known Integration Gotchas

Lessons from the migrations already done; they apply to any future
component adoption:

- `acl/openai`'s `WithExtraFields` replaces the whole extra-fields map
  instead of merging. Component options that internally append their own
  `WithExtraFields` (e.g. `einoqwen.WithEnableThinking`) silently drop any
  extra fields the gateway set. Merge everything into one map at the
  provider layer and pass a single `WithExtraFields` option.
- eino-ext components do not normalize SDK errors. Convert SDK API errors
  into `provider.UpstreamError` at the provider layer (see
  `wrapProviderError` in `pkg/llm/provider/anthropic`), otherwise upstream
  4xx statuses collapse into retried 502s.
- Components do not enforce provider-specific request constraints. The
  gateway keeps them: Anthropic thinking-budget clamping
  (`anthropicbase.ClampThinkingBudget`), sampling-parameter suppression
  under extended thinking, and metadata injection via the SDK's JSON
  request patches (`AdditionalRequestFields`, sjson path semantics).
- Version floors: eino-ext `model/claude` >= v0.1.19 requires eino >=
  v0.9.1; all agentic* components require eino v0.9.x. Keep core eino on
  the latest stable v0.9 line.
- Tool JSON schemas pass through some components lossily (the claude
  component keeps only `properties`/`required` of the top-level schema).
  Acceptable for typical function tools; check when a tool relies on other
  top-level schema keywords.
