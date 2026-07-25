package runtimeapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// DecodeTurnRequest strictly decodes the common northbound turn envelope.
// Runtime remains opaque until the selected backend decodes it.
func DecodeTurnRequest(r io.Reader) (TurnRequest, error) {
	var wire struct {
		Input      string              `json:"input"`
		SessionID  string              `json:"session_id"`
		Permission *PermissionDecision `json:"permission"`
		Options    json.RawMessage     `json:"options"`
	}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		if isUnknownFieldError(err) {
			return TurnRequest{}, WrapError(ErrorUnsupportedOption, "turn option is not supported", err)
		}
		return TurnRequest{}, WrapError(ErrorInvalidRequest, "invalid turn request", err)
	}
	if err := requireJSONEOF(dec); err != nil {
		return TurnRequest{}, WrapError(ErrorInvalidRequest, "invalid turn request", err)
	}
	out := TurnRequest{Input: wire.Input, SessionID: wire.SessionID, Permission: wire.Permission}
	if len(wire.Options) == 0 {
		return out, nil
	}
	var options struct {
		Version string          `json:"version"`
		Runtime json.RawMessage `json:"runtime"`
	}
	optionsDecoder := json.NewDecoder(bytes.NewReader(wire.Options))
	optionsDecoder.DisallowUnknownFields()
	if err := optionsDecoder.Decode(&options); err != nil {
		if isUnknownFieldError(err) {
			return TurnRequest{}, WrapError(ErrorUnsupportedOption, "turn option is not supported", err)
		}
		return TurnRequest{}, WrapError(ErrorInvalidRequest, "invalid turn options", err)
	}
	if options.Version == "" {
		return TurnRequest{}, NewError(ErrorInvalidRequest, "options.version is required")
	}
	if options.Version != TurnOptionsVersionV1 {
		return TurnRequest{}, NewError(ErrorUnsupportedOption, "unsupported options version")
	}
	if err := validateRuntimeObject(options.Runtime); err != nil {
		return TurnRequest{}, err
	}
	out.Options = TurnOptions{Version: options.Version, Runtime: cloneRawMessage(options.Runtime)}
	return out, nil
}

func isUnknownFieldError(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "json: unknown field ")
}

// DecodeRuntimeOptions strictly decodes the selected backend's opaque v1
// object. Unknown or foreign-runtime fields are never ignored.
func DecodeRuntimeOptions(raw json.RawMessage, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		trimmed = []byte("{}")
	}
	if err := validateRuntimeObject(trimmed); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return WrapError(ErrorUnsupportedOption, "runtime options are not supported", err)
	}
	return nil
}

func validateRuntimeObject(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] != '{' {
		return NewError(ErrorUnsupportedOption, "options.runtime must be an object")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return WrapError(ErrorUnsupportedOption, "options.runtime must be an object", err)
	}
	return nil
}

func requireJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request contains multiple JSON values")
		}
		return err
	}
	return nil
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}
