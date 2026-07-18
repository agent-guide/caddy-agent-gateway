package usage

import "testing"

func TestOTLPConfigNormalizedDefaults(t *testing.T) {
	cfg := OTLPConfig{Endpoint: "  127.0.0.1:4317  ", Protocol: "GRPC"}.Normalized()
	if cfg.Endpoint != "127.0.0.1:4317" {
		t.Fatalf("endpoint = %q, want trimmed", cfg.Endpoint)
	}
	if cfg.Protocol != OTLPProtocolGRPC {
		t.Fatalf("protocol = %q, want %q", cfg.Protocol, OTLPProtocolGRPC)
	}
	if cfg.ServiceName != "agent-gateway" {
		t.Fatalf("service name = %q, want agent-gateway", cfg.ServiceName)
	}
	if cfg.TimeoutSeconds != 10 {
		t.Fatalf("timeout = %d, want 10", cfg.TimeoutSeconds)
	}
}

func TestOTLPConfigValidate(t *testing.T) {
	if err := (OTLPConfig{}).Validate(); err != nil {
		t.Fatalf("disabled config must validate, got %v", err)
	}
	if err := (OTLPConfig{Protocol: "bogus"}).Validate(); err != nil {
		t.Fatalf("disabled config ignores protocol, got %v", err)
	}
	if err := (OTLPConfig{Endpoint: "127.0.0.1:4318", Protocol: "http"}).Validate(); err != nil {
		t.Fatalf("http protocol must validate, got %v", err)
	}
	if err := (OTLPConfig{Endpoint: "127.0.0.1:4317", Protocol: "bogus"}).Validate(); err == nil {
		t.Fatal("enabled config with unknown protocol must fail validation")
	}
}

func TestOTLPConfigEnabled(t *testing.T) {
	if (OTLPConfig{}).Enabled() {
		t.Fatal("empty endpoint must be disabled")
	}
	if (OTLPConfig{Endpoint: "   "}).Enabled() {
		t.Fatal("blank endpoint must be disabled")
	}
	if !(OTLPConfig{Endpoint: "collector:4317"}).Enabled() {
		t.Fatal("set endpoint must be enabled")
	}
}
