package gateway

import (
	"strconv"
	"strings"

	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
)

func init() {
	httpcaddyfile.RegisterGlobalOption("agent_gateway", parseApp)
}

func parseApp(d *caddyfile.Dispenser, existingVal any) (any, error) {
	app := &App{}
	if current, ok := existingVal.(*App); ok && current != nil {
		app = current
	}

	if !d.Next() {
		return nil, d.Err("expected directive name")
	}
	if d.NextArg() {
		return nil, d.ArgErr()
	}

	for d.NextBlock(0) {
		switch d.Val() {
		case "provider":
			if err := parseProvider(d, app); err != nil {
				return nil, err
			}
		case "provider_types":
			if err := parseProviderTypes(d, app); err != nil {
				return nil, err
			}
		case "config_store":
			if err := parseConfigStore(d, app); err != nil {
				return nil, err
			}
		case "credential_refresh_command":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			app.CredentialRefreshCommand = strings.TrimSpace(d.Val())
			if app.CredentialRefreshCommand == "" {
				return nil, d.ArgErr()
			}
			app.CredentialRefreshArgs = nil
			for d.NextArg() {
				app.CredentialRefreshArgs = append(app.CredentialRefreshArgs, d.Val())
			}
		case "route":
			if err := parseRoute(d, app); err != nil {
				return nil, err
			}
		case "metrics":
			if err := parseMetrics(d, app); err != nil {
				return nil, err
			}
		case "logical_model":
			return nil, d.Err("logical_model is no longer supported; define model targets inline within route blocks")
		case "virtualkey":
			return nil, d.Err("virtualkey is no longer supported in the Caddyfile; create virtual keys through the Admin API")
		default:
			return nil, d.Errf("unknown subdirective: %s", d.Val())
		}
	}

	return httpcaddyfile.App{
		Name:  "agent_gateway",
		Value: caddyconfig.JSON(app, nil),
	}, nil
}

func parseMetrics(d *caddyfile.Dispenser, app *App) error {
	seg := d.NewFromNextSegment()
	if !seg.Next() {
		return d.Err("expected metrics directive")
	}
	if seg.NextArg() {
		return seg.ArgErr()
	}
	for seg.NextBlock(0) {
		switch seg.Val() {
		case "retention_days":
			if !seg.NextArg() {
				return seg.ArgErr()
			}
			v, err := strconv.Atoi(seg.Val())
			if err != nil || v < 0 {
				return seg.Err("retention_days must be a non-negative integer")
			}
			if seg.NextArg() {
				return seg.ArgErr()
			}
			app.Metrics.RetentionDays = v
		case "max_agent_depth":
			if !seg.NextArg() {
				return seg.ArgErr()
			}
			v, err := strconv.Atoi(seg.Val())
			if err != nil || v < 0 {
				return seg.Err("max_agent_depth must be a non-negative integer")
			}
			if seg.NextArg() {
				return seg.ArgErr()
			}
			app.Metrics.MaxAgentDepth = v
		case "otlp":
			if seg.NextArg() {
				return seg.ArgErr()
			}
			if err := parseMetricsOTLP(seg, app); err != nil {
				return err
			}
		default:
			return seg.Errf("unknown metrics subdirective: %s", seg.Val())
		}
	}
	return nil
}

func parseMetricsOTLP(seg *caddyfile.Dispenser, app *App) error {
	for seg.NextBlock(1) {
		switch seg.Val() {
		case "endpoint":
			if !seg.NextArg() {
				return seg.ArgErr()
			}
			app.Metrics.OTLP.Endpoint = seg.Val()
			if seg.NextArg() {
				return seg.ArgErr()
			}
		case "protocol":
			if !seg.NextArg() {
				return seg.ArgErr()
			}
			app.Metrics.OTLP.Protocol = seg.Val()
			if seg.NextArg() {
				return seg.ArgErr()
			}
		case "insecure":
			if seg.NextArg() {
				return seg.ArgErr()
			}
			app.Metrics.OTLP.Insecure = true
		case "components":
			if seg.NextArg() {
				return seg.ArgErr()
			}
			app.Metrics.OTLP.Components = true
		case "header":
			if !seg.NextArg() {
				return seg.ArgErr()
			}
			name := seg.Val()
			if !seg.NextArg() {
				return seg.ArgErr()
			}
			if app.Metrics.OTLP.Headers == nil {
				app.Metrics.OTLP.Headers = make(map[string]string)
			}
			app.Metrics.OTLP.Headers[name] = seg.Val()
			if seg.NextArg() {
				return seg.ArgErr()
			}
		case "service_name":
			if !seg.NextArg() {
				return seg.ArgErr()
			}
			app.Metrics.OTLP.ServiceName = seg.Val()
			if seg.NextArg() {
				return seg.ArgErr()
			}
		case "timeout_seconds":
			if !seg.NextArg() {
				return seg.ArgErr()
			}
			v, err := strconv.Atoi(seg.Val())
			if err != nil || v <= 0 {
				return seg.Err("timeout_seconds must be a positive integer")
			}
			if seg.NextArg() {
				return seg.ArgErr()
			}
			app.Metrics.OTLP.TimeoutSeconds = v
		default:
			return seg.Errf("unknown otlp subdirective: %s", seg.Val())
		}
	}
	if err := app.Metrics.OTLP.Validate(); err != nil {
		return seg.Err(err.Error())
	}
	return nil
}

func parseProvider(d *caddyfile.Dispenser, app *App) error {
	seg := d.NewFromNextSegment()
	if !seg.Next() {
		return d.Err("expected provider directive")
	}
	cfg, err := parseProviderConfig(seg)
	if err != nil {
		return err
	}
	if app.Providers == nil {
		app.Providers = make(map[string]provider.ProviderConfig)
	}
	if _, exists := app.Providers[cfg.Id]; exists {
		return d.Errf("duplicate provider %q", cfg.Id)
	}
	app.Providers[cfg.Id] = cfg
	return nil
}

func parseProviderConfig(d *caddyfile.Dispenser) (provider.ProviderConfig, error) {
	var cfg provider.ProviderConfig
	if !d.NextArg() {
		return provider.ProviderConfig{}, d.ArgErr()
	}
	cfg.Id = d.Val()
	if cfg.Id == "" {
		return provider.ProviderConfig{}, d.Err("provider id is required")
	}
	if d.NextArg() {
		return provider.ProviderConfig{}, d.ArgErr()
	}
	for d.NextBlock(0) {
		switch d.Val() {
		case "provider_type":
			if cfg.ProviderType != "" {
				return provider.ProviderConfig{}, d.Err("provider_type already configured")
			}
			if !d.NextArg() {
				return provider.ProviderConfig{}, d.ArgErr()
			}
			cfg.ProviderType = strings.ToLower(strings.TrimSpace(d.Val()))
			if d.NextArg() {
				return provider.ProviderConfig{}, d.ArgErr()
			}
		case "api_key":
			if !d.NextArg() {
				return provider.ProviderConfig{}, d.ArgErr()
			}
			cfg.APIKey = d.Val()
			if d.NextArg() {
				return provider.ProviderConfig{}, d.ArgErr()
			}
		case "base_url":
			if !d.NextArg() {
				return provider.ProviderConfig{}, d.ArgErr()
			}
			cfg.BaseURL = d.Val()
			if d.NextArg() {
				return provider.ProviderConfig{}, d.ArgErr()
			}
		case "default_model":
			if !d.NextArg() {
				return provider.ProviderConfig{}, d.ArgErr()
			}
			cfg.DefaultModel = d.Val()
			if d.NextArg() {
				return provider.ProviderConfig{}, d.ArgErr()
			}
		case "request_timeout_seconds":
			v, err := parseProviderIntArg(d)
			if err != nil {
				return provider.ProviderConfig{}, err
			}
			cfg.Network.RequestTimeoutSeconds = v
		case "max_retries":
			v, err := parseProviderIntArg(d)
			if err != nil {
				return provider.ProviderConfig{}, err
			}
			cfg.Network.MaxRetries = v
		case "retry_delay_seconds":
			v, err := parseProviderIntArg(d)
			if err != nil {
				return provider.ProviderConfig{}, err
			}
			cfg.Network.RetryDelaySeconds = v
		case "max_idle_connections":
			v, err := parseProviderIntArg(d)
			if err != nil {
				return provider.ProviderConfig{}, err
			}
			cfg.Network.MaxIdleConnections = v
		case "max_idle_connections_per_host":
			v, err := parseProviderIntArg(d)
			if err != nil {
				return provider.ProviderConfig{}, err
			}
			cfg.Network.MaxIdleConnectionsPerHost = v
		case "idle_keep_alive_timeout_seconds":
			v, err := parseProviderIntArg(d)
			if err != nil {
				return provider.ProviderConfig{}, err
			}
			cfg.Network.IdleKeepAliveTimeoutSeconds = v
		case "proxy_url":
			if !d.NextArg() {
				return provider.ProviderConfig{}, d.ArgErr()
			}
			cfg.Network.ProxyURL = d.Val()
			if d.NextArg() {
				return provider.ProviderConfig{}, d.ArgErr()
			}
		case "header":
			args := d.RemainingArgs()
			if len(args) != 2 {
				return provider.ProviderConfig{}, d.ArgErr()
			}
			if cfg.Network.ExtraHeaders == nil {
				cfg.Network.ExtraHeaders = make(map[string]string)
			}
			cfg.Network.ExtraHeaders[args[0]] = args[1]
		case "option":
			args := d.RemainingArgs()
			if len(args) != 2 {
				return provider.ProviderConfig{}, d.ArgErr()
			}
			if cfg.Options == nil {
				cfg.Options = make(map[string]any)
			}
			cfg.Options[args[0]] = args[1]
		default:
			return provider.ProviderConfig{}, d.Errf("unknown provider subdirective: %s", d.Val())
		}
	}
	if cfg.ProviderType == "" {
		return provider.ProviderConfig{}, d.Err("provider_type is required")
	}
	// Normalization (defaults, fallbacks) is applied authoritatively at
	// provision time, which must also handle JSON-only configs.
	return cfg, nil
}

func parseProviderIntArg(d *caddyfile.Dispenser) (int, error) {
	if !d.NextArg() {
		return 0, d.ArgErr()
	}
	v, err := strconv.Atoi(d.Val())
	if err != nil {
		return 0, err
	}
	if d.NextArg() {
		return 0, d.ArgErr()
	}
	return v, nil
}

func parseProviderTypes(d *caddyfile.Dispenser, app *App) error {
	if len(app.ProviderTypes) > 0 {
		return d.Err("provider_types already configured")
	}
	// Parse the block on an isolated segment so iterating its inner tokens does
	// not disturb the shared dispenser's block bookkeeping for the surrounding
	// agent_gateway directive loop.
	seg := d.NewFromNextSegment()
	if !seg.Next() {
		return d.Err("expected provider_types directive")
	}
	if seg.NextArg() {
		return seg.ArgErr()
	}
	seen := map[string]struct{}{}
	for seg.NextBlock(0) {
		providerType := strings.ToLower(strings.TrimSpace(seg.Val()))
		if providerType == "" {
			return seg.Err("provider type is required")
		}
		// Listing a provider type enables it. Every registered type that is
		// not listed is disabled (exclusive policy applied at startup).
		if seg.NextArg() {
			return seg.ArgErr()
		}
		if _, exists := seen[providerType]; exists {
			return seg.Errf("duplicate provider type %q", providerType)
		}
		seen[providerType] = struct{}{}
		app.ProviderTypes = append(app.ProviderTypes, provider.ProviderTypeSetting{
			ProviderType: providerType,
			Enabled:      true,
		})
	}
	if len(app.ProviderTypes) == 0 {
		return d.Err("provider_types must declare at least one provider type")
	}
	return nil
}

func parseConfigStore(d *caddyfile.Dispenser, app *App) error {
	if len(app.ConfigStoreRaw) != 0 {
		return d.Err("config_store already configured")
	}
	if !d.NextArg() {
		return d.ArgErr()
	}
	name := d.Val()
	modID := "agent_gateway.config_store_backends." + name
	unm, err := caddyfile.UnmarshalModule(d, modID)
	if err != nil {
		return err
	}

	app.ConfigStoreRaw = caddy.ModuleMap{
		name: caddyconfig.JSON(unm, nil),
	}
	return nil
}

func parseRoute(d *caddyfile.Dispenser, app *App) error {
	route, err := parseRouteSegment(d, app)
	if err != nil {
		return err
	}

	for _, declared := range app.LLMRoutes {
		if declared.ID == route.ID {
			return d.Errf("duplicate route %q", route.ID)
		}
	}
	app.LLMRoutes = append(app.LLMRoutes, route)
	return nil
}

// ParseRouteSegment parses a route declaration from the current directive or subdirective.
// The dispenser must already be positioned on the route directive token.
func parseRouteSegment(d *caddyfile.Dispenser, app *App) (routecore.AgentRouteConfig, error) {
	seg := d.NewFromNextSegment()
	if !seg.Next() {
		return routecore.AgentRouteConfig{}, d.Err("expected route directive")
	}

	args := seg.RemainingArgsRaw()
	if len(args) != 1 {
		return routecore.AgentRouteConfig{}, seg.ArgErr()
	}

	routeID := strings.Trim(args[0], "\"`")
	route := llmroutepkg.LLMRouteConfig{
		AgentRouteConfig: routecore.AgentRouteConfig{
			ID:   routeID,
			Kind: routecore.RouteKindLLM,
		},
	}
	logicalTargetPolicy := false

	for seg.NextBlock(0) {
		name := seg.Val()
		args := seg.RemainingArgsRaw()
		switch name {
		case "protocol":
			if len(args) != 1 {
				return routecore.AgentRouteConfig{}, seg.ArgErr()
			}
			route.Protocol = routecore.RouteProtocol(strings.Trim(args[0], "\"`"))
		case "host":
			if len(args) != 1 {
				return routecore.AgentRouteConfig{}, seg.ArgErr()
			}
			route.MatchPolicy.Host = strings.Trim(args[0], "\"`")
		case "path_prefix":
			if len(args) != 1 {
				return routecore.AgentRouteConfig{}, seg.ArgErr()
			}
			route.MatchPolicy.PathPrefix = strings.Trim(args[0], "\"`")
		case "method":
			if len(args) == 0 {
				return routecore.AgentRouteConfig{}, seg.ArgErr()
			}
			for _, arg := range args {
				route.MatchPolicy.Methods = append(route.MatchPolicy.Methods, strings.Trim(arg, "\"`"))
			}
		case "require_virtual_key":
			if len(args) == 0 {
				route.AuthPolicy.RequireVirtualKey = true
				continue
			}
			if len(args) != 1 {
				return routecore.AgentRouteConfig{}, seg.ArgErr()
			}
			v, err := strconv.ParseBool(strings.Trim(args[0], "\"`"))
			if err != nil {
				return routecore.AgentRouteConfig{}, seg.Errf("invalid require_virtual_key value: %s", args[0])
			}
			route.AuthPolicy.RequireVirtualKey = v
		case "target":
			if len(args) < 2 {
				return routecore.AgentRouteConfig{}, seg.ArgErr()
			}
			switch strings.Trim(args[0], "\"`") {
			case "provider":
				if len(args) != 2 {
					return routecore.AgentRouteConfig{}, seg.ArgErr()
				}
				if logicalTargetPolicy {
					return routecore.AgentRouteConfig{}, seg.Err("target provider cannot be mixed with model targets")
				}
				directPolicy, ok := llmroutepkg.DirectProviderPolicyOf(route.TargetPolicy)
				if route.TargetPolicy != nil && !ok {
					return routecore.AgentRouteConfig{}, seg.Err("target provider cannot be mixed with model targets")
				}
				if !ok {
					directPolicy = &llmroutepkg.RouteDirectProviderPolicy{}
					route.TargetPolicy = directPolicy
				}
				if directPolicy.ProviderTarget.ProviderID != "" {
					return routecore.AgentRouteConfig{}, seg.Err("target provider may appear at most once")
				}
				directPolicy.ProviderTarget = llmroutepkg.DirectProviderTarget{ProviderID: strings.Trim(args[1], "\"`")}
			case "model":
				return routecore.AgentRouteConfig{}, seg.Err("target model is no longer supported in the Caddyfile; static routes must use a direct-provider target")
			default:
				return routecore.AgentRouteConfig{}, seg.ArgErr()
			}
		case "target_policy":
			if len(args) != 1 {
				return routecore.AgentRouteConfig{}, seg.ArgErr()
			}
			if err := parseRouteTargetPolicy(seg, &route, strings.Trim(args[0], "\"`"), &logicalTargetPolicy); err != nil {
				return routecore.AgentRouteConfig{}, err
			}
		default:
			return routecore.AgentRouteConfig{}, seg.Errf("unknown subdirective: %s", name)
		}
	}

	if err := route.ValidateStaticDefinition(); err != nil {
		return routecore.AgentRouteConfig{}, err
	}
	return route.ToConfig()
}

func parseRouteTargetPolicy(seg *caddyfile.Dispenser, route *llmroutepkg.LLMRouteConfig, kind string, logicalTargetPolicy *bool) error {
	var directPolicy *llmroutepkg.RouteDirectProviderPolicy
	switch llmroutepkg.RouteTargetPolicyKind(kind) {
	case llmroutepkg.RouteTargetPolicyKindDirectProvider:
		directPolicy = &llmroutepkg.RouteDirectProviderPolicy{}
	case llmroutepkg.RouteTargetPolicyKindLogicalModel:
		*logicalTargetPolicy = true
		return seg.Err("target_policy logical-model is no longer supported in the Caddyfile; static routes must use direct-provider")
	default:
		return seg.Errf("unknown target_policy kind: %s", kind)
	}

	for seg.NextBlock(1) {
		name := seg.Val()
		args := seg.RemainingArgsRaw()
		switch name {
		case "provider":
			if len(args) != 1 {
				return seg.ArgErr()
			}
			directPolicy.ProviderID = strings.Trim(args[0], "\"`")
		case "model_selector_strategy":
			return seg.Err("model_selector_strategy may only be used in target_policy logical-model")
		case "credential_selector":
			if len(args) != 1 {
				return seg.ArgErr()
			}
			directPolicy.CredentialSelectorValue = llmroutepkg.RouteCredentialSelectStrategy(strings.Trim(args[0], "\"`"))
		case "credential_scope_order":
			if len(args) == 0 {
				return seg.ArgErr()
			}
			directPolicy.CredentialScopeOrderValue = nil
			for _, arg := range args {
				directPolicy.CredentialScopeOrderValue = append(directPolicy.CredentialScopeOrderValue, llmroutepkg.RouteCredentialScope(strings.Trim(arg, "\"`")))
			}
		case "credential_type_order":
			if len(args) == 0 {
				return seg.ArgErr()
			}
			directPolicy.CredentialTypeOrderValue = nil
			for _, arg := range args {
				directPolicy.CredentialTypeOrderValue = append(directPolicy.CredentialTypeOrderValue, llmroutepkg.RouteCredentialType(strings.Trim(arg, "\"`")))
			}
		case "fallback":
			return seg.Err("fallback may only be used in target_policy logical-model")
		default:
			return seg.Errf("unknown target_policy subdirective: %s", name)
		}
	}
	route.TargetPolicy = directPolicy
	return nil
}
