package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/agent-guide/agent-gateway/pkg/dispatcher/llmapi/openai"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/openai"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

func TestLoadStaticConfig(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")

	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	writeFile(t, path, `
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle
providers:
  - id: openai-main
    provider_type: openai
    api_key: ${OPENAI_API_KEY}
llmRoutes:
  - id: chat-prod
    protocol: openai
    match:
      path_prefix: /
      methods:
        - POST
    auth_policy:
      require_virtual_key: true
    target_policy:
      provider_target:
        provider_id: openai-main
`)

	cfg, err := loadStaticConfig(context.Background(), Options{StaticConfigPath: path})
	if err != nil {
		t.Fatalf("loadStaticConfig() error = %v", err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("len(Providers) = %d, want 1", len(cfg.Providers))
	}
	if _, ok := cfg.Providers["openai-main"]; !ok {
		t.Fatalf("Providers missing openai-main: %#v", cfg.Providers)
	}
	if len(cfg.LLMRoutes) != 1 || cfg.LLMRoutes[0].ID != "chat-prod" {
		t.Fatalf("LLMRoutes = %#v", cfg.LLMRoutes)
	}
}

func TestBootstrapGatewayWithStaticConfig(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")

	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "gateway.yaml")
	dbPath := filepath.Join(dir, "configstore.db")
	writeFile(t, bundlePath, `
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle
providers:
  - id: openai-main
    provider_type: openai
    api_key: ${OPENAI_API_KEY}
llmRoutes:
  - id: chat-prod
    protocol: openai
    match:
      path_prefix: /
      methods:
        - POST
    auth_policy:
      require_virtual_key: true
    target_policy:
      provider_target:
        provider_id: openai-main
`)

	gw, usageService, err := bootstrapGateway(context.Background(), Options{
		ConfigStorePath:  dbPath,
		StaticConfigPath: bundlePath,
	}, zap.NewNop())
	if err != nil {
		t.Fatalf("bootstrapGateway() error = %v", err)
	}
	if usageService == nil {
		t.Fatal("bootstrapGateway() usageService = nil")
	}
	if gw.ProviderManager() == nil || !gw.ProviderManager().IsStatic("openai-main") {
		t.Fatal("expected static provider openai-main")
	}
	if gw.AgentRouteConfigManager() == nil || !gw.AgentRouteConfigManager().IsStatic("chat-prod") {
		t.Fatal("expected static route chat-prod")
	}
}

func TestProtectAdminHandlerUsesHashedConfiguredPassword(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("generate hash: %v", err)
	}
	var called bool
	handler, err := protectAdminHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}), "admin:"+string(hash))
	if err != nil {
		t.Fatalf("protectAdminHandler() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/llm/providers", nil)
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if !called {
		t.Fatal("wrapped handler was not called")
	}
}

func TestProtectAdminHandlerRejectsHashAsRequestPassword(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("generate hash: %v", err)
	}
	handler, err := protectAdminHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("wrapped handler should not be called")
	}), "admin:"+string(hash))
	if err != nil {
		t.Fatalf("protectAdminHandler() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/llm/providers", nil)
	req.SetBasicAuth("admin", string(hash))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestProtectAdminHandlerExemptsHealthAndPreflight(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("generate hash: %v", err)
	}
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{name: "health probe", method: http.MethodGet, path: "/admin/health"},
		{name: "cors preflight", method: http.MethodOptions, path: "/admin/llm/providers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var called bool
			handler, err := protectAdminHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}), "admin:"+string(hash))
			if err != nil {
				t.Fatalf("protectAdminHandler() error = %v", err)
			}

			// No credentials supplied: the exempt request must still reach the handler.
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if !called {
				t.Fatalf("exempt request %s %s did not reach the wrapped handler", tc.method, tc.path)
			}
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
			}
		})
	}
}

func TestProtectAdminHandlerRejectsInvalidConfiguration(t *testing.T) {
	noop := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	cases := []struct {
		name      string
		basicAuth string
	}{
		{name: "non-bcrypt hash", basicAuth: "admin:secret"},
		{name: "missing separator", basicAuth: "adminsecret"},
		{name: "empty username", basicAuth: ":$2a$10$abc"},
		{name: "empty hash", basicAuth: "admin:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := protectAdminHandler(noop, tc.basicAuth); err == nil {
				t.Fatalf("protectAdminHandler(%q) error = nil, want startup error", tc.basicAuth)
			}
		})
	}
}

func TestProtectAdminHandlerEmptyConfigDisablesAuth(t *testing.T) {
	noop := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler, err := protectAdminHandler(noop, "  ")
	if err != nil {
		t.Fatalf("protectAdminHandler() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/llm/providers", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d (auth disabled)", rec.Code, http.StatusNoContent)
	}
}

func TestIsLoopbackListenAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{addr: "localhost:8019", want: true},
		{addr: "127.0.0.1:8019", want: true},
		{addr: "[::1]:8019", want: true},
		{addr: "0.0.0.0:8019", want: false},
		{addr: "192.168.1.10:8019", want: false},
		{addr: ":8019", want: false},
		{addr: "example.com:8019", want: false},
	}
	for _, tc := range cases {
		if got := isLoopbackListenAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackListenAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestLoadStaticConfigRejectsManagedModels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	writeFile(t, path, `
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle
managedModels:
  - provider_id: openai-main
    upstream_model: gpt-4.1
`)

	_, err := loadStaticConfig(context.Background(), Options{StaticConfigPath: path})
	if err == nil {
		t.Fatal("expected static config managedModels to fail")
	}
}

func TestLoadStaticConfigRejectsVirtualKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	writeFile(t, path, `
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle
virtualKeys:
  - id: vk-local-test
`)

	_, err := loadStaticConfig(context.Background(), Options{StaticConfigPath: path})
	if err == nil {
		t.Fatal("expected static config virtualKeys to fail")
	}
}

func TestLoadStaticConfigRejectsLogicalModelRoutes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	writeFile(t, path, `
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle
providers:
  - id: openai-main
    provider_type: openai
llmRoutes:
  - id: chat-prod
    protocol: openai
    match:
      path_prefix: /
      methods:
        - POST
    auth_policy:
      require_virtual_key: true
    target_policy:
      default_model: chat-default
      model_targets:
        - name: chat-default
          candidates:
            - provider_id: openai-main
              upstream_model: gpt-4.1
              default: true
`)

	_, err := loadStaticConfig(context.Background(), Options{StaticConfigPath: path})
	if err == nil {
		t.Fatal("expected static config logical-model routes to fail")
	}
}

func TestOptionsDefaultCredentialRefreshCommand(t *testing.T) {
	opts := Options{}
	opts.setDefaults()
	if opts.CredentialRefreshCommand != "agw-auth" {
		t.Fatalf("CredentialRefreshCommand = %q, want agw-auth", opts.CredentialRefreshCommand)
	}
	if len(opts.CredentialRefreshArgs) != 1 || opts.CredentialRefreshArgs[0] != "refresh" {
		t.Fatalf("CredentialRefreshArgs = %v, want [refresh]", opts.CredentialRefreshArgs)
	}
}

func TestOptionsPreservesExplicitCredentialRefreshArgv(t *testing.T) {
	opts := Options{
		CredentialRefreshCommand: "/opt/custom-refresher",
		CredentialRefreshArgs:    []string{"--mode", "oauth"},
	}
	opts.setDefaults()
	if opts.CredentialRefreshCommand != "/opt/custom-refresher" {
		t.Fatalf("CredentialRefreshCommand = %q, want /opt/custom-refresher", opts.CredentialRefreshCommand)
	}
	if len(opts.CredentialRefreshArgs) != 2 || opts.CredentialRefreshArgs[0] != "--mode" || opts.CredentialRefreshArgs[1] != "oauth" {
		t.Fatalf("CredentialRefreshArgs = %v, want [--mode oauth]", opts.CredentialRefreshArgs)
	}
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
