// Package einomodel presents a gateway provider as an eino
// model.ToolCallingChatModel, so eino agents, ADK runners, and compose graphs
// can consume gateway-routed models directly. Wrap a *gateway.RoutedProvider
// (obtained from AgentGateway.NewRoutedProvider) rather than a base provider
// whenever possible: credential scheduling, candidate fallback, and usage
// attribution then apply unchanged to in-process callers.
//
// This is the single generic adapter — providers are not rewritten
// per-component (see docs/design/eino-reuse.md §5.3).
package einomodel

import (
	"context"
	"fmt"
	"strings"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

// ChatModel adapts a provider.Provider to model.ToolCallingChatModel. The
// zero value is not usable; construct with New.
type ChatModel struct {
	provider     provider.Provider
	defaultModel string
	// boundOpts carries options baked in by WithTools; per-call options are
	// appended after them so the call site wins on conflicts.
	boundOpts []einomodel.Option
}

var _ einomodel.ToolCallingChatModel = (*ChatModel)(nil)

// New wraps a gateway provider as an eino chat model. defaultModel is used
// when neither the bound nor the per-call options carry a model name; for a
// RoutedProvider it is the route target name (logical model in model-target
// routes, upstream model in direct-provider routes).
func New(p provider.Provider, defaultModel string) (*ChatModel, error) {
	if p == nil {
		return nil, fmt.Errorf("einomodel: provider is nil")
	}
	return &ChatModel{provider: p, defaultModel: strings.TrimSpace(defaultModel)}, nil
}

func (m *ChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	resp, err := m.provider.Chat(ctx, m.chatRequest(input, opts))
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Message == nil {
		return nil, fmt.Errorf("einomodel: provider returned an empty response")
	}
	return resp.Message, nil
}

func (m *ChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	return m.provider.StreamChat(ctx, m.chatRequest(input, opts))
}

// WithTools returns a copy of the model with the tools bound. The receiver is
// not mutated, matching the eino contract that WithTools yields an
// independent instance.
func (m *ChatModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	if len(tools) == 0 {
		return nil, fmt.Errorf("einomodel: no tools to bind")
	}
	for _, t := range tools {
		if t == nil || strings.TrimSpace(t.Name) == "" {
			return nil, fmt.Errorf("einomodel: tool name cannot be empty")
		}
	}
	bound := make([]einomodel.Option, 0, len(m.boundOpts)+1)
	bound = append(bound, m.boundOpts...)
	bound = append(bound, einomodel.WithTools(tools))
	return &ChatModel{
		provider:     m.provider,
		defaultModel: m.defaultModel,
		boundOpts:    bound,
	}, nil
}

func (m *ChatModel) chatRequest(input []*schema.Message, opts []einomodel.Option) *provider.ChatRequest {
	merged := make([]einomodel.Option, 0, len(m.boundOpts)+len(opts))
	merged = append(merged, m.boundOpts...)
	merged = append(merged, opts...)
	modelName := m.defaultModel
	if common := einomodel.GetCommonOptions(nil, merged...); common != nil && common.Model != nil && strings.TrimSpace(*common.Model) != "" {
		modelName = strings.TrimSpace(*common.Model)
	}
	return &provider.ChatRequest{
		Model:    modelName,
		Messages: input,
		Options:  merged,
	}
}
