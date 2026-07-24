# Zhipu Coding Plan Vision Workflow

## 1. Status

This document defines a proposed design. It is not implemented.

It replaces the earlier provider-local proposal that detected image content
inside the `zhipu` provider and silently swapped the upstream model. That
approach is invalid for Zhipu Coding Plan because vision is available through
MCP rather than through the Coding Plan text-model endpoint.

This design depends on the
[Unified Workflow Runtime](workflow-runtime.md). An LLM route invokes a
synchronous, non-durable workflow containing MCP, Transform, and LLM tasks.

## 2. Problem

Codex and Claude Code can send image-bearing requests while remaining pinned to
one coding model:

- Codex uses OpenAI Responses `input_image`, commonly with a base64 data URL;
- OpenAI Chat Completions uses an `image_url` content part;
- Claude Code uses the `cc`/Anthropic Messages profile with base64 image
  source data.

The gateway currently preserves those image parts through protocol conversion,
but a Coding Plan text model rejects them because its chat endpoint accepts
text content only. Zhipu Coding Plan exposes image understanding through its
Vision MCP server, so selecting another chat model inside the provider does not
solve the product constraint.

The desired transparent behavior is:

```text
image-bearing LLM request
  -> Vision MCP analyzes each image
  -> gateway replaces image parts with analysis text
  -> original Coding Plan text model handles the rewritten request
```

The image bytes must not reach the text model.

## 3. Goals

- Support Codex Responses, OpenAI Chat Completions, Anthropic Messages, and the
  `cc` profile through one protocol-neutral workflow.
- Send image content only to the configured Vision MCP service.
- Preserve text-only requests without an MCP call.
- Continue using the LLM route's normal target policy, model catalog,
  credential scheduling, fallback, and usage attribution.
- Process multiple images through bounded fan-out and rewrite them in original
  content order; the initial reused-stdio transport executes that fan-out
  serially until MCP multiplexing or pooling exists. A per-request image-count
  cap bounds serial preprocessing latency, so a request with many images
  cannot push the pre-stream phase past the client's own timeout.
- Fail closed when image analysis does not complete.
- Keep Zhipu Coding Plan text access and Vision MCP access as separate managed
  resources.

## 4. Non-Goals

- No provider-local model swap.
- No claim that a Coding Plan text model has native vision capability.
- No MCP tool schema injection into the client request.
- No hidden LLM/tool/LLM agent loop.
- No durable workflow run, scheduling, handoff, or human approval on this LLM
  ingress path.
- No arbitrary prompt or transform scripting in route configuration.
- No remote URL fetching without a separately designed SSRF policy.

## 5. Architecture

```text
Codex / Claude Code
        |
        v
LLM route (openai / anthropic / cc)
        |
        v
protocol handler prepares normalized LLMWorkflowInput
        |
        v
synchronous Workflow Runner
        |
        +-- Transform Task: prepare-images
        |       decode/validate media into request-scoped artifacts
        |
        +-- MCP Task: analyze-images (bounded for_each)
        |       zhipu-vision / analyze_image
        |
        +-- Transform Task: rewrite-request
        |       image part -> untrusted analysis text
        |
        +-- LLM Task: completion
                target = ingress.llm_target
                uses RoutedProvider
        |
        v
protocol-native response / stream
```

The workflow is an ordinary Workflow Definition. Its invocation policy is
supplied by the LLM route:

```text
mode = synchronous
persistence = none
allow_suspend = false
```

There is no separate `input_processors` runtime or configuration model.

## 6. Resource Model

### 6.1 Coding Plan Provider

`glm-coding-plan` should initially be a Provider **instance id**, not a new
Provider Type:

```yaml
providers:
  - id: glm-coding-plan
    provider_type: zhipu
    base_url: "REPLACE_WITH_CODING_PLAN_LLM_BASE_URL"
    default_model: "REPLACE_WITH_CODING_PLAN_TEXT_MODEL"
```

This reuses the current `zhipu` OpenAI-compatible implementation when Coding
Plan has the same wire protocol and authentication behavior. The endpoint and
model names are deployment-specific and the placeholders above are not literal
defaults.

A new Provider Type such as `zhipu_coding_plan` is justified only if Coding
Plan requires materially different:

- authentication or credential refresh;
- request/response wire behavior;
- error and rate-limit normalization;
- model discovery;
- endpoint semantics.

Even then, the new Provider remains an LLM-only provider. It must not own or
invoke the Vision MCP service.

### 6.2 Vision MCP Service

Vision is a separate gateway-managed MCP service:

```yaml
mcpServices:
  - id: zhipu-vision
    name: Zhipu Coding Plan Vision
    transport: stdio
    command: npx
    args:
      - -y
      - "REPLACE_WITH_ZHIPU_VISION_MCP_PACKAGE"
```

The Vision MCP package/command is deployment-specific. The placeholder above is
**not** a literal default and must be replaced with the actual Zhipu Coding
Plan Vision MCP server package or binary before use; do not copy it verbatim.

The gateway process must receive the actual Zhipu Coding Plan credential in its
environment so the stdio child inherits the credential required by the MCP
server. Credential names and secret injection belong to deployment
configuration and must not be embedded in the workflow definition.

A local stdio service is the preferred first implementation because the MCP
tool accepts a local image path and the child process can read a
gateway-created request-scoped temporary file. This local-path mode requires
the reused MCP stdio child to share the gateway's filesystem — co-located stdio,
not a container with a separate mount namespace — and the child reads each
per-run temporary file by the gateway-generated absolute path. The current MCP
service manager maintains one reused session/child per service rather than a
process pool. A deployment that isolates the MCP child's filesystem must use
one of the remote contracts below instead of local paths.

A remote Streamable HTTP MCP service cannot read a gateway-local path. Remote
deployment requires one of:

- an MCP tool contract that accepts a data URL or binary content;
- a controlled artifact service that returns a short-lived signed URL.

Passing `/tmp/image.png` to a remote MCP server is invalid.

### 6.3 Model Capabilities

The Coding Plan text model remains:

```text
Vision = false
```

Workflow composition is a route execution capability, not a native provider or
model capability. Model catalog filtering and provider capabilities must not be
made inaccurate to advertise the composite route.

## 7. Workflow Definition

Conceptual bundle configuration:

```yaml
workflows:
  - id: zhipu-coding-plan-vision
    name: Zhipu Coding Plan with Vision MCP
    required_bindings:
      - ingress.llm_target
    input_type: llm_request

    tasks:
      - id: prepare-images
        type: transform
        target:
          handler: prepare_llm_images
        input:
          request:
            ref: run.input
        config:
          allowed_media_types:
            - image/png
            - image/jpeg
            - image/webp
          max_image_bytes: 10485760
          max_total_image_bytes: 16777216
          max_images: 8

      - id: analyze-images
        type: mcp
        depends_on:
          - prepare-images
        condition:
          type: output_not_empty
          input:
            ref: tasks.prepare-images.output.images
        for_each:
          collection:
            ref: tasks.prepare-images.output.images
          max_concurrency: 1
        target:
          service_id: zhipu-vision
          tool: analyze_image
        input:
          arguments:
            object:
              image_source:
                ref: item.local_path
        timeout_seconds: 60
        retry:
          max_attempts: 2
          backoff: exponential
        failure_policy: fail_workflow

      - id: rewrite-request
        type: transform
        depends_on:
          - prepare-images
          - analyze-images
        target:
          handler: replace_llm_images_with_analysis
        input:
          request:
            ref: run.input
          prepared_images:
            ref: tasks.prepare-images.output
          analyses:
            ref: tasks.analyze-images.output

      - id: completion
        type: llm
        depends_on:
          - rewrite-request
        target:
          binding: ingress.llm_target
        input:
          request:
            ref: tasks.rewrite-request.output
          stream:
            ref: run.input.stream
        terminal_output: true

    output:
      ref: tasks.completion.output
```

This YAML is the target design shape, not a claim that the current gateway
bundle decoder already supports `workflows`.

## 8. LLM Route Configuration

The ingress route retains its current LLM target policy and adds workflow
execution:

```yaml
llmRoutes:
  - id: codex-glm-coding-plan
    protocol: openai
    match_policy:
      path_prefix: /codex
    auth_policy:
      require_virtual_key: true

    target_policy:
      provider_target:
        provider_id: glm-coding-plan

    execution_policy:
      type: workflow
      workflow_id: zhipu-coding-plan-vision
      workflow_revision: "sha256:<definition-content-hash>"
      timeout_seconds: 120
      max_concurrency: 4
```

`workflow_revision` is the deterministic hash of the canonical definition.
Bundle validation may compute and persist it when the definition and route are
applied together. Updating the workflow does not move this route to the new
revision implicitly; the route must be updated and revalidated so an expanded
resource set cannot be granted silently.

At invocation, `ingress.llm_target` is a capability-like binding containing the
already validated route target policy. The terminal LLM task creates a
`RoutedProvider` from that binding. It does not recursively dispatch an HTTP
request back through the ingress route.

Text-only requests run the same definition:

- `prepare-images` returns an empty image list;
- `analyze-images` is skipped;
- `rewrite-request` returns an equivalent text-only request;
- `completion` calls the Coding Plan model once.

The no-image transforms should be allocation-light; an implementation may
optimize the empty path after preserving identical semantics.

## 9. Protocol-Neutral Input

The Workflow Runner must not parse OpenAI or Anthropic HTTP bodies. Each LLM API
handler converts its wire request into a normalized workflow input:

```go
type LLMWorkflowInput struct {
    RequestType provider.LLMApiRequestType
    Model       string
    Stream      bool
    Request     any
    Media       []LLMMediaRef
}

type LLMMediaRef struct {
    MessageIndex int
    PartIndex    int
    MediaType    string
    Source       MediaSource
}
```

The media reference includes a deterministic location used by the rewrite
transform. It never logs base64 or image bytes.

Required protocol coverage:

- Responses `input_image.image_url`;
- Chat Completions `image_url.url`;
- Anthropic/CC base64 image source;
- URL sources only when allowed by route/media policy.

The protocol handler also owns the terminal response conversion. Internal
Workflow events are not serialized as unknown Responses or Anthropic events.

## 10. Image Preparation

`prepare_llm_images` is a registered, compiled-in Transform handler:

1. enumerate normalized image references;
2. reject unsupported media types;
3. decode base64/data URL without logging it;
4. enforce per-image and aggregate decoded byte limits;
5. enforce the per-request image-count limit (`max_images`); an over-count
   request is rejected as `400` rather than silently dropped, because under
   serial MCP fan-out each extra image is a full extra vision call of latency;
6. compute SHA-256 for correlation/cache eligibility;
7. create a request-scoped artifact;
8. when required by local stdio MCP, materialize a `0600` temporary file;
9. return ordered image descriptors.

The temporary directory is created for the run, not shared by request ids
supplied by a client. Filenames are generated by the gateway and never derived
from user paths. Cleanup runs on success, error, timeout, panic recovery, and
client cancellation.

The first version rejects ordinary remote HTTP(S) image URLs unless the MCP
server can consume the URL directly and the route explicitly enables that
behavior. The gateway must not become an unrestricted URL fetcher.

## 11. MCP Analysis

The MCP task performs bounded `for_each` over prepared images. Each child call
uses:

```json
{
  "name": "analyze_image",
  "arguments": {
    "image_source": "/gateway-owned/request-scope/image-0.png"
  }
}
```

Results are joined by input index, not completion time.

The initial local-stdio workflow sets MCP `for_each.max_concurrency = 1`.
Although the Workflow Runner supports bounded fan-out, the current MCP service
manager reuses one stdio transport per service and that transport does not
provide concurrent request multiplexing. Increasing this value requires the
MCP runtime to add unique request ids plus a concurrency-safe response
dispatcher, or to expose a process/session pool.

The initial workflow uses `analyze_image` for every image. Specialized tools
such as screenshot OCR, error diagnosis, diagram understanding, visualization
analysis, or UI comparison should be introduced only through an explicit
workflow definition or deterministic request hint. The text model must not be
called merely to choose a vision tool.

MCP result validation rejects:

- a tool-level error result;
- empty usable content;
- result content exceeding the configured text limit;
- binary or unsupported result content for the rewrite transform.

## 12. Request Rewrite

`replace_llm_images_with_analysis` creates a new provider-facing request. It
does not mutate shared request objects in place.

Each image part is replaced at the same logical position with text similar to:

```text
[Untrusted image analysis from zhipu-vision/analyze_image, image 1]
<analysis text>
[/Untrusted image analysis]
```

Rules:

- preserve surrounding user text and image order;
- remove all original image URLs, data URLs, and base64 bytes;
- mark analysis as untrusted user-derived content;
- do not put analysis into a system/developer message;
- preserve client-provided tools, tool choice, generation options, metadata,
  and response state;
- cap analysis length before sending it to the text model;
- fail before the LLM task if any original image remains in the rewritten
  request.

The final invariant is:

```text
terminal LLM task input contains zero image content parts
```

This invariant is asserted independently of the selected provider.

## 13. Failure And Retry Semantics

The HTTP status column below is produced by the Workflow Runtime's synchronous
ingress task-error → status classification
([workflow-runtime §11.1](workflow-runtime.md#111-task-error-to-http-status-synchronous-ingress-profile));
this document does not define its own mapping. A `transform` or `mcp` task in
this workflow signals one of those closed classifications, and the runner maps
it to the listed status before any stream bytes are committed.

Default behavior is fail closed:

| Failure | Result |
|---|---|
| Invalid/oversized image | `400` or `413` |
| Too many images (`max_images` exceeded) | `400` |
| Unsupported remote URL policy | `400` |
| MCP service disabled/missing | `503` |
| MCP timeout | `504` |
| MCP/tool failure | `502` |
| Rewrite leaves image content | `500` |
| Text LLM failure | Existing LLM route/provider status |
| Client disconnect | Cancel workflow and active tasks |

The gateway never falls back by:

- dropping the image;
- forwarding the image to the text model;
- skipping a failed image while processing others;
- switching to an undeclared vision chat model.

MCP retry is bounded and only for transport/service failures classified
retryable. A successful tool response with invalid content is not retried
unless explicitly classified safe.

## 14. Streaming

Image preparation and MCP analysis finish before response headers are committed.
This adds pre-stream latency but preserves protocol correctness.

Only the terminal `completion` LLM task owns the client stream:

- OpenAI Responses events remain Responses events;
- Chat Completions remains Chat Completions SSE;
- Anthropic and `cc` retain their expected event profiles.

MCP progress and internal task lifecycle events are recorded for observability
but are not injected into those protocol streams.

## 15. Observability

One image-bearing request produces a span tree:

```text
LLM ingress: codex-glm-coding-plan
└── workflow: zhipu-coding-plan-vision
    ├── transform: prepare-images
    ├── mcp: zhipu-vision/analyze_image[0]
    ├── mcp: zhipu-vision/analyze_image[1]
    ├── transform: rewrite-request
    └── llm: glm-coding-plan/<concrete-model>
```

The MCP and LLM child spans carry the ingress trace, route, VirtualKey, and
unambiguous agent attribution. Metrics expose at least:

- workflow id and run id;
- image count and total decoded bytes, without image content;
- preprocess latency;
- MCP service/tool and child-call status;
- selected provider and concrete upstream text model;
- workflow/task failure phase.

Usage events report the real MCP tool and real text model. They must not report
a fictitious vision model swap.

## 16. Validation

Bundle/apply validation requires:

- workflow definition exists and is enabled;
- route execution policy references and pins that workflow revision;
- required `ingress.llm_target` binding is declared;
- Transform handlers are registered in the linked binary;
- MCP service exists, is enabled, and exposes the configured tool;
- terminal-output task is the unique non-`for_each` DAG sink, is one LLM task
  bound to the ingress target, depends transitively on every other task, and
  supplies the workflow output;
- graph is acyclic and within task/fan-out limits;
- synchronous profile cannot suspend or continue in background;
- provider/model target is valid for streaming/tools requirements after image
  rewrite;
- route timeout is not shorter than an invalid combination of task timeouts.

Live MCP tool discovery may be unavailable during offline bundle validation.
Structural validation checks the service reference and tool name; apply or
startup performs discovery and fails readiness when the required tool is
missing.

## 17. Testing

### Workflow unit tests

- text-only input skips MCP and calls the LLM task once;
- one image creates one MCP child task;
- multiple images serialize calls to the reused stdio service and join results
  in original order;
- dependency failure prevents the LLM task;
- cancellation removes artifacts and stops active MCP calls;
- terminal stream ownership is unique.

### Protocol tests

- Responses data URL is extracted, analyzed, removed, and replaced with text;
- Chat Completions image URL/data URL shape is handled;
- Anthropic/CC base64 image shape is handled;
- client tools and tool history survive rewrite unchanged;
- text-only wire payload remains semantically equivalent;
- rewritten provider request contains no image bytes or image content parts.

### Security tests

- malformed base64;
- MIME mismatch;
- per-image and aggregate size limits;
- generated path cannot escape the run directory;
- cleanup on success, error, timeout, cancellation, and panic;
- remote URL rejected by default;
- logs and errors never contain base64, authorization values, or image bytes.

### Integration tests

- local stdio Vision MCP reads the generated path;
- remote MCP cannot be configured with local-path mode;
- MCP failure maps to the documented protocol error;
- LLM credential scheduling/fallback still applies;
- MCP and LLM usage events share one trace and correct attribution;
- Codex and Claude Code streaming remain protocol-compatible.

## 18. Rollout

1. Implement Workflow Runtime W0 and W1 from
   [workflow-runtime.md](workflow-runtime.md#16-implementation-plan).
2. Add protocol-neutral media extraction and rewrite hooks.
3. Register the two media Transform handlers.
4. Configure and verify the local Zhipu Vision MCP service.
5. Add the `glm-coding-plan` provider instance.
6. Apply the workflow definition without binding production routes.
7. Run protocol and security integration tests.
8. Bind one canary LLM route through `execution_policy.type = workflow`.
9. Compare text-only latency and image-request success/usage.
10. Expand routing after MCP capacity and timeout behavior are verified.

## 19. Rejected Alternatives

### Swap To A Vision Chat Model In `zhipu`

Coding Plan vision is MCP-only. A provider-local swap also bypasses route/model
governance and risks incorrect concrete-model usage attribution.

### Add Vision MCP Tools To The LLM Request

Tool declaration is not tool execution. Completing the tool loop in an LLM
proxy would create an implicit agent and change Codex/Claude Code protocol
semantics.

### Wrap The Request In A Builtin Agent

Codex and Claude Code are already agents. Nesting another agent adds model
turns, session state, permission semantics, and incompatible streaming. The
workflow needs deterministic resource composition, not another reasoning loop.

### Dedicated `input_processors`

The operation is a small synchronous workflow. A separate processor engine
would duplicate the common Workflow Runner while solving only one ingress
phase.
