package opencode

import (
	"context"
	"strings"

	baseacp "github.com/agent-guide/agent-gateway/pkg/acp"
	"github.com/agent-guide/agent-gateway/pkg/acp/agentspi"
	"github.com/agent-guide/agent-gateway/pkg/acp/transport"
)

// modelConfigOptionID is the config option id opencode advertises for model
// selection (verified against the real `opencode acp` session/new response).
const modelConfigOptionID = "model"

func init() {
	agentspi.Register(baseacp.AgentTypeOpencode, New)
}

type Agent struct {
	cwd string
	env map[string]string
}

func New(req agentspi.OpenRequest) (agentspi.Agent, error) {
	return &Agent{cwd: req.CWD, env: req.Service.Env}, nil
}

func (a *Agent) Name() string { return baseacp.AgentTypeOpencode }

func (a *Agent) Open(ctx context.Context, h transport.Handlers) (transport.Transport, error) {
	return transport.Open(ctx, transport.ProcessConfig{
		Command: "opencode",
		Args:    []string{"acp", "--cwd", a.cwd},
		Dir:     a.cwd,
		Env:     agentspi.MergeEnv(a.env),
	}, h)
}

func (a *Agent) InitializeParams() map[string]any {
	return map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{
				"readTextFile":  false,
				"writeTextFile": false,
			},
		},
	}
}

// SessionNewParams omits model selection: ACP defines no model field on
// session/new and no session/set_model method. Per-session model selection, if
// ever supported, must go through the SessionModelSelector capability.
func (a *Agent) SessionNewParams(string) map[string]any {
	return map[string]any{
		"cwd":        a.cwd,
		"mcpServers": []any{},
	}
}

func (a *Agent) SessionLoadParams(sessionID string) map[string]any {
	return map[string]any{
		"sessionId":  sessionID,
		"cwd":        a.cwd,
		"mcpServers": []any{},
	}
}

func (a *Agent) SessionListParams(cwd, cursor string) map[string]any {
	params := map[string]any{}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		params["cwd"] = cwd
	}
	if cursor = strings.TrimSpace(cursor); cursor != "" {
		params["cursor"] = cursor
	}
	return params
}

func (a *Agent) PromptParams(sessionID, input string, _ string) map[string]any {
	return map[string]any{
		"sessionId": sessionID,
		"prompt": []map[string]any{{
			"type": "text",
			"text": input,
		}},
	}
}

func (a *Agent) Cancel(_ context.Context, t transport.Transport, sessionID string) error {
	if t == nil || sessionID == "" {
		return nil
	}
	return t.Notify("session/cancel", map[string]any{"sessionId": sessionID})
}

// SelectSessionModel applies the model by setting opencode's "model" config
// option via session/set_config_option. This is the spec-blessed path (ACP has
// no session/set_model); verified against the real opencode binary.
func (a *Agent) SelectSessionModel(ctx context.Context, t transport.Transport, sessionID, modelID string, _ []agentspi.ConfigOption) ([]agentspi.ConfigOption, error) {
	if t == nil || strings.TrimSpace(modelID) == "" {
		return nil, nil
	}
	_, err := t.Request(ctx, "session/set_config_option", map[string]any{
		"sessionId": sessionID,
		"configId":  modelConfigOptionID,
		"value":     modelID,
	})
	return nil, err
}
