package provider

import (
	"fmt"
	"sort"
	"sync"
)

type ProtocolFeature string
type ProtocolFeatureClass string

const (
	ProtocolFeatureClassModeSelection ProtocolFeatureClass = "mode_selection"
	ProtocolFeatureClassRequirement   ProtocolFeatureClass = "requirement"

	FeatureAnthropicServerToolRequest   ProtocolFeature = "anthropic.server_tool_request"
	FeatureAnthropicNativeResponse      ProtocolFeature = "anthropic.native_response"
	FeatureAnthropicNativeHistoryReplay ProtocolFeature = "anthropic.native_history_replay"
	FeatureAnthropicReasoningReplay     ProtocolFeature = "anthropic.reasoning_replay"
	FeatureAnthropicStreamRelay         ProtocolFeature = "anthropic.native_stream_relay"
	FeatureAnthropicBodyRelay           ProtocolFeature = "anthropic.native_body_relay"
)

type RequirementReason string

const (
	ReasonAnthropicServerTool      RequirementReason = "anthropic_server_tool"
	ReasonAnthropicNativeHistory   RequirementReason = "anthropic_native_history"
	ReasonAnthropicSignedReasoning RequirementReason = "anthropic_signed_reasoning"
	ReasonAnthropicOpaqueContent   RequirementReason = "anthropic_opaque_content"
)

type ProtocolFeatureDefinition struct {
	ID        ProtocolFeature
	Dialect   ProtocolDialect
	Class     ProtocolFeatureClass
	DependsOn []ProtocolFeature
}

var protocolFeatures = struct {
	sync.RWMutex
	values map[ProtocolFeature]ProtocolFeatureDefinition
}{values: map[ProtocolFeature]ProtocolFeatureDefinition{}}

func RegisterProtocolFeature(def ProtocolFeatureDefinition) {
	if def.ID == "" || def.Dialect == "" || def.Class == "" {
		panic("provider: incomplete protocol feature definition")
	}
	protocolFeatures.Lock()
	defer protocolFeatures.Unlock()
	if _, exists := protocolFeatures.values[def.ID]; exists {
		panic("provider: duplicate protocol feature: " + string(def.ID))
	}
	for _, dependency := range def.DependsOn {
		registered, ok := protocolFeatures.values[dependency]
		if !ok {
			panic("provider: protocol feature dependency is not registered: " + string(dependency))
		}
		if registered.Dialect != def.Dialect {
			panic("provider: protocol feature dependency crosses dialects")
		}
	}
	def.DependsOn = append([]ProtocolFeature(nil), def.DependsOn...)
	protocolFeatures.values[def.ID] = def
}

func ProtocolFeatureDefinitionFor(id ProtocolFeature) (ProtocolFeatureDefinition, error) {
	protocolFeatures.RLock()
	def, ok := protocolFeatures.values[id]
	protocolFeatures.RUnlock()
	if !ok {
		return ProtocolFeatureDefinition{}, fmt.Errorf("provider: unknown protocol feature %q", id)
	}
	def.DependsOn = append([]ProtocolFeature(nil), def.DependsOn...)
	return def, nil
}

func validateProtocolFeatureSupport(capabilities ProviderTypeCapabilities) error {
	for feature := range capabilities.ProtocolFeatures {
		definition, err := ProtocolFeatureDefinitionFor(feature)
		if err != nil {
			return err
		}
		for _, dependency := range definition.DependsOn {
			if !capabilities.SupportsProtocolFeature(dependency) {
				return fmt.Errorf("provider: protocol feature %q requires %q", feature, dependency)
			}
		}
	}
	return nil
}

type ProtocolRequirementSet struct {
	values map[ProtocolFeature][]RequirementReason
}

func NewProtocolRequirementSet(values map[ProtocolFeature][]RequirementReason) (ProtocolRequirementSet, error) {
	set := ProtocolRequirementSet{values: make(map[ProtocolFeature][]RequirementReason, len(values))}
	for feature, reasons := range values {
		definition, err := ProtocolFeatureDefinitionFor(feature)
		if err != nil {
			return ProtocolRequirementSet{}, err
		}
		if definition.Class != ProtocolFeatureClassRequirement {
			return ProtocolRequirementSet{}, fmt.Errorf("provider: protocol feature %q is not a request requirement", feature)
		}
		seen := map[RequirementReason]struct{}{}
		for _, reason := range reasons {
			if reason == "" {
				continue
			}
			if _, duplicate := seen[reason]; duplicate {
				continue
			}
			seen[reason] = struct{}{}
			set.values[feature] = append(set.values[feature], reason)
		}
		if len(set.values[feature]) == 0 {
			set.values[feature] = nil
		}
	}
	return set, nil
}

func (s ProtocolRequirementSet) Features() []ProtocolFeature {
	features := make([]ProtocolFeature, 0, len(s.values))
	for feature := range s.values {
		features = append(features, feature)
	}
	sort.Slice(features, func(i, j int) bool { return features[i] < features[j] })
	return features
}

func (s ProtocolRequirementSet) Reasons(feature ProtocolFeature) []RequirementReason {
	return append([]RequirementReason(nil), s.values[feature]...)
}

func (s ProtocolRequirementSet) Empty() bool { return len(s.values) == 0 }

func CloneProtocolRequirementSet(s ProtocolRequirementSet) ProtocolRequirementSet {
	cloned := ProtocolRequirementSet{values: make(map[ProtocolFeature][]RequirementReason, len(s.values))}
	for feature, reasons := range s.values {
		cloned.values[feature] = append([]RequirementReason(nil), reasons...)
	}
	return cloned
}

func (s ProtocolRequirementSet) Equal(other ProtocolRequirementSet) bool {
	left, right := s.Features(), other.Features()
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
		leftReasons, rightReasons := s.Reasons(left[i]), other.Reasons(right[i])
		if len(leftReasons) != len(rightReasons) {
			return false
		}
		for j := range leftReasons {
			if leftReasons[j] != rightReasons[j] {
				return false
			}
		}
	}
	return true
}

func MissingProtocolFeatures(required ProtocolRequirementSet, supported map[ProtocolFeature]struct{}) []ProtocolFeature {
	var missing []ProtocolFeature
	for _, feature := range required.Features() {
		if _, ok := supported[feature]; !ok {
			missing = append(missing, feature)
		}
	}
	return missing
}

func init() {
	RegisterProtocolFeature(ProtocolFeatureDefinition{ID: FeatureAnthropicServerToolRequest, Dialect: ProtocolDialectAnthropic, Class: ProtocolFeatureClassRequirement})
	RegisterProtocolFeature(ProtocolFeatureDefinition{ID: FeatureAnthropicNativeResponse, Dialect: ProtocolDialectAnthropic, Class: ProtocolFeatureClassRequirement})
	RegisterProtocolFeature(ProtocolFeatureDefinition{ID: FeatureAnthropicNativeHistoryReplay, Dialect: ProtocolDialectAnthropic, Class: ProtocolFeatureClassRequirement, DependsOn: []ProtocolFeature{FeatureAnthropicNativeResponse}})
	RegisterProtocolFeature(ProtocolFeatureDefinition{ID: FeatureAnthropicReasoningReplay, Dialect: ProtocolDialectAnthropic, Class: ProtocolFeatureClassRequirement})
	RegisterProtocolFeature(ProtocolFeatureDefinition{ID: FeatureAnthropicStreamRelay, Dialect: ProtocolDialectAnthropic, Class: ProtocolFeatureClassModeSelection})
	RegisterProtocolFeature(ProtocolFeatureDefinition{ID: FeatureAnthropicBodyRelay, Dialect: ProtocolDialectAnthropic, Class: ProtocolFeatureClassModeSelection})
}
