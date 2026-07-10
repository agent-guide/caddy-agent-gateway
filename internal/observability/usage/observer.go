package usage

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type SQLDBProvider interface {
	UsageDB() *gorm.DB
}

type InteractionObserver interface {
	Begin(ctx context.Context, dims InteractionDimensions) (InteractionSpan, context.Context)
}

type InteractionSpan interface {
	SetExtension(v any)
	AddAnnotation(key, value string)
	Finish(outcome InteractionOutcome)
}

type spanContextKey struct{}

func ContextWithSpan(ctx context.Context, span InteractionSpan) context.Context {
	if span == nil {
		return ctx
	}
	return context.WithValue(ctx, spanContextKey{}, span)
}

func SpanFromContext(ctx context.Context) InteractionSpan {
	if ctx == nil {
		return NoopSpan{}
	}
	span, _ := ctx.Value(spanContextKey{}).(InteractionSpan)
	if span == nil {
		return NoopSpan{}
	}
	return span
}

type Observer struct {
	sink        EventSink
	attribution *AgentAttribution
}

func NewObserver(sink EventSink) Observer {
	return Observer{sink: sink}
}

// NewObserverWithAttribution wires an attribution holder so the observer stamps
// agent_id at Begin from the route id when the caller did not set it.
func NewObserverWithAttribution(sink EventSink, attribution *AgentAttribution) Observer {
	return Observer{sink: sink, attribution: attribution}
}

func (o Observer) Begin(ctx context.Context, dims InteractionDimensions) (InteractionSpan, context.Context) {
	if dims.SpanID == "" {
		dims.SpanID = uuid.NewString()
	}
	agentID := dims.AgentID
	if agentID == "" && o.attribution != nil {
		if resolved, ok := o.attribution.ResolveAgentID(dims.RouteID, "", ""); ok {
			agentID = resolved
		}
	}
	eventID := uuid.NewString()
	span := &eventSpan{
		sink: o.sink,
		base: InteractionEvent{
			EventID:       eventID,
			TraceID:       dims.TraceID,
			SpanID:        dims.SpanID,
			ParentSpanID:  dims.ParentSpanID,
			AgentDepth:    dims.AgentDepth,
			StartedAt:     time.Now().UTC(),
			RouteID:       dims.RouteID,
			RouteKind:     dims.RouteKind,
			RouteProtocol: dims.RouteProtocol,
			VirtualKeyID:  dims.VirtualKeyID,
			AgentID:       agentID,
		},
	}
	return span, ContextWithSpan(ctx, span)
}

type eventSpan struct {
	mu          sync.Mutex
	sink        EventSink
	base        InteractionEvent
	llm         LLMExtension
	mcp         MCPExtension
	acp         ACPExtension
	annotations map[string]string
	finished    bool
}

func (s *eventSpan) SetExtension(v any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ext := v.(type) {
	case LLMExtension:
		mergeLLM(&s.llm, ext)
	case *LLMExtension:
		if ext != nil {
			mergeLLM(&s.llm, *ext)
		}
	case MCPExtension:
		mergeMCP(&s.mcp, ext)
	case *MCPExtension:
		if ext != nil {
			mergeMCP(&s.mcp, *ext)
		}
	case ACPExtension:
		mergeACP(&s.acp, ext)
	case *ACPExtension:
		if ext != nil {
			mergeACP(&s.acp, *ext)
		}
	}
}

func (s *eventSpan) AddAnnotation(key, value string) {
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.annotations == nil {
		s.annotations = map[string]string{}
	}
	s.annotations[key] = value
}

func (s *eventSpan) Finish(outcome InteractionOutcome) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	base := s.base
	if outcome.FinishedAt.IsZero() {
		base.FinishedAt = time.Now().UTC()
	} else {
		base.FinishedAt = outcome.FinishedAt.UTC()
	}
	base.Success = outcome.Success
	base.StatusCode = outcome.StatusCode
	base.ErrorType = outcome.ErrorType
	if base.ErrorType == "" && !base.Success && s.annotations != nil {
		base.ErrorType = s.annotations["error_type"]
	}
	if base.ErrorType == "" && !base.Success {
		base.ErrorType = "internal_error"
	}
	base.LatencyMS = base.FinishedAt.Sub(base.StartedAt).Milliseconds()
	llm := s.llm
	mcp := s.mcp
	acp := s.acp
	sink := s.sink
	s.mu.Unlock()

	if sink == nil {
		return
	}
	switch base.RouteKind {
	case "llm":
		sink.Enqueue(llmEvent(base, llm))
	case "mcp":
		sink.Enqueue(mcpEvent(base, mcp))
	case "acp":
		sink.Enqueue(acpEvent(base, acp))
	default:
		sink.Enqueue(base)
	}
}

func llmEvent(base InteractionEvent, ext LLMExtension) LLMUsageEvent {
	ev := LLMUsageEvent{InteractionEvent: base, UsageFinalized: true}
	ev.LLMAPI = ext.LLMAPI
	ev.APIOperation = ext.APIOperation
	ev.ProviderID = ext.ProviderID
	ev.ProviderType = ext.ProviderType
	ev.LogicalModel = ext.LogicalModel
	ev.UpstreamModel = ext.UpstreamModel
	ev.CredentialSource = ext.CredentialSource
	ev.CredentialID = ext.CredentialID
	if ext.Stream != nil {
		ev.Stream = *ext.Stream
	}
	if ext.InputTokens != nil {
		ev.InputTokens = *ext.InputTokens
	}
	if ext.OutputTokens != nil {
		ev.OutputTokens = *ext.OutputTokens
	}
	if ext.TotalTokens != nil {
		ev.TotalTokens = *ext.TotalTokens
	}
	if ext.UsageFinalized != nil {
		ev.UsageFinalized = *ext.UsageFinalized
	}
	if ext.RequestToolCount != nil {
		ev.RequestToolCount = *ext.RequestToolCount
	}
	ev.RequestToolNames = append([]string(nil), ext.RequestToolNames...)
	if ext.ToolCallCount != nil {
		ev.ToolCallCount = *ext.ToolCallCount
	}
	ev.ToolNames = append([]string(nil), ext.ToolNames...)
	return ev
}

func mcpEvent(base InteractionEvent, ext MCPExtension) MCPUsageEvent {
	ev := MCPUsageEvent{InteractionEvent: base}
	ev.RequestID = ext.RequestID
	ev.ServiceID = ext.ServiceID
	ev.Method = ext.Method
	ev.ToolName = ext.ToolName
	ev.PresentedToolName = ext.PresentedToolName
	ev.ExecutedToolName = ext.ExecutedToolName
	ev.ExecutionMode = ext.ExecutionMode
	ev.PolicyAction = ext.PolicyAction
	ev.ResourceURI = ext.ResourceURI
	ev.PromptName = ext.PromptName
	ev.CompletionRefType = ext.CompletionRefType
	ev.CompletionArgument = ext.CompletionArgument
	if ext.ArgCount != nil {
		ev.ArgCount = *ext.ArgCount
	}
	ev.ResultStatus = ext.ResultStatus
	if ext.Cancelled != nil {
		ev.Cancelled = *ext.Cancelled
	}
	ev.ToolArgsJSON = ext.ToolArgsJSON
	return ev
}

func acpEvent(base InteractionEvent, ext ACPExtension) ACPUsageEvent {
	ev := ACPUsageEvent{InteractionEvent: base}
	ev.ServiceID = ext.ServiceID
	ev.AgentType = ext.AgentType
	ev.Operation = ext.Operation
	ev.ThreadID = ext.ThreadID
	ev.SessionID = ext.SessionID
	ev.PermissionRequestID = ext.PermissionRequestID
	ev.FreshSession = ext.FreshSession
	ev.EventCounts = ext.EventCounts
	ev.UsageJSON = ext.UsageJSON
	ev.ResultStatus = ext.ResultStatus
	return ev
}
