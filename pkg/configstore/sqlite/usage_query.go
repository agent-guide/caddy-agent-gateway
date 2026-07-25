package sqlite

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"gorm.io/gorm"
)

type UsageQueries struct {
	db *gorm.DB
}

func NewUsageQueries(db *gorm.DB) *UsageQueries {
	return &UsageQueries{db: db}
}

func (q *UsageQueries) Summary() (usage.Summary, error) {
	var out usage.Summary
	if q == nil || q.db == nil {
		return out, nil
	}
	if err := q.db.Raw(`SELECT COUNT(*), COALESCE(SUM(success),0), COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(total_tokens),0),
		COALESCE(SUM(cached_tokens),0), COALESCE(SUM(reasoning_tokens),0), COALESCE(AVG(latency_ms),0)
		FROM llm_usage_events`).Row().Scan(&out.LLM.RequestCount, &out.LLM.SuccessCount, &out.LLM.FailureCount, &out.LLM.InputTokens, &out.LLM.OutputTokens, &out.LLM.TotalTokens, &out.LLM.CachedTokens, &out.LLM.ReasoningTokens, &out.LLM.AvgLatencyMS); err != nil {
		return out, err
	}
	if err := q.db.Raw(`SELECT COUNT(*), COALESCE(SUM(success),0), COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN method='tools/call' THEN 1 ELSE 0 END),0), COALESCE(AVG(latency_ms),0)
		FROM mcp_usage_events`).Row().Scan(&out.MCP.RequestCount, &out.MCP.SuccessCount, &out.MCP.FailureCount, &out.MCP.ToolsCallCount, &out.MCP.AvgLatencyMS); err != nil {
		return out, err
	}
	if err := q.db.Raw(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN operation='turn' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(success),0), COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END),0), COALESCE(AVG(latency_ms),0)
		FROM acp_usage_events`).Row().Scan(&out.ACP.RequestCount, &out.ACP.TurnCount, &out.ACP.SuccessCount, &out.ACP.FailureCount, &out.ACP.AvgLatencyMS); err != nil {
		return out, err
	}
	if err := q.db.Raw(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN operation='turn' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(success),0), COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END),0), COALESCE(AVG(latency_ms),0)
		FROM builtin_usage_events WHERE COALESCE(operation, '') <> 'permission_expire'`).Row().Scan(&out.Builtin.RequestCount, &out.Builtin.TurnCount, &out.Builtin.SuccessCount, &out.Builtin.FailureCount, &out.Builtin.AvgLatencyMS); err != nil {
		return out, err
	}
	return out, nil
}

func (q *UsageQueries) ListEvents(kind string, opts usage.EventListOptions) (usage.EventListResponse, error) {
	table, filters, err := eventTable(kind)
	if err != nil {
		return usage.EventListResponse{}, err
	}
	return q.listRows("SELECT * FROM "+table, table, filters, opts)
}

func (q *UsageQueries) ListInteractions(opts usage.EventListOptions) (usage.EventListResponse, error) {
	filters := map[string]string{
		"route_kind": "route_kind", "route_protocol": "route_protocol", "route_id": "route_id",
		"virtual_key_id": "virtual_key_id", "trace_id": "trace_id", "parent_span_id": "parent_span_id",
		"agent_depth": "agent_depth", "agent_id": "agent_id", "service_id": "service_id", "session_id": "session_id",
		"run_id": "run_id", "runtime_type": "runtime_type", "permission_request_id": "permission_request_id",
		"link_trace_id": "link_trace_id", "link_span_id": "link_span_id",
	}
	return q.listRows(interactionListBaseQuery(), "interactions", filters, opts)
}

func interactionListBaseQuery() string {
	baseCols := `event_id, trace_id, span_id, parent_span_id, agent_depth, started_at, finished_at,
		route_id, route_kind, route_protocol, virtual_key_id, success, status_code, error_type, latency_ms, agent_id, run_id, runtime_type`
	// Project protocol-specific columns so the UNION lines up, ACP session
	// filtering works through the mixed interaction view, and the consumer can
	// label a span by what it actually did (LLM upstream_model, MCP tool_name,
	// ACP operation) instead of falling back to the synthetic route_id. The LLM
	// tool-use and token columns are carried through as well so the call-chain
	// view can show what tools a model was offered / invoked and its token cost;
	// they only exist on llm_usage_events, so MCP/ACP branches NULL them out.
	// Column order must match across every branch; absent columns are NULL
	// placeholders.
	llmExtra := `request_tool_count, request_tool_names, tool_call_count, tool_names, input_tokens, output_tokens, total_tokens, cached_tokens, reasoning_tokens`
	nullExtra := `NULL AS request_tool_count, NULL AS request_tool_names, NULL AS tool_call_count, NULL AS tool_names, NULL AS input_tokens, NULL AS output_tokens, NULL AS total_tokens, NULL AS cached_tokens, NULL AS reasoning_tokens`
	query := "SELECT * FROM (" +
		"SELECT " + baseCols + ", NULL AS service_id, NULL AS session_id, NULL AS operation, NULL AS permission_request_id, NULL AS link_trace_id, NULL AS link_span_id, NULL AS tool_name, upstream_model, " + llmExtra + " FROM llm_usage_events UNION ALL " +
		"SELECT " + baseCols + ", service_id, NULL AS session_id, NULL AS operation, NULL AS permission_request_id, NULL AS link_trace_id, NULL AS link_span_id, tool_name, NULL AS upstream_model, " + nullExtra + " FROM mcp_usage_events UNION ALL " +
		"SELECT " + baseCols + ", service_id, session_id, operation, permission_request_id, NULL AS link_trace_id, NULL AS link_span_id, NULL AS tool_name, NULL AS upstream_model, " + nullExtra + " FROM acp_usage_events UNION ALL " +
		"SELECT " + baseCols + ", NULL AS service_id, session_id, operation, permission_request_id, link_trace_id, link_span_id, NULL AS tool_name, NULL AS upstream_model, " + nullExtra + " FROM builtin_usage_events) interactions"
	return query
}

// Per-protocol dimension whitelists and measure expressions shared by the
// timeseries and breakdown queries so both surfaces stay column-aligned.
var (
	llmGroups = map[string]string{
		"route_id": "route_id", "provider_id": "provider_id", "virtual_key_id": "virtual_key_id", "upstream_model": "upstream_model", "llm_api": "llm_api", "run_id": "run_id", "runtime_type": "runtime_type",
	}
	llmMeasures = `COUNT(*) AS request_count, COALESCE(SUM(success),0) AS success_count,
		COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END),0) AS failure_count,
		COALESCE(SUM(input_tokens),0) AS input_tokens, COALESCE(SUM(output_tokens),0) AS output_tokens,
		COALESCE(SUM(total_tokens),0) AS total_tokens,
		COALESCE(SUM(cached_tokens),0) AS cached_tokens, COALESCE(SUM(reasoning_tokens),0) AS reasoning_tokens,
		COALESCE(AVG(latency_ms),0) AS avg_latency_ms`

	mcpGroups = map[string]string{
		"route_id": "route_id", "service_id": "service_id", "virtual_key_id": "virtual_key_id", "method": "method", "tool_name": "tool_name", "result_status": "result_status", "run_id": "run_id", "runtime_type": "runtime_type",
	}
	mcpMeasures = `COUNT(*) AS request_count, COALESCE(SUM(success),0) AS success_count,
		COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END),0) AS failure_count,
		COALESCE(SUM(CASE WHEN method='tools/call' THEN 1 ELSE 0 END),0) AS tools_call_count,
		COALESCE(AVG(latency_ms),0) AS avg_latency_ms`

	acpGroups = map[string]string{
		"route_id": "route_id", "route_protocol": "route_protocol", "service_id": "service_id", "virtual_key_id": "virtual_key_id", "agent_type": "agent_type", "operation": "operation", "run_id": "run_id", "runtime_type": "runtime_type",
	}
	acpMeasures = `COUNT(*) AS request_count, COALESCE(SUM(CASE WHEN operation='turn' THEN 1 ELSE 0 END),0) AS turn_count,
		COALESCE(SUM(success),0) AS success_count, COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END),0) AS failure_count,
		COALESCE(AVG(latency_ms),0) AS avg_latency_ms`
)

func (q *UsageQueries) LLMTimeseries(opts usage.TimeseriesOptions) (usage.SeriesResponse, error) {
	return q.timeseries(`llm_usage_events`, opts, llmGroups, llmMeasures)
}

func (q *UsageQueries) LLMBreakdown(opts usage.BreakdownOptions) (usage.BreakdownResponse, error) {
	return q.breakdown(`llm_usage_events`, opts, llmGroups, llmMeasures)
}

func (q *UsageQueries) MCPTimeseries(opts usage.TimeseriesOptions) (usage.SeriesResponse, error) {
	return q.timeseries(`mcp_usage_events`, opts, mcpGroups, mcpMeasures)
}

func (q *UsageQueries) MCPBreakdown(opts usage.BreakdownOptions) (usage.BreakdownResponse, error) {
	return q.breakdown(`mcp_usage_events`, opts, mcpGroups, mcpMeasures)
}

func (q *UsageQueries) ACPTimeseries(opts usage.TimeseriesOptions) (usage.SeriesResponse, error) {
	return q.timeseries(`acp_usage_events`, opts, acpGroups, acpMeasures)
}

func (q *UsageQueries) ACPBreakdown(opts usage.BreakdownOptions) (usage.BreakdownResponse, error) {
	return q.breakdown(`acp_usage_events`, opts, acpGroups, acpMeasures)
}

func (q *UsageQueries) MCPToolsSummary(opts usage.SummaryOptions) (usage.BreakdownResponse, error) {
	bo := usage.BreakdownOptions{From: opts.From, To: opts.To, GroupBy: "tool_name", Limit: opts.Limit, Filters: opts.Filters, OrderBy: "request_count", Attribution: opts.Attribution}
	return q.breakdown(`mcp_usage_events`, bo, map[string]string{"tool_name": "tool_name", "route_id": "route_id", "service_id": "service_id", "run_id": "run_id", "runtime_type": "runtime_type"}, `COUNT(*) AS request_count,
		COALESCE(SUM(success),0) AS success_count, COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END),0) AS failure_count,
		COALESCE(AVG(latency_ms),0) AS avg_latency_ms`)
}

func (q *UsageQueries) ACPSummary(opts usage.BreakdownOptions) (usage.BreakdownResponse, error) {
	return q.breakdown(`acp_usage_events`, opts, acpGroups, acpMeasures)
}

func (q *UsageQueries) InteractionsSummary(opts usage.BreakdownOptions) (usage.BreakdownResponse, error) {
	allowed := map[string]string{
		"route_kind": "route_kind", "route_protocol": "route_protocol", "route_id": "route_id", "virtual_key_id": "virtual_key_id",
		"runtime_type": "runtime_type",
	}
	groupCol, err := allowedGroupBy(opts.GroupBy, allowed)
	if err != nil {
		return usage.BreakdownResponse{}, err
	}
	if groupCol == "" {
		groupCol = "route_kind"
		opts.GroupBy = "route_kind"
	}
	// Filterable columns are a superset of the group-by columns: service_id and
	// session_id are projected from the protocol tables (NULL where absent) so the
	// mixed interaction view can be narrowed to a single ACP service or session
	// without exposing them as group_by dimensions.
	filterable := map[string]string{
		"route_kind": "route_kind", "route_protocol": "route_protocol", "route_id": "route_id", "virtual_key_id": "virtual_key_id",
		"service_id": "service_id", "session_id": "session_id", "run_id": "run_id", "runtime_type": "runtime_type",
	}
	baseCols := `event_id, started_at, route_kind, route_protocol, route_id, virtual_key_id, success, latency_ms, agent_id, run_id, runtime_type`
	query := "SELECT " + groupCol + ` AS group_value, COUNT(*) AS request_count, COALESCE(SUM(success),0) AS success_count,
		COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END),0) AS failure_count, COALESCE(AVG(latency_ms),0) AS avg_latency_ms
		FROM (` +
		"SELECT " + baseCols + ", NULL AS service_id, NULL AS session_id FROM llm_usage_events UNION ALL " +
		"SELECT " + baseCols + ", service_id, NULL AS session_id FROM mcp_usage_events UNION ALL " +
		"SELECT " + baseCols + ", service_id, session_id FROM acp_usage_events UNION ALL " +
		"SELECT " + baseCols + ", NULL AS service_id, session_id FROM builtin_usage_events WHERE COALESCE(operation, '') <> 'permission_expire') interactions"
	where, args, err := buildTimeWhere(opts.From, opts.To)
	if err != nil {
		return usage.BreakdownResponse{}, err
	}
	for key, value := range opts.Filters {
		if value == "" || key == opts.GroupBy {
			continue
		}
		col, ok := filterable[key]
		if !ok {
			continue
		}
		where = append(where, col+" = ?")
		args = append(args, value)
	}
	if clause, attrArgs := attributionWhere(opts.Attribution, serviceArmACPScoped); clause != "" {
		where = append(where, clause)
		args = append(args, attrArgs...)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " GROUP BY " + groupCol + " ORDER BY request_count DESC LIMIT ?"
	limit := normalizedLimit(opts.Limit)
	args = append(args, limit)
	items, err := q.rawItems(query, args...)
	return usage.BreakdownResponse{GroupBy: opts.GroupBy, Items: items, Limit: limit}, err
}

func (q *UsageQueries) listRows(baseQuery, table string, allowedFilters map[string]string, opts usage.EventListOptions) (usage.EventListResponse, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	resp := usage.EventListResponse{Limit: limit}
	if q == nil || q.db == nil {
		return resp, nil
	}
	where, args, err := buildEventWhere(allowedFilters, opts)
	if err != nil {
		return resp, err
	}
	if clause, attrArgs := attributionWhere(opts.Attribution, serviceArmForTable(table)); clause != "" {
		where = append(where, clause)
		args = append(args, attrArgs...)
	}
	query := baseQuery
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY started_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := q.db.Raw(query, args...).Rows()
	if err != nil {
		return resp, err
	}
	defer rows.Close()
	items, err := scanRows(rows)
	if err != nil {
		return resp, err
	}
	resp.Items = items
	return resp, nil
}

func (q *UsageQueries) timeseries(table string, opts usage.TimeseriesOptions, allowedGroups map[string]string, measures string) (usage.SeriesResponse, error) {
	bucket, bucketMS, err := resolveBucket(opts.Bucket)
	if err != nil {
		return usage.SeriesResponse{}, err
	}
	groupCol := ""
	if opts.GroupBy != "" {
		groupCol, err = allowedGroupBy(opts.GroupBy, allowedGroups)
		if err != nil {
			return usage.SeriesResponse{}, err
		}
	}
	selectGroup := "'' AS group_value"
	groupBy := "bucket_ms"
	if groupCol != "" {
		selectGroup = groupCol + " AS group_value"
		groupBy += ", " + groupCol
	}
	where, args, err := buildTimeWhere(opts.From, opts.To)
	if err != nil {
		return usage.SeriesResponse{}, err
	}
	for key, value := range opts.Filters {
		if value == "" {
			continue
		}
		col, ok := allowedGroups[key]
		if !ok {
			continue
		}
		where = append(where, col+" = ?")
		args = append(args, value)
	}
	if clause, attrArgs := attributionWhere(opts.Attribution, serviceArmForTable(table)); clause != "" {
		where = append(where, clause)
		args = append(args, attrArgs...)
	}
	query := `SELECT CAST(started_at / ? AS INTEGER) * ? AS bucket_ms, ` + selectGroup + `, ` + measures + ` FROM ` + table
	allArgs := []any{bucketMS, bucketMS}
	allArgs = append(allArgs, args...)
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " GROUP BY " + groupBy + " ORDER BY bucket_ms ASC"
	items, err := q.rawItems(query, allArgs...)
	if err != nil {
		return usage.SeriesResponse{}, err
	}
	formatBucketItems(items)
	return usage.SeriesResponse{Bucket: bucket, GroupBy: opts.GroupBy, Items: items}, nil
}

func (q *UsageQueries) breakdown(table string, opts usage.BreakdownOptions, allowedGroups map[string]string, measures string) (usage.BreakdownResponse, error) {
	groupCol, err := allowedGroupBy(opts.GroupBy, allowedGroups)
	if err != nil {
		return usage.BreakdownResponse{}, err
	}
	if groupCol == "" {
		for key, col := range allowedGroups {
			opts.GroupBy = key
			groupCol = col
			break
		}
	}
	where, args, err := buildTimeWhere(opts.From, opts.To)
	if err != nil {
		return usage.BreakdownResponse{}, err
	}
	for key, value := range opts.Filters {
		if value == "" || key == opts.GroupBy {
			continue
		}
		col, ok := allowedGroups[key]
		if !ok {
			continue
		}
		where = append(where, col+" = ?")
		args = append(args, value)
	}
	if clause, attrArgs := attributionWhere(opts.Attribution, serviceArmForTable(table)); clause != "" {
		where = append(where, clause)
		args = append(args, attrArgs...)
	}
	query := "SELECT " + groupCol + " AS group_value, " + measures + " FROM " + table
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " GROUP BY " + groupCol + " ORDER BY " + orderByMetric(opts.OrderBy) + " DESC LIMIT ?"
	limit := normalizedLimit(opts.Limit)
	args = append(args, limit)
	items, err := q.rawItems(query, args...)
	return usage.BreakdownResponse{GroupBy: opts.GroupBy, Items: items, Limit: limit}, err
}

func eventTable(kind string) (string, map[string]string, error) {
	switch kind {
	case "llm":
		return "llm_usage_events", map[string]string{
			"route_id": "route_id", "provider_id": "provider_id", "virtual_key_id": "virtual_key_id",
			"logical_model": "logical_model", "upstream_model": "upstream_model", "llm_api": "llm_api",
			"api_operation": "api_operation", "request_tool_name": "request_tool_names", "has_tool_use": "tool_call_count",
			"agent_id": "agent_id", "run_id": "run_id", "runtime_type": "runtime_type",
		}, nil
	case "mcp":
		return "mcp_usage_events", map[string]string{
			"route_id": "route_id", "service_id": "service_id", "virtual_key_id": "virtual_key_id",
			"method": "method", "tool_name": "tool_name", "resource_uri": "resource_uri",
			"prompt_name": "prompt_name", "completion_ref_type": "completion_ref_type",
			"completion_argument": "completion_argument", "result_status": "result_status",
			"agent_id": "agent_id", "run_id": "run_id", "runtime_type": "runtime_type",
		}, nil
	case "acp":
		return "acp_usage_events", map[string]string{
			"route_id": "route_id", "route_protocol": "route_protocol", "service_id": "service_id", "virtual_key_id": "virtual_key_id",
			"agent_type": "agent_type", "operation": "operation", "thread_id": "thread_id", "session_id": "session_id",
			"agent_id": "agent_id", "run_id": "run_id", "runtime_type": "runtime_type",
		}, nil
	case "builtin":
		return "builtin_usage_events", map[string]string{
			"route_id": "route_id", "virtual_key_id": "virtual_key_id", "operation": "operation",
			"session_id": "session_id", "topology_kind": "topology_kind", "result_status": "result_status",
			"run_id": "run_id", "permission_request_id": "permission_request_id",
			"link_trace_id": "link_trace_id", "link_span_id": "link_span_id",
			"agent_id": "agent_id", "runtime_type": "runtime_type",
		}, nil
	default:
		return "", nil, fmt.Errorf("unknown usage event kind %q", kind)
	}
}

func buildEventWhere(allowed map[string]string, opts usage.EventListOptions) ([]string, []any, error) {
	var where []string
	var args []any
	if opts.From != "" {
		t, err := time.Parse(time.RFC3339, opts.From)
		if err != nil {
			return nil, nil, fmt.Errorf("from must be RFC3339")
		}
		where = append(where, "started_at >= ?")
		args = append(args, unixMillis(t))
	}
	if opts.To != "" {
		t, err := time.Parse(time.RFC3339, opts.To)
		if err != nil {
			return nil, nil, fmt.Errorf("to must be RFC3339")
		}
		where = append(where, "started_at <= ?")
		args = append(args, unixMillis(t))
	}
	if opts.Success != nil {
		where = append(where, "success = ?")
		args = append(args, boolInt(*opts.Success))
	}
	for key, value := range opts.Filters {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		col, ok := allowed[key]
		if !ok {
			continue
		}
		if key == "request_tool_name" {
			where = append(where, col+" LIKE ?")
			args = append(args, "%"+value+"%")
			continue
		}
		if key == "has_tool_use" {
			hasToolUse, err := strconv.ParseBool(value)
			if err != nil {
				return nil, nil, fmt.Errorf("has_tool_use must be a boolean")
			}
			if hasToolUse {
				where = append(where, col+" > 0")
			} else {
				where = append(where, col+" = 0")
			}
			continue
		}
		where = append(where, col+" = ?")
		args = append(args, value)
	}
	return where, args, nil
}

func scanRows(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		item := make(map[string]any, len(cols))
		for i, col := range cols {
			item[col] = normalizeSQLValue(col, values[i])
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (q *UsageQueries) rawItems(query string, args ...any) ([]map[string]any, error) {
	if q == nil || q.db == nil {
		return nil, nil
	}
	rows, err := q.db.Raw(query, args...).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

func buildTimeWhere(from, to string) ([]string, []any, error) {
	var where []string
	var args []any
	if strings.TrimSpace(from) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(from))
		if err != nil {
			return nil, nil, fmt.Errorf("from must be RFC3339")
		}
		where = append(where, "started_at >= ?")
		args = append(args, unixMillis(t))
	}
	if strings.TrimSpace(to) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(to))
		if err != nil {
			return nil, nil, fmt.Errorf("to must be RFC3339")
		}
		where = append(where, "started_at <= ?")
		args = append(args, unixMillis(t))
	}
	return where, args, nil
}

// resolveBucket maps a caller-supplied bucket value to its canonical name and
// width in milliseconds. It accepts named buckets with plural/short forms
// ("minute"/"minutes"/"min"/"m", "hour"/"hours"/"hr"/"h", "day"/"days"/"d") and
// Grafana-style duration strings ("3h", "5m", "30s", "1d", "90m"). An empty
// value defaults to "hour". The bucketing SQL works for any positive width, so
// the only constraint is that the resolved duration is greater than zero.
func resolveBucket(bucket string) (string, int64, error) {
	normalized := strings.ToLower(strings.TrimSpace(bucket))
	switch normalized {
	case "minute", "minutes", "min", "m":
		return "minute", int64(time.Minute / time.Millisecond), nil
	case "hour", "hours", "hr", "h", "":
		return "hour", int64(time.Hour / time.Millisecond), nil
	case "day", "days", "d":
		return "day", int64(24 * time.Hour / time.Millisecond), nil
	}
	if d, ok := parseDurationBucket(normalized); ok {
		return normalized, int64(d / time.Millisecond), nil
	}
	return "", 0, fmt.Errorf("bucket %q is not supported (use minute, hour, day, or a duration like 3h, 5m, 1d)", bucket)
}

// parseDurationBucket parses a Grafana-style duration bucket. Go's
// time.ParseDuration has no day unit, so a trailing "<n>d" is expanded to hours
// first. Returns ok=false for non-duration or non-positive values.
func parseDurationBucket(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	if num, found := strings.CutSuffix(s, "d"); found {
		days, err := strconv.ParseFloat(num, 64)
		if err != nil || days <= 0 {
			return 0, false
		}
		return time.Duration(days * float64(24*time.Hour)), true
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

func allowedGroupBy(groupBy string, allowed map[string]string) (string, error) {
	groupBy = strings.TrimSpace(groupBy)
	if groupBy == "" {
		return "", nil
	}
	col, ok := allowed[groupBy]
	if !ok {
		return "", fmt.Errorf("unsupported group_by %q", groupBy)
	}
	return col, nil
}

func normalizedLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}

func orderByMetric(orderBy string) string {
	switch strings.TrimSpace(orderBy) {
	case "total_tokens":
		return "total_tokens"
	case "failure_count":
		return "failure_count"
	case "avg_latency_ms":
		return "avg_latency_ms"
	default:
		return "request_count"
	}
}

func formatBucketItems(items []map[string]any) {
	for _, item := range items {
		switch v := item["bucket_ms"].(type) {
		case int64:
			item["bucket"] = time.UnixMilli(v).UTC().Format(time.RFC3339Nano)
		case int:
			item["bucket"] = time.UnixMilli(int64(v)).UTC().Format(time.RFC3339Nano)
		}
	}
}

func normalizeSQLValue(col string, v any) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case int64:
		switch col {
		case "started_at", "finished_at", "bucket_ms":
			if x <= 0 {
				return ""
			}
			if col == "bucket_ms" {
				return x
			}
			return time.UnixMilli(x).UTC().Format(time.RFC3339Nano)
		case "success", "stream", "usage_finalized", "cancelled", "fresh_session":
			return x != 0
		default:
			return x
		}
	case nil:
		return nil
	default:
		return v
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func unixMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixNano() / int64(time.Millisecond)
}

// serviceArm describes how the attribution service_id arm renders for a given
// query target. The agent's only unambiguous service fallback is its ACP runtime
// service, so the arm must be ACP-scoped: acp_services and mcp_services are
// separate stores with no global id uniqueness, so a bare `service_id IN (...)`
// would also match an unrelated mcp_usage_events row that happens to share the
// id.
type serviceArm int

const (
	// serviceArmNone: do not emit a service arm (llm/mcp tables).
	serviceArmNone serviceArm = iota
	// serviceArmACP: the target is the acp event table, every row is already ACP,
	// so a plain `service_id IN (...)` is ACP-scoped.
	serviceArmACP
	// serviceArmACPScoped: the target is the interactions UNION (mixed protocols),
	// so the service arm must guard on `route_kind = 'acp'`.
	serviceArmACPScoped
)

func serviceArmForTable(table string) serviceArm {
	switch table {
	case "acp_usage_events":
		return serviceArmACP
	case "interactions":
		return serviceArmACPScoped
	default:
		// llm has no service_id; mcp must not be matched by the ACP service arm.
		return serviceArmNone
	}
}

// attributionWhere builds the per-agent OR clause `(agent_id = ? OR route_id IN
// (...) [OR <acp service arm>])`. Route ids are globally unique (single routes
// store), so the route arm is unconditional. The service arm is ACP-only and
// emitted per the serviceArm mode. A non-nil filter that yields no usable arm
// collapses to a match-nothing clause so an attribution filter never silently
// widens to all rows; a nil filter means "no attribution filtering".
func attributionWhere(f *usage.AttributionFilter, svc serviceArm) (string, []any) {
	if f == nil {
		return "", nil
	}
	var clauses []string
	var args []any
	if f.AgentID != "" {
		clauses = append(clauses, "agent_id = ?")
		args = append(args, f.AgentID)
	}
	if ids := dedupeNonEmpty(f.RouteIDs); len(ids) > 0 {
		clauses = append(clauses, "route_id IN ("+sqlPlaceholders(len(ids))+")")
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if svc != serviceArmNone {
		if ids := dedupeNonEmpty(f.ACPServiceIDs); len(ids) > 0 {
			placeholders := sqlPlaceholders(len(ids))
			clause := "service_id IN (" + placeholders + ")"
			if svc == serviceArmACPScoped {
				clause = "(route_kind = 'acp' AND service_id IN (" + placeholders + "))"
			}
			clauses = append(clauses, clause)
			for _, id := range ids {
				args = append(args, id)
			}
		}
	}
	if len(clauses) == 0 {
		return "1 = 0", nil
	}
	return "(" + strings.Join(clauses, " OR ") + ")", args
}

func sqlPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func dedupeNonEmpty(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// nullString returns nil for an empty string so the column is stored as NULL
// (preserving the `WHERE agent_id IS NOT NULL` partial-index semantics) and the
// string value otherwise.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
