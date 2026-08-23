package sqlite

import (
	"strings"
	"time"

	"gorm.io/gorm"
)

func (s *SQLiteConfigStoreCreator) UsageDB() *gorm.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// llmUsageEventsDDL is parameterized by table name so the live table and the
// rebuild scratch table in dropUsageFinalizedDefault cannot drift apart.
func llmUsageEventsDDL(table string) string {
	return `CREATE TABLE IF NOT EXISTS ` + table + ` (
		event_id TEXT PRIMARY KEY, trace_id TEXT, span_id TEXT NOT NULL, parent_span_id TEXT,
		agent_depth INTEGER NOT NULL DEFAULT 0, started_at INTEGER NOT NULL, finished_at INTEGER NOT NULL,
		route_id TEXT, route_kind TEXT NOT NULL DEFAULT 'llm', route_protocol TEXT, virtual_key_id TEXT,
		success INTEGER NOT NULL DEFAULT 0, status_code INTEGER, error_type TEXT, latency_ms INTEGER,
		llm_api TEXT, api_operation TEXT, provider_id TEXT, provider_type TEXT,
		logical_model TEXT, upstream_model TEXT, credential_source TEXT, credential_id TEXT,
		stream INTEGER NOT NULL DEFAULT 0, transport TEXT, response_outcome TEXT, response_committed INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER, output_tokens INTEGER, total_tokens INTEGER,
		cached_tokens INTEGER, reasoning_tokens INTEGER,
		usage_finalized INTEGER NOT NULL, request_tool_count INTEGER NOT NULL DEFAULT 0,
		request_tool_names TEXT, tool_call_count INTEGER NOT NULL DEFAULT 0, tool_names TEXT,
		agent_id TEXT, run_id TEXT, runtime_type TEXT
	)`
}

// MigrateUsageTables creates the typed usage event tables and brings an older
// database up to the current shape.
//
// The mcp_usage_events columns presented_tool_name, executed_tool_name,
// execution_mode, and policy_action are reserved for a future MCP tool-policy
// layer. Nothing populates them in v0.4.x, so they are always NULL; do not read
// them as dimensions.
func MigrateUsageTables(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	tables := []string{
		llmUsageEventsDDL("llm_usage_events"),
		`CREATE TABLE IF NOT EXISTS mcp_usage_events (
			event_id TEXT PRIMARY KEY, trace_id TEXT, span_id TEXT NOT NULL, parent_span_id TEXT,
			agent_depth INTEGER NOT NULL DEFAULT 0, started_at INTEGER NOT NULL, finished_at INTEGER NOT NULL,
			route_id TEXT, route_kind TEXT NOT NULL DEFAULT 'mcp', route_protocol TEXT, virtual_key_id TEXT,
			success INTEGER NOT NULL DEFAULT 0, status_code INTEGER, error_type TEXT, latency_ms INTEGER,
			request_id TEXT, service_id TEXT, method TEXT, tool_name TEXT, presented_tool_name TEXT,
			executed_tool_name TEXT, execution_mode TEXT, policy_action TEXT, resource_uri TEXT, prompt_name TEXT,
			completion_ref_type TEXT, completion_argument TEXT, arg_count INTEGER, result_status TEXT,
			cancelled INTEGER NOT NULL DEFAULT 0, tool_args_json TEXT,
			agent_id TEXT, run_id TEXT, runtime_type TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS acp_usage_events (
			event_id TEXT PRIMARY KEY, trace_id TEXT, span_id TEXT NOT NULL, parent_span_id TEXT,
			agent_depth INTEGER NOT NULL DEFAULT 0, started_at INTEGER NOT NULL, finished_at INTEGER NOT NULL,
			route_id TEXT, route_kind TEXT NOT NULL DEFAULT 'agent', route_protocol TEXT, virtual_key_id TEXT,
			success INTEGER NOT NULL DEFAULT 0, status_code INTEGER, error_type TEXT, latency_ms INTEGER,
			service_id TEXT, agent_type TEXT, operation TEXT, thread_id TEXT, session_id TEXT,
			permission_request_id TEXT, fresh_session INTEGER, event_counts_json TEXT, usage_json TEXT,
			result_status TEXT, agent_id TEXT, run_id TEXT, runtime_type TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS builtin_usage_events (
			event_id TEXT PRIMARY KEY, trace_id TEXT, span_id TEXT NOT NULL, parent_span_id TEXT,
			agent_depth INTEGER NOT NULL DEFAULT 0, started_at INTEGER NOT NULL, finished_at INTEGER NOT NULL,
			route_id TEXT, route_kind TEXT NOT NULL DEFAULT 'agent', route_protocol TEXT, virtual_key_id TEXT,
			success INTEGER NOT NULL DEFAULT 0, status_code INTEGER, error_type TEXT, latency_ms INTEGER,
			operation TEXT, session_id TEXT, run_id TEXT, permission_request_id TEXT,
			link_trace_id TEXT, link_span_id TEXT, topology_kind TEXT, model_steps INTEGER NOT NULL DEFAULT 0,
			tool_steps INTEGER NOT NULL DEFAULT 0, event_counts_json TEXT, result_status TEXT, agent_id TEXT,
			runtime_type TEXT
		)`,
	}
	for _, stmt := range tables {
		if err := db.Exec(stmt).Error; err != nil {
			return err
		}
	}
	// Additive columns for databases created before the column existed; each is
	// added idempotently, ignoring the duplicate-column error on databases that
	// already have it. agent_id is the attribution tag on all three tables;
	// cached_tokens/reasoning_tokens are the LLM token-detail breakdown.
	additive := []struct {
		table  string
		column string
	}{
		{"llm_usage_events", "agent_id TEXT"},
		{"mcp_usage_events", "agent_id TEXT"},
		{"acp_usage_events", "agent_id TEXT"},
		{"llm_usage_events", "run_id TEXT"},
		{"llm_usage_events", "runtime_type TEXT"},
		{"mcp_usage_events", "run_id TEXT"},
		{"mcp_usage_events", "runtime_type TEXT"},
		{"acp_usage_events", "run_id TEXT"},
		{"acp_usage_events", "runtime_type TEXT"},
		{"builtin_usage_events", "runtime_type TEXT"},
		{"llm_usage_events", "cached_tokens INTEGER"},
		{"llm_usage_events", "reasoning_tokens INTEGER"},
		{"llm_usage_events", "transport TEXT"},
		{"llm_usage_events", "response_outcome TEXT"},
		{"llm_usage_events", "response_committed INTEGER NOT NULL DEFAULT 0"},
		{"builtin_usage_events", "run_id TEXT"},
		{"builtin_usage_events", "permission_request_id TEXT"},
		{"builtin_usage_events", "link_trace_id TEXT"},
		{"builtin_usage_events", "link_span_id TEXT"},
	}
	for _, add := range additive {
		if err := db.Exec("ALTER TABLE " + add.table + " ADD COLUMN " + add.column).Error; err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				return err
			}
		}
	}
	if err := dropUsageFinalizedDefault(db); err != nil {
		return err
	}
	// Indexes are created last: dropUsageFinalizedDefault drops the old table and
	// takes its indexes with it, so these statements also recreate them.
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_llm_events_started ON llm_usage_events (started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_llm_events_route ON llm_usage_events (route_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_llm_events_vkey ON llm_usage_events (virtual_key_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_llm_events_trace ON llm_usage_events (trace_id, started_at) WHERE trace_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_llm_events_tool_use ON llm_usage_events (tool_call_count, started_at) WHERE tool_call_count > 0`,
		`CREATE INDEX IF NOT EXISTS idx_llm_events_agent ON llm_usage_events (agent_id, started_at) WHERE agent_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_llm_events_run ON llm_usage_events (run_id, started_at) WHERE run_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_llm_events_runtime ON llm_usage_events (runtime_type, started_at) WHERE runtime_type IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_mcp_events_started ON mcp_usage_events (started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_mcp_events_route ON mcp_usage_events (route_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_mcp_events_request ON mcp_usage_events (route_id, request_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_mcp_events_trace ON mcp_usage_events (trace_id, started_at) WHERE trace_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_mcp_events_tool ON mcp_usage_events (tool_name, started_at) WHERE tool_name IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_mcp_events_agent ON mcp_usage_events (agent_id, started_at) WHERE agent_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_mcp_events_run ON mcp_usage_events (run_id, started_at) WHERE run_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_mcp_events_runtime ON mcp_usage_events (runtime_type, started_at) WHERE runtime_type IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_acp_events_started ON acp_usage_events (started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_acp_events_route ON acp_usage_events (route_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_acp_events_service ON acp_usage_events (service_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_acp_events_trace ON acp_usage_events (trace_id, started_at) WHERE trace_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_acp_events_thread ON acp_usage_events (thread_id, started_at) WHERE thread_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_acp_events_agent ON acp_usage_events (agent_id, started_at) WHERE agent_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_acp_events_run ON acp_usage_events (run_id, started_at) WHERE run_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_acp_events_runtime ON acp_usage_events (runtime_type, started_at) WHERE runtime_type IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_builtin_events_started ON builtin_usage_events (started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_builtin_events_route ON builtin_usage_events (route_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_builtin_events_trace ON builtin_usage_events (trace_id, started_at) WHERE trace_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_builtin_events_run ON builtin_usage_events (run_id, started_at) WHERE run_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_builtin_events_agent ON builtin_usage_events (agent_id, started_at) WHERE agent_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_builtin_events_runtime ON builtin_usage_events (runtime_type, started_at) WHERE runtime_type IS NOT NULL`,
	}
	for _, stmt := range indexes {
		if err := db.Exec(stmt).Error; err != nil {
			return err
		}
	}
	return nil
}

// dropUsageFinalizedDefault removes the `DEFAULT 1` that llm_usage_events
// carried before v0.4. Finalization must be stated by the writer, not assumed by
// the schema: defaulting to 1 turns a caller that forgot the column into a row
// that silently claims exact token counts.
//
// SQLite cannot drop a column default in place, so this rebuilds the table. It
// is guarded on the default actually being present, which is false for every
// database created at or after v0.4 — the rebuild runs at most once per database.
func dropUsageFinalizedDefault(db *gorm.DB) error {
	hasDefault, err := columnHasDefault(db, "llm_usage_events", "usage_finalized")
	if err != nil || !hasDefault {
		return err
	}
	cols := strings.Join(llmUsageInsertColumns, ", ")
	return db.Transaction(func(tx *gorm.DB) error {
		stmts := []string{
			// A prior interrupted rebuild can leave the scratch table behind.
			`DROP TABLE IF EXISTS llm_usage_events_rebuild`,
			llmUsageEventsDDL("llm_usage_events_rebuild"),
			`INSERT INTO llm_usage_events_rebuild (` + cols + `) SELECT ` + cols + ` FROM llm_usage_events`,
			`DROP TABLE llm_usage_events`,
			`ALTER TABLE llm_usage_events_rebuild RENAME TO llm_usage_events`,
		}
		for _, stmt := range stmts {
			if err := tx.Exec(stmt).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// columnHasDefault reports whether the column carries a SQL default. A missing
// table or column reports false rather than erroring: the caller runs after the
// CREATE TABLE statements, so neither can legitimately be absent.
func columnHasDefault(db *gorm.DB, table, column string) (bool, error) {
	rows, err := db.Raw("PRAGMA table_info(" + table + ")").Rows()
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return defaultValue != nil, rows.Err()
		}
	}
	return false, rows.Err()
}

func CleanupUsageEvents(db *gorm.DB, retention time.Duration) error {
	if db == nil || retention <= 0 {
		return nil
	}
	cutoff := time.Now().UTC().Add(-retention).UnixMilli()
	for _, table := range []string{"llm_usage_events", "mcp_usage_events", "acp_usage_events", "builtin_usage_events"} {
		if err := db.Exec("DELETE FROM "+table+" WHERE started_at < ?", cutoff).Error; err != nil {
			return err
		}
	}
	return nil
}
