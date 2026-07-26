# internal/observability — AGENTS.md

Scope: usage events, the event pipeline, OTLP export, and eino callback
integration. The root `AGENTS.md` rules apply. Keep API inventories and
operational descriptions in the architecture documentation rather than here.

## Event invariants

- Usage is stored in typed SQLite event tables (`llm_usage_events`,
  `mcp_usage_events`, `acp_usage_events`, `builtin_usage_events`), not generic
  config stores or internal rollups. Preserve nullable `agent_id`, `run_id`,
  and `runtime_type` correlation fields for direct non-Agent traffic.
- A failed span without an explicit `error_type` maps 4xx statuses to
  `client_error` and other statuses to `internal_error`.
- When the dispatcher passes an unhandled request to the next handler, call
  `InteractionSpan.Discard`; such requests must not emit usage events.
- Retention cleanup runs at startup and through the SQLite sink janitor.
  High-volume aggregation belongs in external Prometheus/Grafana systems.

## Tracing and callbacks

- OTLP export reconstructs the existing interaction tree from stored W3C
  trace/span/parent ids. Builtin inner LLM/MCP events are internal-kind spans;
  ingress interactions are server-kind spans.
- Exporter setup failure disables export without preventing request serving.
  Component calls without an interaction span are intentionally not exported.
- Register `einotap` once across both bootstrap paths. It only folds synchronous
  chat-model completion detail into the current interaction span; it must not
  emit separate usage events or consume/copy streams.
- The process-global component exporter may be replaced on reload, but callback
  handlers must not be registered repeatedly.

## Integration points

- Pipeline sinks are wired in `caddy/gateway/app.go` and
  `standalone/server/server.go`.
- `metrics.max_agent_depth` rejects ingress when `X-Agent-Depth` reaches the
  configured limit; zero disables the gate.
