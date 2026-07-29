# Observability Design

## 1. Scope

This document describes the current design for unified audit logging and usage metrics for `agent-gateway`.

It covers reliable capture, persistence, and query of request-level events and aggregated usage statistics for the gateway traffic implemented today:

- LLM routes through `agent_route_dispatcher.llm_apis.openai`, `agent_route_dispatcher.llm_apis.anthropic`, and `agent_route_dispatcher.llm_apis.cc`
- MCP routes through `agent_route_dispatcher` with MCP enabled, `pkg/dispatcher/mcp_handler.go`, `pkg/mcp/service`, and `pkg/mcp/runtime`
- ACP and builtin execution through unified AgentRoutes with Agent dispatch
  enabled, `pkg/dispatcher/agent_handler.go`, and the runtime selected by the
  target Agent

Future protocol families such as A2A should follow the same shared interaction model, but they are not part of the implementation baseline.

This allows operators and agent builders to:

- audit what LLM tools agents declared and called
- audit which MCP tools, resources, prompts, and completions were invoked
- audit ACP turns, permission resolutions, session listing, and transcript loads
- measure token consumption, request volume, latency, and error rates
- reconstruct multi-agent call chains through trace and span correlation
- govern agent behavior by inspecting depth, frequency, and error patterns across agent chains

This document describes the metrics area implemented by `GET /admin/metrics` and the related per-protocol metrics endpoints. MCP tool policy is a separate concern described in `mcp-tool-policy.md`.

The current implementation baseline is:

- the Caddy middleware module is `http.handlers.agent_route_dispatcher` in `caddy/dispatcher/`
- runtime dispatch is in `pkg/dispatcher/handler.go`
- all protocols first resolve a shared `routecore.AgentRouteConfig` through `AgentGateway.Match`
- `Handler.Dispatch` resolves and validates the VirtualKey before dispatching by `routecore.RouteKind`
- LLM dispatch resolves `pkg/gateway/llmroute.LLMRoute` and calls the selected LLM API handler
- MCP dispatch resolves `pkg/gateway/mcproute.MCPRoute` and calls `pkg/mcp/service`
- Agent dispatch resolves `pkg/gateway/agentroute.AgentRoute`, stamps the
  target Agent directly, and selects ACP or builtin typed storage from the
  resolved `runtime.type`
- MCP runtime inspection uses the in-memory registry in `pkg/mcp/runtime/registry.go`
- ACP runtime inspection uses `pkg/acp/runtime.Manager`
- the persisted config backend is SQLite through `pkg/configstore/sqlite`
- Admin API handlers live under `pkg/admin`

## 2. Goals

- Capture one structured event per completed LLM request, including request-side tool metadata and response-side tool call summary when available.
- Capture one structured event per completed MCP JSON-RPC request.
- Capture one structured event per completed ACP Agent operation.
- Carry agent chain identity (`trace_id`, `span_id`, `parent_span_id`, `agent_depth`) on every persisted event from phase 1.
- Persist events durably to SQLite so history survives restarts.
- Expose useful summaries and recent-event inspection through the Admin API, including a unified cross-protocol view.
- Support aggregate queries for token and request volume trends.
- Keep the request critical path impact minimal.

## 3. Non-Goals

This design does not attempt to:

- store raw LLM prompt text or response text
- store ACP turn input, transcript text, or agent SSE content
- store raw MCP tool arguments by default
- replace provider-native billing systems
- introduce an external observability stack dependency
- implement real-time push or webhook delivery of events
- replace the existing MCP and ACP runtime inspection endpoints

## 4. Capability Overview

### 4.1 LLM Observability

Every completed LLM request produces one persisted usage event containing:

- agent chain dimensions: trace ID, span ID, parent span ID, agent depth
- routing dimensions: route, provider, virtual key, logical model, upstream model, credential
- request shape: LLM API handler, API operation, streaming flag, request-side declared tools summary
- execution outcome: success, error type, status code, latency
- token usage: input tokens, output tokens, total tokens, finalization flag
- tool call summary: count of tool calls and list of tool names invoked in the response when available

LLM observability covers the current LLM ingress handlers:

- OpenAI-compatible chat completions
- OpenAI-compatible Responses API
- Anthropic messages
- Claude Code CLI-compatible Anthropic messages profile (`cc`)

LLM request-side tools refer to tool definitions declared by the client in the incoming request payload. LLM response-side tool calls refer to function calls embedded in model responses (`tool_calls` in OpenAI chat format, output tool call items in Responses API, and `tool_use` blocks in Anthropic-compatible wire formats when available). They are distinct from MCP tool invocations.

### 4.2 MCP Observability

Every completed MCP JSON-RPC request produces one persisted usage event containing:

- agent chain dimensions: trace ID, span ID, parent span ID, agent depth
- routing dimensions: route, service, virtual key
- operation: method, request ID, tool name, resource URI, prompt name, or completion reference where applicable
- execution outcome: result status, error type, cancelled flag, latency
- policy attribution: presented tool name, executed tool name, execution mode, policy action when MCP tool policy is implemented
- argument metadata: argument count only; full argument capture is opt-in per service

The existing in-memory ring buffer in `pkg/mcp/runtime/registry.go` remains a fast real-time inspection surface for in-flight requests, progress, and bounded completed history. Durable audit persistence is owned by the metrics usage event pipeline, not by `CompletedRequest`.

### 4.3 ACP Observability

Every completed ACP Agent operation produces one persisted usage event containing:

- agent chain dimensions: trace ID, span ID, parent span ID, agent depth
- routing dimensions: Agent route, Agent, virtual key
- operation: `turn`, `permission`, `sessions`, or `transcript`
- ACP dimensions: thread ID, session ID when present, adapter type from the
  Agent-owned runtime config, and permission request ID when applicable
- execution outcome: success, error type, status code, latency
- turn summary: emitted event counts by event name and final usage snapshot when available

ACP event payloads must not store turn input, deltas, content, reasoning text, transcript text, raw permission params, or other agent output content.

## 5. Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                        Inbound HTTP                              │
│               http.handlers.agent_route_dispatcher               │
│        extract trace context; match route; validate VirtualKey   │
└────────────────────────┬─────────────────────────────────────────┘
                         │
        ┌────────────────┼────────────────┐
        │                │                │
        ▼                ▼                ▼
  LLM dispatch      MCP dispatch     Agent dispatch
 pkg/dispatcher/   pkg/dispatcher/   pkg/dispatcher/
   llmapi/...       mcp_handler.go    agent_handler.go
        │                │                │
        ▼                ▼                ▼
 pkg/gateway/      pkg/mcp/service   pkg/acp/runtime
 RoutedProvider
        │                │                │
        └────────────────┼────────────────┘
                         ▼
┌──────────────────────────────────────────────────────┐
│              internal/observability/usage             │
│   InteractionObserver / InteractionSpan interfaces    │
│   InteractionEvent base + LLM/MCP/ACP event types     │
│   no-op and pipeline-backed implementations           │
└──────────────────────────────────────────────────────┘
                         │
                         ▼
┌──────────────────────────────────────────────────────┐
│            internal/observability/pipeline            │
│   EventPipeline: buffered channel + fan-out           │
│   SQLiteSink / PrometheusSink / [future: OTel, webhook]│
└──────────────────────────────────────────────────────┘
                         │
                         ▼
┌──────────────────────────────────────────────────────┐
│               pkg/configstore/sqlite                  │
│   llm_usage_events table                             │
│   mcp_usage_events table                             │
│   acp_usage_events table                             │
│   event tables queried directly for aggregates        │
└──────────────────────────────────────────────────────┘
                         │
                         ▼
┌──────────────────────────────────────────────────────┐
│                    pkg/admin                          │
│   GET /admin/metrics                                 │
│   GET /admin/metrics/interactions                    │
│   GET /admin/metrics/llm/...                         │
│   GET /admin/metrics/mcp/...                         │
│   GET /admin/metrics/acp/...                         │
└──────────────────────────────────────────────────────┘
```

### 5.1 Unified Event Model

All protocol-specific events embed a shared `InteractionEvent` base type. This base captures dimensions common to every gateway interaction regardless of protocol, enabling cross-protocol analytics and consistent governance queries.

```go
// InteractionEvent is the common base for all gateway interaction records.
type InteractionEvent struct {
    EventID      string    // globally unique event identifier
    TraceID      string    // caller-supplied or gateway-generated trace correlation ID
    SpanID       string    // unique span identifier for this gateway operation
    ParentSpanID string    // caller's span ID; empty for direct callers
    AgentDepth   int       // agent call chain depth; 0 for direct callers
    StartedAt    time.Time
    FinishedAt   time.Time
    RouteID      string
    RouteKind    string    // llm | mcp | agent
    RouteProtocol string   // openai | anthropic | cc | mcp | agent
    VirtualKeyID string
    Success      bool
    StatusCode   int
    ErrorType    string
    LatencyMS    int64
}
```

`LLMUsageEvent`, `MCPUsageEvent`, and `ACPUsageEvent` embed `InteractionEvent` and add protocol-specific fields. Future route kinds follow the same embedding pattern without modifying the shared base.

The `InteractionObserver` interface is the single call-site interface used by dispatchers:

```go
type InteractionSpan interface {
    SetExtension(v any)
    AddAnnotation(key, value string)
    Finish(outcome InteractionOutcome)
}

type InteractionObserver interface {
    Begin(ctx context.Context, dims InteractionDimensions) (InteractionSpan, context.Context)
}
```

The no-op implementation satisfies this interface for tests and unconfigured deployments. The pipeline-backed implementation enqueues events asynchronously.

### 5.2 Agent Identity And Call Chain Tracing

The gateway extracts agent chain context from inbound HTTP headers on every matched gateway request. OpenTelemetry and other standards-based clients should propagate W3C Trace Context headers; the gateway accepts `X-*` headers as a compatibility path for callers that are not yet using W3C propagation.

| Header | Stored field | Purpose |
|--------|-------------|---------|
| `traceparent` / `tracestate` | `trace_id`, `parent_span_id` | Preferred W3C Trace Context carrier |
| `X-Trace-ID` | `trace_id` | Compatibility carrier for a W3C-shaped trace id |
| `X-Span-ID` | `parent_span_id` | Compatibility carrier for a W3C-shaped caller span id |
| `X-Agent-Depth` | `agent_depth` | Hop count from the originating caller |

Request extraction precedence is:

1. when a `traceparent` header is present, parse it, along with `tracestate`
2. when no `traceparent` header is present, fall back to `X-Trace-ID` and `X-Span-ID`
3. otherwise generate a new trace context locally

Every inbound id is validated against the W3C shape — lowercase hex of the
required length (32 for a trace id, 16 for a span id) and not all zeroes — and
rejected rather than normalized. This applies to the `X-*` compatibility headers
too: they carry the same ids in the same format as `traceparent`, they are just
a different envelope, so an `X-Trace-ID` that is a UUID or a free-form
correlation string is **discarded**, not stored. The gateway echoes `trace_id`
back in its own `traceparent` response header and writes it to the `trace_id`
usage column, and neither may carry an id that no W3C-speaking consumer can
parse.

A malformed inbound trace context is never silently repaired. A `traceparent`
that fails validation starts a fresh trace and suppresses the `X-*` fallback
entirely: the caller declared W3C propagation, and mixing in an `X-*` id it did
not intend as an alternative would fabricate a parent link that does not exist.
Rejecting the whole context also drops `tracestate` and leaves `parent_span_id`
empty. Trace flags are honored: an inbound unsampled traceparent (flags `00`)
is echoed as unsampled rather than upgraded to sampled.

The gateway always generates a new `span_id` for the current operation. The gateway emits `traceparent` and `tracestate` response headers, and may also emit `X-Trace-ID`, `X-Span-ID`, and `X-Agent-Depth` as compatibility headers. `X-Agent-Depth` is returned as `agent_depth + 1`.

Agent depth enforcement is implemented as an optional policy gate through the
`metrics` Caddyfile block and `agwd --max-agent-depth`. A value of `0` disables
the gate.

### 5.3 Event Pipeline

The `InteractionObserver` interface used at call sites does not write to storage directly. It enqueues a completed event into a buffered in-process channel. A background `EventPipeline` goroutine reads from this channel and fans out to registered sinks.

```
InteractionObserver  ->  buffered channel  ->  EventPipeline
                                                   ├── SQLiteSink
                                                   ├── PrometheusSink
                                                   ├── [future] OpenTelemetrySink
                                                   └── [future] WebhookSink
```

Properties:

- request-handling goroutines enqueue and return immediately; SQLite I/O is not on the request critical path
- when the channel is full, enqueue drops the event and increments a `dropped_events` counter instead of blocking the caller
- additional export sinks do not require changing dispatcher call sites
- clean shutdown drains the queue before stopping sinks

The `UsageService` is provisioned by `caddy/gateway/app.go` alongside the other shared runtime services. It owns the `EventPipeline` lifecycle and exposes the `InteractionObserver` to runtime dispatchers and metrics query helpers to the Admin API.

### 5.4 OpenTelemetry Compatibility

This design keeps a gateway-specific internal observability model and Admin API. It does not attempt to make SQLite tables or Admin API payloads identical to OpenTelemetry's wire model. Instead, the internal model must be mappable to OpenTelemetry traces, metrics, and logs so an `OpenTelemetrySink` can export the same interaction data without lossy translation.

Compatibility rules:

- `traceparent` and `tracestate` are the canonical inbound and outbound trace context carriers
- stored `trace_id`, `span_id`, and `parent_span_id` fields must be directly representable as OpenTelemetry span identities
- request latency exports as histograms
- request and operation volume exports as counters
- token usage exports as monotonic counters
- high-cardinality identifiers such as route IDs, virtual key IDs, and credential IDs must be configurable or sampled before export when necessary
- gateway-specific fields such as `virtual_key_id`, `agent_depth`, `presented_tool_name`, and ACP thread IDs export under a gateway-owned namespace such as `agw.*`

Because GenAI, MCP, and ACP telemetry conventions may evolve, the internal SQLite schema and Admin API should not be renamed solely to mirror provisional OpenTelemetry field names. Exporter layers own translation to external conventions.

## 6. LLM Observability

### 6.1 Event Model

Each completed LLM request produces one `LLMUsageEvent`:

```
-- InteractionEvent base --
event_id
trace_id
span_id
parent_span_id
agent_depth
started_at
finished_at
route_id
route_kind          llm
route_protocol      openai | anthropic | cc
virtual_key_id
success
status_code
error_type
latency_ms

-- LLM extension --
llm_api             openai | anthropic | cc
api_operation       chat_completions | messages | responses
provider_id
provider_type
logical_model       client-facing model name in model-target routes
upstream_model      concrete provider-facing model actually called
credential_source   static | cliauth | managed | none
credential_id
stream              bool
input_tokens
output_tokens
total_tokens
cached_tokens       cache-served subset of input_tokens; 0 when unreported
reasoning_tokens    reasoning subset of output_tokens; 0 when unreported
usage_finalized     false if token counts are incomplete
request_tool_count
request_tool_names  JSON array, truncated if necessary
tool_call_count
tool_names          JSON array, truncated if necessary
```

`request_tool_count` and `request_tool_names` capture the tool definitions the client exposed to the model for that request. `tool_call_count` and `tool_names` capture the model's outbound tool invocations embedded in the response. These are not MCP calls.

### 6.2 Capture Points

`pkg/dispatcher/handler.go` initializes the interaction after route match and VirtualKey validation. At that point the stable dimensions are known: route ID, route kind, route protocol, and virtual key ID. For LLM routes, the protocol-specific handler name becomes `llm_api`.

The LLM API handler captures request-side wire metadata before provider execution:

- `pkg/dispatcher/llmapi/openai/handler.go` inspects chat completions and Responses request tools
- `pkg/dispatcher/llmapi/anthropic/handler.go` inspects Anthropic messages request tools when present
- `pkg/dispatcher/llmapi/cc/handler.go` captures the Claude Code-compatible profile as `llm_api=cc`

`pkg/gateway.RoutedProvider` is the provider execution boundary. It owns route target resolution, provider selection, upstream model rewrite, credential scheduling, and provider `Chat` or `StreamChat` execution. It records provider dimensions, latency, token counts (including the `cached_tokens`/`reasoning_tokens` breakdown), and upstream failures.

Protocol handlers capture response-side tool calls before rendering final HTTP JSON or while translating stream completion metadata. Streaming events should finish the interaction when the downstream stream completes or fails.

`internal/observability/einotap` is a supplementary capture point: a
process-global eino callbacks handler (registered once under a `sync.Once`,
reload-safe) that folds chat-model component detail into the current
interaction span on the synchronous `OnEnd` timing. It merges via
`SetExtension` and never enqueues its own event, so it cannot double-count. It
deliberately skips the stream timings: streaming token detail is read by the
protocol handlers from `ResponseMeta.Usage` on the primary stream before the
span finishes, so no stream copies are taken and no late-usage race against
`span.Finish` exists. The self-implemented providers (`codex`, `claudecode`)
fire the eino callback aspect functions in their own `Chat`/`StreamChat`
(via the `provider.OnChat*` helpers), so eino-backed and self-implemented
providers are observable uniformly.

### 6.3 Error Categories

Normalized LLM `error_type` values:

- `route_not_found`
- `route_disabled`
- `virtual_key_rejected`
- `protocol_not_configured`
- `protocol_validation_failed`
- `provider_not_configured`
- `provider_disabled`
- `credential_unavailable`
- `provider_request_failed`
- `provider_stream_failed`
- `internal_error`

`status_code` is the downstream HTTP status returned by the gateway, not a raw upstream provider status code.

### 6.4 Streaming Behavior

Current behavior for streaming requests:

- record the event when the stream completes or errors
- use available final usage metadata if the provider exposes it in the stream
- set `usage_finalized=false` if final usage is not available
- do not invent token counts

This applies separately to chat/messages streaming and Responses API event streaming.

## 7. MCP Observability

### 7.1 Event Model

Each completed MCP JSON-RPC request produces one `MCPUsageEvent`:

```
-- InteractionEvent base --
event_id
trace_id
span_id
parent_span_id
agent_depth
started_at
finished_at
route_id
route_kind          mcp
route_protocol      mcp
virtual_key_id
success
status_code
error_type
latency_ms

-- MCP extension --
request_id          JSON-RPC request id, not globally unique
service_id
method              initialize | tools/list | tools/call | resources/read | ...
tool_name           populated for tools/call
presented_tool_name client-visible tool name after alias/policy application
executed_tool_name  actual execution target when different from presented name
execution_mode      forwarded_tool | wrapped_tool | synthetic_tool
policy_action       forward | deny | alias | replace
resource_uri        populated for resources/read
prompt_name         populated for prompts/get
completion_ref_type populated for completion/complete
completion_argument populated for completion/complete
arg_count           count only, not argument values
result_status       success | error | cancelled
cancelled           bool
tool_args_json      null unless audit.capture_tool_args is enabled
```

`tool_name` and `arg_count` are captured when the method shape includes them. `tool_args_json` is null by default. The `presented_tool_name`, `executed_tool_name`, `execution_mode`, and `policy_action` columns are created in phase 1 but left null until MCP tool policy is implemented; the tool policy layer populates them through `InteractionSpan.SetExtension` (see `mcp-tool-policy.md`).

The existing `CompletedRequest` struct in `pkg/mcp/runtime/registry.go` remains a runtime inspection record. It may gain small display-oriented fields if needed, but it is not the canonical persisted audit event and should not be consumed by the SQLite sink. The dispatcher constructs `MCPUsageEvent` from the resolved route, virtual key, parsed JSON-RPC request, method-specific params, cancellation state, and upstream outcome.

### 7.2 MCP Error Categories

- `route_not_found`
- `route_disabled`
- `virtual_key_rejected`
- `service_not_found`
- `service_unavailable`
- `protocol_validation_failed`
- `method_not_implemented`
- `tool_not_found`
- `tool_denied`
- `upstream_error`
- `upstream_timeout`
- `cancelled`
- `internal_error`

### 7.3 Verbose Audit Mode

Full argument capture for `tools/call` is off by default. It can be enabled per MCP service:

```json
{
  "id": "fs-service",
  "name": "Filesystem",
  "transport": "stdio",
  "audit": {
    "capture_tool_args": true
  }
}
```

When enabled, `tool_args_json` is populated with the serialized argument map. Operators are responsible for ensuring this complies with their data handling policies, since arguments may contain file paths, content, or other sensitive data.

Result content is never stored regardless of this setting.

## 8. ACP Observability

### 8.1 Event Model

Each completed ACP Agent operation produces one `ACPUsageEvent`:

```
-- InteractionEvent base --
event_id
trace_id
span_id
parent_span_id
agent_depth
started_at
finished_at
route_id
route_kind          agent
route_protocol      agent
virtual_key_id
success
status_code
error_type
latency_ms

-- ACP extension --
agent_id
agent_type          codex | opencode | other registered adapter type
operation           turn | permission | sessions | transcript
thread_id           populated for turn and permission when available
session_id          populated when the route operation addresses a session
permission_request_id populated for permission resolution
fresh_session       copied from turn request when present
event_counts_json   JSON object of SSE event name -> count for turn
usage_json          final usage snapshot if the runtime exposes one
result_status       success | error
```

ACP `turn` requests are SSE operations. The event is recorded when `ServeTurn` returns, the client connection fails, or the request context is cancelled. The event should count emitted SSE event names (`session`, `delta`, `reasoning`, `content`, `plan`, `tool_call`, `usage`, `available_commands`, `session_info`, `mode`, `config_options`, `permission`, `done`, `error`) without storing event payload content.

### 8.2 Planned ACP Token Metrics

Status: not implemented in v0.4.x; deferred to v0.5.x. The current
`acp_usage_events` schema stores the final raw ACP usage payload in bounded
`usage_json` when the runtime exposes one, but it does not expose queryable
context-token columns or Prometheus context-token counters.

ACP token metrics should follow the ACP `usage_update` semantics. The protocol reports
**session context occupancy** (a gauge), not per-model request accounting. This
is the exact payload captured live from `codex-acp` v0.16.0 (one `usage_update`
per turn, verified 2026-06-24 by the prompt smoke; the turn only asked the model
to reply "pong", so `used` is almost entirely the agent's standing context, not
the reply):

```json
// codex-acp v0.16.0
{ "sessionUpdate": "usage_update", "used": 14769, "size": 258400 }

// opencode (adds a nested cost object)
{ "sessionUpdate": "usage_update", "used": 11102, "size": 200000,
  "cost": { "amount": 0, "currency": "USD" } }
```

Neither adapter reports `input_tokens`/`output_tokens` — only the `used`/`size`
gauge. `cost` is agent-specific (present on opencode, absent on codex) and is
not persisted by these metrics; in the captured opencode turn `cost.amount` was
`0`, so it is not a reliable billing source either. The parser extracts only
`used`/`size` and must tolerate extra or nested fields like `cost`. `used` and
`size` are the current-context token counts, parsed per the ACP v1
`session/update` schema (the same schema `pkg/acp/runtime/acpupdate` already
decodes and tests against), and should be stored as `context_used_tokens` and
`context_window_tokens` when this follow-up lands. They must not be treated as LLM-style `input_tokens`,
`output_tokens`, or per-request `total_tokens`.

**Coverage caveat.** The field names `used`/`size` are fixed by the ACP v1
schema, so parsing does not depend on a capture to learn key names. What is
agent-specific is whether an adapter actually emits `usage_update` at all. Both
verified adapters do (live captures 2026-06-24, above): `codex-acp` v0.16.0 emits
exactly one `usage_update` per turn carrying only `used`/`size` — note this is a
different update from `session_info_update`, which codex does *not* emit, so the
missing session *title* does not imply missing *token* data — and `opencode`
emits one per turn too, with the same `used`/`size` plus an extra nested `cost`
object the parser ignores.
An adapter that emits no parseable `usage_update` leaves only the raw
`usage_json` evidence for its turns; document per-adapter emission rather than
assuming uniform coverage when the v0.5.x token columns are added.

**Turn-start replay must be excluded.** At the start of every turn the runtime
replays the cached session metadata, including the last `usage` snapshot, as a
`usage` event (`sessionMetaCache.turnStartEvents` in
`pkg/acp/runtime/instance.go`). A replayed snapshot is indistinguishable from a
fresh `usage_update` at the SSE layer, so counting it would mark a turn that
produced no new usage as `usage_observed=true` and re-stamp a stale
`context_used_tokens` (and a spurious `token_delta`). ACP token metrics must
count only **live** (non-replay) usage updates. This requires a source marker on
the runtime event: add a `Replay bool` (set true in `turnStartEvents`) to
`acpruntime.TurnEvent`, and have the dispatcher's usage parser ignore replayed
`usage` events. Without that marker the metric cannot distinguish a turn's own
usage from the joined-session snapshot.

**Why a gauge cannot be summed as throughput.** `context_used_tokens` rises
during a session and periodically drops on context compaction, truncation,
reload, or `fresh_session`. Summing the gauge, or even summing positive
turn-to-turn deltas, does not equal tokens processed: every compaction turn
contributes nothing and subsequent growth is measured from the lowered baseline,
so any "total" systematically under-counts. ACP token metrics are therefore an
**approximate context-growth signal, not a billable token count**. Real token
billing must come from the LLM event path, not from ACP `usage_update`.

`token_delta` is a best-effort, per-turn positive difference between this turn's
final `context_used_tokens` and the previous turn's value for the same
`agent_id` + `session_id`. It is stamped onto the event **before the event is
enqueued to the pipeline**, by a small in-process per-session last-value tracker
in the usage observer — not computed inside a sink. This is required for
consistency: the SQLite sink and the Prometheus sink each receive a copy of the
same `ACPUsageEvent`, so a delta computed inside one sink would be invisible to
the other. Computing it once upstream lets both sinks read the same stamped
`token_delta`, and avoids a per-turn `SELECT`-before-`INSERT` (and the extra
index and retention-janitor hazards) in the write path.

`token_delta` stays null — and is excluded from any total — when the previous
value is unknown (process restart, first turn, evicted tracker entry), the
current value is missing, the value decreased, or the session identity is
unavailable. Because the tracker keys on the finalized turn event, the "previous
value" is the previous turn as ordered by event finalization, not strict
wall-clock turn start; rapid concurrent turns on one session can therefore order
imprecisely, which is acceptable for an approximate signal but is another reason
not to treat the totals as exact.

In v0.4.x only the raw bounded `usage_json` is retained for inspection. The
planned v0.5.x context-token implementation must parse only live (non-replay)
`usage` SSE events and only the known ACP fields; malformed or unknown usage
payloads must not fail the turn.

Route-scoped `sessions` and `transcript` requests and service-scoped Admin ACP session/transcript requests are separate surfaces. Route-scoped ACP traffic is recorded through `agent_route_dispatcher`; ACP Admin runtime/session/transcript operator calls are recorded as management-plane audit spans with the synthetic route `/admin/acp` and `route_protocol=admin`.

### 8.3 ACP Error Categories

- `route_not_found`
- `route_disabled`
- `virtual_key_rejected`
- `service_not_found`
- `service_disabled`
- `protocol_validation_failed`
- `permission_not_found`
- `invalid_request`
- `upstream_error`
- `client_cancelled`
- `internal_error`

## 9. Admin API

### 9.1 Existing Endpoint Updated

`GET /admin/metrics` returns a real summary response plus pipeline health counters:

```json
{
  "llm": {
    "request_count": 1240,
    "success_count": 1198,
    "failure_count": 42,
    "input_tokens": 3820000,
    "output_tokens": 940000,
    "total_tokens": 4760000,
    "avg_latency_ms": 1842
  },
  "mcp": {
    "request_count": 4870,
    "success_count": 4801,
    "failure_count": 69,
    "tools_call_count": 3210,
    "avg_latency_ms": 320
  },
  "acp": {
    "request_count": 220,
    "turn_count": 180,
    "success_count": 214,
    "failure_count": 6,
    "avg_latency_ms": 8840
  },
  "pipeline": {
    "dropped_events": 0,
    "write_failures": 0
  }
}
```

### 9.2 LLM Metrics Endpoints

`GET /admin/metrics/llm/events`

Returns recent LLM usage events. Supports query parameters:

- `route_id`, `provider_id`, `virtual_key_id`, `logical_model`, `upstream_model`
- `llm_api`, `api_operation`
- `from`, `to` (RFC3339)
- `success` (bool)
- `request_tool_name`
- `has_tool_use` (bool)
- `limit` (default 100, max 1000)

`GET /admin/metrics/llm/timeseries`

Returns bucketed request and token counts. Parameters: `from`, `to`, `bucket` (`minute`, `hour`, or `day`, or a Grafana-style duration such as `3h`/`5m`/`30s`/`1d`; defaults to `hour`), `group_by` (`route_id`, `provider_id`, `virtual_key_id`, `upstream_model`, `llm_api`).

`GET /admin/metrics/llm/breakdown`

Returns grouped totals ranked by request count, token count, or failure count.

### 9.3 MCP Metrics Endpoints

`GET /admin/metrics/mcp/events`

Returns recent MCP usage events. Supports query parameters:

- `route_id`, `service_id`, `virtual_key_id`
- `method`
- `tool_name`, `resource_uri`, `prompt_name`
- `from`, `to`
- `result_status`
- `limit` (default 100, max 1000)

`GET /admin/metrics/mcp/tools/summary`

Returns per-tool aggregated statistics. Supports `from`, `to`, `service_id`, and `route_id` filters.

### 9.4 ACP Metrics Endpoints

`GET /admin/metrics/acp/events`

Returns recent ACP usage events. Supports query parameters:

- `route_id`, `virtual_key_id`
- `agent_type`
- `operation` (`turn`, `permission`, `sessions`, `transcript`)
- `thread_id`, `session_id`
- `from`, `to`
- `success` (bool)
- `limit` (default 100, max 1000)

`GET /admin/metrics/acp/summary`

Returns ACP totals grouped by `route_id`, `agent_type`, or
`operation`. Planned ACP context-token fields such as `context_growth_tokens`
and `latest_context_used_tokens` are not part of the current response.

### 9.5 Unified Cross-Protocol Interactions Endpoint

`GET /admin/metrics/interactions`

Returns recent interaction events across all protocol families, backed by the shared `InteractionEvent` base fields. Supports query parameters:

- `route_kind` (`llm`, `mcp`, `agent`)
- `route_protocol`
- `route_id`, `virtual_key_id`
- `trace_id`
- `parent_span_id`
- `agent_depth`
- `service_id` (MCP only), `session_id`
- `from`, `to` (RFC3339)
- `success` (bool)
- `limit` (default 100, max 1000)

Beyond the base fields, the projection surfaces each protocol's labeling column
so a consumer can name a span by what it did rather than falling back to
`route_id`: `upstream_model` (LLM), `tool_name` (MCP), `operation` (ACP), plus
MCP `service_id` and runtime `session_id`. LLM rows also carry the model tool-use and token
fields `request_tool_count`, `request_tool_names`, `tool_call_count`,
`tool_names`, `input_tokens`, `output_tokens`, `total_tokens`,
`cached_tokens`, and `reasoning_tokens`. Columns a row
does not own are `null` (for example, `tool_name` is `null` on LLM and ACP rows,
and token fields are `null` on MCP and ACP rows). Management-plane ACP admin
audit spans carry the synthetic `route_id` `/admin/acp` and `route_protocol`
`admin`; filter on `route_protocol` to separate them from data-plane traffic.

`GET /admin/metrics/interactions/summary`

Returns aggregated totals grouped by `route_kind`, `route_protocol`, `route_id`, or `virtual_key_id`.

## 10. Storage Schema

The usage storage package is introduced as a new concern within the existing SQLite configstore. It does not reuse the generic JSON config stores used for providers, routes, services, credentials, virtual keys, and managed models. It uses typed tables suited for time-series and aggregation queries.

### 10.1 LLM Usage Events Table

Table: `llm_usage_events`

```sql
CREATE TABLE llm_usage_events (
    event_id           TEXT PRIMARY KEY,
    trace_id           TEXT,
    span_id            TEXT NOT NULL,
    parent_span_id     TEXT,
    agent_depth        INTEGER NOT NULL DEFAULT 0,
    started_at         INTEGER NOT NULL,
    finished_at        INTEGER NOT NULL,
    route_id           TEXT,
    route_kind         TEXT NOT NULL DEFAULT 'llm',
    route_protocol     TEXT,
    virtual_key_id     TEXT,
    success            INTEGER NOT NULL DEFAULT 0,
    status_code        INTEGER,
    error_type         TEXT,
    latency_ms         INTEGER,
    request_id         TEXT UNIQUE,
    llm_api            TEXT,
    api_operation      TEXT,
    provider_id        TEXT,
    provider_type      TEXT,
    logical_model      TEXT,
    upstream_model     TEXT,
    credential_source  TEXT,
    credential_id      TEXT,
    stream             INTEGER NOT NULL DEFAULT 0,
    input_tokens       INTEGER,
    output_tokens      INTEGER,
    total_tokens       INTEGER,
    cached_tokens      INTEGER,
    reasoning_tokens   INTEGER,
    usage_finalized    INTEGER NOT NULL,
    request_tool_count INTEGER NOT NULL DEFAULT 0,
    request_tool_names TEXT,
    tool_call_count    INTEGER NOT NULL DEFAULT 0,
    tool_names         TEXT
);

CREATE INDEX idx_llm_events_started ON llm_usage_events (started_at);
CREATE INDEX idx_llm_events_route ON llm_usage_events (route_id, started_at);
CREATE INDEX idx_llm_events_vkey ON llm_usage_events (virtual_key_id, started_at);
CREATE INDEX idx_llm_events_trace ON llm_usage_events (trace_id, started_at)
    WHERE trace_id IS NOT NULL;
CREATE INDEX idx_llm_events_tool_use ON llm_usage_events (tool_call_count, started_at)
    WHERE tool_call_count > 0;
```

### 10.2 MCP Usage Events Table

Table: `mcp_usage_events`

```sql
CREATE TABLE mcp_usage_events (
    event_id             TEXT PRIMARY KEY,
    trace_id             TEXT,
    span_id              TEXT NOT NULL,
    parent_span_id       TEXT,
    agent_depth          INTEGER NOT NULL DEFAULT 0,
    started_at           INTEGER NOT NULL,
    finished_at          INTEGER NOT NULL,
    route_id             TEXT,
    route_kind           TEXT NOT NULL DEFAULT 'mcp',
    route_protocol       TEXT,
    virtual_key_id       TEXT,
    success              INTEGER NOT NULL DEFAULT 0,
    status_code          INTEGER,
    error_type           TEXT,
    latency_ms           INTEGER,
    request_id           TEXT,
    service_id           TEXT,
    method               TEXT,
    tool_name            TEXT,
    presented_tool_name  TEXT,
    executed_tool_name   TEXT,
    execution_mode       TEXT,
    policy_action        TEXT,
    resource_uri         TEXT,
    prompt_name          TEXT,
    completion_ref_type  TEXT,
    completion_argument  TEXT,
    arg_count            INTEGER,
    result_status        TEXT,
    cancelled            INTEGER NOT NULL DEFAULT 0,
    tool_args_json       TEXT
);

CREATE INDEX idx_mcp_events_started ON mcp_usage_events (started_at);
CREATE INDEX idx_mcp_events_route ON mcp_usage_events (route_id, started_at);
CREATE INDEX idx_mcp_events_request ON mcp_usage_events (route_id, request_id, started_at);
CREATE INDEX idx_mcp_events_trace ON mcp_usage_events (trace_id, started_at)
    WHERE trace_id IS NOT NULL;
CREATE INDEX idx_mcp_events_tool ON mcp_usage_events (tool_name, started_at)
    WHERE tool_name IS NOT NULL;
```

`presented_tool_name`, `executed_tool_name`, `execution_mode`, and
`policy_action` are reserved for a future MCP tool-policy layer. They are
created in v0.4.x but remain null because no v0.4.x code populates policy
attribution.

### 10.3 ACP Usage Events Table

Table: `acp_usage_events`

```sql
CREATE TABLE acp_usage_events (
    event_id              TEXT PRIMARY KEY,
    trace_id              TEXT,
    span_id               TEXT NOT NULL,
    parent_span_id        TEXT,
    agent_depth           INTEGER NOT NULL DEFAULT 0,
    started_at            INTEGER NOT NULL,
    finished_at           INTEGER NOT NULL,
    route_id              TEXT,
    route_kind            TEXT NOT NULL DEFAULT 'agent',
    route_protocol        TEXT,
    virtual_key_id        TEXT,
    success               INTEGER NOT NULL DEFAULT 0,
    status_code           INTEGER,
    error_type            TEXT,
    latency_ms            INTEGER,
    service_id            TEXT, -- legacy ACP column; unified writes leave it empty
    agent_type            TEXT,
    operation             TEXT,
    thread_id             TEXT,
    session_id            TEXT,
    permission_request_id TEXT,
    fresh_session         INTEGER,
    event_counts_json     TEXT,
    usage_json            TEXT,
    result_status         TEXT
);

CREATE INDEX idx_acp_events_started ON acp_usage_events (started_at);
CREATE INDEX idx_acp_events_route ON acp_usage_events (route_id, started_at);
CREATE INDEX idx_acp_events_service ON acp_usage_events (service_id, started_at);
CREATE INDEX idx_acp_events_trace ON acp_usage_events (trace_id, started_at)
    WHERE trace_id IS NOT NULL;
CREATE INDEX idx_acp_events_thread ON acp_usage_events (thread_id, started_at)
    WHERE thread_id IS NOT NULL;
```

### 10.4 Rollups (superseded — not implemented)

> Status: internal rollup tables were evaluated and **dropped**. A single-dimension
> rollup row cannot answer filtered/cross-dimension breakdowns, so the typed event
> tables stayed the source of truth regardless; meanwhile the rollups added
> per-event write amplification and unbounded growth. Aggregate Admin API queries
> scan the event tables directly, and high-volume aggregation/trends/alerting are
> delegated to an external system via the Prometheus exposition endpoint
> (`GET /admin/metrics/prometheus`). The original design is kept below for context.

The superseded rollup design derived rows from event tables and introduced:

- `llm_usage_rollups`
- `mcp_usage_rollups`
- `acp_usage_rollups`
- `interaction_usage_rollups`

Rollup dimensions should stay low-cardinality by default. High-cardinality dimensions such as `virtual_key_id`, `thread_id`, and `credential_id` should only be included where the Admin API query requires them.

### 10.5 Retention

Default retention policy (enforced at startup and by a periodic background janitor):

- `llm_usage_events`: 30 days
- `mcp_usage_events`: 30 days
- `acp_usage_events`: 30 days

A background cleanup job deletes expired event rows. The retention window is
configurable through the Caddyfile `metrics` block and `agwd
--metrics-retention-days`; cleanup runs at startup and through a periodic
janitor in the SQLite sink.

## 11. Package Structure

```
internal/observability/usage/
    event.go         InteractionEvent base; LLMUsageEvent, MCPUsageEvent, ACPUsageEvent
    observer.go      InteractionObserver and InteractionSpan interfaces
    noop.go          no-op observer
    service.go       UsageService wired by caddy/gateway/app.go

internal/observability/pipeline/
    pipeline.go      EventPipeline: buffered input channel, fan-out loop, Sink interface
    sqlite_sink.go   SQLite sink
    prom_sink.go     Prometheus sink

pkg/configstore/sqlite/
    usage_writer.go  low-level typed INSERT helpers consumed by sqlite_sink
    usage_query.go   typed query helpers for Admin API handlers
```

The `UsageService` is owned by the gateway app/runtime lifecycle. `AgentGateway` should expose it to dispatcher and admin wiring similarly to the existing runtime managers. The observer field should default to a no-op implementation so unit tests can construct dispatchers without metrics plumbing.

## 12. Integration Points

### 12.1 Shared Dispatcher Entry

`pkg/dispatcher/handler.go`:

- extract trace context and agent depth at the start of `Dispatch`
- match a route through `AgentGateway.Match`
- validate the VirtualKey once and preserve the returned VirtualKey ID
- start an `InteractionSpan` after the route and virtual key are known
- set response propagation headers before protocol dispatch returns
- dispatch by `routecore.RouteKind` and finish the span in protocol-specific code

The shared entry records route match, route disabled, and virtual key failures where enough dimensions are known. Requests that do not match a gateway route may pass to `next` and are not usage events.

### 12.2 LLM Path

`pkg/dispatcher/llmapi/openai/handler.go`, `pkg/dispatcher/llmapi/anthropic/handler.go`, and `pkg/dispatcher/llmapi/cc/handler.go`:

- set `llm_api` and `api_operation`
- extract request-side declared tools where supported by the wire format
- extract response-side tool calls where preserved by the provider response model

`pkg/gateway/routedprovider.go`:

- record provider ID, provider type, logical model, upstream model, credential source, credential ID, token usage, and provider outcome
- distinguish provider request failures from provider stream failures

### 12.3 MCP Path

`pkg/dispatcher/mcp_handler.go`:

- reuse the shared trace context from `Dispatch`
- parse JSON-RPC method and request ID
- record method-specific dimensions from params before upstream execution
- keep the existing `beginRequest` cleanup for runtime inspection
- independently finish `MCPUsageEvent` through the metrics span on upstream success, upstream error, validation error, cancellation, or unsupported method

`pkg/mcp/runtime/registry.go`:

- remains the runtime inspection registry for in-flight requests, progress notifications, cancellation, and bounded completed request history
- should not become the canonical persisted metrics schema

`pkg/mcp/service/types.go`:

- add optional `AuditConfig` to `MCPServiceConfig` if verbose tool argument capture is implemented

### 12.4 ACP Path

`pkg/dispatcher/agent_handler.go`:

- reuse the shared trace context from `Dispatch`
- record the route-scoped operation matched by `matchAgentRouteEndpoint`
- for `turn`, wrap the ACP SSE sink to count emitted event names and capture the final usage snapshot when present
- for `permission`, record permission request ID and resolution status
- for `sessions` and `transcript`, record query shape without storing transcript content
- finish the ACP span on runtime success, validation error, upstream error, client cancellation, or permission lookup failure

`pkg/acp/runtime.Manager`:

- does not own metrics persistence
- may expose structured summaries to the dispatcher through return values or event sink metadata when the dispatcher cannot derive them itself

### 12.5 Admin Handler

`pkg/admin/handler.go`:

- receive `UsageService` or a query-facing metrics service from `AgentGateway`
- wire metrics routes in `pkg/admin/routes.go`
- use typed query helpers rather than reading generic config stores

## 13. Security And Privacy

The metrics layer must not persist sensitive request content by default.

Fields never stored regardless of configuration:

- LLM prompt text
- LLM response text
- ACP turn input
- ACP SSE delta/content/reasoning text
- ACP transcript text
- MCP tool result content
- bearer key values
- provider API keys
- credential secrets

Fields stored as stable identifiers only:

- `virtual_key_id`, not the bearer key string
- `credential_id`, not the credential secret
- `provider_id`
- MCP `service_id`
- `route_id`

MCP tool argument storage is off by default and must be explicitly enabled per service via `audit.capture_tool_args`. Because this is persisted configuration, the field must be part of `pkg/mcp/service.MCPServiceConfig` rather than an undocumented sidecar shape.

ACP permission params may contain sensitive command details and are not stored in phase 1. If a later phase adds permission argument capture, it must be opt-in and separate from the default ACP usage event.

## 14. Implementation Order

### Implemented Foundation And Durable Events

Goal: durable event capture for LLM, MCP, and ACP traffic plus a real `/admin/metrics` summary.

1. Define `InteractionEvent`, `LLMUsageEvent`, `MCPUsageEvent`, and `ACPUsageEvent` in `internal/observability/usage/event.go`.
2. Define `InteractionObserver` and `InteractionSpan`; implement no-op.
3. Implement `EventPipeline` and `SQLiteSink`.
4. Add SQLite writer/query helpers and create `llm_usage_events`, `mcp_usage_events`, and `acp_usage_events`.
5. Wire `UsageService` into `caddy/gateway/app.go` and `AgentGateway`.
6. Add shared trace/span/agent-depth extraction and response header emission in `pkg/dispatcher/handler.go`.
7. Instrument LLM dispatch and `RoutedProvider`.
8. Instrument MCP dispatch without making `CompletedRequest` the persisted event model.
9. Instrument ACP Agent operations, including SSE event-count capture for turns.
10. Replace `GET /admin/metrics` with a real summary from SQLite.
11. Add `GET /admin/metrics/llm/events`, `GET /admin/metrics/mcp/events`, `GET /admin/metrics/acp/events`, and `GET /admin/metrics/interactions`.

### Implemented Aggregated Statistics

Goal: event-table-backed time-series and breakdown endpoints. Rollup tables were
dropped; event tables remain the source of truth.

1. Implement `GET /admin/metrics/llm/timeseries` and `GET /admin/metrics/llm/breakdown`.
2. Implement MCP and ACP timeseries/breakdown/summary endpoints.
3. Implement `GET /admin/metrics/interactions/summary`.
4. Add startup and periodic retention cleanup for event tables.
5. Add verbose audit mode for MCP tool arguments.

### Remaining Follow-Ups

Goal: external exporter integration and protocol-specific refinements.

1. Wire an OpenTelemetry exporter into the `OpenTelemetrySink` adapter seam, at
   the `NewEventPipeline` call sites in `caddy/gateway/app.go` and
   `standalone/server/server.go`.
2. Implement MCP tool policy so policy-attribution columns are populated.
3. Implement planned ACP context-token metrics.
4. Add optional permission-argument capture if a future audit mode requires it.

## 15. Relationship To Existing Documents

`architecture/architecture-overview.md`:

- this document defines the metrics area now implemented by the gateway
- the architecture overview describes metrics as implemented infrastructure and keeps exporter wiring in future/partial scope

`architecture/mcp-architecture.md`:

- MCP runtime inspection remains separate from durable metrics persistence
- the architecture document should continue to describe in-flight/progress/history runtime inspection, while this document owns persisted audit and usage events

`architecture/acp-architecture.md`:

- ACP runtime behavior, session/list, transcript replay, and permission semantics remain defined there
- this document only defines ACP audit/metrics capture around those operations

`mcp-tool-policy.md`:

- tool policy populates `presented_tool_name`, `executed_tool_name`, `execution_mode`, and `policy_action` on MCP events when policy support is implemented; these columns exist from phase 1 and stay null until then
- metrics instrumentation must keep policy attribution explicit so synthetic or wrapped tools remain auditable
