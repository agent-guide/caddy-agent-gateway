# Quick Start: ACP Agent

This guide runs a Codex ACP runtime behind a unified AgentRoute, sends a
streamed turn, and inspects the Agent runtime.

## Prerequisites

- Go toolchain and `jq`
- Codex auth already configured
- `codex-acp` on `PATH`

Install the adapter if needed:

```bash
npm install -g @zed-industries/codex-acp
```

For OpenCode, install and authenticate `opencode`, then set
`runtime.acp.agent_type: opencode`.

## 1. Build And Configure

```bash
make build
./agw hash-password --plaintext 'your-password'
```

Use the generated hash in this minimal Caddyfile:

```caddy
{
	admin localhost:2019
	agent_gateway {
		config_store sqlite {
			path ./data/configstore.db
		}
	}
}

http://localhost:8019 {
	route /admin/* {
		basic_auth {
			admin <bcrypt-hash>
		}
		agent_gateway_admin
	}
}

http://127.0.0.1:8080 {
	agent_route_dispatcher {
		agent
	}
}
```

Start the gateway and create the Agent working directory:

```bash
mkdir -p /tmp/acp-codex-test
./agw run --config ./Caddyfile
```

## 2. Apply An Agent Bundle

```yaml
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle

agents:
  - id: codex-main
    name: Codex
    runtime:
      type: acp
      acp:
        agent_type: codex
        cwd: /tmp/acp-codex-test
        allowed_roots: [/tmp/acp-codex-test]
        max_instances: 4
        permission_mode: auto_approve
    routes: {}
    resources: {}
    policy: {}

agentRoutes:
  - id: agent-codex
    agent_id: codex-main
    match_policy:
      path_prefix: /agents/codex
    auth_policy:
      require_virtual_key: true

virtualKeys:
  - id: codex-key
    allowed_route_ids: [agent-codex]
```

Save it as `gateway.bundle.acp.yaml`, then apply and inspect it:

```bash
export AGW_ADMIN_BASIC_AUTH=admin:your-password
./agwctl apply -f gateway.bundle.acp.yaml
./agwctl agent get codex-main
./agwctl agent-route get agent-codex
./agwctl acp-runtime get
```

`runtime.acp.permission_mode` is `deny` by default. `auto_approve` selects an
allow option automatically; `interactive` emits a permission event for an
explicit client or operator decision.

## 3. Send A Turn

```bash
ACP_API_KEY=$(./agwctl virtualkey get codex-key | jq -r '.key')

curl -N -s http://127.0.0.1:8080/agents/codex/turn \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $ACP_API_KEY" \
  -d '{"input":"Reply with exactly one word: pong","options":{"version":"v1","runtime":{"thread_id":"t-demo-1"}}}'
```

Every SSE data object carries the common `agent_id`, `run_id`, and monotonic
`sequence`. The stream includes a `session` event whose `session_id` can be
passed in `options.runtime.session_id` on a later turn.

## 4. Inspect Sessions And Transcript

The consumer endpoints use the same AgentRoute and VirtualKey:

```bash
curl -s "http://127.0.0.1:8080/agents/codex/sessions?cwd=/tmp/acp-codex-test" \
  -H "Authorization: Bearer $ACP_API_KEY"

curl -s "http://127.0.0.1:8080/agents/codex/sessions/<session-id>/transcript?cwd=/tmp/acp-codex-test" \
  -H "Authorization: Bearer $ACP_API_KEY"
```

The equivalent operator reads are Agent-scoped:

```bash
./agwctl agent sessions codex-main --cwd /tmp/acp-codex-test
./agwctl agent transcript codex-main <session-id> --cwd /tmp/acp-codex-test
```

For `permission_mode: interactive`, inspect and resolve pending requests with:

```bash
./agwctl agent permissions codex-main
./agwctl agent decide codex-main <request-id> \
  --outcome selected --option-id <option-id>
```

ACP-specific pool recovery remains available by Agent identity:

```bash
./agwctl acp-runtime close-thread codex-main t-demo-1
```

AgentRoute IDs default to the deterministic slash-free
`agent:<agent_id>:<path-slug>` form when omitted. The gateway accepts a
VirtualKey through either `Authorization: Bearer` or `x-api-key`.

## Next

- [ACP Architecture](../architecture/acp-architecture.md)
- [ACP Technical Specification](../reference/acp-technical-spec.md)
- [ACP API Reference](../reference/acp-api.md)
