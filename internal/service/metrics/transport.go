package metrics

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// RoundTripperOption configures the measured round tripper.
type RoundTripperOption func(*roundTripperOptions)

type roundTripperOptions struct {
	prefix string
}

// WithPathPrefix strips prefix from the request paths to name the method
// of a call ("/nexo/rpc/v1/storage/read-version" is
// "storage/read-version" with the prefix "/nexo/rpc/v1/").
func WithPathPrefix(prefix string) RoundTripperOption {
	return func(o *roundTripperOptions) { o.prefix = prefix }
}

// rpcBuckets are the latency bounds of the internode calls.
func rpcBuckets() []float64 {
	return []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}
}

// NewRoundTripper returns next measured as the client of the internode
// calls to peer: nexo_rpc_requests_total, nexo_rpc_errors_total (transport
// failures and 5xx answers: the domain errors a node answers, such as a
// missing file, are results) and nexo_rpc_request_duration_seconds (to
// the answer headers: the streams are measured to their first byte), by
// peer and method. The methods are the fixed paths of the internode
// routes: the cardinality is bounded.
func NewRoundTripper(ctx context.Context, next http.RoundTripper, reg Registry, peer string, opts ...RoundTripperOption) (http.RoundTripper, error) {
	if next == nil || reg == nil {
		return nil, fmt.Errorf("%w: metrics: the round tripper needs a transport and a registry", domain.ErrInvalidArgument)
	}
	var o roundTripperOptions
	for _, opt := range opts {
		opt(&o)
	}
	rt := &measuredTransport{next: next, peer: peer, opts: o}
	var err error
	if rt.requests, err = reg.Counter(ctx, Definition{Subsystem: "rpc", Name: "requests_total",
		Help: "Internode calls made by this node, by peer and method.", Labels: []string{"peer", "method"}}); err != nil {
		return nil, fmt.Errorf("metrics: rpc: %w", err)
	}
	if rt.errors, err = reg.Counter(ctx, Definition{Subsystem: "rpc", Name: "errors_total",
		Help: "Internode calls that failed (transport or server error), by peer and method.", Labels: []string{"peer", "method"}}); err != nil {
		return nil, fmt.Errorf("metrics: rpc: %w", err)
	}
	if rt.duration, err = reg.Histogram(ctx, Definition{Subsystem: "rpc", Name: "request_duration_seconds",
		Help: "Latency of the internode calls to their answer, by peer and method.", Labels: []string{"peer", "method"}, Buckets: rpcBuckets()}); err != nil {
		return nil, fmt.Errorf("metrics: rpc: %w", err)
	}
	return rt, nil
}

type measuredTransport struct {
	next     http.RoundTripper
	peer     string
	opts     roundTripperOptions
	requests Counter
	errors   Counter
	duration Histogram
}

// RoundTrip implements http.RoundTripper.
func (t *measuredTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	method := strings.TrimPrefix(req.URL.Path, t.opts.prefix)
	start := time.Now()
	resp, err := t.next.RoundTrip(req)
	t.requests.Add(ctx, 1, t.peer, method)
	t.duration.Observe(ctx, time.Since(start).Seconds(), t.peer, method)
	if err != nil || resp.StatusCode >= http.StatusInternalServerError {
		t.errors.Add(ctx, 1, t.peer, method)
	}
	return resp, err
}
