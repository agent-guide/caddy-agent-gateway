package provider

import (
	"context"

	"github.com/cloudwego/eino/schema"
)

type ServedCandidate struct {
	Dialect       ProtocolDialect
	ProviderType  string
	ProviderID    string
	LogicalModel  string
	ClientModel   string
	UpstreamModel string
	Features      map[ProtocolFeature]struct{}
}

func (c ServedCandidate) Supports(feature ProtocolFeature) bool {
	_, ok := c.Features[feature]
	return ok
}

type AttemptAttribution struct {
	CredentialID     string
	CredentialSource string
}

type ResolvedExecution struct {
	Candidate   ServedCandidate
	Attribution AttemptAttribution
}

type ChatExecution struct {
	Response *ChatResponse
	Resolved ResolvedExecution
}

type StreamExecution struct {
	Stream   *schema.StreamReader[*schema.Message]
	Resolved ResolvedExecution
}

type RoutedChatExecutor interface {
	ExecuteChat(context.Context, *ChatRequest) (*ChatExecution, error)
	ExecuteStreamChat(context.Context, *ChatRequest) (*StreamExecution, error)
}
