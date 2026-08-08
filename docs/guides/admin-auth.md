# Admin API Auth

The gateway Admin API does not implement login, sessions, or bearer session
tokens. Protect the path where `agent_gateway_admin` is mounted with the HTTP
deployment boundary: Caddy `basic_auth`, mTLS, a reverse proxy authenticator, an
OAuth2 proxy, or a private loopback listener.

## Caddy Mode

```caddy
http://localhost:8019 {
	route /admin/* {
		# Exempt the public health probe and CORS preflight (which carries no
		# credentials) so browser admin UIs and health checks keep working.
		@needauth {
			not path /admin/health
			not method OPTIONS
		}
		basic_auth @needauth {
			admin <hashed-password>
		}
		agent_gateway_admin
	}
}
```

`GET /admin/health` and CORS preflight (`OPTIONS`) stay public so health probes
and browser admin UIs keep working; everything else requires Basic Auth.

Generate a Caddy-compatible password hash:

```bash
./agw hash-password --plaintext 'your-password'
```

Admin API clients then send standard Basic Auth:

```bash
curl -u admin:your-password http://localhost:8019/admin/health
```

`agwctl` can send the same credentials:

```bash
export AGW_ADMIN_BASIC_AUTH=admin:your-password
./agwctl provider list
```

For proxy-based auth, use repeated `--admin-header 'Name: value'` flags instead.

## Standalone Mode

`agwd` defaults the Admin API listener to `localhost:8019`. Keep it on loopback
when another local process manages the gateway. To enable standalone Basic Auth:

```bash
AGW_ADMIN_BASIC_AUTH_HASH=admin:<hashed-password> ./agwd
```

The equivalent flag is:

```bash
./agwd --admin-basic-auth-hash admin:<hashed-password>
```

The standalone wrapper also keeps `GET /admin/health` and `OPTIONS` preflight
public; all other admin routes require Basic Auth.

`AGW_ADMIN_BASIC_AUTH_HASH` is validated at startup: a value that is not
`username:bcrypt-hash` (for example a plaintext password) fails the daemon fast
instead of erroring per request. When no hash is set and `--admin-addr` is bound
beyond loopback, the daemon logs a warning because the Admin API is then exposed
without authentication.

## First Admin Call

Create a VirtualKey:

```bash
curl -u admin:your-password -X POST http://localhost:8019/admin/virtual_keys \
  -H 'Content-Type: application/json' \
  -d '{
    "id": "demo-key",
    "allowed_route_ids": ["openai-chat"]
  }'
```

The `id` is the management identifier. The response `key` is the bearer value
that clients send on request traffic.
