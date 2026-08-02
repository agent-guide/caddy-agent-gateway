package modelcatalog

import (
	"encoding/json"
	"time"

	credmodel "github.com/agent-guide/agent-gateway/pkg/credential/model"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

type SnapshotStatus string

const (
	SnapshotStatusOK    SnapshotStatus = "ok"
	SnapshotStatusError SnapshotStatus = "error"
)

type ProviderModelSnapshot struct {
	ProviderID    string                     `json:"provider_id"`
	ProviderType  string                     `json:"provider_type"`
	UpstreamModel string                     `json:"upstream_model"`
	DisplayName   string                     `json:"display_name,omitempty"`
	Description   string                     `json:"description,omitempty"`
	Capabilities  provider.ModelCapabilities `json:"capabilities,omitempty"`
	Status        SnapshotStatus             `json:"status"`
	FetchedAt     time.Time                  `json:"fetched_at"`
	LastError     string                     `json:"last_error,omitempty"`
}

type ManagedModel struct {
	ProviderID          string                      `json:"provider_id"`
	UpstreamModel       string                      `json:"upstream_model"`
	CredentialScope     string                      `json:"credential_scope,omitempty"`
	Enabled             bool                        `json:"enabled"`
	CapabilityOverrides *provider.ModelCapabilities `json:"capability_overrides,omitempty"`
}

type ResolvedManagedModel struct {
	ManagedModel
	Snapshot     *ProviderModelSnapshot     `json:"snapshot,omitempty"`
	Capabilities provider.ModelCapabilities `json:"capabilities,omitempty"`
}

func (m *ManagedModel) Normalize() {
	if m == nil {
		return
	}
	if scope := credmodel.NormalizeCredentialScope(m.CredentialScope); scope != "" {
		m.CredentialScope = scope
	} else {
		m.CredentialScope = ""
	}
	if !m.Enabled && m.CapabilityOverrides == nil {
		m.Enabled = true
	}
}

func (m *ManagedModel) ModelStorageKey() (string, string) {
	if m == nil {
		return "", ""
	}
	return m.ProviderID, m.UpstreamModel
}

func DecodeStoredManagedModel(data []byte) (any, error) {
	var model ManagedModel
	if err := json.Unmarshal(data, &model); err != nil {
		return nil, err
	}
	model.Normalize()
	return &model, nil
}
