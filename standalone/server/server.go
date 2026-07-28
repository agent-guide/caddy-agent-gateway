package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/agent-guide/agent-gateway/internal/observability/einotap"
	"github.com/agent-guide/agent-gateway/internal/observability/otelexport"
	"github.com/agent-guide/agent-gateway/internal/observability/pipeline"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/pkg/admin"
	"github.com/agent-guide/agent-gateway/pkg/cliauth"
	configstore "github.com/agent-guide/agent-gateway/pkg/configstore"
	"github.com/agent-guide/agent-gateway/pkg/configstore/schema"
	configstoresqlite "github.com/agent-guide/agent-gateway/pkg/configstore/sqlite"
	"github.com/agent-guide/agent-gateway/pkg/dispatcher"
	anthropicapi "github.com/agent-guide/agent-gateway/pkg/dispatcher/llmapi/anthropic"
	ccapi "github.com/agent-guide/agent-gateway/pkg/dispatcher/llmapi/cc"
	openaiapi "github.com/agent-guide/agent-gateway/pkg/dispatcher/llmapi/openai"
	"github.com/agent-guide/agent-gateway/pkg/gateway"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	"github.com/agent-guide/agent-gateway/pkg/gatewaybundle"
	"github.com/agent-guide/agent-gateway/pkg/llm/credentialmgr"
	credentialmgrscheduler "github.com/agent-guide/agent-gateway/pkg/llm/credentialmgr/scheduler"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

const shutdownTimeout = 10 * time.Second

type Options struct {
	Addr               string
	AdminAddr          string
	ConfigStorePath    string
	StaticConfigPath   string
	ProviderTypes      []string
	AdminBasicAuthHash string
	Metrics            usage.Config
}

func (o *Options) setDefaults() {
	if o.Addr == "" {
		o.Addr = "127.0.0.1:8080"
	}
	if o.AdminAddr == "" {
		o.AdminAddr = o.Addr
	}
	if o.ConfigStorePath == "" {
		o.ConfigStorePath = "./data/configstore.db"
	}
}

func Run(ctx context.Context, opts Options) error {
	opts.setDefaults()
	if err := configureProviderTypes(opts.ProviderTypes); err != nil {
		return err
	}

	logger, err := zap.NewProduction()
	if err != nil {
		return err
	}
	defer func() { _ = logger.Sync() }()

	agentGateway, cliauthRefresher, usageService, err := bootstrapGateway(ctx, opts, logger)
	if err != nil {
		return err
	}
	defer agentGateway.Close()
	if usageService != nil {
		usageService.Start()
		defer func() { _ = usageService.Close() }()
	}
	if cliauthRefresher != nil {
		cliauthRefresher.Start(ctx)
		defer cliauthRefresher.Stop()
	}

	adminHandler, err := protectAdminHandler(admin.NewHandler(agentGateway, logger.Named("admin")), opts.AdminBasicAuthHash)
	if err != nil {
		return fmt.Errorf("configure admin auth: %w", err)
	}
	warnIfAdminUnprotected(logger, opts.AdminAddr, opts.AdminBasicAuthHash)
	dispatchHandler, err := newDispatchHandler(agentGateway, logger.Named("dispatcher"))
	if err != nil {
		return err
	}

	var servers []*http.Server
	if opts.AdminAddr == opts.Addr {
		servers = append(servers, &http.Server{
			Addr:    opts.Addr,
			Handler: newRouter(adminHandler, dispatchHandler),
		})
	} else {
		servers = append(servers,
			&http.Server{Addr: opts.Addr, Handler: newGatewayRouter(dispatchHandler)},
			&http.Server{Addr: opts.AdminAddr, Handler: newAdminRouter(adminHandler)},
		)
	}

	errCh := make(chan error, len(servers))
	for _, srv := range servers {
		server := srv
		go func() {
			logger.Info("standalone server listening", zap.String("addr", server.Addr))
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
				return
			}
			errCh <- nil
		}()
	}

	select {
	case <-ctx.Done():
		return shutdownServers(context.Background(), servers)
	case err := <-errCh:
		if err != nil {
			_ = shutdownServers(context.Background(), servers)
			return err
		}
		return shutdownServers(context.Background(), servers)
	}
}

func configureProviderTypes(providerTypes []string) error {
	if len(providerTypes) == 0 {
		return nil
	}
	settings := make([]provider.ProviderTypeSetting, 0, len(providerTypes))
	seen := map[string]struct{}{}
	for _, providerType := range providerTypes {
		name := strings.ToLower(strings.TrimSpace(providerType))
		if name == "" {
			return fmt.Errorf("provider type must not be empty")
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("duplicate provider type %q", name)
		}
		seen[name] = struct{}{}
		settings = append(settings, provider.ProviderTypeSetting{ProviderType: name, Enabled: true})
	}
	return provider.ConfigureProviderTypes(settings, true)
}

func newDispatchHandler(agentGateway *gateway.AgentGateway, logger *zap.Logger) (*dispatcher.Handler, error) {
	return dispatcher.NewHandler(
		agentGateway,
		newLLMAPIHandlers(logger),
		logger,
		dispatcher.HandlerOptions{EnableMCP: true, EnableAgent: true},
	), nil
}

func bootstrapGateway(ctx context.Context, opts Options, logger *zap.Logger) (*gateway.AgentGateway, *cliauth.AutoRefresher, *usage.UsageService, error) {
	staticConfig, err := loadStaticConfig(ctx, opts)
	if err != nil {
		return nil, nil, nil, err
	}

	configstoreBackend, err := configstore.OpenBackend(ctx, "sqlite", configstoresqlite.Config{SQLitePath: opts.ConfigStorePath}, logger.Named("sqlite"))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open config store: %w", err)
	}
	if err := schema.RegisterDefaultStores(configstoreBackend); err != nil {
		return nil, nil, nil, err
	}
	usageService := newUsageService(configstoreBackend, logger, opts.Metrics)

	credentialStore, err := configstoreBackend.Get(schema.StoreCredentials)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get credential store: %w", err)
	}
	credentialScheduler := credentialmgrscheduler.NewScheduler(nil)
	credentialManager := credentialmgr.NewManager(credentialStore)
	if schedulerListener, ok := credentialScheduler.(credentialmgr.CredentialLifecycleListener); ok {
		credentialManager.AddListener(schedulerListener)
	}

	cliauthManager := cliauth.NewManager()
	cliauthManager.SetCredentialManager(credentialManager)
	cliauthRefresher := cliauth.NewAutoRefresher(cliauth.WrapSharedCredentialManager(credentialManager), cliauthManager)

	if err := credentialManager.Load(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("load credentials: %w", err)
	}
	if err := cliauthRefresher.Load(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("load cliauth credentials: %w", err)
	}
	agentGateway := gateway.NewAgentGateway()
	if err := agentGateway.Bootstrap(ctx, gateway.BootstrapOptions{
		StaticLLMRoutes:     staticConfig.LLMRoutes,
		StaticProviders:     staticConfig.Providers,
		ConfigStoreBackend:  configstoreBackend,
		CLIAuthManager:      cliauthManager,
		CLIAuthRefresher:    cliauthRefresher,
		CredentialManager:   credentialManager,
		CredentialScheduler: credentialScheduler,
		UsageObserver:       usageService.Observer(),
		UsageQuery:          usageService.Query(),
		UsageStats:          usageService,
		UsagePrometheus:     usageService.Prometheus(),
		UsageConfig:         opts.Metrics.Normalized(),
	}); err != nil {
		return nil, nil, nil, fmt.Errorf("bootstrap agent gateway: %w", err)
	}
	if attribution := usageService.Attribution(); attribution != nil {
		if manager := agentGateway.AgentManager(); manager != nil {
			attribution.Set(manager)
		}
	}
	return agentGateway, cliauthRefresher, usageService, nil
}

func newUsageService(backend configstore.ConfigStoreBackend, logger *zap.Logger, cfg usage.Config) *usage.UsageService {
	einotap.Register()
	dbProvider, ok := backend.(usage.SQLDBProvider)
	if !ok {
		return usage.NewUsageService(nil, nil)
	}
	sink, query, err := pipeline.NewSQLiteSink(dbProvider, cfg)
	if err != nil {
		if logger != nil {
			logger.Warn("usage metrics sqlite sink unavailable", zap.Error(err))
		}
		return usage.NewUsageService(nil, nil)
	}
	promSink := pipeline.NewPrometheusSink()
	sinks := []pipeline.Sink{sink, promSink}
	if otlp := cfg.Normalized().OTLP; otlp.Enabled() {
		exporter, err := otelexport.New(context.Background(), otlp)
		if err != nil {
			if logger != nil {
				logger.Warn("otlp usage exporter unavailable", zap.Error(err))
			}
		} else {
			sinks = append(sinks, pipeline.NewOpenTelemetrySink(exporter))
			if otlp.Components {
				otelexport.EnableComponentTap(exporter)
			}
			if logger != nil {
				logger.Info("otlp usage export enabled",
					zap.String("endpoint", otlp.Endpoint), zap.String("protocol", otlp.Protocol),
					zap.Bool("components", otlp.Components))
			}
		}
	}
	svc := usage.NewUsageService(pipeline.NewEventPipeline(4096, sinks...), query)
	svc.AttachPrometheus(promSink)
	return svc
}

type staticConfig struct {
	Providers map[string]provider.Provider
	LLMRoutes []routecore.AgentRouteConfig
}

func loadStaticConfig(ctx context.Context, opts Options) (*staticConfig, error) {
	_ = ctx
	cfg := &staticConfig{
		Providers: map[string]provider.Provider{},
	}
	if opts.StaticConfigPath == "" {
		return cfg, nil
	}

	bundle, err := gatewaybundle.LoadFile(opts.StaticConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load static gateway bundle: %w", err)
	}
	if err := bundle.ValidateForStaticConfig(); err != nil {
		return nil, fmt.Errorf("validate static gateway bundle: %w", err)
	}
	providers, err := instantiateStaticProviders(bundle)
	if err != nil {
		return nil, err
	}
	cfg.Providers = providers
	cfg.LLMRoutes = append([]routecore.AgentRouteConfig(nil), bundle.LLMRoutes...)
	return cfg, nil
}

func instantiateStaticProviders(bundle *gatewaybundle.GatewayBundle) (map[string]provider.Provider, error) {
	out := make(map[string]provider.Provider, len(bundle.Providers))
	for _, cfg := range bundle.Providers {
		cfg = provider.NormalizeConfig(cfg, cfg.Id, cfg.ProviderType)
		prov, err := provider.NewProvider(cfg)
		if err != nil {
			return nil, fmt.Errorf("init static provider %q: %w", cfg.Id, err)
		}
		out[cfg.Id] = prov
	}
	return out, nil
}

func newLLMAPIHandlers(logger *zap.Logger) map[string]dispatcher.LLMApiHandler {
	openAIHandler := openaiapi.NewHandler()
	openAIHandler.SetLogger(logger.Named("openai"))
	anthropicHandler := anthropicapi.NewHandler(nil)
	anthropicHandler.SetLogger(logger.Named("anthropic"))
	ccHandler := ccapi.NewHandler(nil)
	ccHandler.SetLogger(logger.Named("cc"))
	return map[string]dispatcher.LLMApiHandler{
		openAIHandler.Name():    openAIHandler,
		anthropicHandler.Name(): anthropicHandler,
		ccHandler.Name():        ccHandler,
	}
}

func newRouter(adminHandler http.Handler, dispatchHandler http.Handler) http.Handler {
	router := baseRouter()
	mountAdmin(router, adminHandler)
	router.NoRoute(gin.WrapH(dispatchHandler))
	return router
}

func newGatewayRouter(dispatchHandler http.Handler) http.Handler {
	router := baseRouter()
	router.NoRoute(gin.WrapH(dispatchHandler))
	return router
}

func newAdminRouter(adminHandler http.Handler) http.Handler {
	router := baseRouter()
	mountAdmin(router, adminHandler)
	return router
}

// protectAdminHandler wraps the admin handler with Basic Auth when basicAuth is
// configured as "username:bcrypt-hash". A malformed value is rejected here so
// the daemon fails fast at startup instead of returning errors per request. An
// empty value disables auth and returns the handler unwrapped.
func protectAdminHandler(next http.Handler, basicAuth string) (http.Handler, error) {
	basicAuth = strings.TrimSpace(basicAuth)
	if basicAuth == "" {
		return next, nil
	}
	username, passwordHash, ok := strings.Cut(basicAuth, ":")
	username = strings.TrimSpace(username)
	passwordHash = strings.TrimSpace(passwordHash)
	if !ok || username == "" || passwordHash == "" {
		return nil, fmt.Errorf("admin basic auth must be configured as username:bcrypt-hash")
	}
	if _, err := bcrypt.Cost([]byte(passwordHash)); err != nil {
		return nil, fmt.Errorf("admin basic auth password is not a valid bcrypt hash: %w", err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if adminAuthExempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		gotUser, gotPassword, ok := r.BasicAuth()
		passwordErr := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(gotPassword))
		if !ok ||
			subtle.ConstantTimeCompare([]byte(gotUser), []byte(username)) != 1 ||
			passwordErr != nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="agent-gateway-admin"`)
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}), nil
}

// adminAuthExempt reports whether a request must bypass Basic Auth. CORS
// preflight requests carry no credentials by browser spec, and the health
// probe is intentionally public, so both must reach the handler unauthenticated.
func adminAuthExempt(r *http.Request) bool {
	return r.Method == http.MethodOptions || r.URL.Path == "/admin/health"
}

// warnIfAdminUnprotected logs a loud warning when the admin API has no Basic
// Auth and is reachable beyond loopback, which exposes full admin access to
// anyone who can reach the listener.
func warnIfAdminUnprotected(logger *zap.Logger, adminAddr, basicAuth string) {
	if strings.TrimSpace(basicAuth) != "" {
		return
	}
	if isLoopbackListenAddr(adminAddr) {
		return
	}
	logger.Warn("admin API has no Basic Auth and is bound to a non-loopback address; anyone who can reach it has full admin access. Set AGW_ADMIN_BASIC_AUTH_HASH (or bind --admin-addr to loopback)",
		zap.String("admin_addr", adminAddr))
}

// isLoopbackListenAddr reports whether a host:port listen address is restricted
// to the loopback interface. A wildcard or unknown host is treated as exposed.
func isLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		host = strings.TrimSpace(addr)
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func baseRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	return router
}

func mountAdmin(router *gin.Engine, adminHandler http.Handler) {
	router.Any("/admin", gin.WrapH(adminHandler))
	router.Any("/admin/*path", gin.WrapH(adminHandler))
}

func shutdownServers(ctx context.Context, servers []*http.Server) error {
	shutdownCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	var out error
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			out = errors.Join(out, err)
		}
	}
	return out
}
