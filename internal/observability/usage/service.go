package usage

import "sync/atomic"

type Config struct {
	RetentionDays int `json:"retention_days,omitempty"`
	MaxAgentDepth int `json:"max_agent_depth,omitempty"`
}

func (c Config) Normalized() Config {
	if c.RetentionDays < 0 {
		c.RetentionDays = 0
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = 30
	}
	if c.MaxAgentDepth < 0 {
		c.MaxAgentDepth = 0
	}
	return c
}

type Pipeline interface {
	EventSink
	Start()
	Close() error
	DroppedEvents() uint64
	WriteFailures() uint64
}

// RuntimeStats exposes pipeline health counters for the metrics Admin API so
// silently dropped or failed usage writes remain observable.
type RuntimeStats interface {
	DroppedEvents() uint64
	WriteFailures() uint64
}

type QueryService interface {
	Summary() (Summary, error)
	ListEvents(kind string, opts EventListOptions) (EventListResponse, error)
	ListInteractions(opts EventListOptions) (EventListResponse, error)
	LLMTimeseries(opts TimeseriesOptions) (SeriesResponse, error)
	LLMBreakdown(opts BreakdownOptions) (BreakdownResponse, error)
	MCPTimeseries(opts TimeseriesOptions) (SeriesResponse, error)
	MCPBreakdown(opts BreakdownOptions) (BreakdownResponse, error)
	MCPToolsSummary(opts SummaryOptions) (BreakdownResponse, error)
	ACPTimeseries(opts TimeseriesOptions) (SeriesResponse, error)
	ACPBreakdown(opts BreakdownOptions) (BreakdownResponse, error)
	ACPSummary(opts BreakdownOptions) (BreakdownResponse, error)
	InteractionsSummary(opts BreakdownOptions) (BreakdownResponse, error)
}

type UsageService struct {
	pipeline    Pipeline
	query       QueryService
	observer    InteractionObserver
	prom        PrometheusProvider
	attribution *AgentAttribution
}

func NewUsageService(pipeline Pipeline, query QueryService) *UsageService {
	attribution := NewAgentAttribution()
	svc := &UsageService{pipeline: pipeline, query: query, attribution: attribution}
	if pipeline != nil {
		svc.observer = NewObserverWithAttribution(pipeline, attribution)
	} else {
		svc.observer = NoopObserver{}
	}
	return svc
}

// Attribution returns the settable agent attribution holder. Callers inject the
// concrete attributor (the agent manager) after the gateway is bootstrapped, so
// the observer can stamp agent_id at write time.
func (s *UsageService) Attribution() *AgentAttribution {
	if s == nil {
		return nil
	}
	return s.attribution
}

func (s *UsageService) Observer() InteractionObserver {
	if s == nil || s.observer == nil {
		return NoopObserver{}
	}
	return s.observer
}

func (s *UsageService) Start() {
	if s != nil && s.pipeline != nil {
		s.pipeline.Start()
	}
}

func (s *UsageService) Close() error {
	if s == nil || s.pipeline == nil {
		return nil
	}
	return s.pipeline.Close()
}

func (s *UsageService) Query() QueryService {
	if s == nil {
		return nil
	}
	return s.query
}

// AttachPrometheus registers the in-process Prometheus snapshot provider so the
// Admin API can expose an O(1) /metrics scrape.
func (s *UsageService) AttachPrometheus(p PrometheusProvider) {
	if s != nil {
		s.prom = p
	}
}

func (s *UsageService) Prometheus() PrometheusProvider {
	if s == nil {
		return nil
	}
	return s.prom
}

func (s *UsageService) DroppedEvents() uint64 {
	if s == nil || s.pipeline == nil {
		return 0
	}
	return s.pipeline.DroppedEvents()
}

func (s *UsageService) WriteFailures() uint64 {
	if s == nil || s.pipeline == nil {
		return 0
	}
	return s.pipeline.WriteFailures()
}

type InMemorySink struct {
	Dropped atomic.Uint64
	Events  []any
}

func (s *InMemorySink) Enqueue(v any) bool {
	if s == nil {
		return false
	}
	s.Events = append(s.Events, v)
	return true
}

type Summary struct {
	LLM LLMSummary `json:"llm"`
	MCP MCPSummary `json:"mcp"`
	ACP ACPSummary `json:"acp"`
}

type EventListOptions struct {
	From        string
	To          string
	Limit       int
	Filters     map[string]string
	Success     *bool
	Attribution *AttributionFilter
}

type EventListResponse struct {
	Items []map[string]any `json:"items"`
	Limit int              `json:"limit"`
}

type TimeseriesOptions struct {
	From        string
	To          string
	Bucket      string
	GroupBy     string
	Filters     map[string]string
	Attribution *AttributionFilter
}

type SeriesResponse struct {
	Bucket  string           `json:"bucket"`
	GroupBy string           `json:"group_by,omitempty"`
	Items   []map[string]any `json:"items"`
}

type BreakdownOptions struct {
	From        string
	To          string
	GroupBy     string
	OrderBy     string
	Limit       int
	Filters     map[string]string
	Attribution *AttributionFilter
}

type SummaryOptions struct {
	From        string
	To          string
	Limit       int
	Filters     map[string]string
	Attribution *AttributionFilter
}

type BreakdownResponse struct {
	GroupBy string           `json:"group_by,omitempty"`
	Items   []map[string]any `json:"items"`
	Limit   int              `json:"limit"`
}

type LLMSummary struct {
	RequestCount int64 `json:"request_count"`
	SuccessCount int64 `json:"success_count"`
	FailureCount int64 `json:"failure_count"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	AvgLatencyMS int64 `json:"avg_latency_ms"`
}

type MCPSummary struct {
	RequestCount   int64 `json:"request_count"`
	SuccessCount   int64 `json:"success_count"`
	FailureCount   int64 `json:"failure_count"`
	ToolsCallCount int64 `json:"tools_call_count"`
	AvgLatencyMS   int64 `json:"avg_latency_ms"`
}

type ACPSummary struct {
	RequestCount int64 `json:"request_count"`
	TurnCount    int64 `json:"turn_count"`
	SuccessCount int64 `json:"success_count"`
	FailureCount int64 `json:"failure_count"`
	AvgLatencyMS int64 `json:"avg_latency_ms"`
}
