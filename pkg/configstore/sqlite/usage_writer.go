package sqlite

import (
	"encoding/json"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"gorm.io/gorm"
)

// maxUsagePayloadBytes caps free-form JSON payloads (MCP tool args, ACP usage
// blobs) persisted with a usage event. Raw payloads may carry caller-provided
// arguments that include secrets or PII, so they are truncated before storage.
const maxUsagePayloadBytes = 4096

func InsertLLMUsageEvent(db *gorm.DB, ev usage.LLMUsageEvent) error {
	names, _ := json.Marshal(ev.RequestToolNames)
	toolNames, _ := json.Marshal(ev.ToolNames)
	return db.Exec(`INSERT INTO llm_usage_events (
		event_id, trace_id, span_id, parent_span_id, agent_depth, started_at, finished_at,
		route_id, route_kind, route_protocol, virtual_key_id, success, status_code, error_type, latency_ms,
		llm_api, api_operation, provider_id, provider_type, logical_model, upstream_model,
		credential_source, credential_id, stream, input_tokens, output_tokens, total_tokens,
		usage_finalized, request_tool_count, request_tool_names, tool_call_count, tool_names, agent_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.EventID, ev.TraceID, ev.SpanID, ev.ParentSpanID, ev.AgentDepth, unixMillis(ev.StartedAt), unixMillis(ev.FinishedAt),
		ev.RouteID, ev.RouteKind, ev.RouteProtocol, ev.VirtualKeyID, boolInt(ev.Success), ev.StatusCode, ev.ErrorType, ev.LatencyMS,
		ev.LLMAPI, ev.APIOperation, ev.ProviderID, ev.ProviderType, ev.LogicalModel, ev.UpstreamModel,
		ev.CredentialSource, ev.CredentialID, boolInt(ev.Stream), ev.InputTokens, ev.OutputTokens, ev.TotalTokens,
		boolInt(ev.UsageFinalized), ev.RequestToolCount, string(names), ev.ToolCallCount, string(toolNames), nullString(ev.AgentID),
	).Error
}

func InsertMCPUsageEvent(db *gorm.DB, ev usage.MCPUsageEvent) error {
	return db.Exec(`INSERT INTO mcp_usage_events (
		event_id, trace_id, span_id, parent_span_id, agent_depth, started_at, finished_at,
		route_id, route_kind, route_protocol, virtual_key_id, success, status_code, error_type, latency_ms,
		request_id, service_id, method, tool_name, presented_tool_name, executed_tool_name,
		execution_mode, policy_action, resource_uri, prompt_name, completion_ref_type, completion_argument,
		arg_count, result_status, cancelled, tool_args_json, agent_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.EventID, ev.TraceID, ev.SpanID, ev.ParentSpanID, ev.AgentDepth, unixMillis(ev.StartedAt), unixMillis(ev.FinishedAt),
		ev.RouteID, ev.RouteKind, ev.RouteProtocol, ev.VirtualKeyID, boolInt(ev.Success), ev.StatusCode, ev.ErrorType, ev.LatencyMS,
		ev.RequestID, ev.ServiceID, ev.Method, ev.ToolName, ev.PresentedToolName, ev.ExecutedToolName,
		ev.ExecutionMode, ev.PolicyAction, ev.ResourceURI, ev.PromptName, ev.CompletionRefType, ev.CompletionArgument,
		ev.ArgCount, ev.ResultStatus, boolInt(ev.Cancelled), truncatePayload(ev.ToolArgsJSON), nullString(ev.AgentID),
	).Error
}

func InsertACPUsageEvent(db *gorm.DB, ev usage.ACPUsageEvent) error {
	counts, _ := json.Marshal(ev.EventCounts)
	var fresh any
	if ev.FreshSession != nil {
		fresh = boolInt(*ev.FreshSession)
	}
	return db.Exec(`INSERT INTO acp_usage_events (
		event_id, trace_id, span_id, parent_span_id, agent_depth, started_at, finished_at,
		route_id, route_kind, route_protocol, virtual_key_id, success, status_code, error_type, latency_ms,
		service_id, agent_type, operation, thread_id, session_id, permission_request_id, fresh_session,
		event_counts_json, usage_json, result_status, agent_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.EventID, ev.TraceID, ev.SpanID, ev.ParentSpanID, ev.AgentDepth, unixMillis(ev.StartedAt), unixMillis(ev.FinishedAt),
		ev.RouteID, ev.RouteKind, ev.RouteProtocol, ev.VirtualKeyID, boolInt(ev.Success), ev.StatusCode, ev.ErrorType, ev.LatencyMS,
		ev.ServiceID, ev.AgentType, ev.Operation, ev.ThreadID, ev.SessionID, ev.PermissionRequestID, fresh,
		string(counts), truncatePayload(ev.UsageJSON), ev.ResultStatus, nullString(ev.AgentID),
	).Error
}

func truncatePayload(s string) string {
	if len(s) <= maxUsagePayloadBytes {
		return s
	}
	return s[:maxUsagePayloadBytes]
}
