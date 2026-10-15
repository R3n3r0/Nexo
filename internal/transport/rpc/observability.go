package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/audit"
	"github.com/r3n3r0/nexo/internal/service/logs"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/trace"
)

// ObservabilityRoutes exposes the observability of the node to its peers
// ("/obs/..."): the metric families and the points of the ring buffer of
// the metrics, the live trace and log streams and the audit search
// (docs/05 T-8.1..T-8.4). A peer serving the console or the CLI of an
// operator aggregates them over the cluster.
type ObservabilityRoutes interface{ Routes }

// ObservabilityPeer is the observability of another node: the peer side
// of the metrics, the live trace, the log stream and the audit log.
type ObservabilityPeer interface {
	metrics.Peer
	trace.Peer
	logs.Peer
	audit.Peer
}

type (
	// ObservabilityRoutesOption configures the observability routes.
	ObservabilityRoutesOption func(*observabilityRoutesOptions)
	// ObservabilityClientOption configures the observability client.
	ObservabilityClientOption func(*observabilityClientOptions)

	observabilityRoutesOptions struct{}
	observabilityClientOptions struct{}
)

// ObservabilityRoutesDeps are the dependencies of the observability
// routes (all required; the composition root may hand holders that answer
// domain.ErrNotImplemented until the services are composed).
type ObservabilityRoutesDeps struct {
	Metrics metrics.Exporter
	History metrics.History
	Trace   trace.Service
	Logs    logs.Service
	Audit   audit.Service
}

// observabilityRoutes implements ObservabilityRoutes.
type observabilityRoutes struct {
	deps ObservabilityRoutesDeps
}

var _ ObservabilityRoutes = (*observabilityRoutes)(nil)

// NewObservabilityRoutes exposes the observability of the node.
func NewObservabilityRoutes(deps ObservabilityRoutesDeps, opts ...ObservabilityRoutesOption) (ObservabilityRoutes, error) {
	_ = apply(opts)
	if deps.Metrics == nil || deps.History == nil || deps.Trace == nil || deps.Logs == nil || deps.Audit == nil {
		return nil, fmt.Errorf("%w: rpc: the observability routes need the metrics, history, trace, logs and audit services", domain.ErrInvalidArgument)
	}
	return &observabilityRoutes{deps: deps}, nil
}

// scopeArgs are the arguments of obs/metrics.
type scopeArgs struct {
	Scope metrics.Scope
}

// traceMessage is a message of the trace stream: an event, with the
// events the node dropped since the previous message.
type traceMessage struct {
	Event   *trace.Event
	Dropped uint64
}

// logMessage is a message of the log stream.
type logMessage struct {
	Entry   *logs.Entry
	Dropped uint64
}

// Register implements Routes.
func (h *observabilityRoutes) Register(r chi.Router) {
	r.Route("/obs", func(r chi.Router) {
		r.Post("/metrics", unary(func(ctx context.Context, in scopeArgs) ([]metrics.Family, error) {
			return h.deps.Metrics.Local(ctx, in.Scope)
		}))
		r.Post("/history", unary(func(ctx context.Context, q metrics.HistoryQuery) (metrics.NodeSeries, error) {
			q.Local, q.Nodes = true, nil
			res, err := h.deps.History.Query(ctx, q)
			if err != nil {
				return metrics.NodeSeries{}, err
			}
			if len(res.Nodes) == 0 {
				return metrics.NodeSeries{}, fmt.Errorf("%w: no series", domain.ErrNotFound)
			}
			return res.Nodes[0], nil
		}))
		r.Post("/audit", unary(func(ctx context.Context, q audit.Query) ([]audit.Entry, error) {
			q.Local, q.Nodes = true, nil
			return h.deps.Audit.Search(ctx, q)
		}))
		r.Post("/trace", h.trace)
		r.Post("/logs", h.logs)
	})
}

// trace streams the events of this node selected by the filter of the
// caller until it goes away.
func (h *observabilityRoutes) trace(w http.ResponseWriter, r *http.Request) {
	var f trace.Filter
	if err := decodeBody(w, r, &f); err != nil {
		writeError(w, err)
		return
	}
	f.Local, f.Nodes = true, nil
	sub, err := h.deps.Trace.Subscribe(r.Context(), f)
	if err != nil {
		writeError(w, err)
		return
	}
	lw := startLive(w)
	var reported uint64
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-sub.Events():
			if !ok {
				lw.end()
				return
			}
			msg := traceMessage{Event: &e}
			if d := sub.Dropped(); d > reported {
				msg.Dropped, reported = d-reported, d
			}
			if err := lw.send(msg, len(sub.Events()) == 0); err != nil {
				return
			}
		}
	}
}

// logs streams the entries of this node selected by the filter of the
// caller until it goes away.
func (h *observabilityRoutes) logs(w http.ResponseWriter, r *http.Request) {
	var f logs.Filter
	if err := decodeBody(w, r, &f); err != nil {
		writeError(w, err)
		return
	}
	f.Local, f.Nodes = true, nil
	sub, err := h.deps.Logs.Subscribe(r.Context(), f)
	if err != nil {
		writeError(w, err)
		return
	}
	lw := startLive(w)
	var reported uint64
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-sub.Entries():
			if !ok {
				lw.end()
				return
			}
			msg := logMessage{Entry: &e}
			if d := sub.Dropped(); d > reported {
				msg.Dropped, reported = d-reported, d
			}
			if err := lw.send(msg, len(sub.Entries()) == 0); err != nil {
				return
			}
		}
	}
}

// liveWriter writes a live message stream: the answer starts at once (the
// caller waits for it within its call timeout) and the messages are
// flushed as soon as no other one is waiting.
type liveWriter struct {
	fw *frameWriter
	rc *http.ResponseController
}

func startLive(w http.ResponseWriter) *liveWriter {
	w.Header().Set("Content-Type", contentTypeStream)
	w.WriteHeader(http.StatusOK)
	lw := &liveWriter{fw: newFrameWriter(w), rc: http.NewResponseController(w)}
	_ = lw.rc.Flush()
	return lw
}

// send writes a message, flushed when flush is set or the buffer is full.
func (l *liveWriter) send(v any, flush bool) error {
	if err := l.fw.Msg(v); err != nil {
		return err
	}
	if !flush && l.fw.Buffered() < streamFlushSize {
		return nil
	}
	if err := l.fw.Flush(); err != nil {
		return err
	}
	return l.rc.Flush()
}

// end closes the stream.
func (l *liveWriter) end() {
	_ = l.fw.End()
	_ = l.fw.Flush()
	_ = l.rc.Flush()
}

// ObservabilityClientDeps are the dependencies of the observability
// client.
type ObservabilityClientDeps struct {
	// Conn reaches the node (required).
	Conn Conn
}

// observabilityClient implements ObservabilityPeer over the
// observability routes of another node.
type observabilityClient struct {
	deps ObservabilityClientDeps
}

var _ ObservabilityPeer = (*observabilityClient)(nil)

// NewObservabilityClient returns the observability of another node.
func NewObservabilityClient(deps ObservabilityClientDeps, opts ...ObservabilityClientOption) (ObservabilityPeer, error) {
	_ = apply(opts)
	if deps.Conn == nil {
		return nil, fmt.Errorf("%w: rpc: the observability client needs a connection", domain.ErrInvalidArgument)
	}
	return &observabilityClient{deps: deps}, nil
}

// NodeID implements ObservabilityPeer.
func (c *observabilityClient) NodeID() string { return c.deps.Conn.Node() }

// Families implements metrics.Peer.
func (c *observabilityClient) Families(ctx context.Context, scope metrics.Scope) ([]metrics.Family, error) {
	var out []metrics.Family
	err := c.deps.Conn.Call(ctx, "obs/metrics", scopeArgs{Scope: scope}, &out)
	return out, err
}

// History implements metrics.Peer.
func (c *observabilityClient) History(ctx context.Context, q metrics.HistoryQuery) (metrics.NodeSeries, error) {
	var out metrics.NodeSeries
	err := c.deps.Conn.Call(ctx, "obs/history", q, &out)
	return out, err
}

// SearchAudit implements audit.Peer.
func (c *observabilityClient) SearchAudit(ctx context.Context, q audit.Query) ([]audit.Entry, error) {
	var out []audit.Entry
	err := c.deps.Conn.Call(ctx, "obs/audit", q, &out)
	return out, err
}

// Trace implements trace.Peer.
func (c *observabilityClient) Trace(ctx context.Context, f trace.Filter, fn func(trace.Delivery) error) error {
	s, err := c.deps.Conn.Stream(ctx, "obs/trace", f)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	for {
		var m traceMessage
		if err := s.Next(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := fn(trace.Delivery{Event: m.Event, Dropped: m.Dropped}); err != nil {
			return err
		}
	}
}

// Logs implements logs.Peer.
func (c *observabilityClient) Logs(ctx context.Context, f logs.Filter, fn func(logs.Delivery) error) error {
	s, err := c.deps.Conn.Stream(ctx, "obs/logs", f)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	for {
		var m logMessage
		if err := s.Next(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := fn(logs.Delivery{Entry: m.Entry, Dropped: m.Dropped}); err != nil {
			return err
		}
	}
}
