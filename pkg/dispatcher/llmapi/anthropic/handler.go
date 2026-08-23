// Package anthropic provides the standard Anthropic Messages HTTP profile.
package anthropic

import (
	"net/http"

	"github.com/agent-guide/agent-gateway/pkg/dispatcher"
	"github.com/agent-guide/agent-gateway/pkg/dispatcher/llmapi/anthropicmsg"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"go.uber.org/zap"
)

func init() {
	dispatcher.RegisterLLMApiHandlerType("anthropic")
}

// Handler is the standard Anthropic ingress profile over the shared Messages
// protocol core.
type Handler struct {
	core *anthropicmsg.Handler
}

func NewHandler(_ provider.Provider) *Handler {
	return &Handler{core: anthropicmsg.NewHandler(anthropicmsg.StandardProfile())}
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
