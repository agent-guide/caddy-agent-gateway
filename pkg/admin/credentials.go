package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/pkg/credential"
	"github.com/agent-guide/agent-gateway/pkg/gateway"
	"gorm.io/gorm"
)

type CredentialView struct {
	credential.ManagedCredential
	ReadOnly bool `json:"read_only"`
}

func (h *Handler) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	if h.credentialManager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "credential manager not configured")
		return
	}

	filter := credential.Filter{
		ProviderType: r.URL.Query().Get("provider_type"),
		ProviderID:   r.URL.Query().Get("provider_id"),
		Type:         r.URL.Query().Get("type"),
	}
	items := h.credentialManager.ListCredentials(filter)
	views := make([]CredentialView, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		views = append(views, credentialView(item, false))
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"items": views})
}

func (h *Handler) handleGetCredential(w http.ResponseWriter, r *http.Request) {
	if h.credentialManager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "credential manager not configured")
		return
	}

	id := strings.TrimSpace(r.PathValue("credential_id"))
	if id == "" {
		_ = httpjson.Error(w, http.StatusBadRequest, "credential_id is required")
		return
	}

	item := h.credentialManager.GetCredential(id)
	if item != nil {
		_ = httpjson.Write(w, http.StatusOK, credentialView(item, false))
		return
	}
	_ = httpjson.Error(w, http.StatusNotFound, "credential not found")
}

// credentialCreateRequest is the request body for POST /admin/credentials.
type credentialCreateRequest struct {
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

// credentialUpdateRequest is the request body for PUT /admin/credentials/{credential_id}.
type credentialUpdateRequest struct {
	Type         string            `json:"type,omitempty"`
	ProviderType string            `json:"provider_type,omitempty"`
	ProviderID   string            `json:"provider_id,omitempty"`
	Scope        string            `json:"scope,omitempty"`
	Label        string            `json:"label,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
	Metadata     map[string]any    `json:"metadata,omitempty"`
	Disabled     bool              `json:"disabled,omitempty"`
}

func (h *Handler) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	if h.credentialManager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "credential manager not configured")
		return
	}

	var req credentialCreateRequest
	if err := httpjson.Decode(r, &req); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	cred, err := h.buildCredentialForCreate(r.Context(), req)
	if err != nil {
		h.writeCredentialBuildError(w, err)
		return
	}
	if err := h.credentialManager.RegisterCredential(r.Context(), cred); err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusCreated, cred)
}

func (h *Handler) handleUpdateCredential(w http.ResponseWriter, r *http.Request) {
	if h.credentialManager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "credential manager not configured")
		return
	}

	id := strings.TrimSpace(r.PathValue("credential_id"))
	if id == "" {
		_ = httpjson.Error(w, http.StatusBadRequest, "credential_id is required")
		return
	}
	existing := h.credentialManager.GetCredential(id)
	if existing == nil {
		_ = httpjson.Error(w, http.StatusNotFound, "credential not found")
		return
	}

	var req credentialUpdateRequest
	if err := httpjson.Decode(r, &req); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}

	updated, err := h.buildCredentialForUpdate(existing, req)
	if err != nil {
		h.writeCredentialBuildError(w, err)
		return
	}

	if err := h.credentialManager.UpdateCredential(r.Context(), updated); err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, h.credentialManager.GetCredential(id))
}

func (h *Handler) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	if h.credentialManager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "credential manager not configured")
		return
	}

	id := strings.TrimSpace(r.PathValue("credential_id"))
	if id == "" {
		_ = httpjson.Error(w, http.StatusBadRequest, "credential_id is required")
		return
	}
	if err := h.credentialManager.DeregisterCredential(r.Context(), id); err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]string{"status": "deleted", "credential_id": id})
}

func credentialView(cred *credential.ManagedCredential, readOnly bool) CredentialView {
	if cred == nil {
		return CredentialView{ReadOnly: readOnly}
	}
	return CredentialView{
		ManagedCredential: *cred.Clone(),
		ReadOnly:          readOnly,
	}
}

func (h *Handler) buildCredentialForCreate(ctx context.Context, req credentialCreateRequest) (*credential.Credential, error) {
	credentialType := strings.ToLower(strings.TrimSpace(req.Type))
	if credentialType == "" {
		return nil, fmt.Errorf("type is required")
	}

	switch credentialType {
	case credential.TypeAPIKey:
		manager := h.providerManagerForRoutes()
		if manager == nil {
			return nil, fmt.Errorf("provider manager is not configured")
		}
		providerID := strings.TrimSpace(req.ProviderID)
		if providerID == "" {
			return nil, fmt.Errorf("provider_id is required")
		}
		cfg, err := manager.GetConfig(ctx, providerID)
		if err != nil {
			if errors.Is(err, gateway.ErrProviderNotConfigured) || errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, err
			}
			return nil, fmt.Errorf("get provider config: %w", err)
		}
		return &credential.Credential{
			ID:           strings.TrimSpace(req.ID),
			ProviderType: cfg.ProviderType,
			ProviderID:   cfg.Id,
			Scope:        strings.TrimSpace(req.Scope),
			Type:         credential.TypeAPIKey,
			Label:        req.Label,
			Attributes:   req.Attributes,
			Disabled:     req.Disabled,
		}, nil
	case credential.TypeOAuthToken:
		cred := &credential.Credential{
			ID:           strings.TrimSpace(req.ID),
			ProviderType: strings.TrimSpace(req.ProviderType),
			ProviderID:   strings.TrimSpace(req.ProviderID),
			Scope:        strings.TrimSpace(req.Scope),
			Type:         credential.TypeOAuthToken,
			Label:        req.Label,
			Attributes:   req.Attributes,
			Metadata:     req.Metadata,
			Disabled:     req.Disabled,
		}
		if cred.ProviderType == "" {
			return nil, fmt.Errorf("provider_type is required")
		}
		if cred.ProviderID == "" {
			cred.ProviderID = cred.ProviderType
		}
		return cred, nil
	default:
		return nil, fmt.Errorf("unsupported credential type %q", credentialType)
	}
}

func (h *Handler) buildCredentialForUpdate(existing *credential.ManagedCredential, req credentialUpdateRequest) (*credential.Credential, error) {
	if existing == nil {
		return nil, fmt.Errorf("credential not found")
	}
	switch existing.Type {
	case credential.TypeAPIKey:
		updated := existing.Credential.Clone()
		updated.Label = req.Label
		updated.Disabled = req.Disabled
		if scope := strings.TrimSpace(req.Scope); scope != "" {
			updated.Scope = scope
		}
		if req.Attributes != nil {
			updated.Attributes = req.Attributes
		}
		return updated, nil
	case credential.TypeOAuthToken:
		updated := existing.Credential.Clone()
		updated.Label = req.Label
		updated.Disabled = req.Disabled
		if scope := strings.TrimSpace(req.Scope); scope != "" {
			updated.Scope = scope
		}
		if req.Attributes != nil {
			updated.Attributes = req.Attributes
		}
		if req.Metadata != nil {
			updated.Metadata = req.Metadata
		}
		if providerType := strings.TrimSpace(req.ProviderType); providerType != "" {
			updated.ProviderType = providerType
		}
		if providerID := strings.TrimSpace(req.ProviderID); providerID != "" {
			updated.ProviderID = providerID
		} else if strings.TrimSpace(updated.ProviderID) == "" {
			updated.ProviderID = updated.ProviderType
		}
		return updated, nil
	default:
		return nil, fmt.Errorf("credential type %q cannot be updated via the admin API", existing.Type)
	}
}

func (h *Handler) writeCredentialBuildError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, gateway.ErrProviderNotConfigured), errors.Is(err, gorm.ErrRecordNotFound):
		status = http.StatusNotFound
		msg = "provider not found"
	case strings.Contains(msg, "provider manager is not configured"):
		status = http.StatusServiceUnavailable
	case strings.Contains(msg, "cannot be updated via the admin API"):
		status = http.StatusForbidden
	}
	_ = httpjson.Error(w, status, msg)
}

func matchesCredentialFilter(cred *credential.Credential, filter credential.Filter) bool {
	if cred == nil {
		return false
	}
	if providerType := strings.ToLower(strings.TrimSpace(filter.ProviderType)); providerType != "" && strings.ToLower(cred.ProviderType) != providerType {
		return false
	}
	if providerID := strings.ToLower(strings.TrimSpace(filter.ProviderID)); providerID != "" && strings.ToLower(cred.ProviderID) != providerID {
		return false
	}
	if credentialType := strings.ToLower(strings.TrimSpace(filter.Type)); credentialType != "" && strings.ToLower(cred.Type) != credentialType {
		return false
	}
	return true
}
