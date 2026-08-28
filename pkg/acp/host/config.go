package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/agent-guide/agent-gateway/pkg/acp/hostconfig"
)

func ownerRuntimeConfig(c hostconfig.Config, ownerID string) (hostconfig.Config, error) {
	cfg := c
	cfg.AllowedRoots = append([]string(nil), c.AllowedRoots...)
	cfg.Env = cloneStrings(c.Env)
	cfg.ConfigOverrides = cloneStrings(c.ConfigOverrides)
	if c.Codex != nil {
		codex := *c.Codex
		codex.AdapterArgs = append([]string(nil), c.Codex.AdapterArgs...)
		codex.AppServerArgs = append([]string(nil), c.Codex.AppServerArgs...)
		cfg.Codex = &codex
	}
	cfg.OwnerID = ownerID
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return hostconfig.Config{}, err
	}
	return cfg, nil
}

// configFingerprint hashes the canonical execution config. Management-only
// fields and timestamps are deliberately absent from hostconfig.Config, so only a
// change that can affect process behavior or policy retires Agent-owned pools.
func configFingerprint(cfg hostconfig.Config) string {
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
