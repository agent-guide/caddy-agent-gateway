package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	"github.com/agent-guide/agent-gateway/pkg/credential"
	credmodel "github.com/agent-guide/agent-gateway/pkg/credential/model"
	modelcatalog "github.com/agent-guide/agent-gateway/pkg/gateway/modelcatalog"
	routecore "github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	virtualkeypkg "github.com/agent-guide/agent-gateway/pkg/gateway/virtualkey"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	mcpservice "github.com/agent-guide/agent-gateway/pkg/mcp/service"
)

func RegisterDefaultStores(backend configstore.ConfigStoreBackend) error {
	if backend == nil {
		return fmt.Errorf("config store backend is nil")
	}
	schemas := []configstore.StoreSchema{
		ProviderConfigSchema,
		CredentialSchema,
		RouteSchema,
		VirtualKeySchema,
		ManagedModelSchema,
		MCPServiceSchema,
		AgentSchema,
	}
	for _, storeSchema := range schemas {
		if err := backend.Register(storeSchema.Name, storeSchema); err != nil {
			return fmt.Errorf("register config store schema %q: %w", storeSchema.Name, err)
		}
	}
	return nil
}

var ProviderConfigSchema = configstore.StoreSchema{
	Name:              StoreProviders,
	Kind:              "provider config",
	Table:             "providers",
	PrimaryKeyColumns: []string{"id"},
	TagColumn:         "tag",
	DataColumn:        "config",
	Timestamped:       true,
	Codec: typedJSONCodec{
		kind:     "provider config",
		decode:   provider.DecodeStoredProviderConfig,
		validate: validateProviderConfigObject,
	},
	Metadata: configstore.MetadataFuncs{
		PrimaryKeyFunc: primaryKeyFromStringFields("Id"),
		TagFunc:        requiredTagFromStringField("ProviderType", "provider_type"),
	},
}

// CredentialSchema keeps the historical SQLite table name so existing
// credential data remains visible after the broader credential-domain rename.
var CredentialSchema = configstore.StoreSchema{
	Name:              StoreCredentials,
	Kind:              "credential",
	Table:             "cliauth_credentials",
	PrimaryKeyColumns: []string{"id"},
	TagColumn:         "tag",
	DataColumn:        "data",
	Timestamped:       true,
	Codec: typedJSONCodec{
		kind:     "credential",
		decode:   credential.DecodeCredential,
		validate: validateCredentialObject,
	},
	Metadata: configstore.MetadataFuncs{
		PrimaryKeyFunc: primaryKeyFromStringFields("ID"),
		TagFunc:        requiredTagFromStringField("ProviderType", "provider_type"),
	},
}

var RouteSchema = configstore.StoreSchema{
	Name:              StoreRoutes,
	Kind:              "route",
	Table:             "routes",
	PrimaryKeyColumns: []string{"id"},
	TagColumn:         "tag",
	DataColumn:        "config",
	Timestamped:       true,
	Codec: typedJSONCodec{
		kind:     "route",
		decode:   routecore.DecodeStoredAgentRouteConfig,
		validate: validateRouteObject,
	},
	Metadata: configstore.MetadataFuncs{
		PrimaryKeyFunc: primaryKeyFromStringFields("ID"),
		TagFunc:        routeTagValue,
	},
}

var VirtualKeySchema = configstore.StoreSchema{
	Name:              StoreVirtualKeys,
	Kind:              "virtual key",
	Table:             "virtual_keys",
	PrimaryKeyColumns: []string{"id"},
	TagColumn:         "tag",
	DataColumn:        "config",
	IndexColumns: []configstore.IndexSchema{
		{Name: "key", Column: "key", Unique: true},
	},
	Timestamped: true,
	Codec: typedJSONCodec{
		kind:     "virtual key",
		decode:   virtualkeypkg.DecodeStoredVirtualKey,
		validate: validateVirtualKeyObject,
	},
	Metadata: configstore.MetadataFuncs{
		PrimaryKeyFunc: primaryKeyFromStringFields("ID"),
		TagFunc:        optionalTagFromStringField("Tag"),
		IndexesFunc: func(obj any) (map[string]any, error) {
			key, err := requiredStringField(obj, "Key", "key")
			if err != nil {
				return nil, err
			}
			return map[string]any{"key": key}, nil
		},
	},
}

var ManagedModelSchema = configstore.StoreSchema{
	Name:              StoreManagedModels,
	Kind:              "managed model",
	Table:             "model_configs",
	PrimaryKeyColumns: []string{"provider_id", "upstream_model"},
	DataColumn:        "data",
	Timestamped:       false,
	Codec: typedJSONCodec{
		kind:     "managed model",
		decode:   modelcatalog.DecodeStoredManagedModel,
		validate: validateManagedModelObject,
	},
	Metadata: configstore.MetadataFuncs{
		PrimaryKeyFunc: func(obj any) ([]any, error) {
			providerID, err := requiredStringField(obj, "ProviderID", "provider_id")
			if err != nil {
				return nil, err
			}
			upstreamModel, err := requiredStringField(obj, "UpstreamModel", "upstream_model")
			if err != nil {
				return nil, err
			}
			return []any{providerID, upstreamModel}, nil
		},
	},
}

var MCPServiceSchema = configstore.StoreSchema{
	Name:              StoreMCPServices,
	Kind:              "mcp service",
	Table:             "mcp_services",
	PrimaryKeyColumns: []string{"id"},
	TagColumn:         "tag",
	DataColumn:        "config",
	Timestamped:       true,
	Codec: typedJSONCodec{
		kind:     "mcp service",
		decode:   mcpservice.DecodeStoredMCPServiceConfig,
		validate: validateMCPServiceObject,
	},
	Metadata: configstore.MetadataFuncs{
		PrimaryKeyFunc: primaryKeyFromStringFields("ID"),
		TagFunc:        requiredTagFromStringField("Transport", "transport"),
	},
}

var AgentSchema = configstore.StoreSchema{
	Name:              StoreAgents,
	Kind:              "agent",
	Table:             "agents",
	PrimaryKeyColumns: []string{"id"},
	TagColumn:         "tag",
	DataColumn:        "config",
	Timestamped:       true,
	Codec: typedJSONCodec{
		kind:     "agent",
		decode:   agentpkg.DecodeStoredAgentConfig,
		validate: validateAgentObject,
	},
	Metadata: configstore.MetadataFuncs{
		PrimaryKeyFunc: primaryKeyFromStringFields("ID"),
		TagFunc:        agentRuntimeTagValue,
	},
}

type typedJSONCodec struct {
	kind     string
	decode   func([]byte) (any, error)
	validate func(obj any) error
}

func (c typedJSONCodec) Encode(obj any) ([]byte, error) {
	if err := c.validateObject(obj); err != nil {
		return nil, err
	}
	data, err := json.Marshal(unwrapConfigObject(obj))
	if err != nil {
		return nil, fmt.Errorf("%s marshal: %w", c.kind, err)
	}
	return data, nil
}

func (c typedJSONCodec) Decode(data []byte) (any, error) {
	if c.decode == nil {
		return nil, fmt.Errorf("%s decode is not configured", c.kind)
	}
	obj, err := c.decode(data)
	if err != nil {
		return nil, err
	}
	if err := c.validateObject(obj); err != nil {
		return nil, err
	}
	return obj, nil
}

func (c typedJSONCodec) validateObject(obj any) error {
	if c.validate == nil {
		return nil
	}
	return c.validate(obj)
}

func validateProviderConfigObject(obj any) error {
	switch unwrapConfigObject(obj).(type) {
	case provider.ProviderConfig, *provider.ProviderConfig:
		return nil
	default:
		return fmt.Errorf("provider config object has unexpected type %T", obj)
	}
}

func validateCredentialObject(obj any) error {
	switch value := unwrapConfigObject(obj).(type) {
	case credmodel.Credential:
		return value.Validate()
	case *credmodel.Credential:
		if value == nil {
			return fmt.Errorf("credential object is nil")
		}
		return value.Validate()
	default:
		return fmt.Errorf("credential object has unexpected type %T", obj)
	}
}

func validateRouteObject(obj any) error {
	var cfg *routecore.AgentRouteConfig
	switch value := unwrapConfigObject(obj).(type) {
	case routecore.AgentRouteConfig:
		cfg = &value
	case *routecore.AgentRouteConfig:
		cfg = value
	default:
		return fmt.Errorf("route object has unexpected type %T", obj)
	}
	if cfg == nil {
		return fmt.Errorf("route object is nil")
	}
	switch cfg.Kind {
	case routecore.RouteKindLLM, routecore.RouteKindMCP, routecore.RouteKindAgent:
		return nil
	default:
		return fmt.Errorf("legacy agent runtime route kind %q is not supported; migrate to kind=agent", cfg.Kind)
	}
}

func validateVirtualKeyObject(obj any) error {
	switch value := unwrapConfigObject(obj).(type) {
	case virtualkeypkg.VirtualKey:
		return value.ValidateConfiguration()
	case *virtualkeypkg.VirtualKey:
		if value == nil {
			return fmt.Errorf("virtual key object is nil")
		}
		return value.ValidateConfiguration()
	default:
		return fmt.Errorf("virtual key object has unexpected type %T", obj)
	}
}

func validateManagedModelObject(obj any) error {
	switch unwrapConfigObject(obj).(type) {
	case modelcatalog.ManagedModel, *modelcatalog.ManagedModel:
		return nil
	default:
		return fmt.Errorf("managed model object has unexpected type %T", obj)
	}
}

func validateMCPServiceObject(obj any) error {
	switch value := unwrapConfigObject(obj).(type) {
	case mcpservice.MCPServiceConfig:
		return value.Validate()
	case *mcpservice.MCPServiceConfig:
		if value == nil {
			return fmt.Errorf("mcp service object is nil")
		}
		return value.Validate()
	default:
		return fmt.Errorf("mcp service object has unexpected type %T", obj)
	}
}

func validateAgentObject(obj any) error {
	switch value := unwrapConfigObject(obj).(type) {
	case agentpkg.Agent:
		return value.Validate()
	case *agentpkg.Agent:
		if value == nil {
			return fmt.Errorf("agent object is nil")
		}
		return value.Validate()
	default:
		return fmt.Errorf("agent object has unexpected type %T", obj)
	}
}

// agentRuntimeTagValue uses the runtime type (acp/http) as the secondary tag.
// The value is nested under runtime, so it cannot use the reflect string-field
// helper and reads the struct directly.
func agentRuntimeTagValue(obj any) (string, bool, error) {
	switch value := unwrapConfigObject(obj).(type) {
	case agentpkg.Agent:
		return value.Runtime.Type, true, nil
	case *agentpkg.Agent:
		if value == nil {
			return "", false, fmt.Errorf("agent object is nil")
		}
		return value.Runtime.Type, true, nil
	default:
		return "", false, fmt.Errorf("agent object has unexpected type %T", obj)
	}
}

func primaryKeyFromStringFields(fieldNames ...string) func(obj any) ([]any, error) {
	return func(obj any) ([]any, error) {
		keys := make([]any, 0, len(fieldNames))
		for _, fieldName := range fieldNames {
			value, err := requiredStringField(obj, fieldName, strings.ToLower(fieldName))
			if err != nil {
				return nil, err
			}
			keys = append(keys, value)
		}
		return keys, nil
	}
}

func requiredTagFromStringField(fieldName string, logicalName string) func(obj any) (string, bool, error) {
	return func(obj any) (string, bool, error) {
		value, err := requiredStringField(obj, fieldName, logicalName)
		if err != nil {
			return "", false, err
		}
		return value, true, nil
	}
}

func optionalTagFromStringField(fieldName string) func(obj any) (string, bool, error) {
	return func(obj any) (string, bool, error) {
		value, ok, err := lookupStringField(obj, fieldName)
		if err != nil {
			return "", false, err
		}
		if !ok {
			return "", true, nil
		}
		return value, true, nil
	}
}

func routeTagValue(obj any) (string, bool, error) {
	if carrier, ok := obj.(configstore.TagCarrier); ok {
		return carrier.ConfigStoreTag(), true, nil
	}
	value, ok, err := lookupStringField(obj, "Tag")
	if err != nil {
		return "", false, err
	}
	if ok {
		return value, true, nil
	}
	return "", true, nil
}

func requiredStringField(obj any, fieldName string, logicalName string) (string, error) {
	value, ok, err := lookupStringField(obj, fieldName)
	if err != nil {
		return "", err
	}
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", logicalName)
	}
	return value, nil
}

func lookupStringField(obj any, fieldName string) (string, bool, error) {
	value := reflect.ValueOf(unwrapConfigObject(obj))
	if !value.IsValid() {
		return "", false, fmt.Errorf("object is nil")
	}
	for value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return "", false, fmt.Errorf("object is nil")
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return "", false, fmt.Errorf("object %T is not a struct", obj)
	}
	field := value.FieldByName(fieldName)
	if !field.IsValid() {
		return "", false, nil
	}
	if field.Kind() != reflect.String {
		return "", false, fmt.Errorf("field %s on %T is not a string", fieldName, obj)
	}
	return field.String(), true, nil
}

func unwrapConfigObject(obj any) any {
	if unwrapper, ok := obj.(configstore.ObjectUnwrapper); ok {
		return unwrapper.ConfigStoreObject()
	}
	return obj
}
