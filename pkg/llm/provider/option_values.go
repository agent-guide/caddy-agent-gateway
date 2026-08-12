package provider

import (
	"fmt"
	"strconv"
	"strings"
)

// CapabilityOption identifies one provider capability that may be overridden
// through generic provider options.
type CapabilityOption string

const (
	CapabilityContextWindow   CapabilityOption = "context_window"
	CapabilityMaxOutputTokens CapabilityOption = "max_output_tokens"
	CapabilityVision          CapabilityOption = "vision"
	CapabilityEmbeddings      CapabilityOption = "embeddings"
)

// CapabilitiesFromOptions applies the selected generic capability overrides to
// defaults. Callers choose the supported fields so an unrelated option cannot
// silently advertise a capability the provider does not implement.
func CapabilitiesFromOptions(options map[string]any, defaults ProviderCapabilities, supported ...CapabilityOption) (ProviderCapabilities, error) {
	capabilities := defaults
	for _, option := range supported {
		var err error
		switch option {
		case CapabilityContextWindow:
			capabilities.ContextWindow, err = PositiveIntOption(options, string(option), capabilities.ContextWindow)
		case CapabilityMaxOutputTokens:
			capabilities.MaxOutputTokens, err = PositiveIntOption(options, string(option), capabilities.MaxOutputTokens)
		case CapabilityVision:
			capabilities.Vision, err = BoolOption(options, string(option), capabilities.Vision)
		case CapabilityEmbeddings:
			capabilities.Embeddings, err = BoolOption(options, string(option), capabilities.Embeddings)
		default:
			return ProviderCapabilities{}, fmt.Errorf("unsupported capability option %q", option)
		}
		if err != nil {
			return ProviderCapabilities{}, err
		}
	}
	return capabilities, nil
}

// PositiveIntOption reads a positive integer provider option. String values
// support Caddyfile configuration, while integer and integral float values
// support JSON and YAML configuration.
func PositiveIntOption(options map[string]any, name string, fallback int) (int, error) {
	raw, ok := options[name]
	if !ok {
		return fallback, nil
	}
	value, ok := PositiveIntValue(raw)
	if !ok {
		return 0, fmt.Errorf("option %s must be a positive integer", name)
	}
	return value, nil
}

// PositiveIntValue converts a supported configuration value to a positive int.
func PositiveIntValue(raw any) (int, bool) {
	var value int64
	switch typed := raw.(type) {
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return 0, false
		}
		value = parsed
	case int:
		value = int64(typed)
	case int64:
		value = typed
	case float64:
		value = int64(typed)
		if float64(value) != typed {
			return 0, false
		}
	default:
		return 0, false
	}
	if value <= 0 || int64(int(value)) != value {
		return 0, false
	}
	return int(value), true
}

// BoolOption reads a boolean provider option from bool or string input.
func BoolOption(options map[string]any, name string, fallback bool) (bool, error) {
	raw, ok := options[name]
	if !ok {
		return fallback, nil
	}
	switch typed := raw.(type) {
	case bool:
		return typed, nil
	case string:
		value, err := strconv.ParseBool(strings.TrimSpace(typed))
		if err == nil {
			return value, nil
		}
	}
	return false, fmt.Errorf("option %s must be a boolean", name)
}
