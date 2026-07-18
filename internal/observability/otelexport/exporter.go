// Package otelexport implements the pipeline.OTelExporter seam: it converts
// gateway usage events into OpenTelemetry spans and ships them to an OTLP
// collector. Usage events already carry W3C trace/span/parent ids stamped by
// the dispatcher and the builtin host, so the exporter reconstructs the
// recorded span tree post-hoc — no live tracer, no sampling decisions, and no
// second source of truth: the SQLite event tables remain authoritative and
// export failures never affect request serving.
package otelexport

import (
	"context"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
)

const scopeName = "github.com/agent-guide/agent-gateway/internal/observability/otelexport"

// Exporter adapts usage events to OTLP spans behind a batch span processor,
// which owns queueing, batching, retries, and export timeouts.
type Exporter struct {
	processor sdktrace.SpanProcessor
	resource  *resource.Resource
	scope     instrumentation.Scope
}

// New builds an Exporter shipping to the configured OTLP endpoint. The
// context bounds the transport setup only (the gRPC dial is non-blocking).
func New(ctx context.Context, cfg usage.OTLPConfig) (*Exporter, error) {
	cfg = cfg.Normalized()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	spanExporter, err := otlptrace.New(ctx, client)
	if err != nil {
		return nil, err
	}
	processor := sdktrace.NewBatchSpanProcessor(spanExporter,
		sdktrace.WithExportTimeout(time.Duration(cfg.TimeoutSeconds)*time.Second))
	return newWithProcessor(processor, cfg), nil
}

func newClient(cfg usage.OTLPConfig) (otlptrace.Client, error) {
	endpointIsURL := strings.Contains(cfg.Endpoint, "://")
	switch cfg.Protocol {
	case usage.OTLPProtocolHTTP:
		opts := []otlptracehttp.Option{}
		if endpointIsURL {
			opts = append(opts, otlptracehttp.WithEndpointURL(cfg.Endpoint))
		} else {
			opts = append(opts, otlptracehttp.WithEndpoint(cfg.Endpoint))
			if cfg.Insecure {
				opts = append(opts, otlptracehttp.WithInsecure())
			}
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
		}
		return otlptracehttp.NewClient(opts...), nil
	default: // validated: only grpc remains
		opts := []otlptracegrpc.Option{}
		if endpointIsURL {
			opts = append(opts, otlptracegrpc.WithEndpointURL(cfg.Endpoint))
		} else {
			opts = append(opts, otlptracegrpc.WithEndpoint(cfg.Endpoint))
			if cfg.Insecure {
				opts = append(opts, otlptracegrpc.WithInsecure())
			}
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
		}
		return otlptracegrpc.NewClient(opts...), nil
	}
}

func newWithProcessor(processor sdktrace.SpanProcessor, cfg usage.OTLPConfig) *Exporter {
	return &Exporter{
		processor: processor,
		resource: resource.NewSchemaless(
			attribute.String("service.name", cfg.ServiceName),
		),
		scope: instrumentation.Scope{Name: scopeName},
	}
}

// ExportUsageEvent converts one usage event into a span and hands it to the
// batch processor. A conversion failure is returned so the pipeline's
// write-failure counter keeps unexportable events observable.
func (e *Exporter) ExportUsageEvent(ev any) error {
	stub, err := spanStub(ev)
	if err != nil {
		return err
	}
	stub.Resource = e.resource
	stub.InstrumentationScope = e.scope
	e.processor.OnEnd(stub.Snapshot())
	return nil
}

// exportComponentSpan hands a component-tap span to the batch processor with
// this exporter's resource and scope stamped on.
func (e *Exporter) exportComponentSpan(stub tracetest.SpanStub) {
	stub.Resource = e.resource
	stub.InstrumentationScope = e.scope
	e.processor.OnEnd(stub.Snapshot())
}

// Close detaches the component tap (if it points here), flushes buffered
// spans, and shuts the transport down. The batch processor's Shutdown also
// shuts down the underlying OTLP exporter.
func (e *Exporter) Close() error {
	disableComponentTap(e)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return e.processor.Shutdown(ctx)
}
