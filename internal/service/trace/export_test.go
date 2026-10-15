package trace

import sdktrace "go.opentelemetry.io/otel/sdk/trace"

// WithSpanExporter exports the spans to e instead of an OTLP collector
// (tests: an in-memory recorder).
func WithSpanExporter(e sdktrace.SpanExporter) TelemetryOption { return withSpanExporter(e) }
