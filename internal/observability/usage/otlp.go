package usage

import (
	"fmt"
	"strings"
)

// OTLP transport protocols accepted by OTLPConfig.Protocol.
const (
	OTLPProtocolGRPC = "grpc"
	OTLPProtocolHTTP = "http"
)

const (
	defaultOTLPServiceName    = "agent-gateway"
	defaultOTLPTimeoutSeconds = 10
)

// OTLPConfig configures exporting usage events as OpenTelemetry spans over
// OTLP. Export is disabled unless Endpoint is set. Events already carry W3C
// trace/span/parent ids, so the exporter reconstructs the interaction span
// tree post-hoc instead of running a live tracer.
type OTLPConfig struct {
	// Endpoint is the collector address as host:port, or a full URL
	// (scheme://host:port). Empty disables export.
	Endpoint string `json:"endpoint,omitempty"`
	// Protocol selects the OTLP transport: "grpc" (default) or "http"
	// (OTLP/HTTP protobuf).
	Protocol string `json:"protocol,omitempty"`
	// Insecure disables transport TLS. Ignored when Endpoint is a full URL,
	// where the scheme decides.
	Insecure bool `json:"insecure,omitempty"`
	// Headers are sent with every export request (e.g. auth tokens).
	Headers map[string]string `json:"headers,omitempty"`
	// ServiceName sets the OTel resource service.name. Default "agent-gateway".
	ServiceName string `json:"service_name,omitempty"`
	// TimeoutSeconds bounds one export batch. Default 10.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Components additionally exports one span per eino chat-model component
	// call (via the process-global callbacks tap), nested under the
	// interaction span. Off by default: it multiplies span volume.
	Components bool `json:"components,omitempty"`
}

// Enabled reports whether an export endpoint is configured.
func (c OTLPConfig) Enabled() bool {
	return strings.TrimSpace(c.Endpoint) != ""
}

// Normalized fills defaults. It does not validate; see Validate.
func (c OTLPConfig) Normalized() OTLPConfig {
	c.Endpoint = strings.TrimSpace(c.Endpoint)
	c.Protocol = strings.TrimSpace(strings.ToLower(c.Protocol))
	if c.Protocol == "" {
		c.Protocol = OTLPProtocolGRPC
	}
	c.ServiceName = strings.TrimSpace(c.ServiceName)
	if c.ServiceName == "" {
		c.ServiceName = defaultOTLPServiceName
	}
	if c.TimeoutSeconds <= 0 {
		c.TimeoutSeconds = defaultOTLPTimeoutSeconds
	}
	return c
}

// Validate checks the normalized form of the config.
func (c OTLPConfig) Validate() error {
	n := c.Normalized()
	if !n.Enabled() {
		return nil
	}
	if n.Protocol != OTLPProtocolGRPC && n.Protocol != OTLPProtocolHTTP {
		return fmt.Errorf("otlp protocol must be %q or %q, got %q", OTLPProtocolGRPC, OTLPProtocolHTTP, n.Protocol)
	}
	return nil
}
