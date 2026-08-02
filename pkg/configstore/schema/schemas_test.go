package schema

import (
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/credential"
	"github.com/agent-guide/agent-gateway/pkg/credential/model"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	modelcatalog "github.com/agent-guide/agent-gateway/pkg/gateway/modelcatalog"
	virtualkeypkg "github.com/agent-guide/agent-gateway/pkg/gateway/virtualkey"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

func TestProviderConfigSchemaMetadata(t *testing.T) {
	obj := &provider.ProviderConfig{Id: "openai-main", ProviderType: "openai"}

	keys, err := ProviderConfigSchema.Metadata.PrimaryKey(obj)
	if err != nil {
		t.Fatalf("primary key: %v", err)
	}
	if len(keys) != 1 || keys[0] != "openai-main" {
		t.Fatalf("keys = %#v", keys)
	}

	tag, ok, err := ProviderConfigSchema.Metadata.Tag(obj)
	if err != nil {
		t.Fatalf("tag: %v", err)
	}
	if !ok || tag != "openai" {
		t.Fatalf("tag = %q, ok = %v", tag, ok)
	}
}

func TestVirtualKeySchemaIndexes(t *testing.T) {
	obj := &virtualkeypkg.VirtualKey{ID: "vk-1", Key: "secret", Tag: "team-a"}

	indexes, err := VirtualKeySchema.Metadata.Indexes(obj)
	if err != nil {
		t.Fatalf("indexes: %v", err)
	}
	if indexes["key"] != "secret" {
		t.Fatalf("key index = %#v", indexes["key"])
	}
}

func TestManagedModelSchemaPrimaryKey(t *testing.T) {
	obj := &modelcatalog.ManagedModel{ProviderID: "openai-main", UpstreamModel: "gpt-4.1"}

	keys, err := ManagedModelSchema.Metadata.PrimaryKey(obj)
	if err != nil {
		t.Fatalf("primary key: %v", err)
	}
	if len(keys) != 2 || keys[0] != "openai-main" || keys[1] != "gpt-4.1" {
		t.Fatalf("keys = %#v", keys)
	}
}

func TestRouteSchemaTagDefaultsEmpty(t *testing.T) {
	obj := &llmroutepkg.AgentRouteConfig{ID: "route-1"}

	tag, ok, err := RouteSchema.Metadata.Tag(obj)
	if err != nil {
		t.Fatalf("tag: %v", err)
	}
	if !ok || tag != "" {
		t.Fatalf("tag = %q, ok = %v", tag, ok)
	}
}

func TestCredentialSchemaCodecRejectsWrongType(t *testing.T) {
	if _, err := CredentialSchema.Codec.Encode(&provider.ProviderConfig{}); err == nil {
		t.Fatal("expected type validation error")
	}
}

func TestCredentialSchemaCodecRoundTrip(t *testing.T) {
	obj := &model.Credential{ID: "cred-1", ProviderType: "openai", ProviderID: "openai-main", Scope: "id:openai-main", Type: credential.TypeAPIKey}

	data, err := CredentialSchema.Codec.Encode(obj)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, err := CredentialSchema.Codec.Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	cred, ok := decoded.(*model.Credential)
	if !ok {
		t.Fatalf("decoded type = %T", decoded)
	}
	if cred.ID != "cred-1" || cred.ProviderType != "openai" {
		t.Fatalf("decoded = %#v", cred)
	}
}

func TestCredentialSchemaCodecRejectsEmptyProviderID(t *testing.T) {
	obj := &model.Credential{ID: "cred-1", ProviderType: "openai"}

	if _, err := CredentialSchema.Codec.Encode(obj); err == nil {
		t.Fatal("expected provider_id validation error")
	}
}

func TestCredentialSchemaCodecRejectsEmptyScope(t *testing.T) {
	obj := &model.Credential{ID: "cred-1", ProviderType: "openai", ProviderID: "openai-main", Type: credential.TypeAPIKey}

	if _, err := CredentialSchema.Codec.Encode(obj); err == nil {
		t.Fatal("expected scope validation error")
	}
}
