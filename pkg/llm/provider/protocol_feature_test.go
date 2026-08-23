package provider

import (
	"slices"
	"testing"
)

func TestProtocolRequirementSetIsImmutableSortedAndDeduplicated(t *testing.T) {
	input := map[ProtocolFeature][]RequirementReason{
		FeatureAnthropicReasoningReplay: {"signed", "signed", "redacted"},
		FeatureAnthropicNativeResponse:  {"opaque"},
	}
	set, err := NewProtocolRequirementSet(input)
	if err != nil {
		t.Fatal(err)
	}
	delete(input, FeatureAnthropicNativeResponse)
	input[FeatureAnthropicReasoningReplay][0] = "mutated"
	if got := set.Features(); !slices.Equal(got, []ProtocolFeature{FeatureAnthropicNativeResponse, FeatureAnthropicReasoningReplay}) {
		t.Fatalf("features = %v", got)
	}
	if got := set.Reasons(FeatureAnthropicReasoningReplay); !slices.Equal(got, []RequirementReason{"signed", "redacted"}) {
		t.Fatalf("reasons = %v", got)
	}
	reasons := set.Reasons(FeatureAnthropicReasoningReplay)
	reasons[0] = "caller mutation"
	if set.Reasons(FeatureAnthropicReasoningReplay)[0] != "signed" {
		t.Fatal("Reasons exposed internal storage")
	}
}

func TestProtocolRequirementSetRejectsModeSelectionFeature(t *testing.T) {
	_, err := NewProtocolRequirementSet(map[ProtocolFeature][]RequirementReason{
		FeatureAnthropicStreamRelay: {"invalid"},
	})
	if err == nil {
		t.Fatal("mode-selection feature was accepted as a request requirement")
	}
}

func TestProviderCapabilitiesValidateFeatureDependencies(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("missing protocol feature dependency did not panic")
		}
	}()
	RegisterProviderTypeCapabilities("test-missing-feature-dependency", ProviderTypeCapabilities{
		Dialect:          ProtocolDialectAnthropic,
		ProtocolFeatures: map[ProtocolFeature]struct{}{FeatureAnthropicNativeHistoryReplay: {}},
	})
}
