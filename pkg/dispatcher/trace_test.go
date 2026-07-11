package dispatcher

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
)

func TestExtractTraceContextTraceparentValidation(t *testing.T) {
	validTraceID := "4bf92f3577b34da6a3ce929d0e0e4736"
	validParentID := "00f067aa0ba902b7"

	tests := []struct {
		name           string
		traceparent    string
		wantTraceID    string
		wantParentID   string
		wantFlags      string
		wantGenerated  bool
		wantTraceState bool
	}{
		{
			name:           "valid sampled",
			traceparent:    "00-" + validTraceID + "-" + validParentID + "-01",
			wantTraceID:    validTraceID,
			wantParentID:   validParentID,
			wantFlags:      traceFlagsSampled,
			wantTraceState: true,
		},
		{
			name:           "valid unsampled",
			traceparent:    "00-" + validTraceID + "-" + validParentID + "-00",
			wantTraceID:    validTraceID,
			wantParentID:   validParentID,
			wantFlags:      traceFlagsUnsampled,
			wantTraceState: true,
		},
		{
			name:           "future version ignores trailing fields",
			traceparent:    "01-" + validTraceID + "-" + validParentID + "-01-extra",
			wantTraceID:    validTraceID,
			wantParentID:   validParentID,
			wantFlags:      traceFlagsSampled,
			wantTraceState: true,
		},
		{name: "reserved version ff", traceparent: "ff-" + validTraceID + "-" + validParentID + "-01", wantGenerated: true, wantFlags: traceFlagsSampled},
		{name: "non-hex version", traceparent: "zz-" + validTraceID + "-" + validParentID + "-01", wantGenerated: true, wantFlags: traceFlagsSampled},
		{name: "uppercase rejected", traceparent: "00-" + strings.ToUpper(validTraceID) + "-" + strings.ToUpper(validParentID) + "-01", wantGenerated: true, wantFlags: traceFlagsSampled},
		{name: "version 00 with trailing field", traceparent: "00-" + validTraceID + "-" + validParentID + "-01-extra", wantGenerated: true, wantFlags: traceFlagsSampled},
		{name: "non-hex flags", traceparent: "00-" + validTraceID + "-" + validParentID + "-zz", wantGenerated: true, wantFlags: traceFlagsSampled},
		{name: "all-zero trace", traceparent: "00-00000000000000000000000000000000-" + validParentID + "-01", wantGenerated: true, wantFlags: traceFlagsSampled},
		{name: "all-zero span", traceparent: "00-" + validTraceID + "-0000000000000000-01", wantGenerated: true, wantFlags: traceFlagsSampled},
		{name: "short field", traceparent: "00-" + validTraceID + "-00f067aa0ba902-01", wantGenerated: true, wantFlags: traceFlagsSampled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("traceparent", tt.traceparent)
			req.Header.Set("tracestate", "vendor=state")
			req.Header.Set("X-Trace-ID", strings.Repeat("a", 32))

			got := extractTraceContext(req)
			if tt.wantGenerated {
				if got.TraceID == strings.Repeat("a", 32) || got.TraceID == validTraceID {
					t.Fatalf("trace_id = %q, want fresh generated context", got.TraceID)
				}
				if !usage.ValidTraceID(got.TraceID) {
					t.Fatalf("generated trace_id = %q, want valid W3C trace id", got.TraceID)
				}
				if got.ParentSpanID != "" {
					t.Fatalf("parent_span_id = %q, want empty for invalid traceparent", got.ParentSpanID)
				}
				if got.TraceState != "" {
					t.Fatalf("tracestate = %q, want empty for invalid traceparent", got.TraceState)
				}
			} else {
				if got.TraceID != tt.wantTraceID || got.ParentSpanID != tt.wantParentID {
					t.Fatalf("context = trace %q parent %q, want %q/%q", got.TraceID, got.ParentSpanID, tt.wantTraceID, tt.wantParentID)
				}
				if got.TraceState != "vendor=state" {
					t.Fatalf("tracestate = %q, want propagated", got.TraceState)
				}
			}
			if got.TraceFlags != tt.wantFlags {
				t.Fatalf("trace flags = %q, want %q", got.TraceFlags, tt.wantFlags)
			}
		})
	}
}

func TestExtractTraceContextXHeaderValidation(t *testing.T) {
	validTraceID := "4bf92f3577b34da6a3ce929d0e0e4736"
	validParentID := "00f067aa0ba902b7"

	tests := []struct {
		name          string
		traceID       string
		spanID        string
		wantTraceID   string
		wantParentID  string
		wantGenerated bool
	}{
		{name: "valid pair", traceID: validTraceID, spanID: validParentID, wantTraceID: validTraceID, wantParentID: validParentID},
		{name: "uppercase rejected", traceID: strings.ToUpper(validTraceID), spanID: strings.ToUpper(validParentID), wantGenerated: true},
		{name: "valid trace, invalid span", traceID: validTraceID, spanID: "zzzz", wantTraceID: validTraceID},
		{name: "non-hex trace", traceID: "not-a-trace-id", spanID: validParentID, wantGenerated: true},
		{name: "all-zero trace", traceID: "00000000000000000000000000000000", spanID: validParentID, wantGenerated: true},
		{name: "short trace", traceID: "abc", spanID: validParentID, wantGenerated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("X-Trace-ID", tt.traceID)
			req.Header.Set("X-Span-ID", tt.spanID)

			got := extractTraceContext(req)

			if !usage.ValidTraceID(got.TraceID) {
				t.Fatalf("trace_id = %q, want a valid W3C trace id", got.TraceID)
			}
			if tt.wantGenerated {
				if got.TraceID == strings.ToLower(tt.traceID) {
					t.Fatalf("trace_id = %q, want fresh generated context", got.TraceID)
				}
				if got.ParentSpanID != "" {
					t.Fatalf("parent_span_id = %q, want empty when the trace id is rejected", got.ParentSpanID)
				}
				return
			}
			if got.TraceID != tt.wantTraceID {
				t.Fatalf("trace_id = %q, want %q", got.TraceID, tt.wantTraceID)
			}
			if got.ParentSpanID != tt.wantParentID {
				t.Fatalf("parent_span_id = %q, want %q", got.ParentSpanID, tt.wantParentID)
			}
		})
	}
}

func TestWriteTraceHeadersNeverEmitsMalformedTraceparent(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Trace-ID", "not-a-trace-id")
	req.Header.Set("X-Span-ID", "zzzz")

	rec := httptest.NewRecorder()
	writeTraceHeaders(rec, extractTraceContext(req))

	parts := strings.Split(rec.Header().Get("traceparent"), "-")
	if len(parts) != 4 || !usage.ValidTraceID(parts[1]) || !usage.ValidSpanID(parts[2]) {
		t.Fatalf("traceparent = %q, want a well-formed header", rec.Header().Get("traceparent"))
	}
}

func TestWriteTraceHeadersPreservesUnsampledTraceparent(t *testing.T) {
	rec := httptest.NewRecorder()
	writeTraceHeaders(rec, traceContext{
		TraceID:    "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:     "00f067aa0ba902b7",
		TraceFlags: traceFlagsUnsampled,
	})

	if got := rec.Header().Get("traceparent"); got != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00" {
		t.Fatalf("traceparent = %q, want unsampled", got)
	}
}
