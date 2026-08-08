package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootCommandExposesGatewayResourcesDirectly(t *testing.T) {
	direct := map[string]bool{
		"agent":       false,
		"apply":       false,
		"credential":  false,
		"llm-route":   false,
		"mcp-service": false,
		"provider":    false,
		"virtualkey":  false,
	}
	removed := map[string]struct{}{
		"chat":      {},
		"gateway":   {},
		"responses": {},
	}

	for _, cmd := range rootCmd.Commands() {
		if _, ok := direct[cmd.Name()]; ok {
			direct[cmd.Name()] = true
		}
		if _, forbidden := removed[cmd.Name()]; forbidden {
			t.Errorf("removed root command %q is still registered", cmd.Name())
		}
	}
	for name, found := range direct {
		if !found {
			t.Errorf("root command %q is not registered", name)
		}
	}
}

func TestCaddyRejectsFormerAdminAddrAlias(t *testing.T) {
	_, _, err := executeAGWCTL(t, "caddy", "--admin-addr", "http://remote:2019", "server", "list")
	if err == nil {
		t.Fatal("caddy --admin-addr unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "use --addr for the Caddy admin API") {
		t.Fatalf("error = %q, want Caddy --addr guidance", err)
	}
}

func TestLoadRuntimeEnvPriority(t *testing.T) {
	t.Setenv("AGW_ADMIN_ADDR", "from-shell")
	unsetEnv(t, "AGW_ADMIN_BASIC_AUTH")

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "AGW_ADMIN_ADDR=from-dotenv\nAGW_ADMIN_BASIC_AUTH=from-dotenv-auth\n")
	writeFile(t, filepath.Join(dir, ".env.local"), "AGW_ADMIN_ADDR=from-dotenv-local\n")

	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("os.Chdir() error = %v", err)
	}
	defer func() {
		if err := os.Chdir(prev); err != nil {
			t.Fatalf("restore cwd error = %v", err)
		}
	}()

	if err := loadRuntimeEnv(); err != nil {
		t.Fatalf("loadRuntimeEnv() error = %v", err)
	}

	if got := os.Getenv("AGW_ADMIN_ADDR"); got != "from-shell" {
		t.Fatalf("AGW_ADMIN_ADDR = %q, want %q", got, "from-shell")
	}
	if got := os.Getenv("AGW_ADMIN_BASIC_AUTH"); got != "from-dotenv-auth" {
		t.Fatalf("AGW_ADMIN_BASIC_AUTH = %q, want %q", got, "from-dotenv-auth")
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", path, err)
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	prev, ok := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("os.Unsetenv(%q) error = %v", key, err)
	}
	t.Cleanup(func() {
		var err error
		if ok {
			err = os.Setenv(key, prev)
		} else {
			err = os.Unsetenv(key)
		}
		if err != nil {
			t.Fatalf("restore env %q error = %v", key, err)
		}
	})
}
