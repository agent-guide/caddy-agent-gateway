package pipeline

// OTelExporter translates gateway usage events into OpenTelemetry spans, logs,
// or metrics without forcing an OpenTelemetry dependency into the gateway core.
//
// Sinks are passed to NewEventPipeline at construction time, so wiring an
// exporter means editing the pipeline setup in caddy/gateway/app.go and
// standalone/server/server.go. There is no out-of-module registration seam.
type OTelExporter interface {
	ExportUsageEvent(any) error
	Close() error
}

type OpenTelemetrySink struct {
	exporter OTelExporter
}

func NewOpenTelemetrySink(exporter OTelExporter) *OpenTelemetrySink {
	return &OpenTelemetrySink{exporter: exporter}
}

func (s *OpenTelemetrySink) Write(ev any) error {
	if s == nil || s.exporter == nil {
		return nil
	}
	return s.exporter.ExportUsageEvent(ev)
}

func (s *OpenTelemetrySink) Close() error {
	if s == nil || s.exporter == nil {
		return nil
	}
	return s.exporter.Close()
}
