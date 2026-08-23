package provider

import (
	"encoding/json"
	"testing"
)

func TestNormalizeObjectToolInputSchema(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "absent", raw: "", wantErr: false},
		{name: "null", raw: "null", wantErr: false},
		{name: "empty object", raw: `{}`, wantErr: false},
		{name: "object type", raw: `{"type":"object","properties":{}}`, wantErr: false},
		// JSON Schema allows a list of type names; Anthropic only needs the
		// payload to be usable as an object.
		{name: "type list with object", raw: `{"type":["object","null"]}`, wantErr: false},
		{name: "type list without object", raw: `{"type":["string"]}`, wantErr: true},
		{name: "empty type list", raw: `{"type":[]}`, wantErr: true},
		{name: "type list with non string", raw: `{"type":["object",7]}`, wantErr: true},
		{name: "type list with duplicate", raw: `{"type":["object","object"]}`, wantErr: true},
		{name: "scalar type", raw: `{"type":"string"}`, wantErr: true},
		{name: "non object type value", raw: `{"type":7}`, wantErr: true},
		{name: "not an object", raw: `"schema"`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeObjectToolInputSchema(json.RawMessage(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeObjectToolInputSchema(%s) = %s, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeObjectToolInputSchema(%s) error = %v", tc.raw, err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(got, &decoded); err != nil {
				t.Fatalf("normalized schema %s is not an object: %v", got, err)
			}
		})
	}
}
