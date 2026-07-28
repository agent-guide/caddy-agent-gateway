package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	_ "github.com/glebarez/go-sqlite"
)

const LegacyAgentRuntimeConfigCode = "legacy_agent_runtime_config"

type LegacyAgentRuntimeConfigError struct{ Families map[string][]string }

func (e *LegacyAgentRuntimeConfigError) Error() string {
	if e == nil {
		return ""
	}
	keys := make([]string, 0, len(e.Families))
	for key := range e.Families {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		ids := append([]string(nil), e.Families[key]...)
		sort.Strings(ids)
		parts = append(parts, fmt.Sprintf("%s=[%s]", key, strings.Join(ids, ",")))
	}
	return fmt.Sprintf("%s: legacy Agent runtime configuration detected (%s); the store was left unchanged; export with the old binary, run scripts/migrate-unified-agent-runtime, then apply to a clean new-version store", LegacyAgentRuntimeConfigCode, strings.Join(parts, "; "))
}

// PreflightLegacyAgentRuntime inspects an existing SQLite store through a
// read-only/query-only connection. A missing path is clean and is not created.
func PreflightLegacyAgentRuntime(ctx context.Context, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("sqlite path is required")
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open legacy preflight: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("read legacy preflight: %w", err)
	}
	families := map[string][]string{}
	hasTable := func(name string) (bool, error) {
		var n int
		err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
		return n > 0, err
	}
	if ok, err := hasTable("acp_services"); err != nil {
		return err
	} else if ok {
		rows, err := db.QueryContext(ctx, `SELECT id FROM acp_services ORDER BY id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			families["acp_services"] = append(families["acp_services"], id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(families["acp_services"]) == 0 {
			families["acp_services"] = []string{"<table>"}
		}
	}
	if err := inspectLegacyJSONRows(ctx, db, "agents", func(raw map[string]any) bool {
		runtime, _ := raw["runtime"].(map[string]any)
		acp, _ := runtime["acp"].(map[string]any)
		_, serviceID := acp["service_id"]
		_, ownsService := raw["owns_service"]
		return serviceID || ownsService
	}, "agent.runtime.acp.service_id", families); err != nil {
		return err
	}
	if err := inspectLegacyJSONRows(ctx, db, "routes", func(raw map[string]any) bool {
		kind, _ := raw["kind"].(string)
		return kind == "acp" || kind == "builtin"
	}, "routes(kind=acp|builtin)", families); err != nil {
		return err
	}
	if len(families) > 0 {
		return &LegacyAgentRuntimeConfigError{Families: families}
	}
	return nil
}

func inspectLegacyJSONRows(ctx context.Context, db *sql.DB, table string, legacy func(map[string]any) bool, family string, families map[string][]string) error {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n == 0 {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT id, config FROM `+table+` ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return err
		}
		var raw map[string]any
		if json.Unmarshal(data, &raw) == nil && legacy(raw) {
			families[family] = append(families[family], id)
		}
	}
	return rows.Err()
}
