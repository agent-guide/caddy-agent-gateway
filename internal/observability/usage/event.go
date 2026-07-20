package usage

import "time"

// InteractionEvent captures fields common to every gateway interaction.
type InteractionEvent struct {
	EventID       string
	TraceID       string
	SpanID        string
	ParentSpanID  string
	AgentDepth    int
	StartedAt     time.Time
	FinishedAt    time.Time
	RouteID       string
	RouteKind     string
	RouteProtocol string
	VirtualKeyID  string
	Success       bool
	StatusCode    int
	ErrorType     string
	LatencyMS     int64
	// AgentID is the write-time attribution tag. It is empty for non-agent
	// traffic and for events whose route/service maps to zero or more than one
	// agent (stamp only when unambiguous).
	AgentID string
}

type LLMUsageEvent struct {
	InteractionEvent
	LLMAPI           string
	APIOperation     string
	ProviderID       string
	ProviderType     string
	LogicalModel     string
	UpstreamModel    string
	CredentialSource string
	CredentialID     string
	Stream           bool
	InputTokens      int
	OutputTokens     int
	TotalTokens      int
	// CachedTokens is the cache-served subset of InputTokens; ReasoningTokens
	// is the reasoning subset of OutputTokens. Zero when the upstream does not
	// report the breakdown.
	CachedTokens     int
	ReasoningTokens  int
	UsageFinalized   bool
	RequestToolCount int
	RequestToolNames []string
	ToolCallCount    int
	ToolNames        []string
}

type MCPUsageEvent struct {
	InteractionEvent
	RequestID          string
	ServiceID          string
	Method             string
	ToolName           string
	PresentedToolName  string
	ExecutedToolName   string
	ExecutionMode      string
	PolicyAction       string
	ResourceURI        string
	PromptName         string
	CompletionRefType  string
	CompletionArgument string
	ArgCount           int
	ResultStatus       string
	Cancelled          bool
	ToolArgsJSON       string
}

type ACPUsageEvent struct {
	InteractionEvent
	ServiceID           string
	AgentType           string
	Operation           string
	ThreadID            string
	SessionID           string
	PermissionRequestID string
	FreshSession        *bool
	EventCounts         map[string]int
	UsageJSON           string
	ResultStatus        string
}

// BuiltinUsageEvent captures one builtin-agent turn served by the in-process
// ADK host. It deliberately does not reuse the ACP family: builtin turns have
// no backing service, use checkpoint-based permission continuation rather
// than ACP's side channel, and carry topology step counts instead.
type BuiltinUsageEvent struct {
	InteractionEvent
	Operation           string
	SessionID           string
	RunID               string
	PermissionRequestID string
	// LinkTraceID/LinkSpanID identify the earlier asynchronous segment of the
	// same run. The OTLP adapter reconstructs them as an OpenTelemetry Span
	// Link instead of pretending a resumed HITL turn is a synchronous child.
	LinkTraceID  string
	LinkSpanID   string
	TopologyKind string
	// ModelSteps counts assistant model outputs and ToolSteps counts tool
	// executions observed during the turn.
	ModelSteps   int
	ToolSteps    int
	EventCounts  map[string]int
	ResultStatus string
}

type LLMExtension struct {
	LLMAPI           string
	APIOperation     string
	ProviderID       string
	ProviderType     string
	LogicalModel     string
	UpstreamModel    string
	CredentialSource string
	CredentialID     string
	Stream           *bool
	InputTokens      *int
	OutputTokens     *int
	TotalTokens      *int
	CachedTokens     *int
	ReasoningTokens  *int
	UsageFinalized   *bool
	RequestToolCount *int
	RequestToolNames []string
	ToolCallCount    *int
	ToolNames        []string
}

type MCPExtension struct {
	RequestID          string
	ServiceID          string
	Method             string
	ToolName           string
	PresentedToolName  string
	ExecutedToolName   string
	ExecutionMode      string
	PolicyAction       string
	ResourceURI        string
	PromptName         string
	CompletionRefType  string
	CompletionArgument string
	ArgCount           *int
	ResultStatus       string
	Cancelled          *bool
	ToolArgsJSON       string
}

type ACPExtension struct {
	ServiceID           string
	AgentType           string
	Operation           string
	ThreadID            string
	SessionID           string
	PermissionRequestID string
	FreshSession        *bool
	EventCounts         map[string]int
	UsageJSON           string
	ResultStatus        string
}

type BuiltinExtension struct {
	Operation           string
	SessionID           string
	RunID               string
	PermissionRequestID string
	LinkTraceID         string
	LinkSpanID          string
	TopologyKind        string
	ModelSteps          *int
	ToolSteps           *int
	EventCounts         map[string]int
	ResultStatus        string
}

type InteractionDimensions struct {
	TraceID       string
	SpanID        string
	ParentSpanID  string
	AgentDepth    int
	RouteID       string
	RouteKind     string
	RouteProtocol string
	VirtualKeyID  string
	// AgentID, when set by the caller, is stamped verbatim. When empty the
	// observer attempts attribution from RouteID via the wired AgentAttributor.
	AgentID string
}

type InteractionOutcome struct {
	Success    bool
	StatusCode int
	ErrorType  string
	FinishedAt time.Time
}

type EventSink interface {
	Enqueue(any) bool
}
