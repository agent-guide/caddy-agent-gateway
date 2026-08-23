package main

import (
	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	// Standard Caddy modules
	_ "github.com/caddyserver/caddy/v2/modules/standard"

	// LLM Gateway modules
	_ "github.com/agent-guide/agent-gateway/caddy/admin"
	_ "github.com/agent-guide/agent-gateway/caddy/configstore/sqlite"
	_ "github.com/agent-guide/agent-gateway/caddy/dispatcher"
	_ "github.com/agent-guide/agent-gateway/caddy/dispatcher/llmapi/anthropic"
	_ "github.com/agent-guide/agent-gateway/caddy/dispatcher/llmapi/cc"
	_ "github.com/agent-guide/agent-gateway/caddy/dispatcher/llmapi/openai"
	_ "github.com/agent-guide/agent-gateway/caddy/gateway"

	// ACP agents register runtime factories through init.
	_ "github.com/agent-guide/agent-gateway/pkg/acp/agent/codex"
	_ "github.com/agent-guide/agent-gateway/pkg/acp/agent/opencode"

	// LLM Providers (register runtime factories via init())
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropic"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropicbase"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/claudecode"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/codex"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/deepseek"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/gemini"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/ollama"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/openai"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/openrouter"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/qwen"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/zhipu"
)

func main() {
	caddycmd.Main()
}
