# Quick Start: MCP Gateway

This guide runs the MCP gateway with one upstream MCP service, one MCP route, one VirtualKey, and one verified end-to-end MCP request. All configuration is applied dynamically via `agwctl apply`.

## Prerequisites

- Go toolchain installed
- A running upstream MCP server accessible over HTTP, **or** `npx` available for the local stdio alternative
- `jq` installed

## 1. Build

```bash
make build
```

## 2. Create a Minimal Caddyfile

No MCP services or routes are declared here — they are applied dynamically:

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
			admin <hashed-password>
		}
		agent_gateway_admin
	}
}

http://127.0.0.1:8080 {
	agent_route_dispatcher {
		llm_api openai
		llm_api anthropic
		mcp
	}
}
```

The `mcp` directive in `agent_route_dispatcher` enables MCP protocol handling on the dispatcher.

## 3. Generate the Admin Password Hash

```bash
./agw hash-password --plaintext 'your-password'
```

Replace `<bcrypt-hash>` in the Caddyfile with the generated value.

## 4. Run the Gateway

```bash
./agw run --config ./Caddyfile
```

## 5. Create a Bundle File

### Option A: Streamable HTTP upstream

Use this when you have a remote MCP server reachable over HTTP:

```yaml
# gateway.bundle.mcp.yaml
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle
mcpServices:
  - id: mcp-main
    name: Main MCP Service
    transport: streamable_http
    url: ${MCP_SERVICE_URL}
mcpRoutes:
  - id: mcp-main-route
    service_id: mcp-main
    match_policy:
      path_prefix: /mcp
    auth_policy:
      require_virtual_key: true
virtualKeys:
  - id: mcp-key
    allowed_route_ids:
      - mcp-main-route
```

Route ids must be slash-free. The route above sets an explicit `id` reused in
`allowed_route_ids`. If you omit `id`, the gateway auto-generates a deterministic
id `mcp:<service_id>:<path-slug>` (path prefix lowercased, non-alphanumeric runs
collapsed to `-`, `/` → `root`) — e.g. `mcp:mcp-main:mcp` — which is predictable,
so you can reference it directly instead of setting an explicit id.

### Option B: stdio subprocess upstream

Use this when the MCP server runs as a local child process. The example uses the standard filesystem MCP server:

```bash
npm install -g @modelcontextprotocol/server-filesystem
```

```yaml
# gateway.bundle.mcp.yaml
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle
mcpServices:
  - id: mcp-fs
    name: Filesystem MCP Service
    transport: stdio
    command: npx
    args:
      - -y
      - "@modelcontextprotocol/server-filesystem"
      - /tmp
mcpRoutes:
  - id: mcp-fs-route
    service_id: mcp-fs
    match_policy:
      path_prefix: /mcp
    auth_policy:
      require_virtual_key: true
virtualKeys:
  - id: mcp-key
    allowed_route_ids:
      - mcp-fs-route
```

## 6. Apply the Bundle

Set admin credentials as environment variables, then apply:

```bash
export AGW_ADMIN_BASIC_AUTH=admin:your-password

# Option A
MCP_SERVICE_URL=https://your-mcp-server/mcp ./agwctl apply -f gateway.bundle.mcp.yaml

# Option B (stdio — no extra env var needed)
./agwctl apply -f gateway.bundle.mcp.yaml
```

Expected output:

```
apply: gateway.bundle.mcp.yaml
  create mcp_service mcp-main
  create mcp_route mcp-main-route
  create virtual_key mcp-key
summary: create=3 update=0 skip=0 error=0
```

## 7. Verify via the Admin API

Confirm the service is registered and the upstream session is initialized:

```bash
./agwctl mcp-service list
./agwctl mcp-service session mcp-main   # or mcp-fs for Option B
```

Discover tools exposed by the upstream:

```bash
./agwctl mcp-service tools mcp-main
```

## 8. Send an MCP Request

Retrieve the generated VirtualKey value:

```bash
MCP_API_KEY=$(./agwctl virtualkey get mcp-key | jq -r '.key')
```

Initialize the MCP session and list tools through the gateway route:

```bash
# Initialize
curl -s http://127.0.0.1:8080/mcp \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $MCP_API_KEY" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"quickstart","version":"0.1"},"capabilities":{}}}'

# List tools
curl -s http://127.0.0.1:8080/mcp \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $MCP_API_KEY" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
```

The gateway accepts the VirtualKey as `Authorization: Bearer <key>` or `x-api-key: <key>`.

For a complete Python-based end-to-end client, see [`examples/test_mcp_gateway_client.py`](../../examples/test_mcp_gateway_client.py).

## Notes

- The MCP dispatcher is enabled by the `mcp` directive inside `agent_route_dispatcher`. LLM routes and MCP routes share the same dispatcher and config store.
- MCP route IDs are auto-generated as the deterministic, slash-free `mcp:<service_id>:<path-slug>` when `id` is omitted in the bundle (path prefix lowercased, non-alphanumeric runs collapsed to `-`, `/` → `root`).
- The gateway initializes an upstream session on first use and caches it. Inspect session state with `agwctl mcp-service session <id>`.
- The Admin API has no built-in sessions; authentication is handled by the HTTP mount layer.
- Run `agwctl export` to dump the current gateway state as a bundle YAML.

## Next

- [mcp-gateway.md](../guides/mcp-gateway.md): MCP service and route configuration reference
- [mcp-architecture.md](../architecture/mcp-architecture.md): MCP gateway architecture and design
- [quickstart-llm.md](quickstart-llm.md): LLM gateway quick start
