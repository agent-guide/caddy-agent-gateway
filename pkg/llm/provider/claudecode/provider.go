package claudecode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"

	"github.com/agent-guide/agent-gateway/internal/statuserr"
	"github.com/agent-guide/agent-gateway/pkg/credential"
	"github.com/agent-guide/agent-gateway/pkg/httpclient"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropicbase"
)

const (
	defaultBaseURL             = "https://api.anthropic.com"
	anthropicVersion           = "2023-06-01"
	anthropicBeta              = "claude-code-20250219,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,advisor-tool-2026-03-01,effort-2025-11-24"
	oauthTokenPrefix           = "sk-ant-oat-"
	apiKeyHeaderAuthorization  = "authorization"
	apiKeyHeaderXAPIKey        = "x-api-key"
	defaultClaudeCodeMaxTokens = 32000
	defaultClaudeCodeEffort    = "high"
)

// claudeCodeSystemPreamble is the exact first system block the standard Claude
// Code CLI sends. Relays that gate on a "standard Claude Code client" match this
// string verbatim, so it must stay byte-for-byte identical to the real CLI.
const claudeCodeSystemPreamble = "You are Claude Code, Anthropic's official CLI for Claude."

// defaultClaudeCodeFingerprintHeaders is the full client-fingerprint header set
// the standard Claude Code CLI sends. setHeaders applies it as defaults before
// per-provider ExtraHeaders, so a provider config that omits or only partially
// specifies the fingerprint still presents as the standard CLI instead of
// leaking Go's default User-Agent. ExtraHeaders override any entry here.
//
// Version-coupled values (User-Agent, X-Stainless-Package-Version,
// anthropic-version, anthropic-beta) are the parts an operator overrides via
// ExtraHeaders to match a newer CLI release. Keeping them centralized here means
// the canonical "what does Claude Code look like" knowledge lives in one place.
var defaultClaudeCodeFingerprintHeaders = map[string]string{
	"User-Agent":                                "claude-cli/2.1.158 (external, cli)",
	"X-App":                                     "cli",
	"X-Stainless-Arch":                          "arm64",
	"X-Stainless-Lang":                          "js",
	"X-Stainless-Os":                            "MacOS",
	"X-Stainless-Package-Version":               "0.94.0",
	"X-Stainless-Retry-Count":                   "0",
	"X-Stainless-Runtime":                       "node",
	"X-Stainless-Runtime-Version":               "v24.3.0",
	"X-Stainless-Timeout":                       "3000",
	"Anthropic-Dangerous-Direct-Browser-Access": "true",
	"anthropic-version":                         anthropicVersion,
	"anthropic-beta":                            anthropicBeta,
}

func init() {
	provider.RegisterProviderFactory("claudecode", New)
}

type Provider struct {
	provider.ProviderConfig
	client      *http.Client
	codexCompat bool
}

func New(config provider.ProviderConfig) (provider.Provider, error) {
	if config.BaseURL == "" {
		config.BaseURL = defaultBaseURL
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	config.Network.Defaults()

	if _, err := apiKeyHeaderFromOptions(config.Options); err != nil {
		return nil, err
	}
	compactMode, err := provider.CompactModeFromOptions(config.Options)
	if err != nil {
		return nil, err
	}

	return &Provider{
		ProviderConfig: config,
		client:         httpclient.BuildHTTPClient(config.Network),
		codexCompat:    compactMode == provider.CompactModeCodex,
	}, nil
}

func (p *Provider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	resolved, err := provider.ResolveChatRequest(ctx, p.ProviderConfig, req)
	if err != nil {
		return nil, err
	}
	ctx = provider.OnChatStart(ctx, "claudecode", resolved.ModelName, resolved.Messages)
	resp, err := p.chat(ctx, req)
	if err != nil {
		provider.OnChatError(ctx, err)
		return nil, err
	}
	provider.OnChatEnd(ctx, resolved.ModelName, resp.Message)
	return resp, nil
}

func (p *Provider) chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	return provider.RetryProviderCall(p.ProviderConfig.Network, func() (*provider.ChatResponse, error) {
		state, err := provider.ResolveChatRequest(ctx, p.ProviderConfig, req)
		if err != nil {
			return nil, err
		}

		httpReq, toolNames, err := p.newMessagesRequest(ctx, state, false)
		if err != nil {
			return nil, err
		}

		resp, err := p.client.Do(httpReq)
		if err != nil {
			return nil, statuserr.Wrap(fmt.Errorf("claudecode: request failed: %w", err), http.StatusBadGateway)
		}
		defer resp.Body.Close()

		if err := provider.CheckResponse(resp); err != nil {
			return nil, statuserr.Wrap(err, http.StatusBadGateway)
		}

		var payload anthropicbase.MessagesResponse
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			return nil, statuserr.Wrap(fmt.Errorf("claudecode: decode response: %w", err), http.StatusBadGateway)
		}
		chatResp := payload.ToChatResponse()
		restoreClaudeCodeToolNames(chatResp.Message, toolNames)
		return chatResp, nil
	})
}

func (p *Provider) StreamChat(ctx context.Context, req *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	state, err := provider.ResolveChatRequest(ctx, p.ProviderConfig, req)
	if err != nil {
		return nil, err
	}
	ctx = provider.OnChatStart(ctx, "claudecode", state.ModelName, state.Messages)

	httpReq, toolNames, err := p.newMessagesRequest(ctx, state, true)
	if err != nil {
		provider.OnChatError(ctx, err)
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		err = statuserr.Wrap(fmt.Errorf("claudecode: stream request failed: %w", err), http.StatusBadGateway)
		provider.OnChatError(ctx, err)
		return nil, err
	}
	if err := provider.CheckResponse(resp); err != nil {
		resp.Body.Close()
		err = statuserr.Wrap(err, http.StatusBadGateway)
		provider.OnChatError(ctx, err)
		return nil, err
	}

	sr, sw := schema.Pipe[*schema.Message](16)
	go func() {
		upstream, upstreamWriter := schema.Pipe[*schema.Message](16)
		go anthropicbase.ReadMessageStream(resp.Body, upstreamWriter, "claudecode")
		defer sw.Close()
		defer upstream.Close()
		for {
			msg, err := upstream.Recv()
			if err != nil {
				if err != io.EOF {
					sw.Send(nil, err)
				}
				return
			}
			restoreClaudeCodeToolNames(msg, toolNames)
			sw.Send(msg, nil)
		}
	}()
	return provider.OnChatStreamEnd(ctx, state.ModelName, sr), nil
}

func (p *Provider) CreateResponses(ctx context.Context, req *provider.ResponsesRequest) (*provider.ResponsesResponse, error) {
	return provider.CreateResponsesViaChat(ctx, p, req)
}

func (p *Provider) StreamResponses(ctx context.Context, req *provider.ResponsesRequest) (*schema.StreamReader[*provider.ResponsesStreamEvent], error) {
	return provider.StreamResponsesViaChat(ctx, p, req)
}

func (p *Provider) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.ProviderConfig.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("claudecode: build request: %w", err)
	}
	if err := p.setHeaders(ctx, httpReq, newRequestSession()); err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("claudecode: request failed: %w", err)
	}
	defer resp.Body.Close()

	if err := provider.CheckResponse(resp); err != nil {
		return nil, err
	}

	var modelsResp struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		return nil, fmt.Errorf("claudecode: decode models: %w", err)
	}

	out := make([]provider.ModelInfo, len(modelsResp.Data))
	for i, m := range modelsResp.Data {
		out[i] = provider.ModelInfo{
			ID:           m.ID,
			Name:         m.DisplayName,
			DisplayName:  m.DisplayName,
			Capabilities: provider.ModelCapabilitiesFromProviderSummary(p.Capabilities()),
		}
	}
	return out, nil
}

func (p *Provider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{
		Streaming:       true,
		Tools:           true,
		Vision:          true,
		ContextWindow:   200000,
		MaxOutputTokens: 8192,
	}
}

func (p *Provider) Config() provider.ProviderConfig {
	return p.ProviderConfig
}

// newMessagesRequest builds the upstream HTTP request and, when compact=codex is
// enabled, rewrites Codex tool names to their Claude Code equivalents on the
// freshly built wire request. It returns the reverse map used to restore the
// original names on the response. The shared ChatRequestState is never mutated,
// so the rewrite stays idempotent across retries.
func (p *Provider) newMessagesRequest(ctx context.Context, state *provider.ChatRequestState, stream bool) (*http.Request, map[string]string, error) {
	session := newRequestSession()
	msgReq := buildMessagesRequest(state, stream, session)

	var toolNames map[string]string
	if p.codexCompat {
		var err error
		toolNames, err = applyCodexToolNameAliases(msgReq)
		if err != nil {
			return nil, nil, err
		}
	}

	body, err := json.Marshal(msgReq)
	if err != nil {
		return nil, nil, fmt.Errorf("claudecode: marshal request: %w", err)
	}

	url := p.ProviderConfig.BaseURL + "/v1/messages?beta=true"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("claudecode: build request: %w", err)
	}
	if err := p.setHeaders(ctx, httpReq, session); err != nil {
		return nil, nil, err
	}
	return httpReq, toolNames, nil
}

func (p *Provider) setHeaders(ctx context.Context, req *http.Request, session requestSession) error {
	auth := authFromContextOrConfig(ctx, p.ProviderConfig.APIKey, p.apiKeyHeader())
	if auth.authorization == "" && auth.apiKey == "" {
		return fmt.Errorf("claudecode: missing upstream credential")
	}

	if auth.authorization != "" {
		req.Header.Set("Authorization", auth.authorization)
	}
	if auth.apiKey != "" {
		req.Header.Set("x-api-key", auth.apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-claude-code-session-id", session.SessionID)
	// Apply the Claude Code client fingerprint as defaults, then let
	// per-provider ExtraHeaders override any of them. This keeps a partial or
	// drifted provider config from leaking Go's default User-Agent or omitting
	// fingerprint headers a "standard Claude Code client" gate expects.
	for k, v := range defaultClaudeCodeFingerprintHeaders {
		req.Header.Set(k, v)
	}
	for k, v := range p.ProviderConfig.Network.ExtraHeaders {
		req.Header.Set(k, v)
	}
	return nil
}

type authHeader struct {
	authorization string
	apiKey        string
}

func authFromContextOrConfig(ctx context.Context, fallback string, apiKeyHeader string) authHeader {
	useXAPIKey := apiKeyHeader == apiKeyHeaderXAPIKey

	if cred, ok := provider.CredentialFromContext(ctx); ok && cred != nil {
		if cred.Type == credential.TypeOAuthToken && cred.Metadata != nil {
			if token, _ := cred.Metadata["access_token"].(string); strings.TrimSpace(token) != "" {
				return authHeader{
					authorization: "Bearer " + strings.TrimSpace(token),
				}
			}
		}
		if cred.Type == credential.TypeAPIKey && strings.TrimSpace(cred.APIKey()) != "" {
			if useXAPIKey {
				return authHeader{
					apiKey: strings.TrimSpace(cred.APIKey()),
				}
			}
			return authHeader{
				authorization: "Bearer " + strings.TrimSpace(cred.APIKey()),
			}
		}
	}

	fallback = strings.TrimSpace(fallback)
	if fallback == "" {
		return authHeader{}
	}
	if strings.HasPrefix(fallback, oauthTokenPrefix) {
		return authHeader{
			authorization: "Bearer " + fallback,
		}
	}
	if useXAPIKey {
		return authHeader{
			apiKey: fallback,
		}
	}
	return authHeader{
		authorization: "Bearer " + fallback,
	}
}

// apiKeyHeaderFromOptions resolves the api_key_header option to a normalized,
// validated value. It controls which header carries a plain API key:
// apiKeyHeaderAuthorization (default) sends Authorization: Bearer, while
// apiKeyHeaderXAPIKey sends x-api-key. CLI-auth and sk-ant-oat- OAuth tokens
// always use Authorization: Bearer regardless of this option.
func apiKeyHeaderFromOptions(options map[string]any) (string, error) {
	raw, ok := options["api_key_header"]
	if !ok {
		return apiKeyHeaderAuthorization, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("claudecode: option api_key_header must be a string")
	}
	switch normalized := strings.ToLower(strings.TrimSpace(value)); normalized {
	case "":
		return apiKeyHeaderAuthorization, nil
	case apiKeyHeaderAuthorization, apiKeyHeaderXAPIKey:
		return normalized, nil
	default:
		return "", fmt.Errorf("claudecode: invalid api_key_header %q (want %q or %q)", value, apiKeyHeaderAuthorization, apiKeyHeaderXAPIKey)
	}
}

func (p *Provider) apiKeyHeader() string {
	if p == nil {
		return apiKeyHeaderAuthorization
	}
	header, err := apiKeyHeaderFromOptions(p.ProviderConfig.Options)
	if err != nil {
		return apiKeyHeaderAuthorization
	}
	return header
}

type requestSession struct {
	SessionID string
	DeviceID  string
}

func newRequestSession() requestSession {
	sessionID := uuid.NewString()
	sum := sha256.Sum256([]byte(sessionID))
	return requestSession{
		SessionID: sessionID,
		DeviceID:  fmt.Sprintf("%x", sum[:]),
	}
}

func buildMessagesRequest(state *provider.ChatRequestState, stream bool, session requestSession) *messagesRequest {
	return anthropicbase.BuildMessagesRequest(state, anthropicbase.BuildMessagesOptions{
		DefaultMaxTokens:  defaultClaudeCodeMaxTokens,
		Stream:            stream,
		System:            buildSystemBlocks(),
		Metadata:          buildRequestMetadata(session),
		Thinking:          &thinkingConfig{Type: "adaptive"},
		ContextManagement: &contextManagement{Edits: []contextManagementEdit{{Type: "clear_thinking_20251015", Keep: "all"}}},
		OutputConfig:      &outputConfig{Effort: requestEffort(state), Format: anthropicbase.OutputFormatFromState(state)},
		CacheUserText:     true,
	})
}

var codexToClaudeCodeToolNames = map[string]string{
	"exec_command":       "Bash",
	"write_stdin":        "TaskOutput",
	"update_plan":        "TaskUpdate",
	"get_goal":           "TaskGet",
	"create_goal":        "TaskCreate",
	"update_goal":        "TodoWrite",
	"request_user_input": "AskUserQuestion",
	"view_image":         "Read",
}

// applyCodexToolNameAliases rewrites Codex tool names to their Claude Code
// equivalents on the outbound wire request so a Claude-Code-gated upstream
// accepts Codex traffic. It operates on the freshly built request only (never
// the shared ChatRequestState) and returns the reverse map used to restore the
// original names on the response. It returns nil when nothing was rewritten.
func applyCodexToolNameAliases(req *messagesRequest) (map[string]string, error) {
	if req == nil {
		return nil, nil
	}
	if err := rejectCodexToolAliasCollisions(req); err != nil {
		return nil, err
	}

	claudeToCodex := map[string]string{}
	alias := func(name string) (string, bool) {
		claudeName, ok := codexToClaudeCodeToolNames[name]
		if !ok {
			return name, false
		}
		claudeToCodex[claudeName] = name
		return claudeName, true
	}

	for i := range req.Tools {
		if claudeName, ok := alias(req.Tools[i].Name); ok {
			req.Tools[i].Name = claudeName
		}
	}
	for i := range req.Messages {
		for j := range req.Messages[i].Content {
			block := &req.Messages[i].Content[j]
			if block.Type != "tool_use" {
				continue
			}
			if claudeName, ok := alias(block.Name); ok {
				block.Name = claudeName
			}
		}
	}
	req.ToolChoice = aliasToolChoiceName(req.ToolChoice, alias)

	if len(claudeToCodex) == 0 {
		return nil, nil
	}
	return claudeToCodex, nil
}

func rejectCodexToolAliasCollisions(req *messagesRequest) error {
	names := map[string]struct{}{}
	for _, tool := range req.Tools {
		if strings.TrimSpace(tool.Name) != "" {
			names[tool.Name] = struct{}{}
		}
	}
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			if block.Type == "tool_use" && strings.TrimSpace(block.Name) != "" {
				names[block.Name] = struct{}{}
			}
		}
	}
	if name := toolChoiceName(req.ToolChoice); name != "" {
		names[name] = struct{}{}
	}

	for codexName, claudeName := range codexToClaudeCodeToolNames {
		_, hasCodex := names[codexName]
		_, hasClaude := names[claudeName]
		if hasCodex && hasClaude {
			return fmt.Errorf("claudecode: compact=codex tool name collision: cannot alias %q to %q because both names are present", codexName, claudeName)
		}
	}
	return nil
}

// aliasToolChoiceName rewrites the forced-tool name inside an Anthropic
// tool_choice payload so it matches the aliased tool definitions.
func aliasToolChoiceName(raw json.RawMessage, alias func(string) (string, bool)) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	choice, name, ok := decodeNamedToolChoice(raw)
	if !ok {
		return raw
	}
	claudeName, ok := alias(name)
	if !ok {
		return raw
	}
	choice["name"] = claudeName
	out, err := json.Marshal(choice)
	if err != nil {
		return raw
	}
	return out
}

func toolChoiceName(raw json.RawMessage) string {
	_, name, ok := decodeNamedToolChoice(raw)
	if !ok {
		return ""
	}
	return name
}

func decodeNamedToolChoice(raw json.RawMessage) (map[string]any, string, bool) {
	if len(raw) == 0 {
		return nil, "", false
	}
	var choice map[string]any
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil, "", false
	}
	name, _ := choice["name"].(string)
	if name == "" {
		return nil, "", false
	}
	return choice, name, true
}

func restoreClaudeCodeToolNames(msg *schema.Message, claudeToCodex map[string]string) {
	if msg == nil || len(claudeToCodex) == 0 {
		return
	}
	for i := range msg.ToolCalls {
		if codexName, ok := claudeToCodex[msg.ToolCalls[i].Function.Name]; ok {
			msg.ToolCalls[i].Function.Name = codexName
		}
	}
}

// requestEffort derives Claude's output_config.effort from the inbound reasoning
// effort, preferring the chat-completions field over the Responses reasoning
// object. The result is normalized onto the set Claude accepts.
func requestEffort(state *provider.ChatRequestState) string {
	if state == nil {
		return defaultClaudeCodeEffort
	}
	if extra := provider.ChatExtraFieldsFromOptions(state.Options...); extra != nil {
		if effort := strings.TrimSpace(extra.ReasoningEffort); effort != "" {
			return normalizeEffort(effort)
		}
		if effort := effortFromReasoning(extra.Reasoning); effort != "" {
			return normalizeEffort(effort)
		}
	}
	if ctx := provider.ResponsesRequestContextFromOptions(state.Options...); ctx != nil {
		if effort := effortFromReasoning(ctx.Reasoning); effort != "" {
			return normalizeEffort(effort)
		}
	}
	return defaultClaudeCodeEffort
}

// normalizeEffort maps an inbound reasoning-effort value onto the set Claude's
// output_config.effort accepts (low/medium/high). OpenAI's "minimal" has no
// Claude equivalent and folds to "low"; empty or unrecognized values fall back
// to the Claude Code default.
func normalizeEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "minimal":
		return "low"
	default:
		return defaultClaudeCodeEffort
	}
}

func effortFromReasoning(reasoning map[string]any) string {
	if len(reasoning) == 0 {
		return ""
	}
	effort, _ := reasoning["effort"].(string)
	return strings.TrimSpace(effort)
}

// buildRequestMetadata emits the metadata.user_id payload exactly as the Claude
// Code CLI does. The value is an opaque end-user identifier, so request fields
// without a Messages API equivalent are intentionally not smuggled in here.
func buildRequestMetadata(session requestSession) *requestMetadata {
	userIDPayload, _ := json.Marshal(map[string]string{
		"device_id":    session.DeviceID,
		"account_uuid": "",
		"session_id":   session.SessionID,
	})
	return &requestMetadata{
		UserID: string(userIDPayload),
	}
}

// buildSystemBlocks emits the standard Claude Code CLI system preamble as the
// first block. The inbound (e.g. Codex) system instructions are appended after
// it by anthropicbase.BuildMessagesRequest, yielding the real CLI's two-block
// shape: official-CLI preamble first, agent instructions second.
func buildSystemBlocks() []systemBlock {
	return []systemBlock{
		{
			Type:         "text",
			Text:         claudeCodeSystemPreamble,
			CacheControl: &cacheControl{Type: "ephemeral"},
		},
	}
}

// --- Wire type aliases retained for claudecode package tests. ---

type messagesRequest = anthropicbase.MessagesRequest
type messageItem = anthropicbase.MessageItem
type contentBlock = anthropicbase.ContentBlock
type imageSource = anthropicbase.ImageSource
type systemBlock = anthropicbase.SystemBlock
type cacheControl = anthropicbase.CacheControl
type toolDef = anthropicbase.ToolDef
type requestMetadata = anthropicbase.RequestMetadata
type thinkingConfig = anthropicbase.ThinkingConfig
type contextManagement = anthropicbase.ContextManagement
type contextManagementEdit = anthropicbase.ContextManagementEdit
type outputConfig = anthropicbase.OutputConfig
type responseBlock = anthropicbase.ResponseBlock
type messagesResponse = anthropicbase.MessagesResponse

var (
	_ provider.Provider          = (*Provider)(nil)
	_ provider.ResponsesProvider = (*Provider)(nil)
)
