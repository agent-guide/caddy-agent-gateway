package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/internal/statuserr"
	"github.com/agent-guide/agent-gateway/pkg/credential"
	credentialscheduler "github.com/agent-guide/agent-gateway/pkg/credential/scheduler"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

type RoutedProvider struct {
	route               *llmroutepkg.LLMRoute
	requestRequirements llmroutepkg.RequestRequirements
	providerResolver    ProviderResolver
	providerConfigs     llmroutepkg.ProviderConfigResolver
	modelCatalog        llmroutepkg.ModelCatalogResolver
	credentialMgr       *credential.Manager
	scheduler           credentialscheduler.CredentialScheduler
	logger              *zap.Logger
}

type executionState struct {
	triedCandidates          map[string]struct{}
	triedCredentials         map[string]struct{}
	triedProviderConfigAuths map[string]struct{}
	lastCredentialRefreshID  string
	modelFallbacks           int
}

type resolvedAttempt struct {
	target *llmroutepkg.ResolvedTarget
	base   provider.Provider
	cred   *credential.ManagedCredential
	ctx    context.Context
}

var errManagedCredentialUnavailable = errors.New("managed credential unavailable")

func (p *RoutedProvider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	execution, err := p.ExecuteChat(ctx, req)
	if err != nil {
		return nil, err
	}
	if execution != nil {
		var tokens provider.Usage
		if execution.Response != nil {
			tokens = provider.UsageFromMessage(execution.Response.Message)
		}
		recordResolvedExecutionUsage(ctx, execution.Resolved, tokens)
		return execution.Response, nil
	}
	return nil, nil
}

func (p *RoutedProvider) ExecuteChat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatExecution, error) {
	var out *provider.ChatExecution
	requirements := p.requestRequirements
	if err := validateAttachedProtocolRequirements(req, requirements.ProtocolRequirements); err != nil {
		return nil, err
	}
	p.logProtocolRequirements(req.Model, requirements)
	err := p.executeWithFallback(ctx, req.Model, requirements, func(ctx context.Context, attempt *resolvedAttempt) error {
		cloned := *req
		cloned.ProtocolState = provider.CloneProtocolState(req.ProtocolState)
		cloned.Model = attempt.target.UpstreamModel
		resp, err := attempt.base.Chat(ctx, &cloned)
		if err == nil {
			out = &provider.ChatExecution{Response: resp, Resolved: resolvedExecution(req.Model, attempt)}
		}
		return err
	})
	return out, err
}

func (p *RoutedProvider) StreamChat(ctx context.Context, req *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	execution, err := p.ExecuteStreamChat(ctx, req)
	if err != nil {
		return nil, err
	}
	if execution != nil {
		recordResolvedExecutionUsage(ctx, execution.Resolved, provider.Usage{})
		return execution.Stream, nil
	}
	return nil, nil
}

func (p *RoutedProvider) ExecuteStreamChat(ctx context.Context, req *provider.ChatRequest) (*provider.StreamExecution, error) {
	var out *provider.StreamExecution
	requirements := p.requestRequirements
	if err := validateAttachedProtocolRequirements(req, requirements.ProtocolRequirements); err != nil {
		return nil, err
	}
	p.logProtocolRequirements(req.Model, requirements)
	err := p.executeWithFallback(ctx, req.Model, requirements, func(ctx context.Context, attempt *resolvedAttempt) error {
		cloned := *req
		cloned.ProtocolState = provider.CloneProtocolState(req.ProtocolState)
		cloned.Model = attempt.target.UpstreamModel
		stream, err := attempt.base.StreamChat(ctx, &cloned)
		if err == nil {
			out = &provider.StreamExecution{Stream: stream, Resolved: resolvedExecution(req.Model, attempt)}
		}
		return err
	})
	return out, err
}

func resolvedExecution(clientModel string, attempt *resolvedAttempt) provider.ResolvedExecution {
	if attempt == nil || attempt.target == nil {
		return provider.ResolvedExecution{}
	}
	typeCapabilities := provider.CapabilitiesForProviderType(attempt.target.ProviderType)
	dialect := provider.ProtocolDialect("")
	for feature := range typeCapabilities.ProtocolFeatures {
		definition, err := provider.ProtocolFeatureDefinitionFor(feature)
		if err == nil && (dialect == "" || dialect == definition.Dialect) {
			dialect = definition.Dialect
		}
	}
	resolved := provider.ResolvedExecution{Candidate: provider.ServedCandidate{
		Dialect: dialect, ProviderType: attempt.target.ProviderType, ProviderID: attempt.target.ProviderID,
		LogicalModel: attempt.target.LogicalModel, ClientModel: clientModel, UpstreamModel: attempt.target.UpstreamModel,
		Features: typeCapabilities.ProtocolFeatures,
	}, Attribution: provider.AttemptAttribution{CredentialSource: "static"}}
	if attempt.cred != nil {
		resolved.Attribution.CredentialSource = attempt.cred.Type
		resolved.Attribution.CredentialID = attempt.cred.ID
	}
	return resolved
}

func recordResolvedExecutionUsage(ctx context.Context, resolved provider.ResolvedExecution, tokens provider.Usage) {
	total := tokens.TotalTokens
	if total == 0 {
		total = tokens.InputTokens + tokens.OutputTokens
	}
	usage.SpanFromContext(ctx).SetExtension(usage.LLMExtension{
		ProviderID: resolved.Candidate.ProviderID, ProviderType: resolved.Candidate.ProviderType,
		LogicalModel: resolved.Candidate.LogicalModel, UpstreamModel: resolved.Candidate.UpstreamModel,
		CredentialID: resolved.Attribution.CredentialID, CredentialSource: resolved.Attribution.CredentialSource,
		InputTokens: usage.Int(tokens.InputTokens), OutputTokens: usage.Int(tokens.OutputTokens), TotalTokens: usage.Int(total),
		CachedTokens: usage.Int(tokens.CachedTokens), ReasoningTokens: usage.Int(tokens.ReasoningTokens),
		UsageFinalized: usage.Bool(tokens.InputTokens > 0 || tokens.OutputTokens > 0 || total > 0),
	})
}

func validateAttachedProtocolRequirements(req *provider.ChatRequest, expected provider.ProtocolRequirementSet) error {
	var attached provider.ProtocolRequirementSet
	if req != nil && req.ProtocolState != nil {
		attached = req.ProtocolState.Requirements
	}
	if !attached.Equal(expected) {
		return statuserr.New(http.StatusBadRequest, "chat request protocol requirements disagree with route selection")
	}
	return nil
}

func (p *RoutedProvider) logProtocolRequirements(model string, requirements llmroutepkg.RequestRequirements) {
	if p.logger == nil || requirements.ProtocolRequirements.Empty() {
		return
	}
	features := requirements.ProtocolRequirements.Features()
	featureNames := make([]string, len(features))
	for i, feature := range features {
		featureNames[i] = string(feature)
	}
	fields := []zap.Field{
		zap.String("model", model),
		zap.Strings("required_protocol_features", featureNames),
	}
	if p.route != nil {
		fields = append(fields, zap.String("route_id", p.route.ID))
	}
	p.logger.Debug("request protocol state restricts provider fallback", fields...)
}

func (p *RoutedProvider) CreateResponses(ctx context.Context, req *provider.ResponsesRequest) (*provider.ResponsesResponse, error) {
	var out *provider.ResponsesResponse
	err := p.executeWithFallback(ctx, req.Model, p.requestRequirements, func(ctx context.Context, attempt *resolvedAttempt) error {
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
	err := p.executeWithFallback(ctx, req.Model, p.requestRequirements, func(ctx context.Context, attempt *resolvedAttempt) error {
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
	target, err := p.resolveTarget(ctx, "", p.requestRequirements)
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

func (p *RoutedProvider) executeWithFallback(ctx context.Context, reqModel string, requirements llmroutepkg.RequestRequirements, call func(context.Context, *resolvedAttempt) error) error {
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
			target, err = p.resolveTarget(ctx, reqModel, requirements, state.triedCandidates)
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

func (p *RoutedProvider) resolveTarget(ctx context.Context, reqModel string, req llmroutepkg.RequestRequirements, excluded ...map[string]struct{}) (*llmroutepkg.ResolvedTarget, error) {
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

func (p *RoutedProvider) selectCredential(ctx context.Context, target *llmroutepkg.ResolvedTarget, state *executionState) (context.Context, *credential.ManagedCredential, error) {
	if p.scheduler == nil || p.credentialMgr == nil {
		return ctx, nil, nil
	}
	scopes := p.expandCredentialScopes(target)
	for _, scope := range scopes {
		if scope == "" {
			continue
		}
		for _, credentialType := range p.route.TargetPolicy.CredentialTypeOrder() {
			cred, err := p.scheduler.Pick(ctx, credentialscheduler.Filter{
				Type:            string(credentialType),
				CredentialScope: scope,
				Model:           target.UpstreamModel,
				Selector:        string(p.route.TargetPolicy.CredentialSelector()),
			}, state.triedCredentials)
			if err != nil || cred == nil {
				continue
			}
			if cred.Type == credential.TypeOAuthToken {
				refreshed, err := p.credentialMgr.RefreshCredentialIfNeeded(ctx, cred.ID)
				if err != nil {
					state.lastCredentialRefreshID = cred.ID
					if p.logger != nil {
						p.logger.Error("credential refresh failed",
							zap.String("credential_id", cred.ID),
							zap.String("provider_id", target.ProviderID),
							zap.String("upstream_model", target.UpstreamModel),
							zap.Error(err),
						)
					}
					p.markCredentialRefreshFailure(ctx, cred.ID, err)
					state.triedCredentials[cred.ID] = struct{}{}
					continue
				}
				cred = refreshed
			}
			state.triedCredentials[cred.ID] = struct{}{}
			return provider.WithCredential(ctx, cred.Credential.Clone()), cred, nil
		}
	}
	err := fmt.Errorf("%w: no managed credential available for provider %q model %q", errManagedCredentialUnavailable, target.ProviderID, target.UpstreamModel)
	if state.lastCredentialRefreshID != "" {
		err = fmt.Errorf("%w; credential refresh failed for credential %q", err, state.lastCredentialRefreshID)
	}
	return ctx, nil, err
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
			out = append(out, credential.ProviderIDCredentialScope(target.ProviderID))
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

// markCredentialRefreshFailure cools a credential briefly after a refresh
// failure so later requests do not each invoke the failing refresh subprocess
// before failing over.
func (p *RoutedProvider) markCredentialRefreshFailure(ctx context.Context, credentialID string, err error) {
	if p.scheduler == nil {
		return
	}
	retryAfter := credential.RefreshFailureCooldown
	p.scheduler.MarkResult(ctx, credentialscheduler.Result{
		CredentialID:   credentialID,
		CredentialWide: true,
		Error: &credentialscheduler.Error{
			Code:       http.StatusText(http.StatusBadGateway),
			Message:    fmt.Sprintf("refresh credential: %v", err),
			HTTPStatus: http.StatusBadGateway,
			Retryable:  true,
		},
		RetryAfter: &retryAfter,
	})
}

func (p *RoutedProvider) markResult(ctx context.Context, attempt *resolvedAttempt, err error) {
	if p.scheduler == nil || attempt == nil || attempt.cred == nil {
		return
	}
	result := credentialscheduler.Result{
		CredentialID: attempt.cred.ID,
		Model:        attempt.target.UpstreamModel,
		Success:      err == nil,
	}
	if err != nil {
		status := statuserr.StatusCode(err, http.StatusBadGateway)
		result.Error = &credentialscheduler.Error{
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
