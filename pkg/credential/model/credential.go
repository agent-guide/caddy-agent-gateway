package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/agent-guide/agent-gateway/internal/utils"
)

// Credential holds the persisted definition for a single upstream credential.
type Credential struct {
	// ID uniquely identifies the credential.
	ID string `json:"id"`
	// ProviderType is the upstream provider type/key (e.g. "openai", "anthropic").
	ProviderType string `json:"provider_type"`
	// ProviderID identifies the concrete provider instance this credential belongs to.
	ProviderID string `json:"provider_id"`
	// Type identifies the credential kind (e.g. api_key, oauth_token).
	Type string `json:"type"`
	// Label is a human-readable label for logging and display.
	Label string `json:"label,omitempty"`
	// Scope controls scheduler matching for this credential.
	Scope string `json:"scope,omitempty"`
	// Attributes stores provider-specific configuration (e.g. api_key, base_url, priority).
	Attributes map[string]string `json:"attributes,omitempty"`
	// Metadata stores runtime mutable provider state (e.g. tokens, cookies).
	Metadata map[string]any `json:"metadata,omitempty"`
	// Disabled marks the credential as intentionally excluded from scheduling.
	Disabled bool `json:"disabled,omitempty"`
	// CreatedAt is the creation timestamp.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is the last modification timestamp.
	UpdatedAt time.Time `json:"updated_at"`
}

// ManagedCredential wraps a persisted credential with in-memory runtime state.
type ManagedCredential struct {
	Credential
	// Unavailable flags transient provider unavailability (e.g. quota exceeded).
	Unavailable bool `json:"unavailable,omitempty"`
	// CredentialWideUnavailable flags a transient block across every model.
	CredentialWideUnavailable bool `json:"credential_wide_unavailable,omitempty"`
	// CredentialWideNextRetryAfter bounds a credential-wide transient block.
	CredentialWideNextRetryAfter time.Time `json:"credential_wide_next_retry_after,omitempty"`
	// NextRetryAfter is the earliest time a retry should be attempted.
	NextRetryAfter time.Time `json:"next_retry_after,omitempty"`
	// Quota captures recent quota information for load-balancing decisions.
	Quota QuotaState `json:"quota"`
	// LastError records the last failure encountered.
	LastError *Error `json:"last_error,omitempty"`
	// ModelStates tracks per-model runtime availability data.
	ModelStates map[string]*ModelState `json:"model_states,omitempty"`
	// AuthInvalid flags credentials rejected by upstream authentication (e.g. 401/403).
	AuthInvalid bool `json:"auth_invalid,omitempty"`
	// StateUpdatedAt is the last runtime-state update timestamp.
	StateUpdatedAt time.Time `json:"state_updated_at,omitempty"`
}

// Normalize canonicalizes stable credential identity fields in-place.
func (c *Credential) Normalize() *Credential {
	if c == nil {
		return nil
	}
	c.ID = strings.TrimSpace(c.ID)
	c.ProviderType = strings.ToLower(strings.TrimSpace(c.ProviderType))
	c.ProviderID = strings.ToLower(strings.TrimSpace(c.ProviderID))
	c.Type = strings.ToLower(strings.TrimSpace(c.Type))
	c.Label = strings.TrimSpace(c.Label)
	c.Scope = NormalizeCredentialScope(c.Scope)
	return c
}

// Validate checks the required stable identity fields for persisted credentials.
func (c *Credential) Validate() error {
	if c == nil {
		return fmt.Errorf("credential is nil")
	}
	if c.ProviderType == "" {
		return fmt.Errorf("credential provider_type is required")
	}
	if c.ProviderID == "" {
		return fmt.Errorf("credential provider_id is required")
	}
	if c.Type == "" {
		return fmt.Errorf("credential type is required")
	}
	if c.ScopeValue() == "" {
		return fmt.Errorf("credential scope is required")
	}
	return nil
}

func NormalizeCredentialScope(scope string) string {
	scope = strings.ToLower(strings.TrimSpace(scope))
	return scope
}

func (c *Credential) ScopeValue() string {
	if c == nil {
		return ""
	}
	return NormalizeCredentialScope(c.Scope)
}

// QuotaState captures quota limiter tracking data for a credential.
type QuotaState struct {
	Exceeded      bool      `json:"exceeded"`
	Reason        string    `json:"reason,omitempty"`
	NextRecoverAt time.Time `json:"next_recover_at,omitempty"`
	BackoffLevel  int       `json:"backoff_level,omitempty"`
}

// ModelState captures the execution state for a specific model under a credential.
type ModelState struct {
	// Unavailable flags transient unavailability for this model.
	Unavailable bool `json:"unavailable,omitempty"`
	// NextRetryAfter is the earliest time this model may be retried.
	NextRetryAfter time.Time `json:"next_retry_after,omitempty"`
	// LastError records the latest error observed for this model.
	LastError *Error `json:"last_error,omitempty"`
	// Quota retains quota information if this model hit rate limits.
	Quota QuotaState `json:"quota"`
	// AuthInvalid flags model-specific authentication failure state.
	AuthInvalid bool `json:"auth_invalid,omitempty"`
	// UpdatedAt tracks the last update timestamp.
	UpdatedAt time.Time `json:"updated_at"`
}

// Error describes a credential-related failure in a provider-agnostic format.
type Error struct {
	Code       string `json:"code,omitempty"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// StatusCode implements optional status accessor for retry decision making.
func (e *Error) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.HTTPStatus
}

// APIKey returns the api_key attribute value, or empty string if not set.
func (c *Credential) APIKey() string {
	if c == nil || c.Attributes == nil {
		return ""
	}
	return c.Attributes["api_key"]
}

// BaseURL returns the base_url attribute value, or empty string if not set.
func (c *Credential) BaseURL() string {
	if c == nil || c.Attributes == nil {
		return ""
	}
	return c.Attributes["base_url"]
}

// Priority returns the scheduling priority for this credential (higher = preferred).
func (c *Credential) Priority() int {
	if c == nil || c.Attributes == nil {
		return 0
	}
	raw := strings.TrimSpace(c.Attributes["priority"])
	if raw == "" {
		return 0
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return parsed
}

// ExpirationTime attempts to extract the credential expiration timestamp from metadata.
func (c *Credential) ExpirationTime() (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	return utils.ExpirationFromMap(c.Metadata)
}

// RefreshExpiryDelta returns the per-credential refresh lead time.
func (c *Credential) RefreshExpiryDelta() (time.Duration, bool) {
	if c == nil || c.Metadata == nil {
		return 0, false
	}
	val, ok := c.Metadata["refresh_expiry_delta"]
	if !ok {
		return 0, false
	}
	parsed, ok := utils.ParseDurationAny(val)
	if !ok || parsed < 0 {
		return 0, false
	}
	return parsed, true
}

// DisableCoolingOverride returns the per-credential disable_cooling override when present.
func (c *Credential) DisableCoolingOverride() (bool, bool) {
	if c == nil || c.Metadata == nil {
		return false, false
	}
	for _, key := range []string{"disable_cooling", "disable-cooling"} {
		if val, ok := c.Metadata[key]; ok {
			if parsed, okParse := utils.ParseBoolAny(val); okParse {
				return parsed, true
			}
		}
	}
	return false, false
}

// RequestRetryOverride returns the per-credential request_retry override when present.
func (c *Credential) RequestRetryOverride() (int, bool) {
	if c == nil || c.Metadata == nil {
		return 0, false
	}
	for _, key := range []string{"request_retry", "request-retry"} {
		if val, ok := c.Metadata[key]; ok {
			if parsed, okParse := utils.ParseIntAny(val); okParse {
				if parsed < 0 {
					parsed = 0
				}
				return parsed, true
			}
		}
	}
	return 0, false
}

// Clone shallow copies the Credential, duplicating maps to avoid mutation.
func (c *Credential) Clone() *Credential {
	if c == nil {
		return nil
	}
	cp := *c
	if len(c.Attributes) > 0 {
		cp.Attributes = make(map[string]string, len(c.Attributes))
		for k, v := range c.Attributes {
			cp.Attributes[k] = v
		}
	}
	if len(c.Metadata) > 0 {
		cp.Metadata = make(map[string]any, len(c.Metadata))
		for k, v := range c.Metadata {
			cp.Metadata[k] = v
		}
	}
	return (&cp).Normalize()
}

// Clone duplicates a ManagedCredential including runtime state.
func (c *ManagedCredential) Clone() *ManagedCredential {
	if c == nil {
		return nil
	}
	cp := *c
	cp.Credential = *c.Credential.Clone()
	if len(c.ModelStates) > 0 {
		cp.ModelStates = make(map[string]*ModelState, len(c.ModelStates))
		for k, v := range c.ModelStates {
			cp.ModelStates[k] = v.Clone()
		}
	}
	if c.LastError != nil {
		cp.LastError = &Error{
			Code:       c.LastError.Code,
			Message:    c.LastError.Message,
			Retryable:  c.LastError.Retryable,
			HTTPStatus: c.LastError.HTTPStatus,
		}
	}
	return &cp
}

// Clone duplicates a ModelState.
func (m *ModelState) Clone() *ModelState {
	if m == nil {
		return nil
	}
	cp := *m
	if m.LastError != nil {
		cp.LastError = &Error{
			Code:       m.LastError.Code,
			Message:    m.LastError.Message,
			Retryable:  m.LastError.Retryable,
			HTTPStatus: m.LastError.HTTPStatus,
		}
	}
	return &cp
}
