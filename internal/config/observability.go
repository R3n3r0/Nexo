package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Environment variables of the distributed tracing (OpenTelemetry).
const (
	// EnvOTelEndpoint is the URL of the OTLP collector: the base URL for
	// the HTTP protocol (http://collector:4318, "/v1/traces" appended when
	// it names no path), the host and port for gRPC
	// (http://collector:4317); https verifies the collector.
	EnvOTelEndpoint = "NEXO_OTEL_EXPORTER_OTLP_ENDPOINT"
	// EnvOTelProtocol is "http/protobuf" (default) or "grpc".
	EnvOTelProtocol = "NEXO_OTEL_EXPORTER_OTLP_PROTOCOL"
	// EnvOTelSampleRatio is the share of the traces started by the server
	// that are recorded (0 to 1, default 1).
	EnvOTelSampleRatio = "NEXO_OTEL_TRACES_SAMPLER_ARG"
)

// OTLP protocols.
const (
	OTelProtocolHTTP = "http/protobuf"
	OTelProtocolGRPC = "grpc"
)

// applyObservabilityEnv reads the environment of the distributed tracing.
func (l *loader) applyObservabilityEnv(cfg *Static) error {
	if v, ok := l.env(EnvOTelEndpoint); ok {
		cfg.OTelEndpoint = strings.TrimSpace(v)
	}
	if v, ok := l.env(EnvOTelProtocol); ok {
		cfg.OTelProtocol = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := l.env(EnvOTelSampleRatio); ok && strings.TrimSpace(v) != "" {
		r, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvOTelSampleRatio, err)
		}
		cfg.OTelSampleRatio = r
	}
	return nil
}

// validateObservability checks the settings of the distributed tracing.
func (s Static) validateObservability() []error {
	var errs []error
	if s.OTelEndpoint != "" {
		u, err := url.Parse(s.OTelEndpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, errors.New("the OTLP endpoint (NEXO_OTEL_EXPORTER_OTLP_ENDPOINT) must be an http(s) URL"))
		}
	}
	switch s.OTelProtocol {
	case "", OTelProtocolHTTP, OTelProtocolGRPC:
	default:
		errs = append(errs, fmt.Errorf("OTLP protocol %q must be %s or %s", s.OTelProtocol, OTelProtocolHTTP, OTelProtocolGRPC))
	}
	if s.OTelSampleRatio < 0 || s.OTelSampleRatio > 1 {
		errs = append(errs, fmt.Errorf("OTLP sample ratio %g must be between 0 and 1", s.OTelSampleRatio))
	}
	return errs
}
