# Protocol Support Matrix And Roadmap

## 1. Scope

This document defines the recommended protocol support strategy for `agent-gateway` beyond the currently implemented LLM API path.

It is a proposal and planning document. It does not change the current runtime contract.

The current Go module path is:

- `github.com/agent-guide/agent-gateway`

## 2. Current Runtime Baseline

Today the primary production path is:

- route-based LLM API dispatch
- provider resolution
- VirtualKey validation
- direct-provider or logical-model routing
- JSON or SSE response translation

Current implemented protocol-facing behavior:

- OpenAI-compatible chat and responses handling
- Anthropic-compatible messages handling
- streaming over HTTP SSE

Current non-LLM protocol status:

- MCP is integrated into the main request path through `agent_route_dispatcher` when `mcp` is enabled
- MCP route config, dispatch, discovery, execution, and runtime inspection are active
- ACP route config, native dispatch, session listing, transcript replay, runtime
  inspection, and permission resolution are active
- metrics persist LLM/MCP/ACP usage events, expose summaries/events/Prometheus,
  and support optional `agent_id` attribution
- the agents control plane is active through P0/P1: agent CRUD, workspace,
  resources, activity, usage, interactions, health, gateway-bundle parity, and
  write-time/read-side attribution
- memory remains an earlier-stage stubbed admin area

## 3. Design Principle

The gateway should not add transports just because an upstream or adjacent ecosystem mentions them.

Protocol support should be driven by:

- protocol compliance
- real client interoperability
- operational simplicity behind Caddy and normal HTTP infrastructure
- whether the transport matches the actual interaction pattern

The default preference order should be:

1. plain HTTP request/response
2. HTTP plus SSE when the server needs to stream output
3. WebSocket only when the protocol genuinely needs long-lived bidirectional messaging

## 4. Support Matrix

### 4.1 LLM API Gateway

Recommended support:

- required
  - unary HTTP JSON
  - streaming HTTP SSE
- not required by default
  - WebSocket

Reasoning:

- OpenAI-compatible and Anthropic-compatible APIs are already modeled as HTTP request/response APIs.
- Their mainstream streaming shape is SSE, not WebSocket.
- Existing SDKs, reverse proxies, observability tooling, and browser clients already handle SSE well.
- WebSocket adds connection lifecycle, auth refresh, backpressure, and load-balancing complexity without improving the main text-generation path.

Use WebSocket for LLM APIs only if the product explicitly adds:

- realtime duplex voice or multimodal sessions
- client-issued control messages during generation
- one long-lived connection carrying multiple concurrent exchanges

### 4.2 MCP Gateway

Recommended support:

- required
  - `stdio` for local MCP client/server execution
  - Streamable HTTP for remote MCP
- recommended for compatibility
  - legacy HTTP plus SSE compatibility path
- not required by default
  - WebSocket

Reasoning:

- Modern MCP transport guidance is centered on `stdio` and Streamable HTTP.
- Streamable HTTP already covers normal request/response, optional server streaming, session handling, and remote deployment.
- SSE remains useful as a compatibility target for older MCP clients and servers.
- WebSocket is not the mainline MCP transport and should not be the first remote transport investment.

Important implication for this repository:

- `pkg/mcp` should prioritize `sse` compatibility and Streamable HTTP semantics rather than introducing a WebSocket-first design.

### 4.3 ACP Gateway

This repository does not yet implement ACP. The recommendations below assume ACP means an agent communication gateway rather than a UI-control adapter.

Recommended support:

- required
  - unary HTTP JSON
  - streaming HTTP SSE for long-running runs and event output
- optional
  - polling-based status retrieval for async runs
- not required by default
  - WebSocket

Reasoning:

- ACP-style systems commonly need to expose progress, partial output, status changes, and final results.
- Those requirements fit HTTP plus SSE naturally.
- A separate async run model with `POST` plus `GET` status retrieval often covers the rest.
- WebSocket is only justified if the ACP surface needs true duplex interaction, such as client interrupts, live human-in-the-loop commands, or shared-session event buses over one connection.

## 5. Recommended Build Order

### 5.1 Phase 1: Finish And Stabilize LLM API Streaming

Target:

- keep the current LLM gateway as HTTP plus SSE only
- implement the Anthropic/CC fidelity and streaming boundary defined in
  [Anthropic Messages Protocol Fidelity Architecture](anthropic-protocol-fidelity.md)

Work items:

- keep OpenAI and Anthropic streaming behavior aligned with current wire expectations
- make streaming behavior explicit in README and DESIGN docs
- avoid introducing a parallel WebSocket serving path for the same APIs
- ensure route capability checks can reject unsupported streaming combinations cleanly

Success criteria:

- one clear production story for chat and responses streaming
- no protocol ambiguity between JSON, SSE, and hypothetical WebSocket variants

### 5.2 Phase 2: Introduce MCP As A Separate Gateway Surface

Target:

- add a distinct MCP gateway surface rather than mixing MCP into the existing LLM API handlers

Recommended external shape:

- local execution path through `stdio`
- remote execution path through one MCP HTTP endpoint implementing Streamable HTTP
- optional legacy compatibility endpoints if older SSE clients must be supported

Recommended internal ownership:

- `pkg/mcp`
  - transport semantics
  - client/session abstraction
- `pkg/admin`
  - MCP client and server configuration CRUD
- runtime assembly
  - MCP manager wiring
  - auth and session policies

Do not:

- model MCP as just another `llm_api`
- force MCP traffic through OpenAI or Anthropic route handlers
- make WebSocket the primary remote MCP transport

### 5.3 Phase 3: Add ACP Only After Defining The Product Boundary

Target:

- decide whether ACP in this repository means:
  - agent-to-agent communication
  - agent-to-application communication
  - UI control protocol

Before implementation, define:

- run lifecycle model
- whether sessions are required
- whether the gateway is only a reverse proxy or also a stateful coordinator
- whether streaming is inline SSE or async run plus status polling

Recommended first serving shape:

- `POST` to start a run
- optional SSE stream for inline progress and output
- `GET` for run state or result
- `POST` for cancel or resume if needed

Do not start with:

- bidirectional WebSocket session design
- a transport abstraction before the run model is stable

## 6. Caddy And Standalone Integration Strategy

### 6.1 Caddy Path

Recommended role:

- continue using Caddy HTTP middleware for LLM API routing
- add separate handler modules for future MCP or ACP surfaces when needed

Do not:

- overload `http.handlers.agent_route_dispatcher` with non-LLM protocols that have different request lifecycles

Reasoning:

- LLM API dispatch is route-and-provider oriented
- MCP and ACP have different session, transport, and state-management concerns

### 6.2 Standalone `agwd` Path

Recommended role:

- use the standalone server as the first place to integrate MCP or ACP orchestration concerns
- keep protocol assembly isolated from core route/provider logic

Reasoning:

- non-LLM protocols may need session registries, background workers, or connection state that fit a standalone runtime more naturally than a thin Caddy middleware path

## 7. Repository-Level Recommendations

The next concrete steps for this repository should be:

1. document LLM streaming as HTTP plus SSE only
2. keep future MCP remote transport planning centered on Streamable HTTP rather than generic transport expansion
3. define ACP product scope before adding transport code

If implementation begins soon, the recommended order is:

1. finish LLM API polish
2. build MCP manager plus Streamable HTTP path
3. add legacy MCP SSE compatibility only if target clients require it
4. evaluate ACP after the MCP boundary is stable

## 8. Non-Goals

This roadmap does not recommend:

- adding WebSocket to the current OpenAI-compatible or Anthropic-compatible APIs
- treating all future agent protocols as variants of `llm_api`
- implementing multiple remote transports before the repository has one stable MCP request path

## 9. Decision Summary

Recommended decisions:

- LLM API
  - keep HTTP JSON plus SSE
  - do not add WebSocket by default
- MCP
  - prioritize `stdio` and Streamable HTTP
  - keep SSE only as a compatibility path when needed
  - do not prioritize WebSocket
- ACP
  - prioritize HTTP JSON plus SSE or async run polling
  - add WebSocket only if true duplex interaction becomes a concrete requirement
