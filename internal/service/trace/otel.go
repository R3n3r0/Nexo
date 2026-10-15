package trace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Protocols of the OTLP exporter.
const (
	// ProtocolHTTP is OTLP over HTTP with protobuf bodies (port 4318, the
	// default).
	ProtocolHTTP = "http/protobuf"
	// ProtocolGRPC is OTLP over gRPC (port 4317).
	ProtocolGRPC = "grpc"
)

// TelemetryDeps are the dependencies of the distributed tracing (the
// exporter is configured by WithOTLP).
type TelemetryDeps struct {
	Logger *slog.Logger
}

// TelemetryOption configures the distributed tracing.
type TelemetryOption func(*telemetryOptions)

type telemetryOptions struct {
	endpoint    string
	protocol    string
	sampleRatio float64
	service     string
	version     string
	instance    string
	exporter    sdktrace.SpanExporter
}

// WithOTLP exports the spans to the OTLP collector at endpoint (an http
// or https URL: the base URL of the collector for ProtocolHTTP, to which
// "/v1/traces" is appended when it names no path; the host and port for
// ProtocolGRPC; https verifies the collector with the system roots). The
// standard OTEL_EXPORTER_OTLP_HEADERS, _TIMEOUT and _CERTIFICATE variables
// of the exporter apply.
func WithOTLP(endpoint, protocol string) TelemetryOption {
	return func(o *telemetryOptions) {
		o.endpoint, o.protocol = strings.TrimSpace(endpoint), strings.ToLower(strings.TrimSpace(protocol))
	}
}

// WithSampleRatio records the given share of the traces started by this
// node (0 to 1, default 1); a request carrying a sampled trace context is
// always recorded (parent-based sampling).
func WithSampleRatio(ratio float64) TelemetryOption {
	return func(o *telemetryOptions) {
		if ratio >= 0 && ratio <= 1 {
			o.sampleRatio = ratio
		}
	}
}

// WithServiceInfo names the service, its version and this instance (the
// node) in the resource of the spans.
func WithServiceInfo(name, version, instance string) TelemetryOption {
	return func(o *telemetryOptions) {
		if name != "" {
			o.service = name
		}
		o.version, o.instance = version, instance
	}
}

// withSpanExporter replaces the OTLP exporter (tests: an in-memory
// exporter, see export_test.go).
func withSpanExporter(e sdktrace.SpanExporter) TelemetryOption {
	return func(o *telemetryOptions) { o.exporter = e }
}

// NewTelemetry returns the distributed tracing: disabled (no endpoint and
// no exporter), it records nothing at no cost; enabled, it exports the
// spans in batches. An invalid endpoint or protocol is
// domain.ErrInvalidArgument.
func NewTelemetry(ctx context.Context, deps TelemetryDeps, opts ...TelemetryOption) (Telemetry, error) {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := telemetryOptions{protocol: ProtocolHTTP, sampleRatio: 1, service: "nexo"}
	for _, opt := range opts {
		opt(&o)
	}
	if o.endpoint == "" && o.exporter == nil {
		return noopTelemetry{}, nil
	}
	exporter := o.exporter
	if exporter == nil {
		var err error
		if exporter, err = newOTLPExporter(ctx, o); err != nil {
			return nil, err
		}
	}
	attrs := []attribute.KeyValue{attribute.String("service.name", o.service)}
	if o.version != "" {
		attrs = append(attrs, attribute.String("service.version", o.version))
	}
	if o.instance != "" {
		attrs = append(attrs, attribute.String("service.instance.id", o.instance))
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(o.sampleRatio))),
		sdktrace.WithResource(resource.NewSchemaless(attrs...)),
	)
	deps.Logger.InfoContext(ctx, "trace: OpenTelemetry tracing enabled", "endpoint", domain.RedactURL(o.endpoint), "protocol", o.protocol,
		"sample_ratio", o.sampleRatio)
	return &otelTelemetry{
		provider:   provider,
		tracer:     provider.Tracer("github.com/r3n3r0/nexo"),
		propagator: propagation.TraceContext{},
	}, nil
}

// newOTLPExporter builds the OTLP exporter of the options.
func newOTLPExporter(ctx context.Context, o telemetryOptions) (sdktrace.SpanExporter, error) {
	u, err := url.Parse(o.endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: trace: the OTLP endpoint must be an http(s) URL", domain.ErrInvalidArgument)
	}
	var client otlptrace.Client
	switch o.protocol {
	case ProtocolHTTP, "http", "":
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(o.endpoint)}
		if u.Path == "" || u.Path == "/" {
			opts = append(opts, otlptracehttp.WithURLPath("/v1/traces"))
		}
		client = otlptracehttp.NewClient(opts...)
	case ProtocolGRPC:
		client = otlptracegrpc.NewClient(otlptracegrpc.WithEndpointURL(o.endpoint))
	default:
		return nil, fmt.Errorf("%w: trace: unknown OTLP protocol %q (%s or %s)", domain.ErrInvalidArgument, o.protocol, ProtocolHTTP, ProtocolGRPC)
	}
	// The connection is opened lazily: an unreachable collector does not
	// stop the server, the batches are dropped with a log of the SDK.
	exporter, err := otlptrace.New(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("trace: OTLP exporter: %w", err)
	}
	return exporter, nil
}

// otelTelemetry is the OpenTelemetry implementation.
type otelTelemetry struct {
	provider   *sdktrace.TracerProvider
	tracer     oteltrace.Tracer
	propagator propagation.TextMapPropagator
}

var _ Telemetry = (*otelTelemetry)(nil)

// Start implements lifecycle.Runner.
func (t *otelTelemetry) Start(context.Context) error { return nil }

// Stop implements lifecycle.Runner: the pending spans are flushed.
func (t *otelTelemetry) Stop(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	if err := t.provider.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("trace: shutdown: %w", err)
	}
	return nil
}

// Enabled implements Telemetry.
func (t *otelTelemetry) Enabled() bool { return true }

// StartSpan implements Telemetry.
func (t *otelTelemetry) StartSpan(ctx context.Context, name string, opts SpanOptions) (context.Context, Span) {
	kind := oteltrace.SpanKindInternal
	switch opts.Kind {
	case SpanServer:
		kind = oteltrace.SpanKindServer
	case SpanClient:
		kind = oteltrace.SpanKindClient
	}
	ctx, span := t.tracer.Start(ctx, name, oteltrace.WithSpanKind(kind), oteltrace.WithAttributes(attributes(opts.Attributes)...))
	return ctx, otelSpan{span: span}
}

// Extract implements Telemetry.
func (t *otelTelemetry) Extract(ctx context.Context, h http.Header) context.Context {
	return t.propagator.Extract(ctx, propagation.HeaderCarrier(h))
}

// Inject implements Telemetry.
func (t *otelTelemetry) Inject(ctx context.Context, h http.Header) {
	t.propagator.Inject(ctx, propagation.HeaderCarrier(h))
}

// attributes converts the neutral attributes.
func attributes(attrs []Attribute) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		switch v := a.Value.(type) {
		case string:
			out = append(out, attribute.String(a.Key, v))
		case bool:
			out = append(out, attribute.Bool(a.Key, v))
		case int:
			out = append(out, attribute.Int(a.Key, v))
		case int64:
			out = append(out, attribute.Int64(a.Key, v))
		case float64:
			out = append(out, attribute.Float64(a.Key, v))
		default:
			out = append(out, attribute.String(a.Key, fmt.Sprint(v)))
		}
	}
	return out
}

type otelSpan struct {
	span oteltrace.Span
}

var _ Span = otelSpan{}

// SetName implements Span.
func (s otelSpan) SetName(name string) { s.span.SetName(name) }

// SetAttributes implements Span.
func (s otelSpan) SetAttributes(attrs ...Attribute) { s.span.SetAttributes(attributes(attrs)...) }

// SetError implements Span.
func (s otelSpan) SetError(description string) { s.span.SetStatus(codes.Error, description) }

// End implements Span.
func (s otelSpan) End() { s.span.End() }

// noopTelemetry is the disabled tracing: no allocation, no work.
type noopTelemetry struct{}

var _ Telemetry = noopTelemetry{}

// Start implements lifecycle.Runner.
func (noopTelemetry) Start(context.Context) error { return nil }

// Stop implements lifecycle.Runner.
func (noopTelemetry) Stop(context.Context) error { return nil }

// Enabled implements Telemetry.
func (noopTelemetry) Enabled() bool { return false }

// StartSpan implements Telemetry.
func (noopTelemetry) StartSpan(ctx context.Context, _ string, _ SpanOptions) (context.Context, Span) {
	return ctx, noopSpan{}
}

// Extract implements Telemetry.
func (noopTelemetry) Extract(ctx context.Context, _ http.Header) context.Context { return ctx }

// Inject implements Telemetry.
func (noopTelemetry) Inject(context.Context, http.Header) {}

type noopSpan struct{}

var _ Span = noopSpan{}

func (noopSpan) SetName(string)             {}
func (noopSpan) SetAttributes(...Attribute) {}
func (noopSpan) SetError(string)            {}
func (noopSpan) End()                       {}
