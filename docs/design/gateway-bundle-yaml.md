# Gateway Bundle YAML

## 1. Purpose

This document describes the architecture and current technical implementation of the gateway bundle YAML workflow.

The goal of this design is to give `agwctl` and `agwd` one shared, operator-friendly configuration format for gateway objects, without collapsing the existing distinction between:

- static read-only configuration
- dynamic writable configuration

The bundle format is intentionally scoped to gateway object configuration. It does not replace every daemon process flag, every admin API endpoint, or every runtime control surface.

## 2. Design Goals

The bundle design is built around these goals:

- provide one declarative file format for configuration-type gateway objects
- make batch changes easier than per-object JSON workflows
- reuse existing runtime models instead of inventing a parallel DSL
- preserve the current static-versus-dynamic configuration boundary
- keep standalone daemon behavior aligned with Caddy-backed static object behavior

## 3. Architecture Summary

The same bundle schema is used in two different runtime modes:

```text
gateway bundle YAML
  |
  +-> agwctl validate
  |      -> local parse + local validation
  |
  +-> agwctl apply
  |      -> local parse + local validation
  |      -> remote Admin API create/update
  |
  +-> agwctl export
  |      -> remote Admin API read
  |      -> bundle YAML serialization
  |
  +-> agwd --static-config
         -> local parse + local validation
         -> static read-only runtime objects
```

The object schema is shared, but the runtime semantics are not:

- `agwctl apply` is a dynamic mutation path
- `agwd --static-config` is a startup-only static configuration path

This separation is the core design rule.

## 4. Configuration Model

### 4.1 Sources Of Truth

The runtime can observe configuration from two sources:

1. persisted dynamic config in the config store
2. static configuration loaded at startup

For Caddy-backed runtime, static configuration comes from the Caddyfile.

For standalone runtime, static configuration can now come from a gateway bundle YAML file passed through:

```bash
agwd --config-store ./data/configstore.db --static-config ./gateway.yaml
```

### 4.2 Static And Dynamic Semantics

Static bundle objects are intentionally equivalent to Caddyfile-owned static objects:

- they are loaded during startup
- they are visible through Admin API list/get endpoints
- they are read-only through Admin API mutation paths
- they are merged with dynamic objects at read time

Static route limitation:

- standalone `--static-config` `llmRoutes` only support direct-provider targets
- logical-model routes remain valid in config-store bundle workflows such as `agwctl apply`

Dynamic objects remain writable through the Admin API and are persisted in SQLite.

Static objects are not persisted into SQLite as pre-seeded rows. They remain a distinct runtime source.

## 5. Object Classification

### 5.1 Configuration-Type Objects

Configuration-type objects are:

- `providers`
- `managedModels`
- `llmRoutes`
- `virtualKeys`
- `mcpServices`
- `mcpRoutes`
- `agents`
- `agentRoutes`

These objects are expected to converge on one declarative workflow centered on gateway bundle YAML.

Managed `credentials` are also configuration-type objects, but they are the
one current exception: bundle apply/export does not include them. An exported
bundle is therefore not a complete backup of gateway authentication state.

### 5.2 Operational-Type Objects

Operational-type objects are:

- interactive OAuth login state, which belongs to the external `agw-auth` tool
- ephemeral provider, MCP, Agent, and dispatcher runtime state

These remain command-oriented runtime operations and should continue to use explicit CLI arguments and subcommands rather than bundle YAML.

## 6. CLI Design

### 6.1 `agwctl`

The formal configuration workflow is:

```bash
agwctl export -f gateway.yaml
agwctl validate -f gateway.yaml
agwctl apply -f gateway.yaml
```

The semantics are:

- `export`: read current remote configuration objects and serialize them as bundle YAML
- `validate`: parse and validate the bundle locally without contacting the server
- `apply`: create or update remote configuration objects through the Admin API

For configuration-type objects, per-object JSON `create` / `update` / `upsert` commands are no longer part of the formal CLI path.

The following command families remain for inspection and direct control:

- `list`
- `get`
- `delete`
- `enable`
- `disable`

### 6.2 `agwd`

Standalone daemon flag semantics are:

```bash
agwd --config-store ./data/configstore.db --static-config ./gateway.yaml
```

Flag meaning:

- `--config-store`: SQLite config store path
- `--static-config`: gateway bundle YAML loaded as static read-only configuration

## 7. Bundle Schema

The bundle format is a serialization wrapper over existing runtime object shapes.

Top-level metadata:

- `apiVersion`
- `kind`

Current schema shape:

```yaml
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle

providers:
  - id: openai-main
    provider_type: openai
    api_key: ${OPENAI_API_KEY}
    default_model: gpt-4.1

managedModels:
  - provider_id: openai-main
    upstream_model: gpt-4.1
    enabled: true

llmRoutes:
  - id: chat-prod
    protocol: openai
    match_policy:
      path_prefix: /
      methods:
        - POST
    auth_policy:
      require_virtual_key: true
    target_policy:
      provider_target:
        provider_id: openai-main

virtualKeys:
  - id: vk-local-test
    allowed_route_ids:
      - chat-prod

mcpServices:
  - id: tools
    name: Tools
    transport: stdio
    command: ./tools-server

mcpRoutes:
  - id: mcp-tools
    kind: mcp
    service_id: tools
    match_policy:
      path_prefix: /mcp/tools

agents:
  - id: assistant
    name: Assistant
    runtime:
      type: http
      http:
        endpoint: https://example.com/agent

agentRoutes:
  - id: assistant
    kind: agent
    agent_id: assistant
    match_policy:
      path_prefix: /agents/assistant

```

`virtualKeys[].id` is the declarative identifier for config-store bundle workflows such as `agwctl apply`. Standalone `--static-config` does not support `virtualKeys`.

Field naming is intentionally kept close to existing JSON model fields so the bundle can reuse current runtime types.

## 8. Current Implemented Scope

The current implemented bundle path covers:

- `providers`
- `managedModels`
- `llmRoutes`
- `virtualKeys`
- `mcpServices`
- `mcpRoutes`
- `agents`
- `agentRoutes`

The current implemented bundle path does not yet cover:

- `credentials`
- interactive OAuth login sessions and authenticator configuration
- ephemeral runtime state

That means `credentials` are already classified as configuration-type objects, but are not yet represented in the current bundle implementation.

## 9. Parsing And Serialization

The shared implementation lives in `pkg/gatewaybundle/`.

Current responsibilities of that package:

- define the `GatewayBundle` data model
- load bundle YAML from file
- decode YAML through a normalized intermediate form
- expand environment variables in scalar strings
- serialize bundle data back to YAML

Implementation detail:

- the loader follows `YAML -> generic object -> env expansion -> JSON -> typed runtime structs`

This avoids introducing a second field-tagging strategy across the existing runtime object model and lets the bundle reuse current `json` field names.

## 10. Validation Model

Bundle validation is local and structural. It happens before remote writes and before standalone startup.

Current validation includes:

- required `apiVersion`
- required `kind`
- duplicate provider IDs
- duplicate managed model keys
- duplicate route IDs
- duplicate virtual key keys
- duplicate MCP service IDs
- duplicate Agent IDs
- provider type existence checks
- route target references to provider IDs inside the bundle
- virtual key route references inside the bundle
- Agent resource/route references inside the bundle
- AgentRoute references to Agents inside the bundle
- route normalization and route definition validation through existing route helpers

Validation is implemented in `pkg/gatewaybundle.Validate()`.

Validation errors are aggregated so users can see multiple file problems in one run.

## 11. Apply Semantics

`agwctl apply -f gateway.yaml` is a declarative create-or-update operation.

Current behavior:

- create object if it does not exist
- update object if it exists and differs
- skip object if the remote object already matches the bundle
- error if the object is read-only and differs

Current object application order:

1. `providers`
2. `managedModels`
3. `llmRoutes`
4. `mcpServices`
5. `mcpRoutes`
6. `agents`
7. `agentRoutes`
8. `virtualKeys`

Current non-behavior:

- no automatic deletion
- no prune semantics
- no ownership tracking across files

`apply` returns a summary of:

- `create`
- `update`
- `skip`
- `error`

## 12. Export Semantics

`agwctl export` reads remote configuration objects from the Admin API and serializes them as a gateway bundle YAML file.

Current behavior:

- export providers, managed models, LLM routes, virtual keys, MCP
  services/routes, and Agents/AgentRoutes
- omit Admin API read-only/source wrapper fields from the re-apply file
- emit a re-usable bundle shape intended for later `validate` and `apply`

Export does not read or serialize managed `credentials`. Operators must manage
and back up those separately through `/admin/credentials`, and any UI exposing
bundle import/export must state this limitation before the action.

Export is designed for operator editing and round-tripping, not for lossless archival of every response field.

## 13. Standalone Static Loading

In standalone mode, the bundle is loaded before gateway bootstrap.

Current standalone integration does the following:

- load bundle YAML from `--static-config`
- validate the bundle locally
- instantiate static providers from bundle provider configs
- register static provider API-key credentials into the shared credential manager
- pass static providers, managed models, and routes into `gateway.BootstrapOptions`

The shared schema contains more dynamic object families than standalone static
loading supports. Standalone validates its own narrower startup boundary; the
full schema is intended for config-store workflows through `agwctl
apply`.

The resulting runtime behavior matches the existing static-object model:

- static providers are read-only
- static routes are read-only
- static managed models participate in model catalog reads

## 14. Secrets Handling

The current bundle implementation supports environment-variable expansion in scalar string values.

Example:

```yaml
providers:
  - id: openai-main
    provider_type: openai
    api_key: ${OPENAI_API_KEY}
```

Current behavior:

- if a referenced environment variable is missing, bundle loading fails

The design intentionally keeps secret indirection minimal in the current implementation.

## 15. Error Model

Relevant error classes in the current implementation include:

- file read errors
- YAML decode errors
- missing environment variable errors
- local bundle validation errors
- remote Admin API mutation errors during apply
- read-only mutation conflicts
- partial apply failures

Errors are reported using object-qualified identities where possible, such as:

- provider ID
- managed model key
- route ID
- virtual key key

## 16. Compatibility

This design does not remove:

- existing Admin API CRUD endpoints
- static Caddyfile configuration
- SQLite-backed dynamic configuration

What it changes is the recommended CLI write path:

- configuration-type objects now use bundle YAML as the formal configuration workflow
- operational workflows remain command-driven

For configuration-type objects, the CLI no longer exposes per-object JSON `create` / `update` / `upsert` commands as the normal path.

## 17. Current Limitations

Known current limitations:

- `credentials` are classified as configuration-type objects but are not yet represented in bundle `apply` / `export`
- no `prune` or ownership-aware deletion exists
- no dedicated `diff` or `plan` command exists
- secret handling is limited to environment-variable expansion

These are implementation boundaries, not contradictions in the main design.

## 18. Design Constraints And Risks

The implementation must continue to protect the following invariants:

- bundle YAML must not become a disguised dynamic row preload
- static bundle objects must remain read-only through Admin API mutation paths
- field naming must stay aligned with the underlying runtime object model
- export output must stay suitable for validate/apply round-trip use
- interactive OAuth flows remain outside static configuration in `agw-auth`

The main architectural risk remains semantic drift between:

- shared bundle schema
- Admin API object wrappers
- standalone static bootstrap behavior

The current implementation reduces that risk by centering parsing and validation in one shared package and by reusing existing runtime object types wherever practical.
