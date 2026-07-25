package runtimeapi

import (
	"errors"
	"strings"
	"testing"
)

func TestDecodeTurnRequestOptions(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		code ErrorCode
	}{
		{name: "absent", body: `{"input":"hello"}`},
		{name: "v1 empty", body: `{"input":"hello","options":{"version":"v1","runtime":{}}}`},
		{name: "missing version", body: `{"options":{"runtime":{}}}`, code: ErrorInvalidRequest},
		{name: "null options", body: `{"options":null}`, code: ErrorInvalidRequest},
		{name: "unknown version", body: `{"options":{"version":"v2"}}`, code: ErrorUnsupportedOption},
		{name: "runtime array", body: `{"options":{"version":"v1","runtime":[]}}`, code: ErrorUnsupportedOption},
		{name: "unknown common field", body: `{"input":"hello","secret":"x"}`, code: ErrorUnsupportedOption},
		{name: "unknown options field", body: `{"options":{"version":"v1","timeout":1}}`, code: ErrorUnsupportedOption},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeTurnRequest(strings.NewReader(tt.body))
			if tt.code == "" {
				if err != nil {
					t.Fatalf("DecodeTurnRequest() error = %v", err)
				}
				if got.Input == "hello" && tt.name == "v1 empty" && got.Options.Version != "v1" {
					t.Fatalf("version = %q", got.Options.Version)
				}
				return
			}
			if code, ok := ErrorCodeOf(err); !ok || code != tt.code {
				t.Fatalf("error = %v (%q), want %q", err, code, tt.code)
			}
		})
	}
}

func TestDecodeRuntimeOptionsRejectsForeignFields(t *testing.T) {
	var options struct {
		Model string `json:"model"`
	}
	if err := DecodeRuntimeOptions([]byte(`{"model":"gpt-5"}`), &options); err != nil || options.Model != "gpt-5" {
		t.Fatalf("supported options = %+v, error = %v", options, err)
	}
	err := DecodeRuntimeOptions([]byte(`{"cwd":"/secret/path"}`), &options)
	if !errors.Is(err, ErrUnsupportedOption) {
		t.Fatalf("foreign option error = %v, want unsupported_option", err)
	}
	if public := PublicError(err); strings.Contains(public.Message, "/secret/path") {
		t.Fatalf("public error leaked runtime option: %+v", public)
	}
}
