package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolJSONUsesMCPInputSchemaField(t *testing.T) {
	data, err := json.Marshal(Tool{
		Name:        "echo",
		Description: "Echo input",
		InputSchema: map[string]any{
			"type": "object",
		},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, `"inputSchema"`) {
		t.Fatalf("marshaled tool missing inputSchema: %s", body)
	}
	if strings.Contains(body, `"input_schema"`) {
		t.Fatalf("marshaled tool used legacy input_schema: %s", body)
	}
}

func TestToolJSONParsesMCPInputSchemaField(t *testing.T) {
	var tool Tool
	if err := json.Unmarshal([]byte(`{"name":"echo","description":"Echo input","inputSchema":{"type":"object"}}`), &tool); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if tool.InputSchema["type"] != "object" {
		t.Fatalf("unexpected input schema: %#v", tool.InputSchema)
	}
}

func TestToolResultJSONUsesMCPFieldNames(t *testing.T) {
	data, err := json.Marshal(ToolResult{
		Content:           "ok",
		StructuredContent: map[string]any{"message": "ok"},
		IsError:           true,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	body := string(data)
	for _, want := range []string{`"structuredContent"`, `"isError"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("marshaled tool result missing %s: %s", want, body)
		}
	}
	for _, legacy := range []string{`"structured_content"`, `"is_error"`} {
		if strings.Contains(body, legacy) {
			t.Fatalf("marshaled tool result used legacy field %s: %s", legacy, body)
		}
	}
}

func TestToolResultJSONRoundTripFromUpstream(t *testing.T) {
	var result ToolResult
	if err := json.Unmarshal([]byte(`{"content":"boom","structuredContent":{"code":1},"isError":true}`), &result); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected isError to be preserved from upstream, got %#v", result)
	}
	if sc, ok := result.StructuredContent.(map[string]any); !ok || sc["code"] == nil {
		t.Fatalf("expected structuredContent to be preserved, got %#v", result.StructuredContent)
	}
}

func TestResourceJSONUsesMimeTypeField(t *testing.T) {
	data, err := json.Marshal(Resource{URI: "file:///x", Name: "x", MimeType: "text/plain"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if body := string(data); !strings.Contains(body, `"mimeType"`) || strings.Contains(body, `"mime_type"`) {
		t.Fatalf("unexpected resource JSON: %s", body)
	}

	var content ResourceContent
	if err := json.Unmarshal([]byte(`{"uri":"file:///x","mimeType":"text/plain","text":"hi"}`), &content); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if content.MimeType != "text/plain" {
		t.Fatalf("expected mimeType to be preserved, got %#v", content)
	}
}
