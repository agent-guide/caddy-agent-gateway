package adminclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/gateway/modelcatalog"
)

func TestListProvidersUsesBasicAuth(t *testing.T) {
	t.Parallel()

	providerCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/llm/providers":
			providerCalls++
			user, pass, ok := r.BasicAuth()
			if !ok || user != "admin" || pass != "secret" {
				t.Fatalf("unexpected basic auth: ok=%v user=%q pass=%q", ok, user, pass)
			}
			_ = json.NewEncoder(w).Encode(itemsResponse[Provider]{
				Items: []Provider{{ProviderConfig: ProviderConfig{Id: "openai-main", ProviderType: "openai"}}},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := New(Config{
		BaseURL:   srv.URL,
		BasicAuth: "admin:secret",
	})

	items, err := client.ListProviders(context.Background(), ProviderListOptions{})
	if err != nil {
		t.Fatalf("ListProviders error: %v", err)
	}
	if len(items) != 1 || items[0].Id != "openai-main" {
		t.Fatalf("unexpected providers: %+v", items)
	}
	if providerCalls != 1 {
		t.Fatalf("expected 1 provider call, got %d", providerCalls)
	}
}

func TestRejectsBasicAuthAndTokenTogether(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("request must not reach the server: %s", r.URL.Path)
	}))
	defer srv.Close()

	client := New(Config{
		BaseURL:   srv.URL,
		BasicAuth: "admin:secret",
		Token:     "bearer-token",
	})

	if _, err := client.ListProviders(context.Background(), ProviderListOptions{}); err == nil {
		t.Fatal("expected error when both Basic Auth and a bearer token are configured")
	}
}

func TestCreateProvider(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/llm/providers":
			if r.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", r.Method)
			}
			if got := r.Header.Get("X-Test-Admin"); got != "yes" {
				t.Fatalf("unexpected X-Test-Admin header: %q", got)
			}
			var req ProviderConfig
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if req.Id != "openai-main" || req.ProviderType != "openai" {
				t.Fatalf("unexpected request: %+v", req)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(Provider{
				ProviderConfig: req,
				Source:         "dynamic",
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := New(Config{
		BaseURL: srv.URL,
		Headers: []string{"X-Test-Admin: yes"},
	})

	created, err := client.CreateProvider(context.Background(), ProviderConfig{
		Id:           "openai-main",
		ProviderType: "openai",
	})
	if err != nil {
		t.Fatalf("CreateProvider error: %v", err)
	}
	if created.Id != "openai-main" || created.Source != "dynamic" {
		t.Fatalf("unexpected provider: %+v", created)
	}
}

func TestListLLMRoutesIncludesQuery(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/llm/routes" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("tag_prefix"); got != "prod-" {
			t.Fatalf("unexpected tag_prefix: %q", got)
		}
		_ = json.NewEncoder(w).Encode(itemsResponse[LLMRoute]{Items: []LLMRoute{}})
	}))
	defer srv.Close()

	client := New(Config{
		BaseURL: srv.URL,
		Token:   "preset-token",
	})

	if _, err := client.ListLLMRoutes(context.Background(), LLMRouteListOptions{TagPrefix: "prod-"}); err != nil {
		t.Fatalf("ListLLMRoutes error: %v", err)
	}
}

func TestListProviderTypes(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/llm/provider_types" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(itemsResponse[ProviderType]{
			Items: []ProviderType{{ProviderType: "openai", Enabled: true}},
		})
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, Token: "preset-token"})
	items, err := client.ListProviderTypes(context.Background())
	if err != nil {
		t.Fatalf("ListProviderTypes error: %v", err)
	}
	if len(items) != 1 || items[0].ProviderType != "openai" {
		t.Fatalf("unexpected items: %+v", items)
	}
}

func TestRefreshProviderModels(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/llm/models/providers/openai-main/refresh" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(RefreshDiscoveredModelsResponse{
			ProviderID: "openai-main",
			Items: []DiscoveredModel{{
				ProviderID:    "openai-main",
				ProviderType:  "openai",
				UpstreamModel: "gpt-4.1",
			}},
		})
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, Token: "preset-token"})
	resp, err := client.RefreshProviderModels(context.Background(), "openai-main")
	if err != nil {
		t.Fatalf("RefreshProviderModels error: %v", err)
	}
	if resp.ProviderID != "openai-main" || len(resp.Items) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestCreateManagedModel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if r.URL.Path != "/admin/llm/models/managed" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req modelcatalog.ManagedModel
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ManagedModel{
			ManagedModel: modelcatalog.ManagedModel{
				ProviderID:    "openai-main",
				UpstreamModel: "gpt-4.1",
				Enabled:       true,
			},
		})
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, Token: "preset-token"})
	resp, err := client.CreateManagedModel(context.Background(), modelcatalog.ManagedModel{
		ProviderID:    "openai-main",
		UpstreamModel: "gpt-4.1",
		Enabled:       true,
	})
	if err != nil {
		t.Fatalf("CreateManagedModel error: %v", err)
	}
	if resp.ProviderID != "openai-main" || resp.UpstreamModel != "gpt-4.1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestUpdateManagedModel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if r.URL.Path != "/admin/llm/models/managed/openai-main/gpt-4.1" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req modelcatalog.ManagedModel
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(ManagedModel{
			ManagedModel: modelcatalog.ManagedModel{
				ProviderID:    "openai-main",
				UpstreamModel: "gpt-4.1",
				Enabled:       true,
			},
		})
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, Token: "preset-token"})
	resp, err := client.UpdateManagedModel(context.Background(), "openai-main", "gpt-4.1", modelcatalog.ManagedModel{
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("UpdateManagedModel error: %v", err)
	}
	if resp.ProviderID != "openai-main" || resp.UpstreamModel != "gpt-4.1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}
