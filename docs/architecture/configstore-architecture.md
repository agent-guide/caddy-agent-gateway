# Config Store Architecture

## 1. Scope

This document describes the current `pkg/configstore/` architecture and the technical contract for persisted gateway configuration.

It covers:

- the generic config store interfaces
- backend construction and schema registration
- store schema, codec, and metadata responsibilities
- built-in persisted object schemas
- SQLite JSON persistence behavior
- runtime integration points
- error and validation contracts

This document records the implemented architecture and its operational contract.

## 2. Architecture Overview

The config store layer is schema-driven.

Runtime packages do not use per-object typed store interfaces. They use one schema-bound generic `ConfigStore` interface and perform type assertions at package boundaries after decoding objects.

The main layers are:

- `pkg/configstore/`
  - defines the generic runtime interfaces, shared errors, schema primitives, backend factory registration, backend opening, schema registration, and store lookup
- `pkg/configstore/schema/`
  - defines store-name constants and the built-in schemas for providers, credentials, routes, virtual keys, and managed models
- `pkg/configstore/sqlite/`
  - implements the SQLite JSON backend creator and schema-bound generic store
- `caddy/configstore/sqlite/`
  - adapts the SQLite backend into the Caddy module namespace

The key design rule is:

- schemas are registered once during gateway initialization
- runtime CRUD calls operate on a `ConfigStore` already bound to one schema
- domain objects do not implement a mandatory storage interface

## 3. Runtime Interfaces

### 3.1 `ConfigStore`

Defined in `pkg/configstore/configstore.go`.

```go
type ConfigStore interface {
	List(ctx context.Context) ([]any, error)
	ListByTag(ctx context.Context, tag string) ([]any, error)
	ListByTagPrefix(ctx context.Context, tagPrefix string) ([]any, error)

	Create(ctx context.Context, obj any) error
	Update(ctx context.Context, obj any) error
	Delete(ctx context.Context, keyParts ...any) error

	Get(ctx context.Context, keyParts ...any) (any, error)
	GetByIndex(ctx context.Context, indexName string, value any) (any, error)
}
```

Behavior:

- `List` returns all decoded objects for the bound schema.
- `ListByTag` filters by the schema tag column. An empty tag means no tag filter.
- `ListByTagPrefix` filters by a tag prefix. An empty prefix means no tag filter.
- `Create` derives primary keys, tag values, secondary index values, and payload bytes from the schema metadata and codec.
- `Update` targets the object by its derived primary key and returns `configstore.ErrNotFound` if no row is updated.
- `Delete` deletes by ordered primary-key parts.
- `Get` reads by ordered primary-key parts and returns `configstore.ErrNotFound` for a missing row.
- `GetByIndex` reads by a named secondary index and returns `configstore.ErrNotFound` for a missing row.

`Get` and `Delete` accept variadic key parts so the same interface supports both single-column and composite primary keys.

### 3.2 `ConfigStoreBackend`

Defined in `pkg/configstore/configstore.go`.

```go
type ConfigStoreBackend interface {
	Register(name string, storeSchema configstore.StoreSchema) error
	Get(name string) (ConfigStore, error)
}
```

Behavior:

- `Register` validates a schema, constructs a schema-bound store through the configured backend creator, and caches that store by name.
- `Get` returns the already-registered schema-bound store.
- unknown store names return `configstore.ErrUnknownStoreName`.
- duplicate registrations return `configstore.ErrStoreAlreadyRegistered`.

The generic implementation is `pkg/configstore.Backend`. Storage backends provide a `ConfigStoreCreator`; they do not need to reimplement schema registration and store caching.

### 3.3 `ConfigStoreCreator`

Defined in `pkg/configstore/factory.go`.

```go
type ConfigStoreCreator interface {
	NewStore(storeSchema configstore.StoreSchema) (configstore.ConfigStore, error)
}
```

A creator is responsible for building one concrete store for one schema. For SQLite, `NewStore` also runs table and index migration for that schema.

Backend creators are registered by name with:

```go
configstore.RegisterConfigStoreCreatorFactory(name, factory)
```

The current built-in creator name is `sqlite`.

## 4. Schema Model

### 4.1 `StoreSchema`

Defined in `pkg/configstore/schema.go`.

```go
type StoreSchema struct {
	Name  string
	Kind  string
	Table string

	PrimaryKeyColumns []string
	TagColumn         string
	DataColumn        string

	IndexColumns []IndexSchema
	Timestamped  bool

	Codec    ConfigObjectCodec
	Metadata ObjectMetadata
}
```

Fields:

- `Name`: stable store name used by runtime code.
- `Kind`: human-readable object kind used in errors.
- `Table`: physical table name.
- `PrimaryKeyColumns`: ordered primary-key columns.
- `TagColumn`: optional tag column. Empty means tag queries are unsupported.
- `DataColumn`: payload column containing encoded object data.
- `IndexColumns`: optional named secondary indexes.
- `Timestamped`: whether the table has `created_at` and `updated_at`.
- `Codec`: object-family-specific encoder and decoder.
- `Metadata`: object-family-specific storage metadata extractor.

Schema validation requires:

- non-empty schema name
- schema name matching the registered store name
- non-empty table name
- at least one primary-key column
- non-empty data column
- non-nil codec
- non-nil metadata extractor
- unique, non-empty secondary index names
- non-empty secondary index columns

### 4.2 `IndexSchema`

```go
type IndexSchema struct {
	Name   string
	Column string
	Unique bool
}
```

`Name` is the runtime lookup name passed to `GetByIndex`. `Column` is the physical column name. `Unique` controls whether SQLite creates a normal index or a unique index.

### 4.3 `ConfigObjectCodec`

```go
type ConfigObjectCodec interface {
	Encode(obj any) ([]byte, error)
	Decode(data []byte) (any, error)
}
```

The built-in schemas use JSON codecs in `pkg/configstore/schema`.

Codec responsibilities:

- validate that `Encode` receives the expected Go object family
- unwrap storage adapters before JSON encoding when needed
- decode persisted JSON through the domain package's decode function
- validate the decoded object type before returning it

### 4.4 `ObjectMetadata`

```go
type ObjectMetadata interface {
	PrimaryKey(obj any) ([]any, error)
	Tag(obj any) (string, bool, error)
	Indexes(obj any) (map[string]any, error)
}
```

Metadata responsibilities:

- extract ordered primary-key values from an object
- extract an optional tag value
- extract values for named secondary indexes

`configstore.MetadataFuncs` adapts function fields into `ObjectMetadata`.

### 4.5 Adapter Interfaces

`configstore.ObjectUnwrapper` lets an adapter attach storage metadata without changing the JSON payload:

```go
type ObjectUnwrapper interface {
	ConfigStoreObject() any
}
```

`configstore.TagCarrier` lets an adapter provide a tag value outside the persisted object payload:

```go
type TagCarrier interface {
	ConfigStoreTag() string
}
```

The built-in route schema uses `TagCarrier` before falling back to a `Tag` string field.

## 5. Store Names

Store names are centralized in `pkg/configstore/schema/storenames.go`.

```go
const (
	StoreProviders     = "providers"
	StoreCredentials   = "credentials"
	StoreRoutes        = "routes"
	StoreVirtualKeys   = "virtual_keys"
	StoreManagedModels = "managed_models"
)
```

Runtime code must use these constants rather than duplicating store-name string literals.

## 6. Built-In Schemas

Built-in schemas are defined in `pkg/configstore/schema/schemas.go`.

### 6.1 Providers

- schema variable: `schema.ProviderConfigSchema`
- store name: `providers`
- kind: `provider config`
- table: `providers`
- primary key: `id`
- tag column: `tag`
- tag value: `ProviderConfig.ProviderType`
- data column: `config`
- secondary indexes: none
- timestamped: yes
- decoded type: `*provider.ProviderConfig`

The provider config codec delegates decoding to `provider.DecodeStoredProviderConfig`.

### 6.2 Credentials

- schema variable: `schema.CredentialSchema`
- store name: `credentials`
- kind: `credential`
- table: `cliauth_credentials` (legacy table name; stores all managed upstream credential types)
- primary key: `id`
- tag column: `tag`
- tag value: `Credential.ProviderType`
- data column: `data`
- secondary indexes: none
- timestamped: yes
- decoded type: `*model.Credential`

The credential codec delegates decoding to `credential.DecodeCredential`.

### 6.3 Routes

- schema variable: `schema.RouteSchema`
- store name: `routes`
- kind: `route`
- table: `routes`
- primary key: `id`
- tag column: `tag`
- tag value: `configstore.TagCarrier.ConfigStoreTag()`, then `AgentRoute.Tag`, then empty string
- data column: `config`
- secondary indexes: none
- timestamped: yes
- decoded type: `*route.AgentRoute`

The route codec delegates decoding to `route.DecodeStoredRoute`.

### 6.4 Virtual Keys

- schema variable: `schema.VirtualKeySchema`
- store name: `virtual_keys`
- kind: `virtual key`
- table: `virtual_keys`
- primary key: `id`
- tag column: `tag`
- tag value: `VirtualKey.Tag`, or empty string when absent
- data column: `config`
- secondary indexes:
  - name: `key`
  - column: `key`
  - unique: yes
- timestamped: yes
- decoded type: `*virtualkey.VirtualKey`

Runtime bearer-key lookup uses:

```go
store.GetByIndex(ctx, "key", key)
```

### 6.5 Managed Models

- schema variable: `schema.ManagedModelSchema`
- store name: `managed_models`
- kind: `managed model`
- table: `model_configs`
- primary key:
  - `provider_id`
  - `upstream_model`
- tag column: none
- data column: `data`
- secondary indexes: none
- timestamped: no
- decoded type: `*modelcatalog.ManagedModel`

Managed models are addressed with ordered composite keys:

```go
store.Get(ctx, providerID, upstreamModel)
store.Delete(ctx, providerID, upstreamModel)
```

## 7. Backend Opening and Registration

### 7.1 Runtime Opening

Use `configstore.OpenBackend` for typed backend configuration:

```go
backend, err := configstore.OpenBackend(ctx, "sqlite", configstoresqlite.Config{
	SQLitePath: path,
}, logger)
```

Use `configstore.OpenBackendJSON` when the backend configuration is already available as JSON.

Both functions:

- look up a registered backend creator factory by name
- construct a storage-specific `ConfigStoreCreator`
- wrap it in the generic `configstore.Backend`

### 7.2 Default Schema Registration

After a backend is opened, the gateway registers the built-in schemas with:

```go
schema.RegisterDefaultStores(backend)
```

Registration order:

1. `schema.ProviderConfigSchema`
2. `schema.CredentialSchema`
3. `schema.RouteSchema`
4. `schema.VirtualKeySchema`
5. `schema.ManagedModelSchema`

After registration, runtime packages retrieve stores with:

```go
store, err := backend.Get(schema.StoreRoutes)
```

## 8. SQLite JSON Backend

### 8.1 Packages

Runtime backend implementation:

- `pkg/configstore/sqlite/creator.go`
- `pkg/configstore/sqlite/migrate.go`
- `pkg/configstore/sqlite/json_config_store.go`
- `pkg/configstore/sqlite/logger.go`

Caddy adapter:

- `caddy/configstore/sqlite/configstore.go`

### 8.2 Configuration

The runtime SQLite config is:

```go
type Config struct {
	SQLitePath string `json:"sqlite_path,omitempty"`
}
```

`SQLitePath` is required by the runtime backend.

The Caddy module defaults the path to:

```text
<caddy.AppDataDir()>/agent-gateway/configstore.db
```

The Caddyfile shape is:

```caddy
config_store sqlite {
	path ./data/configstore.db
}
```

### 8.3 Connection

SQLite is opened through GORM with `github.com/glebarez/sqlite`.

The DSN enables:

- WAL journal mode
- normal synchronous mode
- cache size `10000`
- busy timeout `60000`
- WAL autocheckpoint `1000`
- foreign keys

If the database file does not exist, the backend creates the parent directory and file.

### 8.4 Migration

`SQLiteConfigStoreCreator.NewStore(schema)` calls `migrateSchema` before returning a store.

For each schema, migration creates:

- the schema table if it does not already exist
- primary-key columns as `TEXT NOT NULL`
- the optional tag column as `TEXT NOT NULL DEFAULT ''`
- secondary index columns as `TEXT NOT NULL`
- the data column as `TEXT NOT NULL`
- optional `created_at` and `updated_at` columns
- a primary key across the ordered `PrimaryKeyColumns`
- one SQLite index for every `IndexSchema`

Secondary indexes are named:

```text
idx_<table>_<index name>
```

Unique index schemas create `UNIQUE INDEX`.

### 8.5 CRUD Semantics

The concrete generic store is `sqlite.JSONConfigStore`.

Create:

- extracts key, tag, and index values from metadata
- encodes the object with the schema codec
- inserts one row
- sets `created_at` and `updated_at` for timestamped schemas

Update:

- extracts the object primary key
- updates the matching row
- does not update `created_at`
- sets a new `updated_at` for timestamped schemas
- returns `configstore.ErrNotFound` when no row is affected

Delete:

- validates key-part count
- deletes by primary key
- does not treat a missing row as an error

Get:

- validates key-part count
- reads by primary key
- returns `configstore.ErrNotFound` for a missing row

GetByIndex:

- validates the named index exists in the bound schema
- reads by the index column
- returns `configstore.ErrNotFound` for a missing row

List:

- returns all rows ordered by primary key ascending

ListByTag:

- requires `TagColumn`
- an empty tag returns all rows
- otherwise filters with `tag = ?`
- rows are ordered by primary key ascending

ListByTagPrefix:

- requires `TagColumn`
- an empty prefix returns all rows
- otherwise filters with `tag LIKE '<prefix>%'`
- rows are ordered by primary key ascending

### 8.6 Decoding and Timestamps

Rows are decoded from the schema data column through the schema codec.

For timestamped schemas, `JSONConfigStore` applies row timestamps back onto decoded pointer-to-struct objects when they have settable `CreatedAt` or `UpdatedAt` fields of type `time.Time`.

Objects without those fields still decode successfully.

## 9. Error Contract

Shared errors:

- `configstore.ErrNotFound`
  - returned by `Get`, `GetByIndex`, and missing-row `Update`
- `configstore.ErrUnknownStoreName`
  - returned by `Backend.Get` for unregistered stores
- `configstore.ErrStoreAlreadyRegistered`
  - returned by `Backend.Register` for duplicate registrations
- `sqlite.ErrTagUnsupported`
  - returned by tag query methods when the schema has no tag column
- `sqlite.ErrInvalidKeyParts`
  - returned when key-part count does not match the schema primary key
- `sqlite.ErrUnknownIndexName`
  - returned by `GetByIndex` for indexes not declared by the schema

Callers use `errors.Is` for these errors.

## 10. Runtime Integration

### 10.1 Caddy App

`caddy/gateway.App` loads one config store backend from the `agent_gateway` app config.

If no config store is configured, it creates the SQLite Caddy module with the default path.

During provisioning:

1. the Caddy config store module opens the runtime backend
2. the app calls `schema.RegisterDefaultStores`
3. the app retrieves the credential store
4. the app constructs the shared credential manager and configures the external request-time refresh command
5. the app bootstraps `pkg/gateway.AgentGateway` with the backend

### 10.2 Standalone Server

`standalone/server/server.go` opens the SQLite backend from the standalone config store path and calls `schema.RegisterDefaultStores` before constructing managers and gateway runtime state.

### 10.3 Gateway Runtime

`pkg/gateway.AgentGateway` retrieves schema-bound stores from the backend for:

- routes: `schema.StoreRoutes`
- virtual keys: `schema.StoreVirtualKeys`
- providers: `schema.StoreProviders`
- managed models: `schema.StoreManagedModels`

The gateway combines these dynamic stores with static Caddyfile-provided providers, routes, and models during bootstrap.

### 10.4 Managers

Managers consume generic stores:

- provider resolution lists provider configs by provider type tag
- route management lists routes by tag or tag prefix
- virtual key management gets bearer keys by the `key` secondary index
- credential management lists credentials through tag-prefix queries
- model catalog management addresses managed models by composite key

## 11. Extension Contract

Every built-in persisted object family has:

- one store-name constant in `pkg/configstore/schema/storenames.go`
- one `StoreSchema` in `pkg/configstore/schema`
- one codec that validates, encodes, and decodes the object family
- metadata extraction for primary key, tag, and secondary indexes
- registration through `schema.RegisterDefaultStores`
- runtime access through `ConfigStoreBackend.Get`

The generic `ConfigStore` interface is the default persistence boundary. Per-object typed store interfaces are outside the current configstore architecture.

## 12. Technical Invariants

- Runtime CRUD methods do not accept schemas; stores are schema-bound at registration time.
- Store names are stable constants under `pkg/configstore/schema`.
- Schema registration is per backend instance, not global mutable schema state.
- SQLite tables are migrated from schema definitions.
- Primary-key values are always extracted through schema metadata.
- Payloads are always encoded and decoded through schema codecs.
- Tag queries are only valid for schemas with `TagColumn`.
- Secondary index lookups are only valid for indexes declared in `IndexColumns`.
- Composite primary keys must preserve the order declared in `PrimaryKeyColumns`.
- Missing reads and missing updates normalize to `configstore.ErrNotFound`.
