package codex

import (
	"context"
	"fmt"
	"strings"

	baseacp "github.com/agent-guide/agent-gateway/pkg/acp"
	"github.com/agent-guide/agent-gateway/pkg/acp/agentspi"
	"github.com/agent-guide/agent-gateway/pkg/acp/hostconfig"
	"github.com/agent-guide/agent-gateway/pkg/acp/transport"
)

const defaultAdapterCommand = "codex-acp"

func init() {
	agentspi.Register(baseacp.AgentTypeCodex, New)
}

type Agent struct {
	cwd     string
	command string
	args    []string
	env     map[string]string
}

func New(req agentspi.OpenRequest) (agentspi.Agent, error) {
	cfg := req.Config.Codex
	if cfg == nil {
		cfg = &hostconfig.CodexConfig{}
	}
	if strings.TrimSpace(cfg.Mode) != "" && strings.TrimSpace(cfg.Mode) != hostconfig.CodexModeAdapter {
		return nil, fmt.Errorf("codex acp mode %q is not implemented", cfg.Mode)
	}
	command := strings.TrimSpace(cfg.AdapterCommand)
	if command == "" {
		command = defaultAdapterCommand
	}
	return &Agent{
		cwd:     req.CWD,
		command: command,
		args:    append([]string(nil), cfg.AdapterArgs...),
		env:     req.Config.Env,
	}, nil
}

func (a *Agent) Name() string { return baseacp.AgentTypeCodex }

func (a *Agent) Open(ctx context.Context, h transport.Handlers) (transport.Transport, error) {
	return transport.Open(ctx, transport.ProcessConfig{
		Command: a.command,
		Args:    a.args,
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

// SessionNewParams omits model selection: ACP has no model field on
// session/new, so the codex model is the adapter's concern (profile/config),
// not a wire parameter the gateway can set generically.
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
