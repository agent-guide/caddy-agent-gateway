# Observability Implementation Plan

## 0. About This Document

This document records the implementation path for the observability design in
`observability.md` and the remaining follow-up work. The foundation milestones
have landed; the remaining work is called out explicitly rather than mixed into
the completed plan.

Every file path and type below was verified against the current tree. When the
design and the code disagree, this document follows the code.

Conventions:

- completed milestones list their original target files and verification gates
- remaining follow-ups list the target files, change, and verification checks
- `go test ./...` and `go vet ./...` should pass at each milestone boundary

Source-of-truth anchors used throughout:

- lifecycle owner: `caddy/gateway/app.go`
- runtime holder / accessor: `pkg/gateway/agentgateway.go`
- shared dispatch entry: `pkg/dispatcher/handler.go` (`Handler.Dispatch`, line ~69)
- LLM handlers: `pkg/dispatcher/llmapi/{openai,anthropic,cc}/handler.go`
- MCP/ACP dispatch: `pkg/dispatcher/{mcp_handler.go,acp_handler.go}`
- provider boundary: `pkg/gateway/routedprovider.go` (`RoutedProvider`)
- sqlite backend: `pkg/configstore/sqlite/`, `caddy/configstore/sqlite/`
- admin: `pkg/admin/{handler.go,routes.go}`, `caddy/admin/module.go`

## 1. Milestone Map

The durable event pipeline, real `/admin/metrics`, per-protocol event listing,
aggregate metrics, Prometheus exposition, retention cleanup, agent-depth gate,
MCP verbose audit mode, and ACP Admin audit spans are implemented. The original
foundation work was split into milestones M0–M9:

```
M0  Storage seam            open *gorm.DB access on the sqlite backend
M1  usage package           events + observer/span interfaces + no-op
M2  pipeline + sqlite sink   EventPipeline, Sink, SQLiteSink, tables
M3  UsageService + wiring    app.go owns lifecycle; AgentGateway exposes observer
M4  trace context           extract/propagate trace/span/agent-depth in Dispatch
M5  LLM instrumentation      handlers + RoutedProvider
M6  MCP instrumentation      mcp_handler.go
M7  ACP instrumentation      acp_handler.go (+ SSE event counting)
M8  admin summary            replace GET /admin/metrics 501 with real summary
M9  admin events endpoints   llm/mcp/acp/interactions events listing
```

M0-M9 are completed historical milestones. Remaining follow-ups are listed after
the completed sections.

## 1.1 Implementation Contracts

These contracts are part of the executable plan and must be kept stable while
implementing M0-M9.

- The active `InteractionSpan` is stored in `context.Context` by
  `internal/observability/usage.ContextWithSpan` and read with
  `usage.SpanFromContext`. Dispatcher code starts the span; protocol handlers
  and provider wrappers add typed extensions to the same span.
- `InteractionSpan.Finish` is idempotent. The first call wins, later calls are
  ignored. This allows protocol handlers to defer a finish while error branches
  or streaming loops finish early.
- `InteractionSpan.SetExtension` accepts only the typed extension structs from
  `internal/observability/usage`: `LLMExtension`, `MCPExtension`, and
  `ACPExtension`. Repeated calls merge non-zero fields into the existing
  extension instead of replacing unrelated fields.
- HTTP status is captured through a small dispatcher response recorder for
  shared dispatch instrumentation. Explicit outcome status passed to `Finish`
  overrides the recorder when present; otherwise the recorded response status is
  used, defaulting to `200` only after a write succeeds.
- SQLite usage timestamps are stored as Unix milliseconds. Admin API responses
  expose RFC3339 timestamps and do not expose internal integer timestamps.
- `SQLDBProvider` lives in `internal/observability/usage`, not in the pipeline
  package, so `pkg/configstore/sqlite` can expose the capability without
  depending on a concrete metrics sink implementation.
- Admin event listing responses use `{ "items": [...], "limit": n }`.
  Summaries return zero-valued blocks when tables are empty or when no SQLite
  usage store is available.

## 2. M0 — Storage Seam (do this first)

The design wants typed time-series tables, but `configstore.ConfigStoreBackend`
only exposes `Register`/`Get` over generic JSON stores. The concrete sqlite
backend owns the `*gorm.DB`. Nothing downstream can run `CREATE TABLE`/`INSERT`
without a seam. This blocks the SQLite sink, so it is step one.

Target files:

- `pkg/configstore/sqlite/creator.go` (owns the shared `*gorm.DB`)
- `caddy/configstore/sqlite/` (Caddy adapter for the backend)

Steps:

1. Define the `SQLDBProvider` capability interface in
   `internal/observability/usage` (it is consumed by the SQLite sink and query
   service):

   ```go
   // SQLDBProvider is implemented by backends that expose a raw *gorm.DB
   // for typed (non-JSON-config) tables such as usage events.
   type SQLDBProvider interface {
       UsageDB() *gorm.DB
   }
   ```

   Decision: the method is the usage-scoped `UsageDB()`, not a generic `RawDB()`,
   so the generic config-store contract stays clean.

2. Implement `UsageDB() *gorm.DB` on the sqlite backend creator, returning the
   shared handle already held in `SQLiteConfigStoreCreator`.
3. Consumers type-assert the backend to `SQLDBProvider`. If it is not satisfied
   (non-sqlite backend), they fall back to a no-op sink — the gateway must still
   boot and serve.

Verification:

- `go build ./...`
- a unit test that the sqlite backend satisfies `SQLDBProvider` and returns a
  usable handle (`db.Exec("SELECT 1")`).

## 3. M1 — `internal/observability/usage` Package

Target (new) files, per design §11:

```
internal/observability/usage/
    event.go      InteractionEvent base; LLMUsageEvent, MCPUsageEvent, ACPUsageEvent
    observer.go   InteractionObserver, InteractionSpan, InteractionDimensions, InteractionOutcome
    noop.go       NoopObserver / noopSpan
```

Steps:

1. `event.go`: define `InteractionEvent` (design §5.1) and the three protocol
   event structs embedding it (design §6.1 / §7.1 / §8.1). Match the SQLite
   column sets in design §10 field-for-field so the sink is a direct mapping.
2. `observer.go`: define `InteractionObserver`, `InteractionSpan`,
   `InteractionDimensions` (stable dims known at `Begin`: route id/kind/protocol,
   virtual key id, trace context), and `InteractionOutcome` (success, status,
   error type, latency, token usage). `SetExtension(any)` carries the protocol
   extension; `AddAnnotation` is optional metadata. The MCP extension struct must
   include the policy-attribution fields `presented_tool_name`,
   `executed_tool_name`, `execution_mode`, and `policy_action` so the future MCP
   tool policy layer can set them via `SetExtension` without changing this
   interface (they stay zero-valued until then).
3. `noop.go`: `NoopObserver.Begin` returns a `noopSpan` whose `Finish` drops the
   event and returns the unchanged context.

Verification:

- `go build ./...`, `go vet ./...`
- table-driven test that no-op `Begin`/`Finish` is allocation-light and safe to
  call with zero config.

## 4. M2 — Pipeline, Sink, Tables

Target (new) files, per design §11:

```
internal/observability/pipeline/
    pipeline.go      EventPipeline: buffered channel, fan-out loop, Sink interface, dropped_events counter
    sqlite_sink.go   SQLiteSink (consumes SQLDBProvider handle from M0)

pkg/configstore/sqlite/
    usage_writer.go  typed INSERT helpers for the three tables
    usage_query.go   typed query helpers for admin handlers (used from M8/M9)
```

Steps:

1. `pipeline.go`:
   - `Sink` interface: `Write(InteractionEvent-or-typed-event)`; `Close()`.
   - `EventPipeline`: buffered channel (configurable size, default `4096`),
     one fan-out goroutine, registered sinks, `dropped_events` atomic counter.
   - non-blocking enqueue: on full channel, drop + increment counter (design §5.3).
   - `Close()` drains the channel then closes sinks (clean shutdown).
2. `sqlite_sink.go`: build from the `*gorm.DB` obtained via the M0 seam; the
   constructor runs the table DDL (idempotent `CREATE TABLE IF NOT EXISTS` +
   indexes from design §10.1–§10.3) so the typed-table concern stays inside the
   metrics layer, not `schema.RegisterDefaultStores`; `Write` dispatches by event
   type to `usage_writer`.
3. `usage_writer.go`: typed INSERT per table. Keep JSON-array fields
   (`request_tool_names`, `tool_names`, `event_counts_json`, `usage_json`) as
   marshaled TEXT.
4. `usage_query.go`: leave as stubs/signatures here; fill in M8/M9.

Tables created: `llm_usage_events`, `mcp_usage_events`, `acp_usage_events`
(design §10.1–§10.3, exact columns + indexes). Rollup tables were later dropped;
aggregate queries scan the event tables directly.

`mcp_usage_events` includes the policy-attribution columns `presented_tool_name`,
`executed_tool_name`, `execution_mode`, and `policy_action` from phase 1 (created
but unpopulated). They are reserved for MCP tool policy (`mcp-tool-policy.md`) so
that layer needs no schema migration when it lands. Do not omit them.

Verification:

- unit test: open in-memory/temp sqlite, construct sink, write one event of each
  kind, read it back via a raw query.
- unit test: full channel drops and increments `dropped_events`, never blocks.
- unit test: `Close()` flushes a pending event before returning.

## 5. M3 — UsageService + Wiring

This is the `cliauth`/`credentialMgr` pattern, not the `configure*ServiceManager`
pattern. Lifecycle lives in `app.go`; `AgentGateway` only holds + exposes the
observer.

`internal/observability/usage/service.go` (new): `UsageService` owns the
`EventPipeline`, exposes `Observer() InteractionObserver` (pipeline-backed) and
`Close()`.

`caddy/gateway/app.go`:

1. struct field (`app.go:38-46`): `usageService *usage.UsageService`.
2. in `Provision`, after `provisionConfigStore` (`app.go:61-63`): construct the
   sink via the M0 type-assertion on `a.configBackend`; build `usageService`.
   Non-sqlite backend → no-op sink, service still constructed.
3. `Bootstrap` options block (`app.go:97-106`): pass
   `UsageObserver: a.usageService.Observer()`.
4. `Start` (`app.go:154-160`): start the pipeline goroutine if not started in the
   constructor.
5. `Stop` (`app.go:163-171`): after `a.agentGateway.Close()` (`:168`), call
   `a.usageService.Close()` to drain. Ordering matters — stop dispatch first.

`pkg/gateway/agentgateway.go`:

1. `BootstrapOptions` (`:29-40`): add `UsageObserver usage.InteractionObserver`.
2. `AgentGateway` struct (`:42-62`): add `usageObserver usage.InteractionObserver`.
3. `NewAgentGateway` (`:64-69`): default to `usage.NoopObserver{}` so dispatchers
   and tests never nil-check.
4. `Bootstrap` (`:95-98`): `if opts.UsageObserver != nil { g.usageObserver = opts.UsageObserver }`.
5. accessor block (`:220-242`): add `UsageObserver() usage.InteractionObserver`
   (with `RLock`), mirroring `ACPRuntimeManager()`.
6. `Reset` (`:106-130`): reset `usageObserver` back to no-op. Do NOT `Close` here
   — app owns Close; closing in `Reset` would kill a pipeline the app still holds.

Verification:

- `go build ./...`; gateway boots with sqlite and with a non-sqlite backend.
- `Stop` drains without deadlock (smoke run + clean shutdown log).

## 6. M4 — Trace Context Extraction & Propagation

`pkg/dispatcher/handler.go` (`Handler.Dispatch`, ~line 69) and a small new helper
file (e.g. `pkg/dispatcher/trace.go`).

Steps (design §5.2, §12.1):

1. After `h.gateway.Match` and VirtualKey validation, extract trace context:
   precedence `traceparent`/`tracestate` → `X-Trace-ID`/`X-Span-ID` → generate.
   Every inbound id is validated against the W3C shape and rejected rather than
   normalized, `X-*` included; a `traceparent` that is present but malformed
   starts a fresh trace and suppresses the `X-*` fallback. Inbound trace flags
   are preserved. Always generate a fresh `span_id`. Parse `X-Agent-Depth`.
2. Build `InteractionDimensions` from the resolved route + virtual key + trace
   context; call `h.gateway.UsageObserver().Begin(ctx, dims)` to get a span and a
   derived context. Thread that context into `dispatchLLM/MCP/ACP`.
3. Emit response headers before protocol dispatch returns: `traceparent`,
   `tracestate`, and compatibility `X-Trace-ID`/`X-Span-ID`/`X-Agent-Depth`
   (= `agent_depth + 1`).
4. Record route-not-found / route-disabled / virtual-key-rejected outcomes where
   enough dims are known. Requests that do not match a gateway route pass to
   `next` and are NOT usage events.

Verification:

- unit test: header precedence (traceparent wins; X-* fallback; generation).
- unit test: malformed inbound ids (bad version, wrong length, uppercase, all
  zeroes) never reach the response `traceparent` or the `trace_id` column.
- unit test: response carries `traceparent` and `X-Agent-Depth = depth+1`.
- unit test: unmatched request produces no event and calls `next`.

## 7. M5 — LLM Instrumentation (vertical slice)

`pkg/dispatcher/llmapi/{openai,anthropic,cc}/handler.go`:

- set `llm_api` and `api_operation` (`chat_completions` | `messages` | `responses`).
- in `PrepareLLMApiRequest`: extract request-side declared tools
  (`request_tool_count`, `request_tool_names`).
- in `ServeLLMApi` (the response/SSE render point): extract response-side tool
  calls (`tool_call_count`, `tool_names`); finish the span when the response is
  written or the stream completes/fails.

`pkg/gateway/routedprovider.go` (`RoutedProvider.Chat`/`StreamChat`, lines ~44/59):

- record `provider_id`, `provider_type`, `logical_model`, `upstream_model`,
  `credential_source`, `credential_id`, token usage, and provider outcome onto
  the span extension.
- distinguish `provider_request_failed` vs `provider_stream_failed`.

Error categories: design §6.3. Streaming: design §6.4 — set
`usage_finalized=false` when final usage is absent; never invent token counts.

Verification:

- end-to-end: a real LLM request through an openai route produces one
  `llm_usage_events` row with correct route/provider/token/latency dims (use the
  local verification flow noted in repo memory for cc/openai paths).
- streaming request records on completion with `usage_finalized` set correctly.

## 8. M6 — MCP Instrumentation

`pkg/dispatcher/mcp_handler.go` (`Handler.dispatchMCP`, ~line 21):

- reuse the shared trace context from `Dispatch`.
- parse JSON-RPC `method` + `request_id`; record method-specific dims
  (`tool_name`, `resource_uri`, `prompt_name`, completion ref, `arg_count`).
- keep the existing `beginRequest` runtime-registry path intact; finish the
  `MCPUsageEvent` independently on success / upstream error / validation error /
  cancellation / unsupported method.

Tool policy attribution (`presented_tool_name`, `executed_tool_name`,
`execution_mode`, `policy_action`): the columns exist from M2 but stay null in
M6. MCP tool policy is a separate effort (`mcp-tool-policy.md`, its Phase 2/5);
when it lands, `CallToolForRoute` populates these via `InteractionSpan.SetExtension`.
M6 only reserves the seam — do not populate or block on tool policy here.

`pkg/mcp/runtime/registry.go`: unchanged in role — remains in-memory inspection;
`CompletedRequest` is NOT the persisted schema and the sink must not consume it.

`pkg/mcp/service/types.go`: in the original M6 slice `tool_args_json` stayed
null. The later verbose audit follow-up added `AuditConfig`
(`capture_tool_args`) and populates `tool_args_json` only when enabled per
service.

Error categories: design §7.2.

Verification:

- unit test: a `tools/call` produces one `mcp_usage_events` row with `method`,
  `tool_name`, `arg_count`, `result_status`; `tool_args_json` is null.
- cancellation path records `result_status=cancelled`.

## 9. M7 — ACP Instrumentation

`pkg/dispatcher/acp_handler.go` (`Handler.dispatchACP`, ~line 19):

- record the operation matched by the route endpoint matcher: `turn` |
  `permission` | `sessions` | `transcript`.
- for `turn`: wrap the SSE sink to count emitted event names (design §8.1 list)
  and capture the final usage snapshot if present; finish when `ServeTurn`
  returns, the client connection fails, or the context is cancelled.
- for `permission`: record `permission_request_id` and resolution status.
- for `sessions`/`transcript`: record query shape only; never store transcript
  content.

Route-scoped ACP traffic is recorded through `agent_route_dispatcher`. ACP Admin
runtime/session/transcript operator calls are now recorded as management-plane
audit spans with the synthetic route `/admin/acp` and `route_protocol=admin`.

Error categories: design §8.2.

Verification:

- unit test: a turn records one `acp_usage_events` row with `operation=turn`,
  populated `event_counts_json`, and no content fields.
- permission resolution records `operation=permission` + request id.

## 9.1 Remaining Follow-up - ACP Token Metrics

Status: not implemented in v0.4.x; deferred to v0.5.x. v0.4.x stores the raw
bounded ACP usage payload in `acp_usage_events.usage_json` when available, but
does not add queryable context-token fields, token deltas, or Prometheus
context-token counters.

Design source: `observability.md` §8.2.

ACP `usage_update` reports session context occupancy (a gauge), not LLM-style
input/output tokens. Add ACP token metrics as a schema extension without changing
that meaning, and treat the totals as an approximate context-growth signal, not a
billable token count.

Field names are fixed by the ACP v1 `session/update` schema (already decoded and
tested in `pkg/acp/runtime/acpupdate`), so `used`/`size` parsing needs no capture.
Emission is agent-specific but verified for both current adapters (live captures
2026-06-24, one `usage_update` per turn each):

- `codex-acp` v0.16.0: `{"sessionUpdate":"usage_update","used":14769,"size":258400}`
  — only `used`/`size`.
- `opencode`: `{"sessionUpdate":"usage_update","used":11102,"size":200000,"cost":{"amount":0,"currency":"USD"}}`
  — same `used`/`size` plus a nested `cost` object.

So codex turns do expose raw usage payloads. (codex only lacks
`session_info_update`, an unrelated update that affects the session title, not
token data.) The future parser must extract only `used`/`size` and tolerate
extra/nested fields like opencode's `cost`; `cost` is not persisted (and was `0`
in the capture, so it is not a billing source). In v0.4.x an adapter that emits
no parseable `usage_update` simply leaves no structured token evidence beyond
the raw `usage_json`; document per-adapter emission rather than assuming uniform
coverage.

Targets:

- `pkg/acp/runtime/types.go` + `pkg/acp/runtime/instance.go`: add a `Replay bool`
  marker to `TurnEvent`, set true for every event built in
  `sessionMetaCache.turnStartEvents` (the cached config/commands/session-info/
  mode/usage snapshot replayed at each turn start). This is the seam that lets the
  metrics layer tell a turn's own live `usage_update` from the joined-session
  snapshot replay; without it, every turn on a session with cached usage would be
  miscounted.
- `internal/observability/usage/event.go`: add `ContextUsedTokens *int`,
  `ContextWindowTokens *int`, `TokenDelta *int`, and `UsageObserved bool` to
  `ACPUsageEvent` and `ACPExtension`, and merge them in `mergeACP`
  (`internal/observability/usage/merge.go`).
- `pkg/dispatcher/acp_handler.go`: in `wrapACPEventSinkForUsage`, parse only
  live (`!event.Replay`) `event: usage` payloads for the ACP context fields; skip
  replayed snapshots entirely so they never set `UsageObserved` or token fields.
  Set `UsageObserved=true` and the parsed values on the `ACPExtension` (later live
  usage events overwrite earlier ones, so the final live snapshot wins); keep
  malformed payloads non-fatal and continue storing bounded `usage_json`.
- `internal/observability/usage/observer.go`: at ACP event finalization, before
  enqueue, stamp `TokenDelta` using a small in-process per-session last-value
  tracker keyed by `service_id` + `session_id` (guarded by a mutex, with bounded
  entry eviction). Store only a positive delta vs the previous turn's value;
  leave it null on first turn, missing current value, decrease, or missing
  session id, then update the tracker to the current value. This single upstream
  stamp is what keeps the SQLite and Prometheus sinks consistent — neither
  sink computes the delta itself.
- `pkg/configstore/sqlite/usage_schema.go`: add nullable `context_used_tokens`,
  `context_window_tokens`, `token_delta`, and `usage_observed`
  (`INTEGER NOT NULL DEFAULT 0`) columns to `acp_usage_events`, with idempotent
  migration for existing databases. No new index is required because the delta is
  precomputed; the write path stays a single `INSERT`.
- `pkg/configstore/sqlite/usage_writer.go`: persist the new ACP fields in
  `InsertACPUsageEvent`; no per-turn `SELECT`.
- `pkg/configstore/sqlite/usage_query.go`: `context_growth_tokens` =
  `SUM(token_delta)` over the window; `latest_context_used_tokens` =
  `context_used_tokens` of the most recent (`finished_at`) row with a non-null
  value. Label growth as approximate, not "tokens used".
- `internal/observability/pipeline/prom_sink.go`: in `eventMetrics`, return the
  stamped `TokenDelta` (not `context_used_tokens`) as the ACP token contribution
  so the Prometheus counter matches the SQLite sum.

Verification:

- a turn with a live `usage_update` stores context token fields and sets
  `usage_observed=true`.
- a turn with no live `usage_update` leaves token fields null and
  `usage_observed=false`, even when the runtime replays a cached usage snapshot
  at turn start (replayed `usage` events with `Replay=true` are ignored).
- malformed usage JSON does not fail the ACP turn and leaves token fields null.
- two consecutive same-session turns `100 -> 160` stamp `token_delta=60` on the
  second event; the first turn's `token_delta` is null.
- a decreasing snapshot such as `160 -> 80` stamps a null `token_delta` and
  contributes nothing to `context_growth_tokens`.
- the Prometheus ACP token counter and the SQLite `context_growth_tokens` for the
  same window agree (same stamped `token_delta` source).

## 10. M8 — Real `/admin/metrics` Summary

`pkg/admin/routes.go` (currently returns 501 at ~line 603-605) +
`pkg/admin/handler.go` (inject the query service) + `caddy/admin/module.go` if a
new dependency must be threaded.

Steps:

1. give the admin `Handler` access to a query-facing metrics service (from
   `AgentGateway` accessor or the `UsageService`), using typed `usage_query.go`
   helpers — not generic config stores.
2. replace the 501 with the summary payload shape in design §9.1 (llm/mcp/acp
   blocks: counts, tokens, latency).

Verification:

- after generating traffic, `GET /admin/metrics` returns the aggregated summary;
  numbers reconcile with row counts in the tables.

## 11. M9 — Admin Event-Listing Endpoints

`pkg/admin/routes.go` (register routes) + `usage_query.go` (filters):

- `GET /admin/metrics/llm/events` (filters: design §9.2)
- `GET /admin/metrics/mcp/events` (design §9.3)
- `GET /admin/metrics/acp/events` (design §9.4)
- `GET /admin/metrics/interactions` (cross-protocol base fields, design §9.5)

All support `from`/`to` (RFC3339), `limit` (default 100, max 1000), and the
per-family filters listed in the design.

Verification:

- each endpoint returns recent rows with working filters; `limit` clamped to
  1000; bad `from`/`to` rejected.

## 12. Implemented Follow-ons

Aggregate queries run directly over the typed event tables. Internal rollup
tables were intentionally dropped: the single-dimension rollup shape could not
serve filtered/cross-dimension breakdowns (so the event tables remained the
source of truth anyway), while it added per-event write amplification and
unbounded table growth. High-volume aggregation, trends, and alerting are
delegated to an external system through the Prometheus exposition endpoint.

Implemented follow-ons:

1. aggregate endpoints scan the event tables: `GET /admin/metrics/llm/timeseries`,
   `GET /admin/metrics/llm/breakdown`, `GET /admin/metrics/mcp/tools/summary`,
   `GET /admin/metrics/acp/summary`, `GET /admin/metrics/interactions/summary`.
2. retention cleanup runs at startup and on a periodic janitor in the SQLite
   sink (configurable window; design §10.5).
3. MCP verbose audit mode: `AuditConfig`/`capture_tool_args` on
   `pkg/mcp/service.MCPServiceConfig`; populate `tool_args_json` when enabled.
   Persisted free-form payloads (`tool_args_json`, ACP `usage_json`) are
   truncated to a fixed byte cap to bound secret/PII exposure.
4. `PrometheusSink` (`internal/observability/pipeline/prom_sink.go`) is wired
   into the pipeline and exposed at `GET /admin/metrics/prometheus`; pipeline
   drop/failure counters are surfaced both there and in the `pipeline` object of
   `GET /admin/metrics`.
5. configurable retention window through the Caddyfile `metrics` block and
   `agwd --metrics-retention-days`.
6. agent-depth enforcement policy gate through `max_agent_depth` and
   `agwd --max-agent-depth`.
7. admin-audit events for operator Admin API runtime/session/transcript
   calls.

## 13. Remaining Exporter & Policy Hooks

Remaining work:

1. wire a push exporter into the `OpenTelemetrySink` adapter seam, at the
   `NewEventPipeline` call sites in `caddy/gateway/app.go` and
   `standalone/server/server.go`.
2. implement MCP tool policy so `presented_tool_name`, `executed_tool_name`,
   `execution_mode`, and `policy_action` are populated.
3. implement ACP token metrics as described in §9.1.

## 14. Cross-Cutting Test Strategy

- unit tests live beside each new package (`internal/observability/usage`,
  `internal/observability/pipeline`, `pkg/configstore/sqlite` usage
  writer/query).
- dispatcher instrumentation tested with the `NoopObserver` default plus a
  capturing fake observer to assert emitted dimensions without sqlite.
- one end-to-end smoke per protocol family asserting exactly one persisted row.
- `go test ./...` and `go vet ./...` green at each milestone boundary.

## 15. Documentation Status

`AGENTS.md`, `README.md`, `docs/architecture/architecture-overview.md`, and
`observability.md` describe metrics as implemented infrastructure. Future
observability changes should update those documents in the same change when they
alter Admin API paths, metric semantics, storage schema, or dispatcher behavior.

## 16. Resolved Decisions

These were the open questions; all are now decided and baked into the steps above.

1. **Storage seam**: the sqlite backend exposes `UsageDB() *gorm.DB` via the
   `SQLDBProvider` capability interface (usage-scoped name, not a generic
   `RawDB()`), keeping the generic config-store contract clean. See M0.
2. **Pipeline buffer + drop policy**: buffered channel with default capacity
   `4096`, non-blocking enqueue, drop-on-full with a `dropped_events` counter.
   See M2.
3. **Table DDL location**: created in the `SQLiteSink` constructor (idempotent
   `CREATE TABLE IF NOT EXISTS` + indexes), so the typed-table concern stays
   inside the metrics layer rather than `schema.RegisterDefaultStores`. See M2.
