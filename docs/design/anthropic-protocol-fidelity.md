# Anthropic Messages Protocol Fidelity Architecture

Status: implemented (2026-08-23)

This document defines the implemented architecture for Anthropic Messages ingress,
the Claude Code (`cc`) ingress profile, protocol-native state preservation, and
Anthropic SSE generation. It replaces incremental handler-specific fixes with
explicit protocol contracts and a testable streaming state machine.

The migration was delivered as independently verified phases:

| Phase | Commit | Result |
|---|---|---|
| 0 | `78c9ba9` | Froze lifecycle, block, usage, and terminal invariants. |
| 1 | `8e90d60` | Centralized response lifecycle ownership and stream encoding. |
| 2 | `ee370a5` | Split standard Anthropic and Claude Code into sibling profiles over one Messages core. |
| 3 | `3344fe0` | Added scoped protocol state, registered codecs, enriched execution, and native relay. |
| 4 | `272e37e` | Derived immutable atomic requirements from the AST and made route filtering generic. |
| 5 | `f2fa3a9` | Proved extension with a second test-only dialect and no routing-core branch. |

A post-implementation review then closed the following regression gaps:

| Commit | Result |
|---|---|
| `f5b716a` | Restored pre-commit HTTP errors and delayed SSE header commitment. |
| `48037a8` | Restored Claude Code tool names inside native relay events. |
| `af55a8b` | Classified encoder, invalid-state, and sink terminal outcomes. |
| `5e466ed` | Made the transition table executable and added fuzz/race coverage. |
| `76ae622` | Made provider dialect registration explicit and deterministic. |
| `291b877` | Hardened relay completeness, usage, metrics, folding, and requirement-gap status. |
| `1e2d252` | Removed replaced paths and made fragment validation explicit. |

The phase exit gates and completion criteria below remain the regression
contract for future changes.

## 1. Problem Statement

The gateway accepts Anthropic Messages requests but may execute them through
providers whose internal model is `eino/schema.Message` or whose upstream wire
protocol is OpenAI-compatible. The generic message model cannot represent every
Anthropic construct, including:

- server-tool definitions and results;
- citations and citation stream deltas;
- signed and redacted thinking state;
- newly introduced or otherwise unknown content blocks;
- exact native history required by a later tool turn.

The current implementation therefore carries both a generic decoded projection
and selected raw Anthropic JSON/SSE state. That is necessary for fidelity, but
the ownership and transition rules are spread across the Anthropic handler,
converter, provider extras, routed-provider checks, and provider-type capability
registration.

The streaming response path has an additional problem: converting generic
provider chunks into Anthropic SSE is a protocol state machine, not a stateless
serialization operation. It must coordinate the HTTP commit point, message
identity and usage, block identity, indexes, ordering, tool argument fragments,
deferred text, native events, finish reasons, keepalives, EOF, and errors.
Encoding these rules as local booleans and closures inside an HTTP handler makes
one correction likely to expose another untested event sequence.

The same-dialect path also incurs avoidable loss. An Anthropic upstream stream
currently passes through a generic message projection and is then rebuilt as
Anthropic SSE. Rebuilding changes indexes and message identity and may discard
message-level events even when the gateway needs no semantic conversion. The
non-streaming path has the same shape: a same-dialect upstream response is
rebuilt from the generic projection, which is why the current non-streaming
response carries an empty message ID. A fidelity architecture therefore needs
both normalized conversion and a validated native relay mode under one
lifecycle owner, for streaming and non-streaming responses alike.

The repeated defects in this area have a common architectural cause:

> protocol fidelity, stream lifecycle, and route eligibility are inferred in
> several layers instead of being represented once and enforced by one owner.

## 2. Goals

The target architecture must:

1. make the Anthropic HTTP, message, content-block, and terminal stream
   lifecycle an explicit, independently tested state machine;
2. keep standard Anthropic and Claude Code ingress separate as profiles while
   sharing one Messages protocol core;
3. represent native protocol state through a dialect-neutral envelope with
   defined raw/decoded overlay semantics;
4. express provider fidelity as atomic protocol features instead of one broad
   native-support flag;
5. calculate route requirements once from the parsed protocol AST and carry
   them with the normalized request;
6. preserve complete same-dialect responses, streaming and non-streaming,
   through a validated native relay mode when only a closed set of fields needs
   rewriting;
7. make streaming and non-streaming responses share one ordered semantic model;
8. fail closed without silently dropping tools, blocks, citations, usage, or
   buffered output;
9. make unsupported routes and fidelity pinning observable and actionable;
10. provide invariant, sequence, replay, fuzz, conformance, and
    captured-traffic tests that cover combinations rather than only happy-path
    examples.

## 3. Non-Goals

This design does not:

- replace eino or require every provider to use a native wire implementation;
- create separate copies of the Anthropic protocol for `anthropic` and `cc`;
- make signed reasoning portable across providers or models;
- add WebSocket transport;
- solve session affinity beyond declaring the requirements that an affinity
  implementation must preserve;
- add attempt-level token accounting for failed fallback candidates. The client
  interaction records only the attempt that served the response; a future
  attempt event/span requires a separate observability design and a provider
  error contract that can report usage;
- introduce legacy compatibility aliases or maintain duplicated handlers,
  lifecycle owners, or serializers after migration. Normalized conversion and
  native relay are two explicit modes of the same response coordinator, not
  independent execution paths;
- add a same-dialect request-body relay. Relay is a response-side mechanism.
  The gateway always reconstructs the outbound request and may apply route-,
  client-, or provider-specific rewrites such as route-model restoration,
  client tool names, token and thinking-budget normalization, and compatibility
  shaping. An upstream request is rebuilt by design, not by accident.
  Request-side fidelity is therefore the job of request-scoped `ProtocolState`
  envelopes and their overlay rules, which preserve unmodeled tool and content
  fields through a rebuild. A future request relay would have to prove that its
  rewrite set is as closed as the response one; that is not assumed here.

## 4. Architectural Decisions

### 4.1 Separate Profiles, Shared Protocol Core

`anthropic` and `cc` are separate ingress profiles, not separate wire
protocols. They must have distinct thin handlers but share parsing, conversion,
native-state handling, and SSE encoding.

Target package ownership:

```text
pkg/dispatcher/llmapi/
  anthropic/
    handler.go                 standard Anthropic HTTP profile
  cc/
    handler.go                 Claude Code HTTP profile and CLI-only shims
  anthropicmsg/
    request.go                 Messages parsing and validation
    converter.go               generic projection and overlay integration
    response_item.go           ordered response item model and reducers
    response.go                 non-stream JSON serializer/coordinator
    stream_encoder.go           message/block/terminal SSE state machine
    stream_event.go             typed provider and downstream event model
    profile.go                 explicit profile contract
```

The exact shared-package name may change during implementation, but it must be
a sibling importable by both handlers. It must not live under an
`anthropic/internal` tree that `cc` cannot import.

The handlers own only HTTP/profile concerns:

```text
anthropic.Handler
  -> standard profile
  -> shared Messages request/converter/response/stream core

cc.Handler
  -> Claude Code profile
  -> CC-only validation/defaults/shims
  -> shared Messages request/converter/response/stream core
```

`cc.Handler` must use explicit composition rather than embedding
`anthropic.Handler`. Embedding hides which behavior is protocol-wide and which
is Claude Code-specific.

The shared profile contract should be narrow. The current proven difference is
the token-counting estimate shim expected by Claude Code, which is a
non-generative endpoint answered before requirement derivation as described in
section 4.5; future profile fields must correspond to observable ingress
behavior such as endpoint availability, request validation, or defaults. Upstream authentication, Claude Code
fingerprint headers, and provider compact/tool-name shaping remain provider
concerns and must not move into the ingress profile merely because the
`claudecode` provider uses them. Content-block lifecycle and SSE ordering are
never profile overrides.

### 4.2 One Response Lifecycle Owner

The shared core owns an `AnthropicStreamEncoder`. It consumes a typed provider
event stream and emits typed Anthropic SSE events through an injected sink. The
input algebra includes generic chunks and every native message-level and
block-level event, including `message_start`, `message_delta`, `message_stop`,
`ping`, `error`, and `content_block_*`. A provider-boundary adapter may convert
legacy `schema.Message` chunks into that algebra during migration, but the
encoder must not inspect message extras to infer event kinds.

The encoder owns the logical response commit point and the complete message,
block, and terminal state. It does not own `http.ResponseWriter`, route
resolution, credentials, provider execution, or a keepalive timer. The HTTP
sink performs the physical header write when the encoder emits its first
committing event.

Illustrative API:

```go
type StreamEventSink interface {
    Emit(context.Context, AnthropicStreamEvent) error
}

type ProviderStreamEvent struct {
    // exactly one typed generic or native event payload
}

// ResolvedExecution is the single routing result for the attempt that actually
// served the request. Credential attribution is retained for observability but
// is never exposed to a protocol encoder or dialect codec.
type ResolvedExecution struct {
    Candidate   ServedCandidate
    Attribution AttemptAttribution
}

type ServedCandidate struct {
    Dialect       ProtocolDialect
    ProviderType  string
    ProviderID    string
    ClientModel   string
    UpstreamModel string
}

type AttemptAttribution struct {
    CredentialID     string
    CredentialSource string
}

type ChatExecution struct {
    Response *provider.ChatResponse
    Resolved ResolvedExecution
}

type StreamExecution struct {
    Stream   *schema.StreamReader[*schema.Message]
    Resolved ResolvedExecution
}

// RoutedChatExecutor is the enriched route-facing contract. RoutedProvider's
// ordinary provider.Provider methods remain adapters for eino and other generic
// callers that do not need execution metadata.
type RoutedChatExecutor interface {
    ExecuteChat(context.Context, *provider.ChatRequest) (*ChatExecution, error)
    ExecuteStreamChat(context.Context, *provider.ChatRequest) (*StreamExecution, error)
}

type ResponseLifecycle interface {
    Committed()
    ObserveUsage(UsageObservation)
    Finish(ResponseFinish) error
    Fail(ResponseFailure) error
    Cancel(ResponseCancellation) error
}

type StreamOpen struct {
    // Candidate contains only the protocol-safe view derived by the response
    // coordinator from StreamExecution.Resolved.
    Candidate ServedCandidate
    // Mode is resolved before the first downstream event from the ingress
    // dialect, the resolved candidate's capability registration, and the
    // rewrite set derived from the request. It is never inferred from response
    // content.
    Mode StreamMode
    // RewriteSet is the closed, request-derived set of declared rewrites.
    RewriteSet RewriteSet
}

type AnthropicStreamEncoder struct {
    // private state
}

func NewAnthropicStreamEncoder(
    StreamEncoderOptions,
    StreamEventSink,
    ResponseLifecycle,
) *AnthropicStreamEncoder
func (*AnthropicStreamEncoder) Open(StreamOpen) error
func (*AnthropicStreamEncoder) Accept(ProviderStreamEvent) error
func (*AnthropicStreamEncoder) Finish(StreamFinish) error
func (*AnthropicStreamEncoder) Fail(StreamFailure) error
func (*AnthropicStreamEncoder) Cancel(StreamCancellation) error
```

The dispatcher creates one `ResponseLifecycle` before handing an LLM request to
the shared response coordinator. For a streaming provider execution, the
coordinator calls `ExecuteStreamChat`, receives one `StreamExecution`, derives
mode and `StreamOpen` from its single `ResolvedExecution`, then reads typed
events. An open failure is delivered to the coordinator while the response is
still uncommitted. Unit tests use an in-memory sink and lifecycle recorder and
do not need an HTTP recorder.

`ResolvedExecution` is a required routing-layer output, not something the
encoder may infer. The routing layer resolves the model target, may fall back
across candidates, and selects a credential inside one enriched execution call.
Today its `provider.Provider` methods return only the response or stream and
discard the attempt that served it. The new `ExecuteChat` and
`ExecuteStreamChat` methods return that metadata without changing the base
provider or eino interfaces. Their ordinary `Chat` and `StreamChat` adapters
unwrap the enriched result for callers that do not need it. A protocol handler
must use the enriched contract; placing the result on a response message would
arrive too late to select stream relay mode.

The neutral result values and optional `RoutedChatExecutor` interface live in
`pkg/llm/provider`, where both the gateway and protocol handlers can depend on
them without a gateway/dispatcher import cycle. They contain no route object or
Anthropic payload. `pkg/gateway.RoutedProvider` implements the enriched
interface; base providers do not. The provider-execution entry point of an LLM
API handler accepts that enriched interface because every direct or logical LLM
route is already represented by a `RoutedProvider`. The eino bridge continues to
consume its ordinary `provider.Provider` adapter.

For the same reason the routing layer must stop rewriting `ChatRequest.Model` in
place. The upstream model belongs on the outbound clone and in
`ResolvedExecution.UpstreamModel`. Mutating the caller's request destroys the
client-visible model that invariant A2 requires the encoder to emit and that
model-name restoration is defined against.

The lifecycle is the exactly-once response finalizer shared by provider and
local execution and by stream and batch transport. It owns outcome, last-known
usage, commit metadata, and terminal observability, but it does not own provider
execution or content-block state. The stream encoder closes or reports block
state before the coordinator delegates the terminal result to the lifecycle.
The batch coordinator has no block drain and delegates after encoding and sink
delivery. A second terminal call is a typed state error, not a second interaction
outcome.

Exactly-once has one semantic caller. Provider errors, context cancellation,
EOF, and sink errors are typed inputs to the response coordinator; a provider's
producer goroutine never calls the lifecycle. The coordinator serializes those
signals with encoder state, drains or reports blocks and buffers, observes the
last available usage, and only then chooses one terminal method. Lifecycle
synchronization is a defensive guard against an implementation defect, not a
normal multi-writer control path. A losing accidental caller receives the typed
state error, and a partially written outcome or usage record is a defect.

The HTTP sinks notify `Committed` at the point response headers become
irreversible; encoders do not infer commitment merely from an attempted write.
The dispatcher creates the interaction span and explicitly transfers its finish
ownership to `ResponseLifecycle` immediately before calling the provider or
local response coordinator. Before that transfer, dispatcher preparation errors
use the dispatcher's normal finalizer. After it, the generic dispatcher defer is
disabled for that request and the lifecycle is the only caller of
`InteractionSpan.Finish`. The lifecycle enriches that existing span and never
starts a second interaction event.

#### State model

The encoder has explicit response and terminal states as well as at most one
active downstream content block:

```text
uncommitted
  -> stream_opened
  -> message_started
       -> idle
       -> reasoning
       -> text
       -> tool_use
       -> native_block
       -> idle
  -> completed
   | upstream_open_error | upstream_stream_error
   | client_cancel | invalid_state | sink_error
```

The terminal names are the same closed set that section 4.6 records as
`response_outcome`; the non-streaming coordinator uses `upstream_error` in place
of the open/stream split, which only a stream can distinguish.

Opening the provider stream must precede the HTTP success response. If opening
fails, the handler can still return the mapped upstream HTTP error. In native
relay mode, the first upstream `message_start` supplies the message ID, model,
and initial usage. In normalized mode, the encoder creates a collision-free
gateway message ID and commits a synthesized `message_start` no later than the
first meaningful generic output. An upstream failure after commitment is an SSE
error and a terminal outcome, not a different HTTP status.

Synthesized identifiers must be dialect-valid, collision-free across concurrent
responses, opaque to clients, and stable across a history round trip. In
particular, a synthesized `tool_use.id` is echoed back as
`tool_result.tool_use_id` on the next turn and must still resolve. The Anthropic
codec may currently generate conventional `msg_` and `toolu_` prefixes, but the
prefix is codec policy backed by captured-client fixtures rather than a
cross-version core invariant. `message_id_source` records whether the identity
came from upstream or from the gateway.

Initial usage follows an explicit authority order. Native relay preserves the
upstream `message_start` usage exactly. Normalized mode uses provider-reported
initial usage when the typed stream supplies it; otherwise it may use a
request-time tokenizer estimate. If neither exists, it emits the
protocol-required numeric fallback and records `usage_source=unavailable`.
The encoder never buffers the whole response merely to replace initial usage
with a final count, and documentation must not describe an estimate or fallback
as exact. Final usage still updates from later provider events.

The committing event set is closed. In both modes the normal committing event
is `message_start`. A relayed stream must begin with `message_start` under the
current Anthropic event contract; the gateway does not commit merely because it
received an arbitrary upstream event.

`ping` is explicitly not a committing event. A keepalive must never move the
encoder out of `stream_opened`, because committing the response for a keepalive
would destroy the pre-commit HTTP error path that invariant A1 exists to
protect. A ping requested while the response is uncommitted is dropped; a
pre-commit stall is a transport-coordinator timeout that resolves as an HTTP
status, not as an empty committed stream.

A pre-message upstream `error` is mapped to an HTTP error while the downstream
response remains uncommitted. A pre-message unknown event is a typed
`invalid_state` error under the current dialect contract. After
`message_started`, upstream `ping` and unknown events are preserved without
changing message or block state. Supporting a future protocol version that
legally moves an event before `message_start` requires an explicit dialect-codec
update rather than an implicit change to the commit boundary.

Tool fragments may be buffered for several upstream tool indexes, but buffered
upstream calls are not the same as open downstream content blocks. Parallel
OpenAI-style tool calls are serialized into Anthropic blocks in deterministic
first-seen order.

Every transition is centralized. No caller may write a `content_block_*` event
directly.

#### Required invariants

Invariants are grouped by the mode that must enforce them. Normalized mode
constructs the downstream stream and therefore owns construction rules. Native
relay preserves an upstream stream and therefore validates only what is
structurally impossible to serialize.

**Group A — stream transport and terminal invariants, enforced in both modes**

A1. an HTTP success response is not committed before the provider stream opens;
A2. `message_start` is emitted exactly once before content events and carries a
    non-empty ID, the client-visible model, and protocol-valid initial usage;
    the encoder separately records whether its authority is exact, estimated,
    or unavailable;
A3. `message_delta` and `message_stop` occur only after every block the mode
    tracks is closed;
A4. message ID, stop reason, and all supported usage fields have one authority;
A5. EOF, cancellation, provider errors, invalid state, and sink errors each have
    an explicit terminal and buffer policy;
A6. every terminal path records exactly one outcome and its last known usage
    when any usage was observed;
A7. no meaningful buffered tool, text, native content, or usage is silently
    discarded.

**Group B — normalized-mode construction invariants**

B1. at most one downstream content block is open;
B2. every block index is allocated once and increases monotonically;
B3. every started block is stopped exactly once before normal termination;
B4. `tool_use` start contains a non-empty `id` and `name`;
B5. fragments for one upstream tool index produce at most one downstream
    `tool_use` block;
B6. text never splits one tool call into duplicate blocks.

**Group C — native relay validation invariants**

C1. `content_block_start` never repeats an index that is already open;
C2. no delta or stop references an index that was never started or is already
    stopped;
C3. a relayed `tool_use` start carries a non-empty `id` and `name`;
C4. any index still open at termination is named by the terminal routine;
C5. the current Anthropic codec permits at most one open content block.

**Group D — non-streaming response invariants, enforced in both modes**

Group A is phrased for the stream transport. The non-streaming coordinator has
no commit boundary and no incremental block lifecycle, so it enforces a shorter
list:

D1. the response carries a non-empty message ID from the same authority as the
    streaming path;
D2. stop reason and all supported usage fields have one authority;
D3. exactly one `response_outcome` is recorded through `ResponseLifecycle`,
    together with the last known usage when any usage was observed;
D4. no decoded block, tool, or unmodeled field is dropped without being named in
    a typed error.

Relay does not enforce normalized construction rules merely because they are
convenient for the generic adapter. Under the current Anthropic event contract,
however, content blocks form an ordered series of complete
start/delta/stop lifecycles, so the dialect codec also rejects overlapping open
blocks. Forward compatibility with unknown block and event types means retaining
unknown payloads inside that lifecycle; it does not silently redefine current
ordering rules. If a later protocol version permits overlapping blocks, its
versioned codec and fixtures may relax that validation without changing
normalized-mode construction.

#### Tool-call completion and missing identity

The encoder buffers tool calls by upstream index. A tool becomes emit-ready
only when its name is known and its accumulated arguments form a complete JSON
value at a valid boundary. Boundaries include an upstream finish signal, a
transition to text/native content, or the completion of arguments after text
was already deferred.

If an otherwise valid tool call has no upstream ID, the encoder creates a
gateway-local ID through the dialect-valid generator described above. A missing
name cannot be repaired safely and must produce a protocol error, not a
debug-only drop.

If text arrives while tool arguments are incomplete, the encoder buffers the
text. As soon as the pending tool calls become emit-ready, it serializes and
closes them, then immediately flushes buffered text. It does not wait for stream
EOF when a safe boundary is already known.

Buffering is bounded. Deferred text and accumulated tool arguments each have a
documented byte limit, because an upstream that never completes a tool call
would otherwise grow encoder memory without limit. Exceeding a limit is a
defined transition, not an implementation detail: the encoder terminates with
`invalid_state` and names the overflowing buffer kind and byte count in the
terminal error rather than emitting a partial `tool_use` input or dropping the
buffer silently. The error and metrics never include buffered text or tool
arguments.

#### Normalized and native relay modes

Native Anthropic stream events enter the same state machine. They are never
written through an unvalidated bypass. The mode is an `Open` input, resolved
once from the ingress dialect, the provider capability registration, and the
rewrite set derived from the request. It is never inferred by observing
response content, so the selected mode is reproducible from the request alone
and a fixture can pin it:

- **normalized mode** consumes generic output and partial native envelopes,
  allocates downstream indexes, and serializes parallel generic tool calls;
- **native relay mode** consumes a complete ordered Anthropic event stream,
  preserves upstream message identity and block indexes, validates their
  lifecycle, and applies only a closed set of declared rewrites.

Native relay is eligible only when ingress and upstream use the same dialect,
the served candidate in `ResolvedExecution` declares the mode-specific native
stream-event or response-body capability, and the requested gateway
transformations fit the closed rewrite set. Eligibility is evaluated against the
candidate that actually served the request, never against the route's candidate
set: fallback may have moved execution to a provider with weaker fidelity, and
relaying on the strength of a candidate that did not answer would forward
rebuilt content as if it were preserved. Initial rewrites are limited to
restoring the client-visible route model and client tool names. Provider
authentication, headers, policy, and usage accounting still run; relay does not
mean raw socket proxying.

The relay path retains each event payload as the fidelity authority. A shared
dialect codec reads selected fields such as message identity, usage, block
index, and event kind for validation and observability without rebuilding the
payload. An invalid lifecycle produces a typed terminal error; it is never
silently repaired by reallocating an upstream index.

Because relay commits early, it cannot fall back. Eligibility must therefore be
provable from the request and the capability registration alone. If a relayed
event contains a field known to have undergone a provider-side rewrite but the
declared inverse transformation is unavailable, the encoder terminates with
`invalid_state` and reports the offending field. A tool name merely absent from
a restoration map is not itself an error: it may already be the client name or
belong to a native server tool. The encoder does not forward a value known to be
unrestored and does not switch modes mid-response. Rewrite-set mismatch is an
explicit conformance case, not an assumed impossibility.

Native relay is a response mode, not a streaming-only mode. A same-dialect
non-streaming response is relayed by retaining the upstream response body as the
fidelity authority and applying the same closed rewrite set, so `stream: false`
and `stream: true` reach the same fidelity. Without that counterpart the
stream/non-stream equivalence contract could only be satisfied at the weaker of
the two paths.

Keepalive scheduling belongs to the HTTP transport/session coordinator because
it owns connection time and cancellation. The encoder accepts a typed `ping`
event, preserves upstream pings in relay mode, and emits a requested gateway
ping without changing message or block state and without committing the
response.

#### Shared response semantics

Streaming and non-streaming paths use the same ordered Anthropic response-item
model, overlay rules, and ordering reducer. The non-streaming path materializes
the sequence and serializes a JSON content array. The streaming path reduces it
incrementally into SSE events. Native relay may retain raw event payloads or a
raw response body as the fidelity authority, but its decoded semantic sequence
must satisfy the same response contract in either form.

Non-streaming relay has an explicit provider and encoder boundary; it is not
implemented by reaching into a concrete provider or recovering bytes from a
generic message extra:

```go
type ChatResponse struct {
    Message *schema.Message // carries ProtocolState; see section 4.3
}

type ResponseOpen struct {
    Candidate  ServedCandidate
    Mode       ResponseMode
    RewriteSet RewriteSet
}

// ResponseBody names status, media type, and payload instead of passing them
// positionally, so a sink cannot be called with transposed arguments.
type ResponseBody struct {
    Status      int
    ContentType string
    Payload     []byte
}

type ResponseBodySink interface {
    Emit(context.Context, ResponseBody) error
}

func NewAnthropicResponseEncoder(
    ResponseEncoderOptions,
    ResponseLifecycle,
) *AnthropicResponseEncoder

func (*AnthropicResponseEncoder) Emit(
    context.Context,
    ResponseOpen,
    *provider.ChatResponse,
    ResponseBodySink,
) error
```

The concrete names are illustrative, but the contract is mandatory:
the batch response coordinator calls `ExecuteChat`, retains the one returned
`ResolvedExecution`, and derives `ResponseOpen.Candidate`, lifecycle attribution,
and mode selection from it. Callers do not pass independent copies of resolved
metadata to the encoder and lifecycle.

`Provider.Chat` must be able to return the raw successful response body and its
complete modeled-field baseline digests through the same dialect-neutral
`ProtocolState` envelope used elsewhere, attached to the returned message
rather than beside it. A second raw-body container or provider-specific extra
would create another source of truth. The batch encoder owns mode validation,
declared rewrites, message-ID policy, JSON output, and sink delivery, so encoding
and write failures reach the shared lifecycle. A provider error before `Emit`
is reported by the handler through the same lifecycle created before
`Provider.Chat`. The stream encoder owns the corresponding incremental
lifecycle. Both compose the same response-item reducer and dialect codec under
the shared response coordinator; neither handler serializes a provider response
directly.

### 4.3 Explicit Protocol State Envelope

Raw protocol state remains necessary, but it must have a documented ownership
model instead of dialect-specific extra keys.

Target provider request, message, and response metadata:

```go
type ChatRequest struct {
    Model         string
    Messages      []*schema.Message
    Options       []einomodel.Option
    ProtocolState *ProtocolState // request-scoped state and requirements
}

type ProtocolState struct {
    Envelopes    []NativeEnvelope
    Requirements ProtocolRequirementSet
}

type NativeStateScope string

const (
    NativeScopeRequest           NativeStateScope = "request"
    NativeScopeMessageHistory    NativeStateScope = "message_history"
    NativeScopeResponseEphemeral NativeStateScope = "response_ephemeral"
    NativeScopeStreamEvent       NativeStateScope = "stream_event"
)

type ModeledFieldBaseline struct {
    Path    string
    Present bool
    Digest  [32]byte
}

type NativeEnvelope struct {
    Dialect   ProtocolDialect
    Scope     NativeStateScope
    Kind      NativeStateKind
    Location  NativeLocation
    Raw       json.RawMessage
    Baselines []ModeledFieldBaseline
}
```

Conceptually:

- `Raw` is the fidelity authority for fields the generic model cannot express;
- the generic/typed projection is the semantic authority for explicit gateway
  or client modifications;
- `Baselines` records canonical digests for every modeled field that can change,
  including presence, so replay can detect intentional changes without storing
  a second full copy of text, reasoning, or tool arguments;
- `Scope` defines the envelope's carrier, retention, merge, and consumption
  rules;
- `Location` associates state with a request tool ordinal, message, content
  block, whole non-streaming response body, message-level stream event, or
  stream source index without relying on parallel unkeyed slices.

Replay uses the existing raw-plus-decoded differential overlay principle:

1. decode the original object when possible;
2. project every currently modeled mutable field through the dialect codec,
   canonicalize it, and compare its presence and digest with the baseline;
3. overlay only fields that changed intentionally;
4. retain unknown and unmodeled raw fields;
5. fail closed or retain an opaque envelope if decoding is impossible.

This promises field-for-field preservation, not byte-for-byte preservation;
JSON key order is not significant and may change.

The baseline set is complete for modeled mutable fields, not merely fields the
gateway currently expects to rewrite. It includes text, reasoning, tool IDs,
tool names, tool arguments, model, index/order, and presence of optional modeled
fields. Canonical JSON hashing avoids retaining a second full payload while
still detecting transformations performed by eino, an agent, or a provider.
Adding a modeled field requires adding its baseline path and overlay rule in the
same change. A digest collision is treated according to the repository's chosen
cryptographic hash guarantees; raw values are never logged as a fallback.

#### Scope-specific carriers and lifetime

There is one `ProtocolState` model but two scope-appropriate carriers:

- `ChatRequest.ProtocolState` is authoritative for request-wide envelopes such
  as tools and tool choice and is the only carrier allowed to contain
  `Requirements`;
- message/history/response/stream envelopes travel under one well-known
  dialect-neutral key in the `schema.Message.Extra` of the message they
  describe. A message-carried state must have an empty requirement set.

Request state must never be attached to an arbitrary system or user message.
Message removal, insertion, or reordering must not change request tools or route
eligibility. The normalizer creates `ChatRequest.ProtocolState` from the parsed
AST, while internal callers use the same normalizer or an explicit typed
request-state adapter.

The message carrier is a deliberate constraint of the eino boundary:

- `Provider.StreamChat` yields `*schema.StreamReader[*schema.Message]`. The
  streaming path has no response wrapper, so native stream state needs a message
  carrier once the current per-dialect extras are removed;
- `einomodel.ChatModel` must satisfy eino's `model.ToolCallingChatModel`, whose
  `Generate` returns `*schema.Message`. Response state placed only beside the
  message would be discarded there, silently stripping history fidelity.

The request carrier meets the same boundary from the other side.
`model.ToolCallingChatModel` passes only `[]*schema.Message` and
`...einomodel.Option`, so an in-process builtin-agent turn has no request
wrapper in which to place `ChatRequest.ProtocolState`. Request-scoped state
therefore crosses the eino boundary as one impl-specific option, exactly as the
current native tool union already does, and request resolution lands it on
`ChatRequest.ProtocolState`. Providers read only the resolved field; the option
is transport, not a second authority. Without this rule the field would be
populatable by HTTP ingress alone and every builtin-agent turn would silently
lose request-side fidelity.

This replaces `ChatExtraFields.AnthropicTools` and
`ChatExtraFields.AnthropicToolChoice`, which are today's request-scoped native
envelopes in dialect-specific form. They become `NativeEnvelope` values with
`Scope: request` and a tool-ordinal `Location`, and their current readers move to
the resolved `ProtocolState`. Keeping both would leave two request-side
authorities for the same native tool union, which is the condition this section
exists to remove.

The envelope scope controls retention:

- `request` envelopes live only on `ChatRequest` and are consumed while building
  the upstream request;
- `response_ephemeral` contains a whole non-streaming body only until the batch
  encoder emits the client response or an eino adapter converts the result for
  generic use;
- `stream_event` contains ordered raw events only while a stream is being relayed
  or materialized;
- `message_history` is the persistent form retained on assistant messages and
  replayed in later turns.

Before `einomodel.Generate` returns, the dialect codec derives ordered
`message_history` block envelopes from `response_ephemeral` and removes the
whole-body envelope. Stream materialization similarly folds `stream_event`
envelopes into persistent history blocks and removes transport-only
`message_start`, `ping`, `message_delta`, `message_stop`, and error envelopes.
The HTTP response encoders may consume ephemeral state without mutating the
provider result, because an in-process caller may still need the history form.
No complete response body or full SSE event log remains attached to every later
conversation turn.

The registered stream concat function has a closed merge algebra:

1. preserve input group and envelope order;
2. require message-carried `Requirements` to be empty;
3. reject duplicate `(dialect, scope, kind, location)` values unless the
   dialect codec defines an ordered fragment merge for that kind;
4. reject conflicting response-body envelopes and mixed dialect lifecycle
   events that cannot form one response;
5. fold stream events through the typed dialect reducer into
   `message_history`, then discard transport-only envelopes;
6. return a controlled concat error on invalid state; never take last-writer
   wins and never silently omit an envelope.

A provider or transform that rewrites or replaces a message must carry its
message-scoped `ProtocolState` forward and run differential overlay, or return a
typed error proving why an equivalent generic representation makes each
envelope unnecessary. An unqualified explicit drop is not allowed.

The migration therefore replaces the per-dialect
`_agent_gateway_anthropic_*` keys with one neutral message key, replaces the
dialect-specific request fields on `ChatExtraFields` with request-scoped
envelopes, and adds the resolved `ChatRequest.ProtocolState` field. The goal is
one state model with explicit scopes, not one physical carrier for unrelated
lifetimes.

The repository change policy does not require permanent dual-read or legacy
aliases for the removed per-dialect keys.

Package ownership follows the dependency boundary:

- the root `pkg/llm/provider` package owns only dialect-neutral envelope,
  requirement, capability, and registry contracts;
- the shared Messages package owns the ingress AST, validation, requirement
  derivation, ordered response-item model, and downstream event semantics;
- `anthropicbase` or a dedicated lower-level Anthropic dialect package owns
  upstream wire decoding, raw overlay/replay, and native event codecs shared by
  Anthropic-capable providers and ingress;
- the route layer sees only immutable requirement and capability sets.

The generic provider package must contain no Anthropic JSON field inspection or
tool/content detector. Moving all upstream codecs into the dispatcher package
would invert the dependency for providers, so dialect wire codecs remain in a
shared lower-level dialect package rather than in an HTTP handler.

#### Dialect codec registry

That boundary creates an obligation the rest of this section depends on. Several
dialect-neutral components must run dialect-specific logic: the `einomodel`
bridge folds `response_ephemeral` into `message_history` before returning, the
registered stream-chunk concat function folds `stream_event` through the typed
reducer, and the merge algebra defers to the codec for ordered fragment merges.
None of them may import a dialect package, so the neutral provider package owns a
codec registry keyed by `ProtocolDialect`:

```go
// ModeledProjection is an opaque dialect-owned typed value. Neutral callers
// carry it back to the same registered codec and never inspect it.
type ModeledProjection any

type NativeCaptureInput struct {
    Scope    NativeStateScope
    Kind     NativeStateKind
    Location NativeLocation
    Raw      json.RawMessage
    Modeled  ModeledProjection
}

type NativeOverlayInput struct {
    Envelope NativeEnvelope
    Current  ModeledProjection
}

type DialectCodec interface {
    Capture(NativeCaptureInput) (NativeEnvelope, error)
    Overlay(NativeOverlayInput) (json.RawMessage, error)
    FoldResponse([]NativeEnvelope) ([]NativeEnvelope, error)
    FoldStreamEvents([]NativeEnvelope) ([]NativeEnvelope, error)
    ValidateFragments(NativeStateKind, []NativeEnvelope) error
    ValidateOrder([]NativeEnvelope) error
}

func RegisterDialectCodec(ProtocolDialect, DialectCodec)
```

Codec registration follows the repository's existing factory rule: the dialect
package registers itself in `init`, and every binary that links it adds a blank
import in `cmd/agw`, `cmd/agwd`, and `cmd/agwctl`. This is stated rather than
left implicit because a missing blank import does not fail the build. It surfaces
at runtime as an unregistered dialect, which is exactly the silent fidelity
degradation this document exists to prevent. An envelope whose dialect has no
registered codec is a controlled error at capture, overlay, fold, merge, and
validation time; it is never dropped and never passed through unvalidated.

`Capture` validates the dialect-owned `ModeledProjection`, derives every
presence-aware baseline from it, and returns an envelope whose raw value is the
fidelity authority. `Overlay` validates the current projection, compares all
modeled fields with those baselines, and returns replayable raw JSON with only
intentional changes applied. It is therefore the single executable home of the
capture/differential-overlay/replay contract described above; neutral callers do
not compute baselines or edit raw JSON themselves. A projection of the wrong
dialect-owned type is a controlled error.

### 4.4 Fine-Grained Dialect Capabilities

`NativeDialects` is too broad to describe a provider that can send an Anthropic
server-tool request but cannot preserve its response or replay the next turn.
The initial request-requirement vocabulary contains only the four distinctions
already exercised by current behavior:

```go
type ProtocolFeature string

const (
    FeatureServerToolRequest    ProtocolFeature = "server_tool_request"
    FeatureNativeResponse       ProtocolFeature = "native_response"
    FeatureNativeHistoryReplay  ProtocolFeature = "native_history_replay"
    FeatureReasoningReplay      ProtocolFeature = "reasoning_replay"

    // mode selection only; never derived from a request
    FeatureNativeStreamRelay    ProtocolFeature = "native_stream_relay"
    FeatureNativeBodyRelay      ProtocolFeature = "native_body_relay"
)

// ProtocolFeatureClass separates what a request may require from what only
// selects an execution mode, while keeping both in one registry.
type ProtocolFeatureClass string

const (
    FeatureClassRequirement   ProtocolFeatureClass = "requirement"
    FeatureClassModeSelection ProtocolFeatureClass = "mode_selection"
)

type ProtocolFeatureDefinition struct {
    Class ProtocolFeatureClass
}

type ProtocolFeatureRegistry map[ProtocolFeature]ProtocolFeatureDefinition
type DialectCapabilities map[ProtocolDialect]map[ProtocolFeature]struct{}
```

Every closed feature is registered once with its class. Registration rejects an
unknown feature in a provider support set, a duplicate definition, or a class
change. The class is enforced in both directions: constructing a
`ProtocolRequirementSet` rejects any feature that is not
`FeatureClassRequirement`, so requirement derivation cannot smuggle a
mode-selection feature into route filtering. The initial registry assigns the
first four constants above to `FeatureClassRequirement` and the two relay
constants to `FeatureClassModeSelection`; the class is therefore executable
metadata, not a comment or naming convention.

`ProtocolRequirementSet` is the immutable value that carries those checks. It
also records why each feature was required, so section 4.6 can report a
rejection without re-deriving it and section 4.5 can keep derivation in one
place:

```go
type RequirementReason string

// ProtocolRequirementSet is immutable after construction and contains only
// FeatureClassRequirement features.
type ProtocolRequirementSet struct {
    // private: feature -> ordered, deduplicated reasons
}

func NewProtocolRequirementSet(
    ProtocolFeatureRegistry,
    map[ProtocolFeature][]RequirementReason,
) (ProtocolRequirementSet, error)

func (ProtocolRequirementSet) Features() []ProtocolFeature
func (ProtocolRequirementSet) Reasons(ProtocolFeature) []RequirementReason

type CandidateIdentity struct {
    ProviderID    string
    ProviderType  string
    UpstreamModel string
}

// RequirementGap is what candidate filtering returns instead of a bare
// "no candidate" error.
type RequirementGap struct {
    Candidate CandidateIdentity
    Missing   []ProtocolFeature
}
```

The constructor is the single enforcement point for the class rule and for
immutability: it defensively copies inputs, sorts features, and deduplicates each
ordered reason list; accessors return copies. There is no setter and no
post-construction merge. Route filtering consumes `Features()`, diagnostics
consume `Reasons`, and rejection responses and debug logs consume
`RequirementGap`. Metrics aggregate only bounded feature and reason codes;
provider IDs and upstream model names are never metric labels.

Names are illustrative; implementation must choose a closed, documented set.
Do not introduce a growing family of `RequireAnthropicX` or `RequireOpenAIX`
fields. Citations and opaque content initially derive the appropriate native
response/history requirements instead of creating speculative feature names.
New atomic features are added only when a real provider or second dialect
demonstrates a routing distinction that the existing set cannot express.

Complete native event delivery and complete native response-body delivery are
separate provider transport capabilities used to select streaming and
non-streaming relay respectively. Neither is inferred as an ordinary request
requirement merely because the client selected a response mode. If a future
request semantically requires exact native event timing or shape, that
requirement must be introduced explicitly rather than overloading
`RequireStreaming`.

Both mode features are registered in the same `DialectCapabilities` set as
requirement features. Their definitions in the single
`ProtocolFeatureRegistry` assign `FeatureClassModeSelection`; the four request
features are assigned `FeatureClassRequirement`. Route set inclusion considers
only requirement-class features, while the response coordinator consults the
mode-selection class after provider selection. A separate parallel registry for
transport capabilities must not be introduced; one definition registry plus one
provider support set is the source of truth.

Provider-type registration remains the pre-construction source of truth. Each
provider type declares one explicit `ProtocolDialect`; registration rejects a
feature from another dialect, so served-candidate mode selection never depends
on map iteration order.
Runtime marker interfaces must not duplicate it.

Feature declarations describe end-to-end fidelity, not merely request
serialization. For example, a provider that can place a web-search declaration
on the upstream request but loses the result block does not satisfy a streaming
server-tool interaction that requires native response and history replay.

Capability dependency validation runs at provider registration. Examples:

- `native_history_replay` requires the provider to retain native responses;
- native stream relay requires complete message-level and block-level event
  delivery;
- native body relay requires the raw successful response body and complete
  modeled-field baselines on the response message's `ProtocolState`;
- authentic reasoning replay remains model/provider-affine even when its
  structure fits a generic reasoning part.

Dependency validation is necessary but cannot prove that an implementation
matches its declaration. Every declared feature must opt the provider into a
feature-indexed conformance suite covering the applicable request,
non-streaming response, streaming response, and history replay round trips. CI
enforces that suite; registration metadata remains the sole runtime source of
truth.

### 4.5 Calculate Requirements Once

Protocol parsing produces a `ProtocolRequirementSet` directly from the parsed
`MessagesRequest` AST and attaches it to the normalized request before route
resolution. The set records why each feature is required, for example request
server tools, assistant history citations, opaque content, or signed reasoning.
Requirement derivation happens before conversion to `schema.Message`; it must
never inspect generic message extras to rediscover Anthropic state.

Requirement derivation applies only to endpoints that execute a provider.
`/v1/messages/count_tokens` is non-generative: it must branch before route
target resolution, must not derive fidelity requirements, must not filter route
candidates, and must not select or charge a credential. The implemented handler
sets `ExecutionLocal` during preparation, so a `count_tokens` request carrying
server tools or native history bypasses provider routing while retaining normal
ingress governance. Any future
non-generative endpoint follows the same rule.

This branch is expressed through a protocol-neutral dispatcher contract, not an
Anthropic path check in the dispatcher:

```go
type ExecutionDisposition string

const (
    ExecutionProvider ExecutionDisposition = "provider"
    ExecutionLocal    ExecutionDisposition = "local"
)

type PreparedLLMApiRequest struct {
    Disposition ExecutionDisposition
    // existing normalized request fields
}
```

After route matching, request-size enforcement, VirtualKey validation, and
protocol parsing, the dispatcher sends `ExecutionLocal` directly to an explicit
local handler entry point. It does not construct a `RoutedProvider`. Only an
`ExecutionProvider` request carries `ProtocolRequirementSet` into target,
candidate, and credential selection. The handler interface may expose
`ServeLocalLLMApi` and `ServeProviderLLMApi`, or an equivalent typed execution
object, but it must not encode local execution by passing a misleading non-nil
provider or by reparsing the URL later.

Skipping provider and credential selection does not mean skipping governance.
An `ExecutionLocal` request still passes VirtualKey validation, request-size
limits, and rate limiting, and still records one interaction event carrying the
route and VirtualKey. Execution disposition and response transport are separate
closed dimensions:

```text
execution = provider | local
transport = stream | batch
```

`count_tokens` is `execution=local, transport=batch`; a future local streaming
endpoint would be `execution=local, transport=stream`. Local execution
terminates through the same `ResponseLifecycle` as provider execution rather
than through a private outcome recorder, but it has no `ResolvedExecution` and
never emits a provider-open transition. A successful local token estimate
records its value with `usage_source=estimated`. A local failure such
as an unsupported endpoint records the outcome without inventing token usage,
and a future local endpoint with no token semantics does the same. A locally
answered endpoint must not become an unmetered compute surface with no trace in
observability. Local execution enriches the dispatcher-created interaction span;
it does not open a second span or emit a duplicate interaction event.

The route layer performs only generic set inclusion:

```text
request requirements subset-of provider-type capabilities
```

It must not inspect Anthropic JSON or contain an Anthropic-specific `if` branch.

Internal callers that construct `provider.ChatRequest` without an HTTP handler
must pass through the same protocol parser/normalizer or attach a complete typed
request-scoped `ProtocolState` through an explicit dialect adapter. Message
history state is collected from the individual message carriers, but it never
substitutes for the request-wide state or requirements. Until all internal
callers do so, one registered dialect inspector may provide a temporary
execution-time assertion. That assertion must compare against the attached
requirements and report disagreement; it must not become a second silent source
of truth.

The handler must not recover a missing or invalid prepared request by reparsing
the body and then executing an already selected provider without applying the
new requirements. It either requires a valid prepared request or reruns the
same preparation and provider-eligibility validation as the normal dispatcher
path. Any disagreement fails closed.

The requirement set is immutable after candidate selection. A provider may not
discover a stronger requirement only after a credential has been selected or
charged.

### 4.6 Failure and Observability Contract

The architecture is fail-closed for fidelity but never fail-silent.

- malformed client input returns `400` before provider selection;
- an unsupported direct provider returns `501` with the missing dialect
  features and an actionable alternative when known;
- a logical route with no candidate returns the requested features and the
  rejection reason counts;
- malformed upstream tool identity or an impossible state transition emits a
  typed stream error and records the encoder state;
- undecodable native content remains opaque and pins routing rather than being
  treated as generic;
- buffered text, tools, and native blocks are either emitted or named in the
  terminal error; they are never debug-only drops.

`Finish`, `Fail`, and `Cancel` converge on the shared `ResponseLifecycle`.
Before delegation, the stream encoder closes every legally closable block and
reports any block or buffer that cannot be emitted. The lifecycle records the
last known usage when one exists and exactly one `response_outcome`.
Expected outcomes include `completed`, `upstream_open_error`,
`upstream_stream_error`, `upstream_error`, `client_cancel`, `invalid_state`, and
`sink_error`. A locally rejected, unsupported endpoint additionally records
`unsupported_local`, which never appears on provider execution. A failure after response commit emits the protocol error event
when the sink is still writable; a failure before commit remains an HTTP error.

The non-streaming coordinator shares that lifecycle finalizer rather than
owning a parallel outcome recorder. It records the same `response_outcome` with
`transport=batch`, reports `upstream_error` where a stream would distinguish
open from mid-stream failure, and retains the last known usage across provider,
relay rewrite, encoding, and sink failures whenever any usage was observed. A
batch response is never returned without an outcome merely because it has no
incremental block lifecycle; usage remains absent rather than fabricated when
no observation was available.

Usage has one writer per interaction. The enriched routed-execution methods
return provider, model, and credential attribution but do not write the
interaction span. The lifecycle writes that attribution and all observed token
values together, then flips the finalized marker at terminal time. Legacy
`provider.Provider` adapters used outside this response coordinator may retain
their existing instrumentation during migration, but one call path never uses
both writers.

The interaction's client-visible usage belongs only to the attempt that served
the response. Earlier fallback attempts are not merged into it, and absent usage
is not represented as measured zero. Attempt-level token accounting is not part
of this design because the current provider error contract carries no usage and
the event model has no attempt record. If a provider later exposes reportable
usage for a failed attempt, supporting it requires a separate bounded
`LLMAttemptEvent` or child-span design; until then the document does not claim
that those tokens are persisted.

Structured debug fields should include:

```text
route_id
provider_type
model
dialect
execution
transport
response_mode
relay_ineligible_reason
required_features
stream_state
active_block_kind
upstream_tool_indexes
buffered_text_bytes
failure_reason
response_outcome
response_committed
message_id_source
usage_source
```

`response_mode` and `relay_ineligible_reason` are the first fields to read when
a same-dialect request unexpectedly ran normalized. Counting relay selections in
metrics without recording the per-request reason leaves that question
unanswerable from a single trace.

Metrics should count synthesized message and tool IDs, invalid tool identities,
deferred text flushes, native affinity rejections, native relay selections,
unmappable native events, invalid state transitions, and terminal outcomes.
Every terminal outcome records the last known usage when available, even when
that usage is partial; absence is explicit when no observation exists. Metrics
must not include raw prompts, tool arguments, signatures, or opaque native
payloads.

## 5. End-to-End Flow

The target request and response path is:

```text
Anthropic or CC HTTP request
  -> thin profile handler
  -> shared Messages parser/validator
  -> prepared execution disposition
       -> local: count_tokens answers without provider/credential selection
       -> provider: continue below
  -> requirements derived from the AST
  -> generic projection + request-scoped ChatRequest.ProtocolState
       + message-scoped ProtocolState envelopes
  -> dialect-neutral route capability filtering
  -> response coordinator calls RoutedChatExecutor.ExecuteStreamChat
  -> routing opens the provider stream before HTTP success is committed
       and returns one StreamExecution with its ResolvedExecution
  -> typed generic events or a complete native Anthropic event stream
  -> shared AnthropicStreamEncoder chooses one mode from the served candidate
       -> normalized response-item reduction and SSE serialization
       -> validated native relay with a closed rewrite set
  -> profile-neutral Anthropic Messages SSE and terminal outcome
  -> thin handler writes HTTP/SSE
```

The non-streaming path uses the same parser, requirements, `ProtocolState`, and
ordered response-item semantics, then selects the JSON serializer instead of
the SSE serializer. Model-name restoration and tool-name restoration are
declared transformations shared by both serializers.

The non-streaming path selects a mode by the same rule:

```text
  -> same dialect + served candidate declares native body relay
       + closed rewrite set
       -> retain upstream response body, apply declared rewrites
  -> otherwise
       -> normalized response-item materialization and JSON serialization
```

A relayed non-streaming response preserves the upstream message ID, model, stop
reason, and complete usage. A normalized one must still emit a non-empty
gateway message ID from the same authority as the streaming path.

The `cc` profile may apply explicitly documented ingress defaults or endpoint
shims before provider execution, but it does not own upstream provider
fingerprints, fork the response block state machine, or implicitly change tool
names.

## 6. Testing Strategy

### 6.1 Characterization Fixtures

Before moving code, capture the current valid behavior as typed input/output
fixtures for:

- standard Anthropic ingress;
- `cc` ingress;
- native `claudecode` upstream events;
- native non-streaming upstream response bodies, including cache-token usage
  fields and unknown top-level fields, which native body relay must preserve;
- upstream message IDs, models, initial/final usage, and cache-token fields;
- the empty `id` on the non-streaming response, which must change rather than be
  frozen, and the initial-usage authority cases: exact native/provider usage,
  estimated usage, and protocol fallback when unavailable;
- `count_tokens` requests carrying server tools or native history, which must
  be answered without route filtering or credential selection;
- provider open failures before HTTP commit and provider errors after commit;
- generic OpenAI-compatible text and tool chunks;
- signed/redacted reasoning;
- citations and unknown blocks;
- single and parallel indexed tool calls;
- missing first-fragment ID or name;
- a synthesized `tool_use.id` echoed back by the next request as
  `tool_result.tool_use_id`, which must still resolve;
- a candidate-fallback capture in which the first candidate fails to open and
  the served candidate lacks the first candidate's relay capability;
- text before, between, and after tool fragments;
- upstream and generated `ping` events;
- EOF, cancellation, provider error, invalid state, and sink error at every
  open state.

Real sanitized upstream captures should complement constructed fixtures. They
must contain no credentials, prompts, signatures, or customer content.

### 6.2 Model-Based State Tests

Tests drive the encoder with sequences of typed inputs and verify emitted event
sequences. Reusable assertions follow the invariant grouping in section 4.2:
normalized stream fixtures assert groups A and B, relay stream fixtures assert
groups A, C, and current dialect ordering, and non-streaming fixtures assert
group D in both modes. A relay fixture with overlapping open blocks must fail
under the current Anthropic codec. A separate test-only future codec
may explicitly permit interleaving to prove that versioned validation can evolve
without weakening the current contract.

The state transition table itself must be test data. Adding a new input kind
requires declaring its behavior from every reachable state.

Lifecycle tests cover the provider/local execution dispositions crossed with
the applicable stream/batch transports. They assert that provider open/return
errors before emission remain uncommitted, a sink reports the exact physical
commit point, every provider/rewrite/encoding/sink failure produces one outcome,
observed partial usage survives failure, absent usage stays absent, and an
accidental second terminal call returns a typed state error without recording a
second outcome.

Exactly-once is additionally tested under `-race`. A dedicated test delivers
context cancellation concurrently with stream EOF and with a sink error to the
response coordinator, and asserts that it serializes those inputs, drains or
reports encoder state, and makes exactly one lifecycle terminal call. A
defensive lifecycle-only race test verifies that an accidental second caller
gets a typed state error and cannot tear finalized usage. A separate fallback
case asserts that the served attempt alone populates client-visible usage and
that an earlier attempt with no reportable usage contributes neither tokens nor
a fabricated zero.

### 6.3 Fuzz and Property Tests

Fuzzers generate bounded combinations of:

- chunk ordering;
- tool indexes and fragment boundaries;
- partial and complete JSON;
- native start/delta/stop indexes;
- message-level start/delta/stop, usage, error, and ping events;
- text/reasoning/tool transitions;
- EOF and errors.

Properties assert structural validity and absence of panics or silent loss. A
fuzzer does not need to assert that arbitrary upstream input is semantically
valid; it must assert that invalid input produces a controlled error rather
than an invalid downstream stream.

One property is explicitly resource-shaped: retained encoder memory stays within
the documented deferred-text and tool-argument limits for every generated
sequence, including a never-completing tool call interleaved with unbounded
text. Exceeding a limit must appear as an `invalid_state` terminal outcome that
names the buffer, not as growth.

Replay property tests exercise a complete conversation turn:

```text
request -> provider -> response -> response used as next-turn history -> replay
```

Native tools, content blocks, ordering, and unmodeled fields must remain
field-for-field equal after the round trip. Differential overlay is granular to
the top-level fields of the dialect-owned `ModeledObject`: content-block callers
capture each block as its own object, while response-body relay currently
rewrites only the top-level model. Adding a nested response-body rewrite first
requires block-level capture or a recursive, ordering-aware overlay; replacing
an entire containing array would not satisfy this contract. Tests verify that a
changed modeled field retains sibling unknown fields and that optional-field
presence participates in its baseline digest.

A second round trip covers the carrier rule in section 4.3: the same turn is
driven through the `einomodel` bridge and eino stream materialization, and
`ProtocolState` must survive both `Generate` and a concatenated stream. This is
the test that would have caught state placed beside `*schema.Message` instead of
on it. It asserts that response-ephemeral bodies and transport events are folded
away while persistent history blocks remain.

Request-carrier tests reorder and remove input messages while keeping
`ChatRequest.ProtocolState` fixed and verify that tools, tool choice, and
requirements do not move or disappear. They reject requirements on a
message-carried state. Concat tests cover ordered fragments, duplicate location,
conflicting response bodies, mixed invalid lifecycles, and deterministic
history folding; every invalid case returns a controlled error rather than
last-writer wins.

Registry tests exercise the dialect-neutral components directly. An envelope
whose dialect has no registered codec must produce a controlled error from
capture, overlay, fold, merge, and validation instead of being dropped, which is
the observable symptom of a missing blank import. Codec tests pass a projection
of the wrong dialect-owned type and require the same controlled failure. The
request-carrier tests run twice, once through HTTP ingress and once through the
`einomodel` option path, and assert that both produce the same resolved
`ChatRequest.ProtocolState`.

Requirement tests derive one feature from multiple AST causes and verify that
all ordered reasons survive defensive copies. Candidate-gap tests use two model
bindings for the same provider and verify that diagnostics retain the distinct
upstream model while metrics expose only bounded feature/reason codes.

### 6.4 Cross-Profile Contract Tests

Given equivalent normalized input, `anthropic` and `cc` must emit the same
Messages content events. Tests separately verify only the intentional profile
differences. This prevents profile drift without coupling the two handler
implementations through embedding.

The same normalized provider output must also pass a stream/non-stream
equivalence contract: replaying the emitted SSE into a final response produces
the same ordered content, stop reason, supported usage fields, model, and
message identity policy as the non-streaming JSON response. Native relay
fixtures verify both raw event preservation and decoded semantic equivalence,
and the equivalence pair must be run in both modes: a same-dialect fixture is
checked as streamed relay against relayed JSON, so neither path can satisfy the
contract by degrading to the weaker one.

### 6.5 Provider Feature Conformance

Each provider capability declaration selects a reusable conformance matrix.
The matrix covers the applicable request serialization, non-streaming response,
stream response, and next-turn history replay behavior. Native stream relay
requires full message-level and block-level event preservation, closed-set
rewrite tests, upstream index preservation, and lifecycle rejection tests.
Native body relay separately requires raw-body preservation and the same batch
rewrite semantics. Both include a rewrite-set mismatch case that must terminate
with `invalid_state` instead of forwarding a value known to be unrestored.

Relay matrices also include a candidate-fallback case: the first candidate fails
to open and the served candidate does not declare the relay capability. The
response must run normalized and record `relay_ineligible_reason`, proving that
mode selection reads `ResolvedExecution` rather than the route's declared
candidate set.

A declaration is not accepted merely because its dependencies are internally
consistent. The provider package must run and pass every selected conformance
case in CI.

### 6.6 Commit-Level Verification

Every migration commit must compile and pass its relevant tests independently.
Tests may not depend on helpers introduced by a later commit. This constraint is
part of the design because independently reviewable changes reduce the chance
of hiding protocol behavior inside a large mixed diff.

## 7. Migration Plan

### Phase 0: Freeze message, block, and terminal invariants

- document the full HTTP/message/block/terminal transition table, grouped as in
  section 4.2;
- add characterization and captured-traffic fixtures;
- strengthen the block-discipline assertion to reject globally overlapping
  blocks on normalized output and on relay under the current Anthropic codec;
- add pre-commit/open-error, message metadata, usage, ping, buffer-limit, and
  terminal-path tests before moving implementation.

Exit gate: current valid behavior and known corrections are reproducible without
an HTTP server.

### Phase 1: Fix the typed stream contract and extract the encoder

- introduce the complete typed provider stream-event algebra and an in-memory
  sink, including message-level and block-level native events;
- introduce the exactly-once `ResponseLifecycle` finalizer, make the response
  coordinator its sole semantic terminal caller, and make the stream encoder
  delegate outcome, usage, and commit metadata through that coordinator;
- make the dispatcher explicitly transfer interaction-span finish ownership to
  the lifecycle before handing off a provider-backed or local LLM response;
- introduce the dialect-valid identifier generator for synthesized message and
  tool-use IDs, with Anthropic prefix choice owned by its codec;
- move block indexes, tool buffers, deferred text, native index remapping, and
  message/terminal handling out of `handler.go`;
- delay HTTP success commitment until the provider stream opens and the encoder
  emits its committing event;
- enforce the deferred-text and tool-argument buffer limits;
- keep wire output stable except where the old output violated an invariant;
- make both handlers use the encoder.

Message-level native inputs have no producer in this phase; provider adapters
still consume them upstream until phase 3. Defining their encoder contract here
is deliberate, so phase 3 becomes a producer-only change. They are covered by
fixtures and encoder tests only.

Exit gate: the HTTP handler contains no message or content-block lifecycle state,
cannot write protocol events directly, and every terminal path records an
outcome plus last-known usage when available.

### Phase 2: Replace handler embedding with profiles

- introduce the shared Messages core and explicit profile contract;
- make standard Anthropic and `cc` thin sibling handlers;
- move only proven Claude Code-specific behavior into the `cc` profile;
- add cross-profile contract tests.

Exit gate: `cc` does not embed `anthropic.Handler`, and the shared core contains
no Claude Code fingerprint or compact-mode policy.

### Phase 3: Introduce `ProtocolState`, shared response semantics, and relay

- introduce scoped `ProtocolState` and `NativeEnvelope` values for request
  tools/requirements, message history, native stream events, and non-streaming
  response bodies;
- add `ChatRequest.ProtocolState` for request scope and one neutral
  `schema.Message.Extra` key for message/history/response/stream scopes, and
  carry request scope across the eino boundary as one impl-specific option;
- replace `ChatExtraFields.AnthropicTools` and `AnthropicToolChoice` with
  request-scoped envelopes and move their readers to the resolved state;
- introduce the dialect codec registry with `init` registration and the blank
  imports in `cmd/agw`, `cmd/agwd`, and `cmd/agwctl`;
- add the enriched `ExecuteChat`/`ExecuteStreamChat` routed contract returning a
  single `ResolvedExecution`, keep ordinary provider methods as adapters, and
  stop rewriting `ChatRequest.Model` in place;
- define response/body consumption and history-folding rules plus the closed
  stream-concat merge algebra;
- centralize capture, projection, differential overlay, and replay in the
  dialect codec using complete modeled-field baseline digests;
- introduce the ordered response-item model shared by batch and stream output;
- introduce the single feature-definition registry and provider support-set
  shape, initially registering only the two `mode_selection` relay features;
- carry the response-body envelope on the returned message, exposing at most a
  convenience accessor on `ChatResponse`;
- add native relay mode for streaming and non-streaming responses, selected once
  per response through `StreamOpen` or `ResponseOpen`, with a closed rewrite set;
- make the batch encoder use the shared `ResponseLifecycle` for provider,
  rewrite, encoding, and sink outcomes, and make enriched routed execution
  return attribution without writing usage so the lifecycle is the single span
  extension writer on that path;
- stop swallowing upstream message-level events and response bodies in provider
  adapters;
- add stream/non-stream equivalence and replay round-trip properties.

Exit gate: one protocol state explains native fidelity, normalized and relay
responses have one lifecycle owner, `stream: false` and `stream: true` reach
equal same-dialect fidelity, both relay transports have an explicit provider
contract, `ProtocolState` survives an `einomodel` `Generate` and a concatenated
eino stream without retaining transport-only payloads, an in-process agent turn
carries the same request state as HTTP ingress, mode selection reads the served
candidate, no dialect-specific field remains on `ChatExtraFields`, request state
remains independent of message ordering, and stream/non-stream semantic
equivalence passes.

### Phase 4: Derive requirements from the AST and feature route eligibility

- derive immutable requirements only from the parsed Messages AST;
- attach request-wide envelopes and requirements to
  `ChatRequest.ProtocolState`, never to an arbitrary message;
- register the four `requirement`-class features in the feature-definition
  registry introduced in phase 3, then replace broad native/reasoning sets;
- construct requirements only through `NewProtocolRequirementSet` and return
  `RequirementGap` from candidate filtering instead of a bare no-candidate
  error;
- branch non-generative endpoints before requirement derivation and route target
  resolution through the generic execution-disposition contract;
- make route selection a generic set-inclusion operation;
- migrate internal callers through the parser/normalizer or an explicit typed
  protocol-state adapter;
- remove handler reparse bypasses, Anthropic-specific generic-provider helpers,
  old extra keys, and temporary duplicate detectors;
- add feature-indexed provider conformance tests and improve rejection metrics.

Exit gate: `pkg/gateway/llmroute` contains no Anthropic-specific capability
logic or payload inspection, no layer rediscovers requirements from a generic
message projection, and `count_tokens` no longer participates in candidate
filtering or credential selection.

### Phase 5: Prove extension with a second dialect

Before adding production support for another raw-replay dialect, implement a
test-only dialect using the same envelope, minimal feature registry, route
filtering, conformance matrix, and encoder-facing native event contract.

Exit gate: the extension adds registrations and dialect-owned codecs without
adding new dialect fields or `if dialect == ...` branches to routing core.

## 8. Rollout and Compatibility

This repository does not preserve internal backward compatibility by default.
Each phase should switch all in-repository callers atomically and remove the old
internal shape in the same phase. Do not keep permanent dual writers, aliases,
or fallback serializers.

Client-visible compatibility is different: valid Anthropic Messages JSON and
SSE output should remain stable. Intentional changes are limited to correcting
invalid streams, preserving upstream identity and usage that were previously
lost, replacing silent loss with typed errors, and making route rejections more
precise. Those changes require fixtures and release notes.

Three of those corrections are user-visible and must be called out explicitly:
the non-streaming response gains a non-empty message ID, `message_start`
preserves exact initial usage when available and identifies estimate/fallback
authority internally when it is not, and a `count_tokens` request carrying
native state is answered instead of rejected by native-dialect filtering.

No phase should combine provider rewrites, unrelated route behavior, or new
product features. The migration is complete only when the old state owner is
deleted, not when a second abstraction is layered on top of it.

## 9. Risks and Mitigations

| Risk | Mitigation |
|---|---|
| A generic abstraction hides Anthropic rules | Keep Anthropic parsing and encoder semantics dialect-owned; only envelopes and capability sets are dialect-neutral. |
| Native relay becomes an unvalidated bypass | Select the mode once, keep it inside the same lifecycle owner, validate every message/block transition, and restrict rewrites to a closed tested set. |
| Success is committed before the upstream stream opens | Make `stream_opened` and logical response commitment explicit states and test pre-commit failures separately. |
| A keepalive commits the response and forfeits the HTTP error path | Keep the committing event set closed, exclude `ping` from it, and resolve pre-commit stalls as a transport timeout. |
| Generic streaming cannot know exact input usage at commit time | Preserve exact native/provider usage when available; otherwise use an explicitly observed estimate or protocol fallback without buffering the full response. |
| Relay validation drifts from the upstream protocol | Keep ordering rules in the versioned dialect codec; current fixtures reject overlapping blocks, while a future test codec may opt into a changed lifecycle. |
| The served candidate is unknown, so relay is selected against a route-level capability | Return one `ResolvedExecution` inside the enriched routed result, derive encoder and lifecycle inputs from it in the coordinator, and cover candidate fallback in relay conformance. |
| Resolved metadata gains multiple authorities or leaks credentials into the protocol core | Keep one enriched result, pass only its `ServedCandidate` view to the encoder, and keep `AttemptAttribution` lifecycle-only. |
| The client-visible model is lost because the request is mutated in place | Keep the upstream model on the outbound clone and in `ResolvedExecution`; assert the client model in `message_start`. |
| Request state is unreachable from in-process agent turns | Carry request scope across the eino boundary as one impl-specific option and test HTTP and `einomodel` paths for an identical resolved state. |
| A missing codec blank import degrades fidelity silently at runtime | Make an unregistered dialect a controlled error at capture/overlay/fold/merge/validate and test it directly. |
| Local execution is confused with response transport | Record `execution=provider|local` separately from `transport=stream|batch` and test valid combinations. |
| Dispatcher and lifecycle both finish the interaction span | Transfer finish ownership explicitly at the response-coordinator handoff and disable the dispatcher finalizer after transfer. |
| Two writers record usage on one interaction span | Make enriched routed execution return attribution without writing the span; let the lifecycle write served-attempt attribution and usage together. |
| Failed fallback usage is promised without an attempt event or provider error payload | Keep it outside this design, never merge or fabricate it, and require a separate attempt-telemetry design before claiming persistence. |
| Concurrent cancellation and EOF both record an outcome | Deliver both as coordinator inputs, keep one semantic terminal caller, and retain an atomic lifecycle guard as defense. |
| A synthesized ID breaks client validation or the next-turn tool result | Require dialect-valid, collision-free, round-trip-stable IDs; keep any `msg_`/`toolu_` prefix policy inside the Anthropic codec. |
| Relay commits and then meets an undeclared rewrite | Prove eligibility from the request alone and terminate only when a field known to require inverse transformation cannot be restored. |
| Unbounded deferred text or tool arguments exhaust memory | Document buffer limits, make overflow a defined `invalid_state` transition, and assert boundedness in fuzz properties. |
| Non-generative endpoints inherit generative routing constraints | Use a protocol-neutral local/provider disposition and branch `count_tokens` before requirement derivation, candidate filtering, and credential selection. |
| Request-wide state is attached to an arbitrary message | Put tools, tool choice, and requirements on `ChatRequest.ProtocolState`; reject requirements on message carriers and test message reorder/removal. |
| Message state placed beside `*schema.Message` is dropped at the eino bridge | Attach message/history state under one neutral key and test the round trip through `einomodel` and stream concatenation. |
| Ephemeral bodies or event logs accumulate in conversation history | Give envelopes explicit scope, fold them into persistent history state, and test that transport-only payloads are removed. |
| Stream concat silently overwrites protocol state | Define ordered merge and conflict rules and return controlled errors instead of last-writer wins. |
| A batch response returns without an outcome | Share one exactly-once `ResponseLifecycle` finalizer while keeping stream block draining inside the stream encoder. |
| A locally answered endpoint becomes unmetered and untraced | Keep VirtualKey validation, limits, and one interaction event on the `ExecutionLocal` path; attach estimated usage only when produced. |
| Batch and stream response semantics drift | Share the ordered response-item model, require stream/non-stream replay equivalence, and give relay a non-streaming counterpart so neither path is the weaker one. |
| A broad relay flag hides transport asymmetry | Declare stream-event relay and response-body relay separately in one feature registry and test each transport independently. |
| JSON completeness is mistaken for an upstream boundary | Require both complete JSON and a boundary/transition signal; test deferred-text completion explicitly. |
| Parallel tools create overlapping downstream blocks | Buffer by upstream index and serialize in first-seen order through one active-block owner. |
| Raw and modeled state diverge | Baseline every modeled mutable field with a canonical presence-aware digest and apply differential overlay through the dialect codec. |
| Capability declarations overstate provider support | Keep the initial vocabulary minimal, validate dependencies, and require feature-indexed provider conformance tests. |
| Error or cancellation loses consumed-token accounting | Route every completion, failure, and cancellation through one terminal routine that records the outcome and any observed usage. |
| Migration creates two sources of truth | Use phase exit gates and delete the replaced path in each phase. |
| `cc` behavior leaks into standard Anthropic | Use explicit profiles and cross-profile contract tests. |

## 10. Completion Criteria

The architectural problem is considered resolved when all of the following are
true:

- `anthropic` and `cc` are thin profile handlers over one shared Messages core;
- neither handler owns message/content-block lifecycle state or writes protocol
  events directly;
- provider-open failure remains an HTTP error, while post-commit failure follows
  the typed SSE terminal contract;
- the stream encoder enforces the documented invariant groups for normal, EOF,
  error, cancellation, ping, native relay, normalized, and parallel-tool
  sequences, and enforces construction rules only where they apply;
- the committing event set is closed and a keepalive cannot commit a response;
- complete same-dialect responses preserve upstream message identity, usage,
  block indexes, and unmodified fields through validated native relay in both
  streaming and non-streaming form;
- streaming event relay and non-streaming body relay have separate atomic
  capabilities and explicit provider/encoder contracts in one feature registry;
- streaming and non-streaming responses share one ordered semantic model and
  pass the replay-equivalence contract in both modes;
- encoder buffering is bounded and overflow is a named terminal outcome;
- initial usage preserves exact provider data when available and records an
  internal estimated/unavailable authority otherwise without delaying the
  stream to final usage;
- non-generative endpoints use the generic local execution disposition and
  answer before requirement derivation, candidate filtering, and credential
  selection;
- raw replay and explicit modifications use one `ProtocolState` overlay
  contract with complete modeled-field baseline digests;
- request state and requirements live on `ChatRequest.ProtocolState`, reachable
  identically from HTTP ingress and from an in-process agent turn, with no
  dialect-specific request field left on `ChatExtraFields`, while
  message/history/response/stream state uses one neutral message key with
  explicit scope and retention rules;
- requirements are constructed only through the class-checking constructor and
  retain every ordered reason, while route rejections identify the complete
  provider/model candidate and report its missing features;
- message state survives the `einomodel` bridge and stream concatenation,
  concat conflicts fail deterministically, and transport-only state does not
  accumulate in history;
- the routing layer returns the served candidate as `ResolvedExecution`, mode
  selection and the client-visible model derive from it, and the caller's
  request is never mutated in place;
- dialect-neutral components reach dialect logic only through the registered
  codec, capture/overlay/replay are executable codec operations, and an
  unregistered dialect or wrong projection type is a controlled error rather
  than a drop;
- provider/local execution and stream/batch transport use one exactly-once
  `ResponseLifecycle` contract, the coordinator is the sole semantic terminal
  caller, and `response_outcome` carries the last known usage when available;
- the dispatcher explicitly transfers interaction-span finish ownership to the
  lifecycle, and enriched routed execution returns attribution without becoming
  a second usage writer;
- client-visible usage contains only the served attempt; attempt-level usage for
  failed fallback candidates is outside this design and is never fabricated;
- synthesized message and tool-use IDs are dialect-valid, collision-free, and
  survive the client's next-turn `tool_result` reference;
- local execution remains metered, rate limited, and traced, with estimated
  usage only for successful endpoints that actually produce an estimate;
- requirements are derived once from the parsed protocol AST and never inferred
  from generic message extras;
- provider fidelity is declared through the minimal dialect feature set,
  provider declarations pass the conformance matrix, and route filtering is
  generic set inclusion;
- internal and HTTP callers attach the same immutable requirement set before
  provider or credential selection, with no handler reparse bypass;
- every terminal path records its outcome and last known usage when available;
- no malformed tool, native block, buffered text, or usage is silently
  discarded;
- captured fixtures, state-table tests, replay properties, fuzz properties,
  stream/non-stream equivalence, provider conformance, and cross-profile
  contract tests pass;
- adding a test dialect requires no new dialect-specific field or routing-core
  branch.
