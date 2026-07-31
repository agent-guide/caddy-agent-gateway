package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreflightLegacyAgentRuntimeMissingFileDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if err := PreflightLegacyAgentRuntime(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing database was created: %v", err)
	}
}

func TestPreflightLegacyAgentRuntimeAcceptsRelativePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clean.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativePath, err := filepath.Rel(workingDir, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := PreflightLegacyAgentRuntime(context.Background(), relativePath); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightLegacyAgentRuntimeCollectsFamiliesWithoutChangingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE acp_services (id TEXT PRIMARY KEY, config BLOB)`,
		`INSERT INTO acp_services VALUES ('svc-old','{}')`,
		`CREATE TABLE agents (id TEXT PRIMARY KEY, config BLOB)`,
		`INSERT INTO agents VALUES ('agent-old','{"runtime":{"type":"acp","acp":{"service_id":"svc-old"}}}')`,
		`CREATE TABLE routes (id TEXT PRIMARY KEY, config BLOB)`,
		`INSERT INTO routes VALUES ('route-old','{"kind":"builtin"}')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	beforeHash := sha256.Sum256(before)
	err = PreflightLegacyAgentRuntime(context.Background(), path)
	var legacy *LegacyAgentRuntimeConfigError
	if !errors.As(err, &legacy) || !strings.Contains(err.Error(), LegacyAgentRuntimeConfigCode) {
		t.Fatalf("error = %v", err)
	}
	after, _ := os.ReadFile(path)
	if sha256.Sum256(after) != beforeHash {
		t.Fatal("preflight modified database")
	}
	for _, family := range []string{"acp_services", "agent.runtime.acp.service_id", "routes(kind=acp|builtin)"} {
		if len(legacy.Families[family]) == 0 {
			t.Fatalf("missing family %q: %#v", family, legacy.Families)
		}
	}
}
