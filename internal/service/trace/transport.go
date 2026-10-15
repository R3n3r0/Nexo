package trace

import (
	"net/http"
	"strconv"
	"strings"
)

// NewRoundTripper returns next with a client span around every call and
// the W3C trace context injected in its headers, so that the node called
// continues the trace (the internode RPC: the span is named "rpc
// <method>", the method being the path without prefix). Without
// telemetry, or when it is disabled, next is returned as is.
func NewRoundTripper(next http.RoundTripper, t Telemetry, peer, prefix string) http.RoundTripper {
	if t == nil || !t.Enabled() || next == nil {
		return next
	}
	return &tracedTransport{next: next, t: t, peer: peer, prefix: prefix}
}

type tracedTransport struct {
	next   http.RoundTripper
	t      Telemetry
	peer   string
	prefix string
}

// RoundTrip implements http.RoundTripper. The request is cloned before
// its headers change (a RoundTripper must not modify its request).
func (t *tracedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	method := strings.TrimPrefix(req.URL.Path, t.prefix)
	ctx, span := t.t.StartSpan(req.Context(), "rpc "+method, SpanOptions{Kind: SpanClient, Attributes: []Attribute{
		{Key: "rpc.system", Value: "nexo"},
		{Key: "rpc.method", Value: method},
		{Key: "server.address", Value: t.peer},
	}})
	defer span.End()
	out := req.Clone(ctx)
	t.t.Inject(ctx, out.Header)
	resp, err := t.next.RoundTrip(out)
	if err != nil {
		span.SetError(err.Error())
		return nil, err
	}
	span.SetAttributes(Attribute{Key: "http.response.status_code", Value: resp.StatusCode})
	if resp.StatusCode >= http.StatusInternalServerError {
		span.SetError("status " + strconv.Itoa(resp.StatusCode))
	}
	return resp, nil
}
