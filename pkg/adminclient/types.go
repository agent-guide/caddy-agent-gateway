package adminclient

import (
	"time"

	adminapi "github.com/agent-guide/agent-gateway/pkg/admin"
	"github.com/agent-guide/agent-gateway/pkg/credential"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/modelcatalog"
	virtualkeypkg "github.com/agent-guide/agent-gateway/pkg/gateway/virtualkey"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

type Provider = adminapi.ProviderView
type ProviderType = adminapi.ProviderTypeView
type LLMAPIHandlerType = adminapi.LLMApiHandlerTypeView
type LLMRoute = adminapi.LLMRouteView
type VirtualKey = adminapi.VirtualKeyView
type Credential = adminapi.CredentialView
type ManagedModel = adminapi.ManagedConcreteModelView
type DiscoveredModel = modelcatalog.ProviderModelSnapshot

type ProviderConfig = provider.ProviderConfig
type LLMRouteConfig = llmroutepkg.LLMRouteConfig
type ManagedCredential = credential.ManagedCredential

type VirtualKeyConfig struct {
	ID              string                              `json:"id,omitempty"`
	Tag             string                              `json:"tag,omitempty"`
	Description     string                              `json:"description,omitempty"`
	Disabled        bool                                `json:"disabled,omitempty"`
	AllowedRouteIDs []string                            `json:"allowed_route_ids,omitempty"`
	RateLimits      *virtualkeypkg.VirtualKeyRateLimits `json:"rate_limits,omitempty"`
	StatusMessage   string                              `json:"status_message,omitempty"`
	ExpiresAt       time.Time                           `json:"expires_at,omitempty"`
}

type ProviderListOptions struct {
	ProviderType string
}

type LLMRouteListOptions struct {
	Tag       string
	TagPrefix string
}

type VirtualKeyListOptions struct {
	Tag string
}

type CredentialListOptions struct {
	ProviderType string
	ProviderID   string
	Type         string
}

type CreateCredentialRequest struct {
	ID           string            `json:"id,omitempty"`
	Type         string            `json:"type"`
	ProviderType string            `json:"provider_type,omitempty"`
	ProviderID   string            `json:"provider_id"`
	Scope        string            `json:"scope,omitempty"`
	Label        string            `json:"label,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
	Metadata     map[string]any    `json:"metadata,omitempty"`
	Disabled     bool              `json:"disabled,omitempty"`
}

type UpdateCredentialRequest struct {
	Type         string            `json:"type,omitempty"`
	ProviderType string            `json:"provider_type,omitempty"`
	ProviderID   string            `json:"provider_id,omitempty"`
	Scope        string            `json:"scope,omitempty"`
	Label        string            `json:"label,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
	Metadata     map[string]any    `json:"metadata,omitempty"`
	Disabled     bool              `json:"disabled,omitempty"`
}

type itemsResponse[T any] struct {
	Items []T `json:"items"`
}
