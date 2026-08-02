package llmroute

import (
	"fmt"
	"slices"
)

func (r LLMRouteConfig) ValidateDefinition() error {
	r.Normalize()
	if r.ID == "" {
		return fmt.Errorf("route_id is required")
	}
	if r.Protocol == "" {
		return fmt.Errorf("route %q protocol is required", r.ID)
	}
	if r.Protocol == RouteProtocolMCP {
		return fmt.Errorf("route %q protocol %q is invalid for llm routes", r.ID, r.Protocol)
	}
	if r.TargetPolicy == nil {
		return fmt.Errorf("route %q targets are required in model-target mode", r.ID)
	}
	return r.TargetPolicy.ValidateDefinition(r.ID)
}

func (r LLMRouteConfig) ValidateStaticDefinition() error {
	r.Normalize()
	if err := r.ValidateDefinition(); err != nil {
		return err
	}
	if r.UsesLogicalModel() {
		return fmt.Errorf("route %q logical-model target_policy is not supported in static config", r.ID)
	}
	return nil
}

func (c RouteTargetPolicyCommon) validateCredentialPolicy(routeID string) error {
	switch c.CredentialSelector() {
	case "", RouteCredentialSelectRoundRobin, RouteCredentialSelectFillFirst:
	default:
		return fmt.Errorf("route %q invalid credential_selector %q", routeID, c.CredentialSelector())
	}
	seenScopes := map[RouteCredentialScope]struct{}{}
	for _, scope := range c.CredentialScopeOrder() {
		switch scope {
		case RouteCredentialScopeModelCustom, RouteCredentialScopeProviderID:
		default:
			return fmt.Errorf("route %q invalid credential_scope_order %q", routeID, scope)
		}
		if _, ok := seenScopes[scope]; ok {
			return fmt.Errorf("route %q duplicate credential_scope_order %q", routeID, scope)
		}
		seenScopes[scope] = struct{}{}
	}
	if len(c.CredentialTypeOrder()) == 0 {
		return fmt.Errorf("route %q credential_type_order must not be empty", routeID)
	}
	seenTypes := map[RouteCredentialType]struct{}{}
	for _, credentialType := range c.CredentialTypeOrder() {
		switch credentialType {
		case RouteCredentialTypeAPIKey, RouteCredentialTypeOAuthToken:
		default:
			return fmt.Errorf("route %q invalid credential_type_order %q", routeID, credentialType)
		}
		if _, ok := seenTypes[credentialType]; ok {
			return fmt.Errorf("route %q duplicate credential_type_order %q", routeID, credentialType)
		}
		seenTypes[credentialType] = struct{}{}
	}
	return nil
}

func (p *RouteDirectProviderPolicy) ValidateDefinition(routeID string) error {
	p.Normalize()
	if p.ProviderID == "" {
		return fmt.Errorf("route %q target_policy.provider_id is required", routeID)
	}
	if err := p.RouteTargetPolicyCommon.validateCredentialPolicy(routeID); err != nil {
		return err
	}
	for _, scope := range p.CredentialScopeOrder() {
		if scope == RouteCredentialScopeModelCustom {
			return fmt.Errorf("route %q direct-provider target_policy cannot use credential_scope_order %q", routeID, scope)
		}
	}
	return nil
}

func (p *RouteLogicalModelTargetPolicy) ValidateDefinition(routeID string) error {
	p.Normalize()
	if err := p.RouteTargetPolicyCommon.validateCredentialPolicy(routeID); err != nil {
		return err
	}
	if len(p.ModelTargets) == 0 {
		return fmt.Errorf("route %q target_policy.model_targets are required in logical-model mode", routeID)
	}
	switch p.ModelSelectorStrategy {
	case "", RouteSelectionStrategyAuto, RouteSelectionStrategyWeighted, RouteSelectionStrategyPriority:
	default:
		return fmt.Errorf("route %q invalid model_selector_strategy %q", routeID, p.ModelSelectorStrategy)
	}
	if p.Fallback.MaxNum < 0 || p.Fallback.MaxNum > 5 {
		return fmt.Errorf("route %q fallback.max_num must be between 0 and 5", routeID)
	}

	targetNames := make([]string, 0, len(p.ModelTargets))
	seenNames := map[string]struct{}{}
	defaultCandidateTargets := map[string]struct{}{}
	for _, target := range p.ModelTargets {
		if target.Name == "" {
			return fmt.Errorf("route %q target name is required", routeID)
		}
		if _, exists := seenNames[target.Name]; exists {
			return fmt.Errorf("route %q target %q is duplicated", routeID, target.Name)
		}
		seenNames[target.Name] = struct{}{}
		targetNames = append(targetNames, target.Name)
		if len(target.Candidates) == 0 {
			return fmt.Errorf("route %q target %q must define at least one candidate", routeID, target.Name)
		}
		candidateIDs := map[string]struct{}{}
		defaultCandidateCount := 0
		for _, candidate := range target.Candidates {
			if candidate.ProviderID == "" || candidate.UpstreamModel == "" {
				return fmt.Errorf("route %q target %q candidates require provider_id and upstream_model", routeID, target.Name)
			}
			key := candidate.ProviderID + "/" + candidate.UpstreamModel
			if _, exists := candidateIDs[key]; exists {
				return fmt.Errorf("route %q target %q duplicates candidate %q", routeID, target.Name, key)
			}
			candidateIDs[key] = struct{}{}
			if candidate.Default {
				defaultCandidateCount++
			}
		}
		if defaultCandidateCount > 1 {
			return fmt.Errorf("route %q target %q may define at most one default candidate", routeID, target.Name)
		}
		if defaultCandidateCount == 1 {
			defaultCandidateTargets[target.Name] = struct{}{}
		}
	}
	if p.DefaultModel != "" && !slices.Contains(targetNames, p.DefaultModel) {
		return fmt.Errorf("route %q default_model %q must appear in targets", routeID, p.DefaultModel)
	}
	if len(defaultCandidateTargets) > 1 {
		return fmt.Errorf("route %q default candidates must belong to a single target model", routeID)
	}
	if p.DefaultModel != "" {
		for targetName := range defaultCandidateTargets {
			if targetName != p.DefaultModel {
				return fmt.Errorf("route %q default candidate target %q must match default_model %q", routeID, targetName, p.DefaultModel)
			}
		}
	}
	return nil
}

func (r LLMRouteConfig) ProviderIDs() []string {
	r.Normalize()
	if r.TargetPolicy == nil {
		return nil
	}
	ids := r.TargetPolicy.ProviderIDs()
	slices.Sort(ids)
	return ids
}

func (r LLMRoute) ValidateDefinition() error {
	return r.Config().ValidateDefinition()
}

func (r LLMRoute) ValidateStaticDefinition() error {
	return r.Config().ValidateStaticDefinition()
}

func (r LLMRoute) ProviderIDs() []string {
	return r.Config().ProviderIDs()
}
