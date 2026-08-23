// Package cc provides the Claude Code CLI LLM API profile.
package cc

import (
	"net/http"

	"github.com/agent-guide/agent-gateway/pkg/dispatcher"
	"github.com/agent-guide/agent-gateway/pkg/dispatcher/llmapi/anthropicmsg"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"go.uber.org/zap"
)

func init() {
	dispatcher.RegisterLLMApiHandlerType("cc")
}

// Handler handles Claude Code CLI requests. The wire format is Anthropic
// Messages API compatible, with the token-counting shim Claude Code expects.
type Handler struct {
	core *anthropicmsg.Handler
}

// NewHandler creates a Claude Code CLI handler.
func NewHandler(_ provider.Provider) *Handler {
	return &Handler{core: anthropicmsg.NewHandler(anthropicmsg.ClaudeCodeProfile())}
}

func (h *Handler) Name() string { return h.core.Name() }

func (h *Handler) SetLogger(logger *zap.Logger) { h.core.SetLogger(logger) }

func (h *Handler) MatchLLMApi(r *http.Request) bool { return h.core.MatchLLMApi(r) }

func (h *Handler) PrepareLLMApiRequest(r *http.Request) (*dispatcher.PreparedLLMApiRequest, llmroutepkg.RequestRequirements, error) {
	return h.core.PrepareLLMApiRequest(r)
}

func (h *Handler) ServeLLMApi(w http.ResponseWriter, r *http.Request, prov provider.Provider, prepared *dispatcher.PreparedLLMApiRequest) error {
	return h.core.ServeLLMApi(w, r, prov, prepared)
}
