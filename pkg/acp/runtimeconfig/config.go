// Package runtimeconfig defines the ACP process configuration shared by Agent
// definitions, runtime adapters, and native agent implementations. It contains
// no persisted service object or management lifecycle.
package runtimeconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	baseacp "github.com/agent-guide/agent-gateway/pkg/acp"
)

const (
	CodexModeAdapter   = "adapter"
	CodexModeAppServer = "app_server"
)

// Config is the process-facing ACP runtime configuration. OwnerID is assigned
// by the runtime adapter from the owning Agent and is never serialized as an
// independent management identity.
type Config struct {
	OwnerID         string            `json:"-"`
	AgentType       string            `json:"agent_type"`
	CWD             string            `json:"cwd"`
	AllowedRoots    []string          `json:"allowed_roots,omitempty"`
	DefaultModel    string            `json:"default_model,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	ConfigOverrides map[string]string `json:"config_overrides,omitempty"`
	IdleTTL         time.Duration     `json:"idle_ttl,omitempty"`
	MaxInstances    int               `json:"max_instances,omitempty"`
	PermissionMode  string            `json:"permission_mode,omitempty"`
	Codex           *CodexConfig      `json:"codex,omitempty"`
}

type CodexConfig struct {
	Mode             string   `json:"mode,omitempty"`
	AdapterCommand   string   `json:"adapter_command,omitempty"`
	AdapterArgs      []string `json:"adapter_args,omitempty"`
	AppServerCommand string   `json:"app_server_command,omitempty"`
	AppServerArgs    []string `json:"app_server_args,omitempty"`
	DefaultProfile   string   `json:"default_profile,omitempty"`
	InitialAuthMode  string   `json:"initial_auth_mode,omitempty"`
	TraceJSON        bool     `json:"trace_json,omitempty"`
	RetryTurnOnCrash bool     `json:"retry_turn_on_crash,omitempty"`
}

// Fingerprint returns a canonical process-config fingerprint. The Agent owner
// is validated but excluded from the serialized configuration.
func (c Config) Fingerprint(ownerID string) (string, error) {
	if strings.TrimSpace(ownerID) == "" {
		return "", fmt.Errorf("runtime owner id is required")
	}
	c.OwnerID = strings.TrimSpace(ownerID)
	c.Normalize()
	if err := c.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (c *Config) Normalize() {
	if c == nil {
		return
	}
	c.OwnerID = strings.TrimSpace(c.OwnerID)
	c.AgentType = strings.TrimSpace(c.AgentType)
	c.CWD = strings.TrimSpace(c.CWD)
	c.DefaultModel = strings.TrimSpace(c.DefaultModel)
	c.PermissionMode = strings.TrimSpace(c.PermissionMode)
	if c.PermissionMode == "" {
		c.PermissionMode = baseacp.PermissionModeDeny
	}
	for i := range c.AllowedRoots {
		c.AllowedRoots[i] = strings.TrimSpace(c.AllowedRoots[i])
	}
	if len(c.AllowedRoots) == 0 && c.CWD != "" {
		c.AllowedRoots = []string{c.CWD}
	}
	if len(c.Env) > 0 {
		normalized := make(map[string]string, len(c.Env))
		for key, value := range c.Env {
			normalized[strings.TrimSpace(key)] = value
		}
		c.Env = normalized
	}
	if c.Codex != nil {
		c.Codex.Mode = strings.TrimSpace(c.Codex.Mode)
		if c.Codex.Mode == "" {
			c.Codex.Mode = CodexModeAdapter
		}
		c.Codex.AdapterCommand = strings.TrimSpace(c.Codex.AdapterCommand)
		if c.Codex.AdapterCommand == "" && c.Codex.Mode == CodexModeAdapter {
			c.Codex.AdapterCommand = "codex-acp"
		}
		c.Codex.AppServerCommand = strings.TrimSpace(c.Codex.AppServerCommand)
		c.Codex.DefaultProfile = strings.TrimSpace(c.Codex.DefaultProfile)
		c.Codex.InitialAuthMode = strings.TrimSpace(c.Codex.InitialAuthMode)
	}
}

func (c Config) Validate() error {
	switch c.AgentType {
	case baseacp.AgentTypeCodex, baseacp.AgentTypeOpencode:
	default:
		return fmt.Errorf("unsupported acp agent_type %q", c.AgentType)
	}
	if c.CWD == "" {
		return fmt.Errorf("cwd is required")
	}
	if !filepath.IsAbs(c.CWD) {
		return fmt.Errorf("cwd must be absolute")
	}
	if len(c.AllowedRoots) == 0 {
		return fmt.Errorf("allowed_roots is required")
	}
	if err := ValidateCWDAllowed(c.CWD, c.AllowedRoots); err != nil {
		return err
	}
	switch c.PermissionMode {
	case "", baseacp.PermissionModeDeny, baseacp.PermissionModeAutoApprove, baseacp.PermissionModeInteractive:
	default:
		return fmt.Errorf("unsupported permission_mode %q", c.PermissionMode)
	}
	if c.MaxInstances < 0 {
		return fmt.Errorf("max_instances must be non-negative")
	}
	for key, value := range c.Env {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("env key must not be empty")
		}
		if strings.ContainsAny(key, "=\x00") {
			return fmt.Errorf("env key %q must not contain '=' or NUL", key)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("env value for key %q must not contain NUL", key)
		}
	}
	if c.AgentType == baseacp.AgentTypeCodex && c.Codex != nil {
		switch c.Codex.Mode {
		case "", CodexModeAdapter:
			if c.Codex.AdapterCommand != "" && filepath.Base(c.Codex.AdapterCommand) != "codex-acp" {
				return fmt.Errorf("codex adapter_command must be codex-acp")
			}
		case CodexModeAppServer:
			return fmt.Errorf("codex mode %q is not implemented", c.Codex.Mode)
		default:
			return fmt.Errorf("unsupported codex mode %q", c.Codex.Mode)
		}
	}
	return nil
}

func ValidateCWDAllowed(cwd string, roots []string) error {
	cwd = filepath.Clean(strings.TrimSpace(cwd))
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("cwd must be absolute")
	}
	for _, root := range roots {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "" {
			continue
		}
		if !filepath.IsAbs(root) {
			return fmt.Errorf("allowed root %q must be absolute", root)
		}
		if cwd == root {
			return nil
		}
		rel, err := filepath.Rel(root, cwd)
		if err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
			return nil
		}
	}
	return fmt.Errorf("cwd %q is outside allowed_roots", cwd)
}
