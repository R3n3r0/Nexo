package trace_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/trace"
)

// recorder is an in-memory span exporter that survives the shutdown of
// the provider (which flushes the batches).
type recorder struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (r *recorder) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, spans...)
	return nil
}

func (r *recorder) Shutdown(context.Context) error { return nil }

func (r *recorder) byName(name string) sdktrace.ReadOnlySpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.spans)
}

func attrsOf(s sdktrace.ReadOnlySpan) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes() {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

func TestTelemetryDisabled(t *testing.T) {
	ctx := context.Background()
	tel, err := trace.NewTelemetry(ctx, trace.TelemetryDeps{})
	require.NoError(t, err)
	assert.False(t, tel.Enabled())
	got, span := tel.StartSpan(ctx, "x", trace.SpanOptions{})
	assert.Equal(t, ctx, got, "a disabled tracing does not touch the context")
	span.SetAttributes(trace.Attribute{Key: "k", Value: "v"})
	span.SetError("e")
	span.End()
	h := http.Header{}
	tel.Inject(ctx, h)
	assert.Empty(t, h)
	assert.Equal(t, ctx, tel.Extract(ctx, http.Header{"Traceparent": {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}}))
	next := http.DefaultTransport
	assert.Same(t, next, trace.NewRoundTripper(next, tel, "peer", "/rpc"), "zero overhead: the transport is not wrapped")
	require.NoError(t, tel.Start(ctx))
	require.NoError(t, tel.Stop(ctx))
}

func TestTelemetryInvalidEndpoint(t *testing.T) {
	ctx := context.Background()
	for name, opt := range map[string]trace.TelemetryOption{
		"scheme":   trace.WithOTLP("ftp://collector:4318", trace.ProtocolHTTP),
		"no host":  trace.WithOTLP("http://", trace.ProtocolHTTP),
		"protocol": trace.WithOTLP("http://collector:4318", "carrier-pigeon"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := trace.NewTelemetry(ctx, trace.TelemetryDeps{}, opt)
			require.ErrorIs(t, err, domain.ErrInvalidArgument)
		})
	}
	for _, p := range []string{trace.ProtocolHTTP, trace.ProtocolGRPC, "HTTP"} {
		// The connection is lazy: an unreachable collector is not an error.
		tel, err := trace.NewTelemetry(ctx, trace.TelemetryDeps{}, trace.WithOTLP("http://127.0.0.1:1", p))
		require.NoError(t, err, p)
		assert.True(t, tel.Enabled())
		require.NoError(t, tel.Stop(ctx))
	}
}

// TestTelemetryPropagation checks a client span around an RPC call, the
// W3C traceparent it sends, and the server span that continues the trace.
func TestTelemetryPropagation(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	tel, err := trace.NewTelemetry(ctx, trace.TelemetryDeps{}, trace.WithSpanExporter(rec), trace.WithServiceInfo("nexo", "1.2.3", "n1"))
	require.NoError(t, err)
	require.True(t, tel.Enabled())

	var traceparent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparent = r.Header.Get("Traceparent")
		sctx := tel.Extract(r.Context(), r.Header)
		_, span := tel.StartSpan(sctx, "rpc server", trace.SpanOptions{Kind: trace.SpanServer})
		span.End()
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	pctx, parent := tel.StartSpan(ctx, "s3 PutObject", trace.SpanOptions{Kind: trace.SpanServer,
		Attributes: []trace.Attribute{{Key: "nexo.bucket", Value: "b"}, {Key: "n", Value: 3}, {Key: "ok", Value: true}}})
	client := &http.Client{Transport: trace.NewRoundTripper(http.DefaultTransport, tel, "node2:9000", "/nexo/rpc/v1")}
	req, err := http.NewRequestWithContext(pctx, http.MethodPost, srv.URL+"/nexo/rpc/v1/storage/read", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Empty(t, req.Header.Get("Traceparent"), "the request of the caller is not modified")
	parent.SetError("boom")
	parent.End()
	require.NoError(t, tel.Stop(ctx))

	require.NotEmpty(t, traceparent, "the trace context is propagated")
	root, call, server := rec.byName("s3 PutObject"), rec.byName("rpc /storage/read"), rec.byName("rpc server")
	require.NotNil(t, root)
	require.NotNil(t, call)
	require.NotNil(t, server)
	assert.Equal(t, root.SpanContext().TraceID(), call.SpanContext().TraceID())
	assert.Equal(t, root.SpanContext().SpanID(), call.Parent().SpanID())
	assert.Equal(t, call.SpanContext().TraceID(), server.SpanContext().TraceID(), "the server continues the trace")
	assert.Equal(t, call.SpanContext().SpanID(), server.Parent().SpanID())
	assert.True(t, server.Parent().IsRemote())
	assert.Contains(t, traceparent, call.SpanContext().TraceID().String())

	assert.Equal(t, oteltrace.SpanKindClient, call.SpanKind())
	assert.Equal(t, oteltrace.SpanKindServer, server.SpanKind())
	callAttrs := attrsOf(call)
	assert.Equal(t, "/storage/read", callAttrs["rpc.method"])
	assert.Equal(t, "node2:9000", callAttrs["server.address"])
	assert.Equal(t, "502", callAttrs["http.response.status_code"])
	assert.Equal(t, codes.Error, call.Status().Code, "a 5xx is an error")
	assert.Equal(t, codes.Error, root.Status().Code)
	assert.Equal(t, "boom", root.Status().Description)
	rootAttrs := attrsOf(root)
	assert.Equal(t, "b", rootAttrs["nexo.bucket"])
	assert.Equal(t, "3", rootAttrs["n"])
	assert.Equal(t, "true", rootAttrs["ok"])

	res := map[string]string{}
	for _, kv := range root.Resource().Attributes() {
		res[string(kv.Key)] = kv.Value.String()
	}
	assert.Equal(t, "nexo", res["service.name"])
	assert.Equal(t, "1.2.3", res["service.version"])
	assert.Equal(t, "n1", res["service.instance.id"])
}

// TestTelemetrySampling checks the ratio of the root spans and the
// parent-based decision for the requests carrying a trace context.
func TestTelemetrySampling(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	tel, err := trace.NewTelemetry(ctx, trace.TelemetryDeps{}, trace.WithSpanExporter(rec), trace.WithSampleRatio(0))
	require.NoError(t, err)
	_, root := tel.StartSpan(ctx, "unsampled root", trace.SpanOptions{})
	root.End()
	sampled := tel.Extract(ctx, http.Header{"Traceparent": {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}})
	_, child := tel.StartSpan(sampled, "sampled child", trace.SpanOptions{Kind: trace.SpanServer})
	child.End()
	notSampled := tel.Extract(ctx, http.Header{"Traceparent": {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-00"}})
	_, other := tel.StartSpan(notSampled, "unsampled child", trace.SpanOptions{})
	other.End()
	require.NoError(t, tel.Stop(ctx))

	assert.Equal(t, 1, rec.count())
	got := rec.byName("sampled child")
	require.NotNil(t, got)
	assert.Equal(t, "0af7651916cd43dd8448eb211c80319c", got.SpanContext().TraceID().String())
}
