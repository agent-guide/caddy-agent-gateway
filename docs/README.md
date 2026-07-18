# Documentation

This directory holds the detailed documentation for `agent-gateway`. The repository root `README.md` is intentionally limited to project overview and quick start; detailed operational, architectural, and design material belongs here.

## Sections

The documentation is organized into these categories:

- `getting-started/`
  - first-run setup for `agw`, `agwd`, and `agwctl`
  - first route, first VirtualKey, first successful request
- `guides/`
  - task-oriented usage guides such as providers, routes, credentials, CLI auth, bundle YAML, and MCP operations
- `reference/`
  - Caddyfile syntax, Admin API endpoints, CLI command reference, provider options, and runtime mode reference
- `architecture/`
  - current implemented system architecture and request flow
- `design/`
  - durable design notes, technical specifications, and roadmap documents
- `plans/`
  - time-bound execution plans tied to a version line; deleted once their
    work lands and the permanent documents describe the result
- `development/`
  - contributor-facing process or collaboration material

Category index:

- [getting-started/](getting-started/README.md)
- [guides/](guides/README.md)
- [reference/](reference/README.md)
- [architecture/](architecture/README.md)
- [design/](design/README.md)
- [plans/](plans/README.md)
- [development/](development/README.md)

## Current Documents

Primary detailed documents:

- [architecture/architecture-overview.md](architecture/architecture-overview.md): current repository architecture overview
- [architecture/configstore-architecture.md](architecture/configstore-architecture.md): ConfigStore architecture and persistence contract
- [architecture/mcp-architecture.md](architecture/mcp-architecture.md): MCP gateway architecture and implementation status
- [architecture/acp-architecture.md](architecture/acp-architecture.md): ACP gateway architecture and implementation status
- [getting-started/quickstart-acp.md](getting-started/quickstart-acp.md): ACP gateway quick start
- [reference/acp-api.md](reference/acp-api.md): ACP dispatcher and Admin API reference
- [reference/acp-technical-spec.md](reference/acp-technical-spec.md): ACP service, route, runtime, and event specification
- [design/gateway-bundle-yaml.md](design/gateway-bundle-yaml.md): bundle YAML architecture and workflow
- [design/mcp-tool-policy.md](design/mcp-tool-policy.md): MCP tool policy design
- [design/model-first-routing.md](design/model-first-routing.md): model-first routing architecture
- [design/route-target-policy.md](design/route-target-policy.md): route target policy architecture
- [design/memory.md](design/memory.md): memory subsystem design
- [design/observability.md](design/observability.md): observability design
- [plans/observability-implementation.md](plans/observability-implementation.md): observability implementation plan
- [design/protocol-support-roadmap.md](design/protocol-support-roadmap.md): protocol support roadmap
- [development/ai-assisted-refactor-collaboration-templates.md](development/ai-assisted-refactor-collaboration-templates.md): contributor collaboration templates

## Notes

- the root `README.md` is intentionally limited to overview and quick start
