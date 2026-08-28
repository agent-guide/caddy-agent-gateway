// Package runtimetest provides reusable fake runtime backends for
// dispatcher and Admin contract tests.
package runtimetest

import (
	"context"
	"sync"

	"github.com/agent-guide/agent-gateway/pkg/agent"
	agentruntime "github.com/agent-guide/agent-gateway/pkg/agent/runtime"
)

type TurnCall struct {
	Agent   agent.Agent
	Request agentruntime.TurnRequest
}

// Backend implements only agentruntime.Backend. Tests that need optional
// capabilities can embed it in a small type implementing the relevant narrow
// interface, so unsupported capabilities remain truthful.
type Backend struct {
	Type string

	CapabilitiesResult agentruntime.Capabilities
	CapabilitiesError  error
	ServeTurnError     error
	ServeTurnFunc      func(context.Context, agent.Agent, agentruntime.TurnRequest, agentruntime.EventSink) error

	mu              sync.Mutex
	capabilityCalls []agent.Agent
	turnCalls       []TurnCall
}

func NewBackend(runtimeType string) *Backend {
	return &Backend{Type: runtimeType}
}

func (b *Backend) RuntimeType() string {
	return b.Type
}

func (b *Backend) Capabilities(_ context.Context, a agent.Agent) (agentruntime.Capabilities, error) {
	b.mu.Lock()
	b.capabilityCalls = append(b.capabilityCalls, a)
	result := b.CapabilitiesResult
	err := b.CapabilitiesError
	b.mu.Unlock()
	return result, err
}

// SetCapabilities safely replaces the fake result while contract tests are
// running concurrent calls.
func (b *Backend) SetCapabilities(result agentruntime.Capabilities, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.CapabilitiesResult = result
	b.CapabilitiesError = err
}

func (b *Backend) ServeTurn(ctx context.Context, a agent.Agent, req agentruntime.TurnRequest, emit agentruntime.EventSink) error {
	b.mu.Lock()
	b.turnCalls = append(b.turnCalls, TurnCall{Agent: a, Request: req})
	fn := b.ServeTurnFunc
	err := b.ServeTurnError
	b.mu.Unlock()
	if fn != nil {
		return fn(ctx, a, req, emit)
	}
	return err
}

func (b *Backend) CapabilityCalls() []agent.Agent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]agent.Agent(nil), b.capabilityCalls...)
}

func (b *Backend) TurnCalls() []TurnCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]TurnCall(nil), b.turnCalls...)
}
