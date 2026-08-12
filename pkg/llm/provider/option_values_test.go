package provider

import "testing"

func TestPositiveIntOptionAcceptsConfigurationRepresentations(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  int
	}{
		{name: "string", value: " 128000 ", want: 128000},
		{name: "int", value: 128000, want: 128000},
		{name: "int64", value: int64(128000), want: 128000},
		{name: "integral float", value: float64(128000), want: 128000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PositiveIntOption(map[string]any{"limit": tt.value}, "limit", 1)
			if err != nil || got != tt.want {
				t.Fatalf("PositiveIntOption() = %d, %v, want %d, nil", got, err, tt.want)
			}
		})
	}
}

func TestOptionValueParsersRejectInvalidValues(t *testing.T) {
	for _, value := range []any{0, -1, 1.5, "many", true} {
		if _, err := PositiveIntOption(map[string]any{"limit": value}, "limit", 1); err == nil {
			t.Errorf("PositiveIntOption(%#v) succeeded, want error", value)
		}
	}
	for _, value := range []any{"sometimes", 1} {
		if _, err := BoolOption(map[string]any{"enabled": value}, "enabled", false); err == nil {
			t.Errorf("BoolOption(%#v) succeeded, want error", value)
		}
	}
}

func TestOptionValueParsersUseFallbackAndParseBoolStrings(t *testing.T) {
	if got, err := PositiveIntOption(nil, "limit", 42); err != nil || got != 42 {
		t.Fatalf("PositiveIntOption fallback = %d, %v", got, err)
	}
	if got, err := BoolOption(map[string]any{"enabled": " true "}, "enabled", false); err != nil || !got {
		t.Fatalf("BoolOption() = %t, %v, want true, nil", got, err)
	}
}

func TestCapabilitiesFromOptionsAppliesOnlySelectedOverrides(t *testing.T) {
	defaults := ProviderCapabilities{Vision: true, Embeddings: true, ContextWindow: 128000, MaxOutputTokens: 8192}
	got, err := CapabilitiesFromOptions(map[string]any{
		"context_window":    "1000000",
		"max_output_tokens": 128000,
		"vision":            "false",
		"embeddings":        false,
	}, defaults, CapabilityContextWindow, CapabilityMaxOutputTokens, CapabilityVision)
	if err != nil {
		t.Fatalf("CapabilitiesFromOptions() error = %v", err)
	}
	if got.ContextWindow != 1000000 || got.MaxOutputTokens != 128000 || got.Vision || !got.Embeddings {
		t.Fatalf("capabilities = %+v, want selected overrides with embeddings unchanged", got)
	}
}

func TestCapabilitiesFromOptionsRejectsUnknownCapability(t *testing.T) {
	if _, err := CapabilitiesFromOptions(nil, ProviderCapabilities{}, CapabilityOption("audio")); err == nil {
		t.Fatal("CapabilitiesFromOptions() succeeded for unknown capability")
	}
}
