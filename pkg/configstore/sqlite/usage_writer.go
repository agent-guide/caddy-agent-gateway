package sqlite

import (
	"encoding/json"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"gorm.io/gorm"
)

// maxUsagePayloadBytes caps free-form JSON payloads (MCP tool args, ACP usage
// blobs) persisted with a usage event. Raw payloads may carry caller-provided
// arguments that include secrets or PII, so they are truncated before storage.
const maxUsagePayloadBytes = 4096

var llmUsageInsertColumns = []string{
	"event_id", "trace_id", "span_id", "parent_span_id", "agent_depth", "started_at", "finished_at",
	"route_id", "route_kind", "route_protocol", "virtual_key_id", "success", "status_code", "error_type", "latency_ms",
	"llm_api", "api_operation", "provider_id", "provider_type", "logical_model", "upstream_model",
	"credential_source", "credential_id", "stream", "input_tokens", "output_tokens", "total_tokens",
	"cached_tokens", "reasoning_tokens",
	"usage_finalized", "request_tool_count", "request_tool_names", "tool_call_count", "tool_names", "agent_id", "run_id", "runtime_type",
}

var mcpUsageInsertColumns = []string{
	"event_id", "trace_id", "span_id", "parent_span_id", "agent_depth", "started_at", "finished_at",
	"route_id", "route_kind", "route_protocol", "virtual_key_id", "success", "status_code", "error_type", "latency_ms",
	"request_id", "service_id", "method", "tool_name", "presented_tool_name", "executed_tool_name",
	"execution_mode", "policy_action", "resource_uri", "prompt_name", "completion_ref_type", "completion_argument",
	"arg_count", "result_status", "cancelled", "tool_args_json", "agent_id", "run_id", "runtime_type",
}

var builtinUsageInsertColumns = []string{
	"event_id", "trace_id", "span_id", "parent_span_id", "agent_depth", "started_at", "finished_at",
	"route_id", "route_kind", "route_protocol", "virtual_key_id", "success", "status_code", "error_type", "latency_ms",
	"operation", "session_id", "run_id", "permission_request_id", "link_trace_id", "link_span_id",
	"topology_kind", "model_steps", "tool_steps",
	"event_counts_json", "result_status", "agent_id", "runtime_type",
}

var acpUsageInsertColumns = []string{
	"event_id", "trace_id", "span_id", "parent_span_id", "agent_depth", "started_at", "finished_at",
	"route_id", "route_kind", "route_protocol", "virtual_key_id", "success", "status_code", "error_type", "latency_ms",
	"service_id", "agent_type", "operation", "thread_id", "session_id", "permission_request_id", "fresh_session",
	"event_counts_json", "usage_json", "result_status", "agent_id", "run_id", "runtime_type",
}

func InsertLLMUsageEvent(db *gorm.DB, ev usage.LLMUsageEvent) error {
	names, _ := json.Marshal(ev.RequestToolNames)
	toolNames, _ := json.Marshal(ev.ToolNames)
	return db.Exec(usageInsertSQL("llm_usage_events", llmUsageInsertColumns),
		ev.EventID, ev.TraceID, ev.SpanID, ev.ParentSpanID, ev.AgentDepth, unixMillis(ev.StartedAt), unixMillis(ev.FinishedAt),
		ev.RouteID, ev.RouteKind, ev.RouteProtocol, ev.VirtualKeyID, boolInt(ev.Success), ev.StatusCode, ev.ErrorType, ev.LatencyMS,
		ev.LLMAPI, ev.APIOperation, ev.ProviderID, ev.ProviderType, ev.LogicalModel, ev.UpstreamModel,
		ev.CredentialSource, ev.CredentialID, boolInt(ev.Stream), ev.InputTokens, ev.OutputTokens, ev.TotalTokens,
		ev.CachedTokens, ev.ReasoningTokens,
		boolInt(ev.UsageFinalized), ev.RequestToolCount, string(names), ev.ToolCallCount, string(toolNames), nullString(ev.AgentID), nullString(ev.RunID), nullString(ev.RuntimeType),
	).Error
}

func InsertMCPUsageEvent(db *gorm.DB, ev usage.MCPUsageEvent) error {
	return db.Exec(usageInsertSQL("mcp_usage_events", mcpUsageInsertColumns),
		ev.EventID, ev.TraceID, ev.SpanID, ev.ParentSpanID, ev.AgentDepth, unixMillis(ev.StartedAt), unixMillis(ev.FinishedAt),
		ev.RouteID, ev.RouteKind, ev.RouteProtocol, ev.VirtualKeyID, boolInt(ev.Success), ev.StatusCode, ev.ErrorType, ev.LatencyMS,
		ev.RequestID, ev.ServiceID, ev.Method, ev.ToolName, ev.PresentedToolName, ev.ExecutedToolName,
		ev.ExecutionMode, ev.PolicyAction, ev.ResourceURI, ev.PromptName, ev.CompletionRefType, ev.CompletionArgument,
		ev.ArgCount, ev.ResultStatus, boolInt(ev.Cancelled), truncatePayload(ev.ToolArgsJSON), nullString(ev.AgentID), nullString(ev.RunID), nullString(ev.RuntimeType),
	).Error
}

func InsertACPUsageEvent(db *gorm.DB, ev usage.ACPUsageEvent) error {
	counts, _ := json.Marshal(ev.EventCounts)
	var fresh any
	if ev.FreshSession != nil {
		fresh = boolInt(*ev.FreshSession)
	}
	return db.Exec(usageInsertSQL("acp_usage_events", acpUsageInsertColumns),
		ev.EventID, ev.TraceID, ev.SpanID, ev.ParentSpanID, ev.AgentDepth, unixMillis(ev.StartedAt), unixMillis(ev.FinishedAt),
		ev.RouteID, ev.RouteKind, ev.RouteProtocol, ev.VirtualKeyID, boolInt(ev.Success), ev.StatusCode, ev.ErrorType, ev.LatencyMS,
		ev.ServiceID, ev.AgentType, ev.Operation, ev.ThreadID, ev.SessionID, ev.PermissionRequestID, fresh,
		string(counts), truncatePayload(ev.UsageJSON), ev.ResultStatus, nullString(ev.AgentID), nullString(ev.RunID), nullString(ev.RuntimeType),
	).Error
}

func InsertBuiltinUsageEvent(db *gorm.DB, ev usage.BuiltinUsageEvent) error {
	counts, _ := json.Marshal(ev.EventCounts)
	return db.Exec(usageInsertSQL("builtin_usage_events", builtinUsageInsertColumns),
		ev.EventID, ev.TraceID, ev.SpanID, ev.ParentSpanID, ev.AgentDepth, unixMillis(ev.StartedAt), unixMillis(ev.FinishedAt),
		ev.RouteID, ev.RouteKind, ev.RouteProtocol, ev.VirtualKeyID, boolInt(ev.Success), ev.StatusCode, ev.ErrorType, ev.LatencyMS,
		ev.Operation, ev.SessionID, nullString(ev.RunID), ev.PermissionRequestID, ev.LinkTraceID, ev.LinkSpanID,
		ev.TopologyKind, ev.ModelSteps, ev.ToolSteps,
		string(counts), ev.ResultStatus, nullString(ev.AgentID), nullString(ev.RuntimeType),
	).Error
}

func usageInsertSQL(table string, columns []string) string {
	return "INSERT INTO " + table + " (" + strings.Join(columns, ", ") + ") VALUES (" + placeholders(len(columns)) + ")"
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?, ", n), ", ")
}

func truncatePayload(s string) string {
	if len(s) <= maxUsagePayloadBytes {
		return s
	}
	return s[:maxUsagePayloadBytes]
}
