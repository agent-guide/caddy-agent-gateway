package anthropic

import (
	"errors"
	"net/http"
	"sync"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
)

var errResponseLifecycleFinalized = errors.New("anthropic response lifecycle already finalized")

type usageObservation struct {
	InputTokens     int
	OutputTokens    int
	CachedTokens    int
	ReasoningTokens int
	Final           bool
}

type responseFinish struct {
	StatusCode int
	Outcome    string
}

type responseFailure struct {
	StatusCode int
	Outcome    string
	ErrorType  string
}

type responseLifecycle interface {
	Committed()
	ObserveUsage(usageObservation)
	Finish(responseFinish) error
	Fail(responseFailure) error
	Cancel(responseFailure) error
}

type spanResponseLifecycle struct {
	mu        sync.Mutex
	span      usage.InteractionSpan
	transport string
	committed bool
	finished  bool
	usage     *usageObservation
}

func newSpanResponseLifecycle(span usage.InteractionSpan, transport string) *spanResponseLifecycle {
	return &spanResponseLifecycle{span: span, transport: transport}
}

func (l *spanResponseLifecycle) Committed() {
	l.mu.Lock()
	l.committed = true
	l.mu.Unlock()
}

func (l *spanResponseLifecycle) ObserveUsage(observed usageObservation) {
	l.mu.Lock()
	copy := observed
	l.usage = &copy
	l.mu.Unlock()
}

func (l *spanResponseLifecycle) Finish(finish responseFinish) error {
	if finish.StatusCode == 0 {
		finish.StatusCode = http.StatusOK
	}
	return l.finalize(true, finish.StatusCode, "", finish.Outcome)
}

func (l *spanResponseLifecycle) Fail(failure responseFailure) error {
	if failure.StatusCode == 0 {
		failure.StatusCode = http.StatusBadGateway
	}
	return l.finalize(false, failure.StatusCode, failure.ErrorType, failure.Outcome)
}

func (l *spanResponseLifecycle) Cancel(failure responseFailure) error {
	if failure.StatusCode == 0 {
		failure.StatusCode = 499
	}
	if failure.ErrorType == "" {
		failure.ErrorType = "client_cancelled"
	}
	return l.finalize(false, failure.StatusCode, failure.ErrorType, failure.Outcome)
}

func (l *spanResponseLifecycle) finalize(success bool, status int, errorType, responseOutcome string) error {
	l.mu.Lock()
	if l.finished {
		l.mu.Unlock()
		return errResponseLifecycleFinalized
	}
	l.finished = true
	observed := l.usage
	committed := l.committed
	l.mu.Unlock()

	extension := usage.LLMExtension{
		Transport:         l.transport,
		ResponseOutcome:   responseOutcome,
		ResponseCommitted: usage.Bool(committed),
	}
	if observed != nil {
		total := observed.InputTokens + observed.OutputTokens
		extension.InputTokens = usage.Int(observed.InputTokens)
		extension.OutputTokens = usage.Int(observed.OutputTokens)
		extension.TotalTokens = usage.Int(total)
		extension.CachedTokens = usage.Int(observed.CachedTokens)
		extension.ReasoningTokens = usage.Int(observed.ReasoningTokens)
		extension.UsageFinalized = usage.Bool(observed.Final)
	}
	l.span.SetExtension(extension)
	l.span.Finish(usage.InteractionOutcome{Success: success, StatusCode: status, ErrorType: errorType})
	return nil
}
