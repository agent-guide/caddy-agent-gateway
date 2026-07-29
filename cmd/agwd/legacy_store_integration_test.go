package main

import (
	"crypto/sha256"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	_ "github.com/glebarez/go-sqlite"
)

func TestBuiltBinaryRejectsLegacyStoreWithoutMutation(t *testing.T) {
	repoRoot := repositoryRoot(t)
	tempDir := t.TempDir()
	binaryPath := filepath.Join(tempDir, "agwd")
	build := exec.Command("go", "build", "-o", binaryPath, "./cmd/agwd")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agwd: %v\n%s", err, output)
	}

	dbPath := filepath.Join(tempDir, "legacy.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE acp_services (id TEXT PRIMARY KEY); INSERT INTO acp_services(id) VALUES ('legacy-acp')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := directorySnapshot(t, tempDir)

	command := exec.Command(binaryPath,
		"--config-store", dbPath,
		"--addr", "127.0.0.1:0",
		"--admin-addr", "127.0.0.1:0",
	)
	command.Dir = tempDir
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("legacy store startup unexpectedly succeeded: %s", output)
	}
	message := string(output)
	for _, want := range []string{
		"legacy_agent_runtime_config",
		"store was left unchanged",
		"export with the old binary",
		"scripts/migrate-unified-agent-runtime",
		"apply to a clean new-version store",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("startup error missing %q: %s", want, message)
		}
	}
	if after := directorySnapshot(t, tempDir); !reflect.DeepEqual(after, before) {
		t.Fatalf("legacy store directory changed after rejected startup:\nbefore=%x\nafter=%x", before, after)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func directorySnapshot(t *testing.T, dir string) map[string][sha256.Size]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := make(map[string][sha256.Size]byte, len(entries))
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		snapshot[entry.Name()] = sha256.Sum256(data)
	}
	return snapshot
}
