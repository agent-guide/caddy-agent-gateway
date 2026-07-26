# pkg/configstore — AGENTS.md

Scope: the generic config store and its backends. Paths are repository-root
relative; the root `AGENTS.md` global rules apply. Broader context:
`docs/architecture/configstore-architecture.md`.

Important packages:

- `pkg/configstore/`: generic store/backend interfaces, shared schema primitives, backend factory, and backend registration
- `pkg/configstore/schema/`: store names and built-in business schemas for persisted config object families
- `pkg/configstore/sqlite/`: SQLite JSON backend implementation
- `caddy/configstore/sqlite/`: SQLite backend Caddy adapter

The top-level storage interface is `ConfigStoreBackend`, which registers and returns schema-bound generic stores:

- `Register(name string, schema StoreSchema) error`
- `Get(name string) (ConfigStore, error)`

Current store names:

- `providers`
- `credentials`
- `routes`
- `mcp_services`
- `acp_services`
- `agents`
- `virtual_keys`
- `managed_models`

Current persisted backend:

- `sqlite`
