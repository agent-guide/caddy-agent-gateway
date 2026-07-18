package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/internal/statuserr"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	"github.com/agent-guide/agent-gateway/pkg/llm/credentialmgr"
	credentialmgrscheduler "github.com/agent-guide/agent-gateway/pkg/llm/credentialmgr/scheduler"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/cloudwego/eino/schema"
)

type RoutedProvider struct {
	route               *llmroutepkg.LLMRoute
	requestRequirements llmroutepkg.RequestRequirements
	providerResolver    ProviderResolver
	providerConfigs     llmroutepkg.ProviderConfigResolver
	modelCatalog        llmroutepkg.ModelCatalogResolver
	credentialMgr       *credentialmgr.Manager
	scheduler           credentialmgrscheduler.CredentialScheduler
}

type executionState struct {
	triedCandidates          map[string]struct{}
	triedCredentials         map[string]struct{}
	triedProviderConfigAuths map[string]struct{}
	modelFallbacks           int
}

type resolvedAttempt struct {
	target *llmroutepkg.ResolvedTarget
	base   provider.Provider
	cred   *credentialmgr.ManagedCredential
	ctx    context.Context
}

var errManagedCredentialUnavailable = errors.New("managed credential unavailable")

func (p *RoutedProvider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	var out *provider.ChatResponse
	err := p.executeWithFallback(ctx, req.Model, func(ctx context.Context, attempt *resolvedAttempt) error {
		cloned := *req
		cloned.Model = attempt.target.UpstreamModel
		resp, err := attempt.base.Chat(ctx, &cloned)
		if err == nil {
			req.Model = cloned.Model
			out = resp
			var tokens provider.Usage
			if resp != nil {
				tokens = provider.UsageFromMessage(resp.Message)
			}
			recordProviderUsage(ctx, attempt, tokens)
		}
		return err
	})
	return out, err
}

func (p *RoutedProvider) StreamChat(ctx context.Context, req *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	var out *schema.StreamReader[*schema.Message]
	err := p.executeWithFallback(ctx, req.Model, func(ctx context.Context, attempt *resolvedAttempt) error {
		cloned := *req
		cloned.Model = attempt.target.UpstreamModel
		stream, err := attempt.base.StreamChat(ctx, &cloned)
		if err == nil {
			req.Model = cloned.Model
			out = stream
			recordProviderUsage(ctx, attempt, provider.Usage{})
		}
		return err
	})
	return out, err
}

func (p *RoutedProvider) CreateResponses(ctx context.Context, req *provider.ResponsesRequest) (*provider.ResponsesResponse, error) {
	var out *provider.ResponsesResponse
	err := p.executeWithFallback(ctx, req.Model, func(ctx context.Context, attempt *resolvedAttempt) error {
		base, ok := attempt.base.(provider.ResponsesProvider)
		if !ok {
			return statuserr.New(http.StatusNotImplemented, "responses api is not supported by this provider")
		}
		cloned := *req
		cloned.Model = attempt.target.UpstreamModel
		resp, err := base.CreateResponses(ctx, &cloned)
		if err == nil {
			req.Model = cloned.Model
			out = resp
			if resp != nil && resp.Usage != nil {
				recordProviderUsage(ctx, attempt, provider.Usage{
					InputTokens:     resp.Usage.InputTokens,
					OutputTokens:    resp.Usage.OutputTokens,
					TotalTokens:     resp.Usage.TotalTokens,
					CachedTokens:    resp.Usage.InputTokensDetails.CachedTokens,
					ReasoningTokens: resp.Usage.OutputTokensDetails.ReasoningTokens,
				})
			} else {
				recordProviderUsage(ctx, attempt, provider.Usage{})
			}
		}
		return err
	})
	return out, err
}

func (p *RoutedProvider) StreamResponses(ctx context.Context, req *provider.ResponsesRequest) (*schema.StreamReader[*provider.ResponsesStreamEvent], error) {
	var out *schema.StreamReader[*provider.ResponsesStreamEvent]
	err := p.executeWithFallback(ctx, req.Model, func(ctx context.Context, attempt *resolvedAttempt) error {
		base, ok := attempt.base.(provider.ResponsesProvider)
		if !ok {
			return statuserr.New(http.StatusNotImplemented, "responses api is not supported by this provider")
		}
		cloned := *req
		cloned.Model = attempt.target.UpstreamModel
		stream, err := base.StreamResponses(ctx, &cloned)
		if err == nil {
			req.Model = cloned.Model
			out = stream
			recordProviderUsage(ctx, attempt, provider.Usage{})
		}
		return err
	})
	return out, err
}

func recordProviderUsage(ctx context.Context, attempt *resolvedAttempt, tokens provider.Usage) {
	if attempt == nil || attempt.target == nil {
		return
	}
	total := tokens.TotalTokens
	if total == 0 {
		total = tokens.InputTokens + tokens.OutputTokens
	}
	ext := usage.LLMExtension{
		ProviderID:       attempt.target.ProviderID,
		ProviderType:     attempt.target.ProviderType,
		LogicalModel:     attempt.target.LogicalModel,
		UpstreamModel:    attempt.target.UpstreamModel,
		InputTokens:      usage.Int(tokens.InputTokens),
		OutputTokens:     usage.Int(tokens.OutputTokens),
		TotalTokens:      usage.Int(total),
		CachedTokens:     usage.Int(tokens.CachedTokens),
		ReasoningTokens:  usage.Int(tokens.ReasoningTokens),
		UsageFinalized:   usage.Bool(tokens.InputTokens > 0 || tokens.OutputTokens > 0 || total > 0),
		CredentialSource: "static",
	}
	if attempt.cred != nil {
		ext.CredentialSource = attempt.cred.Type
		ext.CredentialID = attempt.cred.ID
	}
	usage.SpanFromContext(ctx).SetExtension(ext)
}

func (p *RoutedProvider) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	target, err := p.resolveTarget(ctx, "")
	if err != nil {
		return nil, err
	}
	base, err := p.resolveProvider(ctx, target.ProviderID)
	if err != nil {
		return nil, err
	}
	return base.ListModels(ctx)
}

func (p *RoutedProvider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{}
}

func (p *RoutedProvider) Config() provider.ProviderConfig {
	return provider.ProviderConfig{}
}

func (p *RoutedProvider) executeWithFallback(ctx context.Context, reqModel string, call func(context.Context, *resolvedAttempt) error) error {
	if p.route == nil {
		return statuserr.New(http.StatusServiceUnavailable, "llm route is not configured")
	}
	state := &executionState{
		triedCandidates:          map[string]struct{}{},
		triedCredentials:         map[string]struct{}{},
		triedProviderConfigAuths: map[string]struct{}{},
	}
	maxFallbacks := 0
	if p.route.UsesLogicalModel() {
		fallback := p.route.TargetPolicy.FallbackPolicy()
		if fallback.Enabled {
			maxFallbacks = fallback.MaxNum
		}
	}

	var lastErr error
	var target *llmroutepkg.ResolvedTarget
	var base provider.Provider
	for {
		var err error
		if target == nil {
			target, err = p.resolveTarget(ctx, reqModel, state.triedCandidates)
			if err != nil {
				if lastErr != nil {
					return lastErr
				}
				return err
			}
			base, err = p.resolveProvider(ctx, target.ProviderID)
			if err != nil {
				if lastErr != nil {
					return lastErr
				}
				return err
			}
		}

		credCtx, cred, err := p.selectCredential(ctx, target, state)
		if err != nil {
			if errors.Is(err, errManagedCredentialUnavailable) && p.markProviderConfigFallbackAttempt(state, target, base) {
				credCtx = ctx
				cred = nil
			} else {
				if p.advanceCandidate(state, target, maxFallbacks) {
					target = nil
					base = nil
					continue
				}
				if lastErr != nil {
					return lastErr
				}
				return err
			}
		}

		attempt := &resolvedAttempt{target: target, base: base, cred: cred, ctx: credCtx}
		err = call(attempt.ctx, attempt)
		p.markResult(ctx, attempt, err)
		if err == nil {
			return nil
		}
		lastErr = err

		if p.classifyFailure(err) != failureReselectModel {
			return err
		}
		if p.scheduler == nil || p.credentialMgr == nil {
			if !p.advanceCandidate(state, target, maxFallbacks) {
				return err
			}
			target = nil
			base = nil
		}
	}
}

func (p *RoutedProvider) advanceCandidate(state *executionState, target *llmroutepkg.ResolvedTarget, maxFallbacks int) bool {
	if !p.route.UsesLogicalModel() || target == nil || state.modelFallbacks >= maxFallbacks {
		return false
	}
	state.triedCandidates[llmroutepkg.CandidateKey(target.ProviderID, target.UpstreamModel)] = struct{}{}
	state.modelFallbacks++
	return true
}

func (p *RoutedProvider) resolveTarget(ctx context.Context, reqModel string, excluded ...map[string]struct{}) (*llmroutepkg.ResolvedTarget, error) {
	req := p.requestRequirements
	req.Model = reqModel
	if len(excluded) > 0 {
		req.ExcludedCandidates = excluded[0]
	}
	return p.route.ResolveTarget(ctx, p.modelCatalog, p.providerConfigs, req)
}

func (p *RoutedProvider) resolveProvider(ctx context.Context, providerID string) (provider.Provider, error) {
	prov, err := p.providerResolver.ResolveProvider(ctx, providerID)
	if err != nil || prov == nil {
		if errors.Is(err, ErrProviderDisabled) {
			return nil, statuserr.New(http.StatusForbidden, fmt.Sprintf("route target provider %q is disabled", providerID))
		}
		return nil, statuserr.New(http.StatusBadGateway, fmt.Sprintf("route target provider %q is not configured", providerID))
	}
	return prov, nil
}

func (p *RoutedProvider) selectCredential(ctx context.Context, target *llmroutepkg.ResolvedTarget, state *executionState) (context.Context, *credentialmgr.ManagedCredential, error) {
	if p.scheduler == nil || p.credentialMgr == nil {
		return ctx, nil, nil
	}
	scopes := p.expandCredentialScopes(target)
	for _, scope := range scopes {
		if scope == "" {
			continue
		}
		for _, credentialType := range p.route.TargetPolicy.CredentialTypeOrder() {
			cred, err := p.scheduler.Pick(ctx, credentialmgrscheduler.Filter{
				Type:            string(credentialType),
				CredentialScope: scope,
				Model:           target.UpstreamModel,
				Selector:        string(p.route.TargetPolicy.CredentialSelector()),
			}, state.triedCredentials)
			if err != nil || cred == nil {
				continue
			}
			if cred.Type == credentialmgr.TypeCLIAuthToken {
				refreshed, err := p.credentialMgr.RefreshCredentialIfNeeded(ctx, cred.ID)
				if err != nil {
					state.triedCredentials[cred.ID] = struct{}{}
					continue
				}
				cred = refreshed
			}
			state.triedCredentials[cred.ID] = struct{}{}
			return provider.WithCredential(ctx, cred.Credential.Clone()), cred, nil
		}
	}
	return ctx, nil, fmt.Errorf("%w: %s", errManagedCredentialUnavailable, fmt.Sprintf("no managed credential available for provider %q model %q", target.ProviderID, target.UpstreamModel))
}

func (p *RoutedProvider) markProviderConfigFallbackAttempt(state *executionState, target *llmroutepkg.ResolvedTarget, base provider.Provider) bool {
	if target == nil || base == nil {
		return false
	}
	apiKey := strings.TrimSpace(base.Config().APIKey)
	if apiKey == "" {
		return false
	}
	key := llmroutepkg.CandidateKey(target.ProviderID, target.UpstreamModel)
	if _, ok := state.triedProviderConfigAuths[key]; ok {
		return false
	}
	state.triedProviderConfigAuths[key] = struct{}{}
	return true
}

func (p *RoutedProvider) expandCredentialScopes(target *llmroutepkg.ResolvedTarget) []string {
	out := make([]string, 0, len(p.route.TargetPolicy.CredentialScopeOrder()))
	for _, scope := range p.route.TargetPolicy.CredentialScopeOrder() {
		switch scope {
		case llmroutepkg.RouteCredentialScopeModelCustom:
			if target.CredentialScope != "" {
				out = append(out, target.CredentialScope)
			}
		case llmroutepkg.RouteCredentialScopeProviderID:
			out = append(out, credentialmgr.ProviderIDCredentialScope(target.ProviderID))
		}
	}
	return out
}

type failureAction int

const (
	failureStop failureAction = iota
	failureReselectModel
)

func (p *RoutedProvider) classifyFailure(err error) failureAction {
	status := statuserr.StatusCode(err, http.StatusBadGateway)
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return failureReselectModel
	default:
		if status >= 500 {
			return failureReselectModel
		}
		return failureStop
	}
}

func (p *RoutedProvider) markResult(ctx context.Context, attempt *resolvedAttempt, err error) {
	if p.scheduler == nil || attempt == nil || attempt.cred == nil {
		return
	}
	result := credentialmgrscheduler.Result{
		CredentialID: attempt.cred.ID,
		Model:        attempt.target.UpstreamModel,
		Success:      err == nil,
	}
	if err != nil {
		status := statuserr.StatusCode(err, http.StatusBadGateway)
		result.Error = &credentialmgrscheduler.Error{
			Code:       http.StatusText(status),
			Message:    err.Error(),
			HTTPStatus: status,
			Retryable:  status == http.StatusTooManyRequests || status >= 500,
		}
	}
	p.scheduler.MarkResult(ctx, result)
}

var (
	_ provider.Provider          = (*RoutedProvider)(nil)
	_ provider.ResponsesProvider = (*RoutedProvider)(nil)
)
