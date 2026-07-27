package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
)

// RuntimeConfig is the identity-free ACP execution configuration consumed by
// the Agent runtime adapter. Management ids, names, descriptions, timestamps,
// and disabled state remain outside the protocol runtime boundary.
type RuntimeConfig struct {
	AgentType       string
	CWD             string
	AllowedRoots    []string
	DefaultModel    string
	Env             map[string]string
	ConfigOverrides map[string]string
	IdleTTL         time.Duration
	MaxInstances    int
	PermissionMode  string
	Codex           *acpservice.CodexConfig
}

func (c RuntimeConfig) serviceConfig(ownerID string) (acpservice.ServiceConfig, error) {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return acpservice.ServiceConfig{}, fmt.Errorf("runtime owner id is required")
	}
	cfg := acpservice.ServiceConfig{
		ID: ownerID, Name: ownerID, AgentType: c.AgentType, CWD: c.CWD,
		AllowedRoots: append([]string(nil), c.AllowedRoots...), DefaultModel: c.DefaultModel,
		Env: cloneStrings(c.Env), ConfigOverrides: cloneStrings(c.ConfigOverrides),
		IdleTTL: c.IdleTTL, MaxInstances: c.MaxInstances, PermissionMode: c.PermissionMode,
	}
	if c.Codex != nil {
		codex := *c.Codex
		codex.AdapterArgs = append([]string(nil), c.Codex.AdapterArgs...)
		codex.AppServerArgs = append([]string(nil), c.Codex.AppServerArgs...)
		cfg.Codex = &codex
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return acpservice.ServiceConfig{}, err
	}
	return cfg, nil
}

// Fingerprint returns the canonical content fingerprint of this runtime
// config for one owner. Pooled instances record the fingerprint they were
// created under; a changed fingerprint prevents stale instances from
// accepting new turns and drives RetireOwner cleanup.
func (c RuntimeConfig) Fingerprint(ownerID string) (string, error) {
	cfg, err := c.serviceConfig(ownerID)
	if err != nil {
		return "", err
	}
	return configFingerprint(cfg), nil
}

// configFingerprint hashes the canonical execution config. Management-only
// fields and timestamps are deliberately absent from RuntimeConfig, so only a
// change that can affect process behavior or policy retires Agent-owned pools.
func configFingerprint(cfg acpservice.ServiceConfig) string {
	data, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func cloneStrings(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
