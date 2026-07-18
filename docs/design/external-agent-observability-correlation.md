# External Agent Observability Correlation

## 1. Status And Purpose

Status: design requirement; not implemented.

This document records the observability gap between the three agent runtime
types (`builtin`, `acp`, and `http`) and proposes an incremental way to correlate
an external agent's LLM and MCP callbacks with the session and turn that caused
them.

The existing observability design remains authoritative for event capture,
storage, metrics, and W3C trace propagation. This document only adds the missing
agent-runtime correlation requirements.

## 2. Current Capability

There are three different levels of observability:

1. **Agent attribution** answers "which configured agent owns this event?"
2. **Conversation correlation** answers "which session and turn caused it?"
3. **Call-tree correlation** answers "which agent turn is the parent of this LLM
   or MCP operation?"

The current implementation provides different levels for each runtime:

| Runtime | Agent attribution | Session and turn visibility | Inner LLM/MCP call tree |
|---------|-------------------|-----------------------------|-------------------------|
| `builtin` | Direct and explicit | Session and turn event are visible | Complete in-process parent/child spans |
| `acp` | Route/service attribution | ACP `thread_id`, `session_id`, turn events, session listing, and transcript are visible | Not automatically correlated across the process boundary |
| `http` | Owned-route attribution for traffic that passes through the gateway | Not visible to the gateway today | Not automatically correlated; runtime dispatch is not implemented yet |

### 2.1 Builtin Runtime

The builtin host owns the entire execution graph. A builtin turn produces a
`builtin_usage_events` row, and its model and tool wrappers create child LLM and
MCP spans using the same `trace_id` and the turn's `span_id` as
`parent_span_id`. The host also stamps `agent_id` explicitly.

The resulting relationship is reconstructable:

```text
builtin agent session
└── builtin turn
    ├── LLM call
    ├── MCP tool call
    └── LLM call
```

### 2.2 ACP Runtime

The gateway owns the ACP process pool and records ACP operations with
`service_id`, `agent_type`, `thread_id`, `session_id`, event counts, permission
information, and the final usage payload when available. Session listing and
transcript replay are also available.

This is enough to inspect ACP conversations and group ACP turns. It is not
enough to associate an LLM or MCP callback made by the external ACP process
with one particular ACP session or turn. Such callbacks may receive the same
`agent_id` through owned-route attribution, but they normally start a new trace
and LLM/MCP event rows do not carry the ACP session identity.

### 2.3 HTTP Runtime

The HTTP runtime currently defines the control-plane shape only; the gateway
does not yet dispatch turns to it. An external HTTP agent owns its lifecycle and
conversation state. Calls it sends through agent-owned LLM or MCP routes can be
attributed to the agent, but the gateway cannot infer the external service's
session or turn boundaries.

### 2.4 Important Distinction

`agent_id` attribution is not causal correlation. It can prove that a request
belongs to an agent's configured traffic, but it cannot prove that the request
was caused by a particular session or turn. Route ownership alone must not be
used to fabricate that relationship.

## 3. Requirement

For ACP and HTTP runtimes, operators should eventually be able to reconstruct:

```text
agent
└── session
    └── turn
        ├── LLM call
        ├── MCP tool call
        ├── nested agent call
        └── LLM call
```

The implementation should support the following queries:

- list all turns in one agent session
- list all LLM, MCP, ACP, builtin, and future agent events caused by one turn
- reconstruct parent/child ordering using trace and span identifiers
- aggregate token, tool, latency, and failure data by agent, session, and turn
- distinguish direct user traffic from traffic emitted by an agent runtime
- preserve the relationship across process and HTTP boundaries
- show partial correlation honestly when an external runtime cannot propagate
  the required context

The solution must not:

- trust a caller-supplied `agent_id`, `session_id`, or `turn_id` without
  authenticating and authorizing the caller
- infer a session from route ownership, timing proximity, or a reused process
- treat `trace_id` as a conversation identifier; one session can contain many
  traces and turns
- require storage of prompt, response, transcript, or tool-result content
- break existing LLM, MCP, ACP, or builtin clients that do not propagate the new
  context

## 4. Proposed Correlation Model

Use separate identifiers for separate meanings:

| Field | Meaning | Lifetime |
|-------|---------|----------|
| `agent_id` | Authenticated configured agent identity | Agent definition |
| `session_id` | Runtime conversation identity | Multiple turns |
| `turn_id` | Gateway-issued identity for one logical turn | One turn and all work caused by it |
| `trace_id` | Distributed execution trace | One causal call tree |
| `span_id` | One observed operation | One event |
| `parent_span_id` | Direct causal parent operation | One event relationship |
| `agent_depth` | Nested agent-hop count | One call chain |

`turn_id` is the missing durable join key. It should be generated by the gateway
when a turn begins and returned to the caller in the initial session/turn event.
For ACP, the runtime-produced `session_id` may become available after the turn
starts; the outer turn span should accept and persist the finalized session ID,
as it does today, while keeping the gateway-issued `turn_id` stable.

Add nullable `session_id` and `turn_id` correlation dimensions to the shared
interaction model so every protocol event can carry them. Protocol-specific
session fields may remain during migration, but queries should expose one
canonical pair. Builtin child spans should inherit both fields from their parent
context.

The outer ACP, HTTP, or builtin turn is the root operation for the turn. LLM,
MCP, and nested-agent operations caused by it reuse its `trace_id` and set their
`parent_span_id` to the actual calling span when that value is available.

## 5. Cross-Process Propagation

### 5.1 Transport Contract

Continue using W3C `traceparent` and `tracestate` for trace identity. Add a
gateway-owned correlation carrier for the non-W3C agent dimensions:

- `agent_id`
- `session_id`
- `turn_id`
- `agent_depth`
- expiry and issuer/audience information

The exact wire representation is an implementation decision. A signed,
short-lived, opaque `X-AGW-Agent-Context` token is preferred over independent
trusted `X-Agent-ID`, `X-Session-ID`, and `X-Turn-ID` headers because the latter
are easy to spoof. The W3C headers remain separate so standard tracing tools can
understand the trace without decoding gateway-specific context.

On an inbound LLM, MCP, or agent request, the gateway should:

1. authenticate the normal VirtualKey or route credential
2. validate the correlation token's signature, expiry, and audience
3. verify that the authenticated identity may act for the token's `agent_id`
4. verify that referenced routes/resources belong to or are allowed for the
   agent
5. accept the session/turn context only after those checks pass
6. otherwise reject the correlation context or the request according to the
   selected fail-closed policy, and record a correlation error reason

### 5.2 HTTP Runtime

When HTTP runtime dispatch is implemented, the gateway can send
`traceparent`, the signed agent context, and the gateway `turn_id` with the turn
request. A conforming HTTP agent propagates them on every callback to gateway
LLM, MCP, or nested-agent endpoints.

This is the simplest external-runtime path because the gateway controls the
outbound HTTP request and can publish an SDK/helper for correct propagation.

### 5.3 ACP Runtime

ACP uses a long-lived external process and stdio JSON-RPC, so ordinary HTTP
header injection at turn ingress is insufficient. A reused process may serve
many sessions over its lifetime, and static process environment variables must
not be treated as turn context.

ACP propagation therefore needs an explicit adapter seam that binds correlation
context for the duration of `session/prompt`. Possible implementations include:

- native support in an ACP adapter for applying per-turn headers to its gateway
  LLM/MCP clients
- a gateway-provided callback client configuration that the adapter updates per
  turn
- a local per-instance forwarding proxy that injects the currently bound turn
  context, provided the runtime guarantees one active turn for that instance

The first option is preferred. The proxy option requires careful lifecycle,
concurrency, cancellation, and stale-context handling and should be used only
for opaque third-party ACP binaries that cannot propagate context themselves.

ACP adapters that cannot implement the contract remain supported, but their
LLM/MCP events stay agent-attributed only and must be reported as
`correlation_status = unavailable`, not guessed.

## 6. Storage And Query Changes

Extend `InteractionDimensions` and the common persisted event columns with:

```text
session_id          nullable
turn_id             nullable
correlation_status  correlated | agent_only | unavailable | invalid
```

The schema change should be additive and indexed selectively:

- `(agent_id, session_id, started_at)` where `session_id IS NOT NULL`
- `(turn_id, started_at)` where `turn_id IS NOT NULL`
- existing trace indexes continue to reconstruct the span tree

Avoid indexing a raw correlation token; it is authentication material and must
never be persisted or logged.

Extend the unified interactions and per-agent endpoints with `turn_id` filters.
A future session view can be exposed under the agent API, for example:

```text
GET /admin/agents/{id}/sessions
GET /admin/agents/{id}/sessions/{session_id}/interactions
GET /admin/agents/{id}/turns/{turn_id}/interactions
```

The ACP transcript remains the authoritative content replay surface for ACP.
The interaction view is metadata and usage correlation, not transcript storage.

## 7. Incremental Delivery

### Phase 1: Shared correlation fields

- add `turn_id` and canonical `session_id` to `InteractionDimensions` and all
  usage event families
- issue a `turn_id` for ACP and builtin turns
- inherit both dimensions on builtin child spans
- add SQLite columns, indexes, query filters, and Admin response fields
- expose `correlation_status`

This phase improves the common model without claiming cross-process ACP/HTTP
correlation.

### Phase 2: HTTP propagation contract

- implement HTTP runtime turn dispatch
- define and validate the signed agent context
- provide a small propagation helper and conformance tests
- correlate HTTP-agent callbacks with the originating session and turn

### Phase 3: ACP adapter propagation

- add the per-turn context binding seam to `pkg/acp/agentspi` and the runtime
- implement it for adapters that can set callback headers safely
- evaluate a local forwarding proxy only for adapters that cannot cooperate
- report adapter correlation capability through runtime inspection

### Phase 4: Operator views and export

- add session/turn interaction endpoints and UI views
- export session/turn attributes through the future OpenTelemetry sink under
  the gateway namespace
- add completeness metrics showing correlated versus agent-only events

## 8. Acceptance Criteria

The capability is complete for a runtime only when:

- a turn receives one stable gateway-issued `turn_id`
- all callbacks from a conforming runtime carry the same authenticated
  `agent_id`, `session_id`, and `turn_id`
- callback spans reuse the turn trace and have a valid causal parent
- querying by `turn_id` returns the outer turn plus its LLM/MCP/nested-agent
  operations and excludes unrelated calls from the same agent
- querying by `session_id` returns multiple turns without mixing another agent's
  same-named session
- forged, expired, cross-agent, and cross-resource contexts are rejected or
  ignored according to the documented policy and are auditable
- a non-conforming external runtime continues to work and is clearly shown as
  agent-only/unavailable correlation
- no prompt, response, transcript, secret, or raw correlation token is added to
  usage storage

## 9. Open Decisions

- correlation carrier format, signing-key ownership, rotation, and maximum TTL
- whether invalid correlation rejects the entire request or only discards the
  claimed relationship on routes where correlation is optional
- canonical session identity when an external runtime exposes both thread and
  session concepts
- whether `turn_id` should be a UUID, UUIDv7, or another sortable opaque ID
- which current ACP adapters can propagate per-turn callback headers without a
  forwarding proxy
- retention and cardinality limits for session-level query indexes

## 10. Related Documents

- [`observability.md`](observability.md): event pipeline, trace propagation,
  storage, and metrics APIs
- [`agents-control-plane.md`](agents-control-plane.md): agent runtime and
  attribution model
- [`../architecture/acp-architecture.md`](../architecture/acp-architecture.md):
  ACP runtime, sessions, and process lifecycle

