package dispatcher

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
)

const (
	traceFlagsSampled   = "01"
	traceFlagsUnsampled = "00"

	traceVersionCurrent = "00"
	traceVersionInvalid = "ff"
)

type traceContext struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	TraceState   string
	TraceFlags   string
	AgentDepth   int
}

func extractTraceContext(r *http.Request) traceContext {
	ctx := traceContext{SpanID: usage.GenerateSpanID(), TraceFlags: traceFlagsSampled}
	if r == nil {
		ctx.TraceID = usage.GenerateTraceID()
		return ctx
	}
	traceparentSeen := false
	if tp := strings.TrimSpace(r.Header.Get("traceparent")); tp != "" {
		traceparentSeen = true
		if traceID, parentSpanID, flags, ok := parseTraceparent(tp); ok {
			ctx.TraceID = traceID
			ctx.ParentSpanID = parentSpanID
			ctx.TraceFlags = flags
			ctx.TraceState = strings.TrimSpace(r.Header.Get("tracestate"))
		}
	}
	if ctx.TraceID == "" && !traceparentSeen {
		// The X-Trace-ID/X-Span-ID fallback is as client-controlled as traceparent,
		// so it gets the same W3C shape check. An unusable id must not reach the
		// outbound traceparent header or the trace_id usage column.
		if traceID := strings.TrimSpace(r.Header.Get("X-Trace-ID")); usage.ValidTraceID(traceID) {
			ctx.TraceID = traceID
			if parentSpanID := strings.TrimSpace(r.Header.Get("X-Span-ID")); usage.ValidSpanID(parentSpanID) {
				ctx.ParentSpanID = parentSpanID
			}
		}
	}
	if ctx.TraceID == "" {
		ctx.TraceID = usage.GenerateTraceID()
	}
	if depth, err := strconv.Atoi(strings.TrimSpace(r.Header.Get("X-Agent-Depth"))); err == nil && depth > 0 {
		ctx.AgentDepth = depth
	}
	return ctx
}

func writeTraceHeaders(w http.ResponseWriter, tc traceContext) {
	if w == nil {
		return
	}
	flags := tc.TraceFlags
	if flags == "" {
		flags = traceFlagsSampled
	}
	w.Header().Set("traceparent", "00-"+tc.TraceID+"-"+tc.SpanID+"-"+flags)
	if tc.TraceState != "" {
		w.Header().Set("tracestate", tc.TraceState)
	}
	w.Header().Set("X-Trace-ID", tc.TraceID)
	w.Header().Set("X-Span-ID", tc.SpanID)
	w.Header().Set("X-Agent-Depth", strconv.Itoa(tc.AgentDepth+1))
}

// parseTraceparent parses a W3C traceparent header. Version 00 must carry
// exactly four fields; a higher version may append fields, which this parser
// ignores rather than rejects, as the spec's forward-compatibility rule
// requires. Version ff is reserved and never valid.
func parseTraceparent(tp string) (traceID, parentSpanID, flags string, ok bool) {
	parts := strings.Split(tp, "-")
	if len(parts) < 4 {
		return "", "", "", false
	}
	if !validTraceVersion(parts[0]) {
		return "", "", "", false
	}
	if parts[0] == traceVersionCurrent && len(parts) != 4 {
		return "", "", "", false
	}
	if !usage.ValidTraceID(parts[1]) || !usage.ValidSpanID(parts[2]) || !usage.ValidLowerHex(parts[3], 2) {
		return "", "", "", false
	}
	flags = traceFlagsUnsampled
	if sampledTraceFlags(parts[3]) {
		flags = traceFlagsSampled
	}
	return parts[1], parts[2], flags, true
}

func validTraceVersion(v string) bool {
	return usage.ValidLowerHex(v, 2) && v != traceVersionInvalid
}

func sampledTraceFlags(v string) bool {
	n, err := strconv.ParseUint(v, 16, 8)
	return err == nil && n&1 == 1
}
