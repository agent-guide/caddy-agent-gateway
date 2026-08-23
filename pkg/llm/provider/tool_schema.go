package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
)

var emptyObjectToolInputSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// NormalizeObjectToolInputSchema converts an absent parameter schema into the
// canonical no-argument object schema and validates an explicitly supplied
// schema's top-level shape. An empty JSON Schema object remains valid.
func NormalizeObjectToolInputSchema(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return append(json.RawMessage(nil), emptyObjectToolInputSchema...), nil
	}
	var schemaValue map[string]any
	if err := json.Unmarshal(trimmed, &schemaValue); err != nil {
		return nil, fmt.Errorf("must be a JSON object: %w", err)
	}
	// JSON Schema allows type to be a string or a list of type names, and an
	// absent type means "unconstrained", which an object payload satisfies.
	switch typ := schemaValue["type"].(type) {
	case nil:
	case string:
		if typ != "object" {
			return nil, fmt.Errorf("type must be object")
		}
	case []any:
		if len(typ) == 0 {
			return nil, fmt.Errorf("type list must not be empty")
		}
		seen := make(map[string]struct{}, len(typ))
		hasObject := false
		for _, item := range typ {
			name, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("type list entries must be strings")
			}
			if _, duplicate := seen[name]; duplicate {
				return nil, fmt.Errorf("type list entries must be unique")
			}
			seen[name] = struct{}{}
			hasObject = hasObject || name == "object"
		}
		if !hasObject {
			return nil, fmt.Errorf("type must include object")
		}
	default:
		return nil, fmt.Errorf("type must be a string or a list of strings")
	}
	return append(json.RawMessage(nil), trimmed...), nil
}
