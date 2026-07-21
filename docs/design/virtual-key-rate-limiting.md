# VirtualKey Request Rate Limiting

## 1. Status

This document defines the proposed first version of request-frequency rate
limiting by VirtualKey. The feature described here is not implemented yet.

This first version is deliberately scoped for a fast landing: admission is a
single central check keyed by VirtualKey ID and route kind, performed right
after VirtualKey validation and interaction-span setup, and before protocol
dispatch. It counts requests, not operations, so it needs no per-protocol
integration points.

## 2. Goals

- Configure request-frequency limits on each persisted VirtualKey.
- Limit LLM, MCP, and agent traffic independently.
- Use one `agent` configuration for both ACP and builtin traffic while keeping
  their runtime counters independent.
- Apply the same behavior to every route protected by the same VirtualKey.
- Reject excess traffic immediately with HTTP `429 Too Many Requests`.
- Keep the request hot path in memory and avoid config-store reads per request.

## 3. Non-Goals

The first version does not provide:

- token-per-minute, token-per-day, or monetary quota enforcement
- request queuing or delayed admission
- one aggregate limit shared by LLM, MCP, and agent traffic
- exact rate limiting shared across multiple gateway processes
- per-route, per-provider, per-service, or per-agent-definition overrides
- rate limiting for routes that do not require a VirtualKey
- per-operation accounting: admission happens at route-kind granularity before
  the protocol operation is parsed, so malformed requests, protocol handshakes
  (MCP `initialize`/`tools/list`), and ACP read operations each consume one
  token of their route kind like any other request

Those capabilities may be added separately without changing the configuration
defined in this document.

## 4. Configuration

A VirtualKey may contain a `rate_limits` block with `llm`, `mcp`, and `agent`
entries:

```yaml
virtualKeys:
  - id: team-a
    description: Shared key for team A
    allowed_route_ids:
      - llm-main
      - mcp-tools
      - acp-codex
      - builtin-researcher
    rate_limits:
      llm:
        requests_per_minute: 60
        burst: 10
      mcp:
        requests_per_minute: 120
        burst: 20
      agent:
        requests_per_minute: 20
        burst: 5
```

The Admin API uses the same JSON field names:

```json
{
  "id": "team-a",
  "rate_limits": {
    "llm": { "requests_per_minute": 60, "burst": 10 },
    "mcp": { "requests_per_minute": 120, "burst": 20 },
    "agent": { "requests_per_minute": 20, "burst": 5 }
  }
}
```

Suggested Go shape:

```go
type RateLimit struct {
    RequestsPerMinute int `json:"requests_per_minute"`
    Burst             int `json:"burst"`
}

type VirtualKeyRateLimits struct {
    LLM   *RateLimit `json:"llm,omitempty"`
    MCP   *RateLimit `json:"mcp,omitempty"`
    Agent *RateLimit `json:"agent,omitempty"`
}

type VirtualKey struct {
    // Existing fields omitted.
    RateLimits *VirtualKeyRateLimits `json:"rate_limits,omitempty"`
}
```

### 4.1 Validation

- An omitted limit means that traffic class is unlimited.
- `requests_per_minute` must be greater than zero when a limit is present.
- `burst` must be greater than zero when a limit is present.
- Unknown fields anywhere inside `rate_limits` are rejected, even though the
  existing general VirtualKey decoder is not strict. Rate limiting is a
  fail-closed policy surface: a misspelled dimension such as `lmm` instead of
  `llm`, or a misspelled setting such as `brust`, must return a configuration
  error rather than silently leaving traffic unlimited. Because
  `DecodeStoredVirtualKey` and the Admin decoder use non-strict
  `json.Unmarshal`, this strictness is new code scoped to the `rate_limits`
  subtree (for example a `UnmarshalJSON` on the policy types, or a
  `DisallowUnknownFields` pass over that subtree), not a tightening of the
  whole VirtualKey decode.
- The VirtualKey secret remains server-generated and is not accepted on update.

Using omission for unlimited traffic avoids assigning two meanings to zero and
makes accidental zero-throughput configurations less likely.

`RateLimits` is a pointer so `omitempty` actually omits the field from JSON and
GatewayBundle output when no policy is configured. There are two VirtualKey
clone sites in `pkg/gateway/virtualkey/manager.go` — `decodeVirtualKeyItem`
(store record -> runtime value) and `cloneVirtualKey` (values returned to
the manager's cache) — and both currently deep-copy only `AllowedRouteIDs`.
Both must also deep-copy `RateLimits` and each non-nil nested `RateLimit`.

Those two clone sites alone are not sufficient to isolate callers from cached
policy pointers. `BaseConfigManager.Get` and `Snapshot` may return cached values
without invoking the configured clone function. `VirtualKeyManager` must
therefore deep-clone every value at its public read boundaries, including
`GetByKey`, `GetByID`, and every item returned by `List`. Internal cache
insertion paths must continue to clone as well. Cached objects, store-decoded
objects, and values returned to callers must never share mutable slices or
policy pointers.

## 5. Runtime Dimensions

Configuration has three entries, but runtime enforcement uses four independent
limiter dimensions:

| Route kind | Configuration | Runtime limiter key |
| --- | --- | --- |
| LLM | `rate_limits.llm` | `(virtual_key_id, llm)` |
| MCP | `rate_limits.mcp` | `(virtual_key_id, mcp)` |
| ACP | `rate_limits.agent` | `(virtual_key_id, acp)` |
| builtin | `rate_limits.agent` | `(virtual_key_id, builtin)` |

ACP and builtin therefore use the same configured rate and burst values but do
not share available capacity. For example, with agent RPM 20, exhausting the
ACP bucket does not prevent the same VirtualKey from starting builtin turns.

> Note: a single `agent` policy produces two independent buckets, so the
> effective aggregate agent admission for a VirtualKey that uses both ACP and
> builtin routes is up to twice the configured `requests_per_minute`. This is
> intentional for the first version — the runtime bucket falls out of the route
> kind for free — but operators sizing an `agent` limit should account for it.
> Its effective aggregate instantaneous capacity is likewise up to twice the
> configured `burst`.

Selecting the runtime bucket is trivial: it is the matched route kind
(`llm` / `mcp` / `acp` / `builtin`), which is already resolved before protocol
dispatch. No per-protocol operation parsing is involved.

All routes of the same runtime dimension share the VirtualKey's bucket. Two LLM
routes used by `team-a`, for example, consume the same `(team-a, llm)` capacity.
This version does not create a bucket for every route, ACP service, or builtin
agent definition.

## 6. Rate and Burst Semantics

Each runtime dimension uses a token bucket:

- `requests_per_minute` is the long-term refill rate.
- `burst` is the maximum stored capacity and therefore the maximum number of
  requests that may pass at once after the bucket has filled.
- Every admitted request consumes one token.
- Rejected requests do not consume another token.
- The gateway does not wait for capacity; it rejects immediately.

For example:

```yaml
requests_per_minute: 60
burst: 10
```

The bucket refills at approximately one request per second and can hold at most
ten tokens. After an idle period, ten simultaneous requests may pass. An
immediate eleventh request is rejected. Approximately one second later, one
more request can pass.

`burst` is not additional per-minute quota. Over time, admission remains
bounded by `requests_per_minute`; burst only controls how concentrated the
traffic may be.

## 7. What Counts as One Request

Admission counts inbound HTTP requests at route-kind granularity, decided
before the protocol operation is parsed. One admitted request to a route of a
given kind consumes exactly one token of that kind's bucket:

- one request matched to an LLM route consumes one LLM token
- one request matched to an MCP route consumes one MCP token
- one request matched to an ACP route consumes one ACP token
- one request matched to a builtin route consumes one builtin token

A streaming request consumes one token when the request is admitted. Stream
duration and the number of emitted events do not consume additional tokens.

Because admission is per inbound request and not per parsed operation, this
first version does not distinguish operation types within a route kind. In
particular:

- MCP protocol handshakes (`initialize`, `tools/list`) and notifications each
  consume one MCP token, not only `tools/call`.
- ACP permission decisions, session listing, and transcript reads each consume
  one ACP token, the same as an ACP turn.
- Malformed requests that reach the dispatcher on a VirtualKey-protected route
  consume one token of the matched route kind, because the limiter runs before
  protocol parsing. This is acceptable and mildly protective: a client cannot
  bypass the limit by sending malformed payloads.
- A request whose host, path prefix, and method match an AgentRouteConfig
  consumes one token even if the protocol handler later rejects its rewritten
  subpath or would otherwise pass it to the next Caddy handler. When its bucket
  is exhausted, the dispatcher returns 429 instead of passing that request
  through. This is an intentional first-version tradeoff required by the single
  central admission point. Operators must not overlap a VirtualKey-protected
  gateway route prefix with unrelated downstream endpoints. Note the
  second-order effect: an admitted request that is then passed to the next
  handler (`serveNextOrNotFound` discards the interaction span) still consumes a
  token but emits no usage event, so that token is spent without an
  observability record. This is acceptable for the first version.

A future version may refine any of these to per-operation accounting.

Only requests that enter through the HTTP dispatcher are counted. In-process
builtin LLM and MCP calls do not re-enter the dispatcher, so a builtin turn
consumes one builtin token and its nested calls consume no additional ingress
tokens. The same rule applies to ACP or any future runtime: an internal action
that does not re-enter the dispatcher is not counted, while an agent process
that explicitly calls a gateway route with a VirtualKey creates a new inbound
request and consumes that route kind's token. Internal calls continue to
produce their normal attributed usage events where supported.

## 8. Request Flow

Admission is a single central check in the dispatcher, placed after VirtualKey
validation and before the route-kind switch that dispatches to a protocol
handler. There is exactly one injection point; the protocol handlers are not
modified.

```text
match route
    -> reject disabled route
    -> resolve and validate VirtualKey
    -> no VirtualKey resolved (route does not require one) -> skip admission
    -> map matched route kind to llm / mcp / acp / builtin bucket
    -> load the matching VirtualKey rate-limit policy (omitted -> unlimited)
    -> allow: consume one token and dispatch to the protocol handler
    -> deny: return 429 without invoking any protocol handler or downstream work
```

VirtualKey validation must happen before rate-limit lookup so invalid secrets
cannot create limiter entries or discover configured limits. The matched route
kind is already resolved at this point, so no protocol parsing is required to
select the bucket.

Admission is skipped entirely when no VirtualKey is resolved. Routes with
`require_virtual_key = false` reach the dispatch switch with a nil VirtualKey
(the `virtualKey != nil` guard already present in the handler), and rate
limiting is keyed by VirtualKey ID, so there is nothing to key a bucket on.
This matches the non-goal of not limiting routes that do not require a
VirtualKey.

The interaction span is already open when admission runs. A denied request
must explicitly call `AddAnnotation("error_type", "rate_limited")` before
writing the 429. The existing deferred span-finish path then records the
outcome, so no new observability pipeline is introduced: `eventSpan.Finish`
falls back to `annotations["error_type"]` when the outcome carries no explicit
error type and the request was not successful. The admission call must
therefore be placed after the dispatcher opens the interaction span and
attaches it to the request context, while still remaining before the route-kind
dispatch switch.

The 429 must be written through the dispatcher's `httpcapture` response
recorder (the same `rec` used by every other dispatch path), not the raw
`http.ResponseWriter`. The deferred finish computes success from
`rec.StatusCode()`; a 429 written past the recorder would leave the recorded
status below 400, mark the interaction successful, and drop the `rate_limited`
annotation.

## 9. Rejection Contract

An exceeded limit returns:

- HTTP status `429 Too Many Requests`
- a `Retry-After` header containing an advisory whole-second estimate of when
  one token may next become available, clamped to at least one second
- a generic gateway JSON error envelope; because admission runs before protocol
  dispatch, the first version does not render a protocol-specific error body
  (this may be refined later using the already-known route protocol)
- an interaction outcome with `error_type = "rate_limited"`
- the resolved `virtual_key_id`, route id, route kind, and route protocol in
  observability dimensions

No downstream provider, MCP service, ACP process, or builtin host call may
start after admission is denied.

Rate-limit errors are gateway admission failures. They must not trigger LLM
candidate fallback, credential rotation, or provider retry behavior.

## 10. Runtime Ownership and Lifecycle

The runtime limiter registry belongs alongside VirtualKey runtime management.
It is keyed by VirtualKey ID and concrete runtime dimension, never by the
secret bearer value.

The registry must:

- serve the request hot path without config-store access
- be safe under concurrent request admission and VirtualKey updates
- create a limiter lazily on first use or eagerly when the key is cached
- replace the complete immutable bucket when rate settings change; a published
  bucket's limiter and refill-rate configuration are never mutated in place
- remove all limiter entries when a VirtualKey is deleted
- clear limiter state when `VirtualKeyManager.Reset` runs
- avoid retaining limiter entries for deleted keys

Updates may reset available burst capacity. Exact preservation of tokens while
changing rate or burst is not required for the first version. Replacing the
complete bucket makes an admission attempt observe either the old configuration
or the new configuration, never a mixture of both. Replacement behavior must
be deterministic and covered by tests.

The implementation may use `golang.org/x/time/rate`. The refill rate must use
floating-point division:

```go
rate.Limit(float64(requestsPerMinute) / 60.0)
```

`burst` maps directly to the token-bucket capacity.

Admission uses `rate.Limiter.AllowN(now, 1)`, which already provides exactly the
semantics §6 requires: it consumes one token if and only if a token is
available at `now`, and consumes nothing when it rejects. It is internally
concurrency safe, so no per-bucket admission mutex, reservation, or
cancellation is needed:

```go
now := clock.Now()
if !bucket.limiter.AllowN(now, 1) {
    // Reject: 429 with Retry-After = max(1, ceil(secondsUntilNextToken)).
    return deniedWithRetryAfter(bucket.retryAfter(now))
}
// Admitted.
return admitted
```

`Retry-After` is advisory and approximate. When admission is rejected the bucket
holds fewer than one token, so the estimated wait until the next token is
`(1 - tokens) / refillRate`, read without consuming through
`limiter.TokensAt(now)`:

```go
func (b *bucket) retryAfter(now time.Time) time.Duration {
    tokens := b.limiter.TokensAt(now)
    missing := max(0.0, 1.0-tokens)
    seconds := missing / float64(b.refillRate)
    return time.Duration(seconds * float64(time.Second))
}
```

A small race between `AllowN` returning false and the `TokensAt` read only
perturbs the hint, never enforcement, which is acceptable for an advisory
header. `refillRate` is immutable and belongs to the same bucket as the limiter,
so the calculation cannot combine a limiter with settings from a concurrent
policy update. The header value is `max(1, ceil(seconds))` so it is never zero.
The clock is injected so refill and `Retry-After` behavior can be tested
deterministically.

This is a deliberate simplification over a reservation/cancellation scheme that
would compute an exact `Retry-After`: `AllowN` is the native fit for
"reject without consuming," and an exact wait is not worth an admission mutex on
the request hot path for the first version.

## 11. Persistence and Management Surfaces

`rate_limits` is part of the persisted VirtualKey object. It must round-trip
through all existing VirtualKey surfaces:

- config-store codec and SQLite JSON records
- VirtualKey Admin create, get, list, and update APIs
- `pkg/adminclient` request and response models
- GatewayBundle validate, apply, export, and comparison logic
- `agwctl` output where VirtualKey policy is displayed

No SQLite schema migration is required because VirtualKey configuration is
stored as JSON. Existing records without `rate_limits` decode as unlimited.

Static Caddyfile and standalone static-bundle VirtualKeys remain unsupported;
this feature does not change the existing VirtualKey configuration boundary.

## 12. Observability

Rate-limited requests should be visible in the normal interaction event stream
with their VirtualKey attribution. The first version does not require a new
usage-event table or a durable counter used for enforcement.

Recommended log fields are:

- `virtual_key_id`
- `route_id`
- `route_kind`
- `rate_limit_dimension`
- `requests_per_minute`
- `burst`
- `retry_after_seconds`

The raw VirtualKey bearer secret must never be logged.

## 13. Multi-Instance Boundary

Limiter state is in process memory. Each gateway process therefore enforces a
separate bucket. With two replicas configured for 60 RPM, the deployment may
admit approximately 120 RPM for the same VirtualKey if traffic is evenly
distributed.

This is an explicit first-version constraint. SQLite must not be used as a
per-request distributed counter because it would add contention and storage IO
to the request hot path. Exact deployment-wide limits require a future shared
limiter backend, such as Redis, or enforcement in an upstream load balancer.

## 14. Test Requirements

The implementation should cover at least:

- omitted policies remain unlimited
- invalid rate and burst values are rejected on create, update, and bundle
  validation
- unknown dimensions and settings inside `rate_limits` are rejected
- an omitted `rate_limits` field remains omitted after Admin and GatewayBundle
  round trips
- `GetByKey`, `GetByID`, and `List` results do not share mutable slices or
  rate-limit pointers with the manager cache, store objects, or one another
- LLM and MCP buckets are independent for one VirtualKey
- ACP and builtin both inherit `agent` settings
- ACP and builtin counters remain independent
- routes of the same kind share one bucket for a VirtualKey
- different VirtualKeys never share buckets
- burst capacity and refill behavior are deterministic under a fake clock
- concurrent admission never exceeds the configured burst
- concurrent denied admissions do not consume tokens or delay future capacity
- a route that does not require a VirtualKey (nil resolved key) bypasses
  admission entirely and is never rate limited
- streaming requests consume exactly one token
- builtin internal LLM and MCP calls do not consume ingress buckets
- an explicit HTTP re-entry from an agent consumes the target route kind's
  ingress token
- matched requests that would later pass through still consume a token and
  return 429 when the bucket is exhausted
- rejected requests return 429 and a `Retry-After` header that is never zero
  (always `>= 1` second)
- the 429 is written through the response recorder so the finished interaction
  records status 429 and the `rate_limited` annotation is preserved
- rejected requests emit `rate_limited` with VirtualKey attribution
- updates atomically replace the applicable immutable bucket; concurrent
  admission observes either the old settings or the new settings, never mixed
  limiter and refill-rate state
- delete and reset remove limiter state
- Admin and GatewayBundle round trips preserve the complete policy

## 15. Implementation Scope

The expected primary change areas are:

- `pkg/gateway/virtualkey`: configuration, validation, and the limiter registry
  keyed by `(virtual_key_id, route_kind)`, including strict decoding for the
  otherwise non-strict `rate_limits` policy subtree and deep-clone support
- `pkg/dispatcher`: one central admission check in the request handler, after
  VirtualKey validation and before the route-kind dispatch switch
- `pkg/admin` and `pkg/adminclient`: management API propagation
- `pkg/gatewaybundle` and `cmd/agwctl`: apply/export/validation propagation
- dispatcher, manager, Admin API, and bundle tests
- user-facing configuration and architecture documentation

Because route matching, VirtualKey resolution, and route-kind selection are
already centralized in the dispatcher, the in-memory first version needs a
single admission call and does not touch the protocol handlers, providers, MCP
services, ACP drivers, or the builtin ADK host.
