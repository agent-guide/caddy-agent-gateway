package runtimeconfig

import (
	"path/filepath"
	"testing"

	baseacp "github.com/agent-guide/agent-gateway/pkg/acp"
)

func TestConfigValidateRejectsUnsupportedAgentType(t *testing.T) {
	root := t.TempDir()
	config := Config{
		AgentType:    "qwen",
		CWD:          root,
		AllowedRoots: []string{root},
	}
	config.Normalize()
	if err := config.Validate(); err == nil {
		t.Fatal("Validate returned nil, want unsupported agent_type error")
	}
}

func TestConfigNormalizeTrimsEnvKeys(t *testing.T) {
	root := t.TempDir()
	config := Config{
		AgentType:    baseacp.AgentTypeCodex,
		CWD:          root,
		AllowedRoots: []string{root},
		Env:          map[string]string{"  CODEX_HOME  ": "/home/.codex"},
	}
	config.Normalize()
	if err := config.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if got := config.Env["CODEX_HOME"]; got != "/home/.codex" {
		t.Fatalf("normalized CODEX_HOME = %q, want /home/.codex", got)
	}
}

func TestConfigValidateRejectsEnvKeyContainingEquals(t *testing.T) {
	root := t.TempDir()
	config := Config{
		AgentType:    baseacp.AgentTypeCodex,
		CWD:          root,
		AllowedRoots: []string{root},
		Env:          map[string]string{"BAD=KEY": "value"},
	}
	config.Normalize()
	if err := config.Validate(); err == nil {
		t.Fatal("Validate returned nil, want invalid env key error")
	}
}

func TestValidateCWDAllowedRejectsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "project")
	if err := ValidateCWDAllowed(outside, []string{root}); err == nil {
		t.Fatal("ValidateCWDAllowed returned nil, want outside allowed_roots error")
	}
}
