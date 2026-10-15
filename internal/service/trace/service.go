// Package trace holds the request tracing of the server:
//
//   - Service is the live trace (S-OPS-07, docs/05 T-8.3): every request
//     served by the S3 API, the Admin API, the web listeners and the
//     internode RPC is published as an Event to the subscribers of the
//     node, with their filters (node, type, API, status, bucket, errors
//     only, duration threshold); a subscription of the console or of the
//     CLI also receives the events of every other node of the cluster,
//     streamed through the internode RPC (Peer). Publishing never blocks a
//     request: a subscriber that does not keep up loses events, counted.
//     The events never carry a secret: no header, no request body, the
//     presigned signatures, credentials and tokens of the query redacted;
//   - Telemetry is the distributed tracing (OpenTelemetry, docs/05 T-8.5):
//     a span per HTTP request and per internode call, exported over OTLP
//     and propagated between the nodes with the W3C traceparent header.
//     Disabled (no endpoint configured) it records nothing and costs
//     nothing. The OpenTelemetry types never leave the package.
package trace

import (
	"context"
	"net/http"
	"time"

	"github.com/r3n3r0/nexo/internal/lifecycle"
)

// Type is the API that served a request.
type Type string

// Types of the traced requests.
const (
	// TypeS3 is the S3 API (port 9000).
	TypeS3 Type = "s3"
	// TypeAdmin is the Admin API and the console (port 9001).
	TypeAdmin Type = "admin"
	// TypeWeb is the web listener of the distributions.
	TypeWeb Type = "web"
	// TypeRPC is the internode RPC (only traced on request: Filter.Types).
	TypeRPC Type = "rpc"
)

// Event is one request served by a node.
type Event struct {
	Time time.Time
	// Node is the node that served the request.
	Node string
	Type Type
	// API is the operation (PutObject, "GET /buckets/{bucket}",
	// "storage/read-version"), "" when the request was refused before the
	// routing (authentication).
	API    string
	Method string
	// Path is the request path; Query its query string with the values of
	// the sensitive parameters (signatures, credentials, tokens) redacted.
	Path  string
	Query string
	// Status is the HTTP status of the answer.
	Status   int
	Duration time.Duration
	// InputBytes and OutputBytes are the sizes of the request and answer
	// bodies.
	InputBytes  int64
	OutputBytes int64
	RequestID   string
	ClientIP    string
	// User is the access key of the caller ("" for anonymous requests).
	User string
	// Bucket is the bucket of an S3 request.
	Bucket string
	// Error describes a failed request.
	Error string
}

// Filter selects the events of a subscription. The zero Filter selects
// every event of the S3 API, the Admin API and the web listeners of every
// node (not the internode calls).
type Filter struct {
	// Nodes restricts the nodes (empty: every node).
	Nodes []string
	// Types restricts the types (empty: every type but TypeRPC).
	Types []Type
	// APIs restricts the operations (exact names, case-insensitive).
	APIs []string
	// Buckets restricts the buckets.
	Buckets []string
	// StatusCodes and StatusClasses restrict the statuses: an event passes
	// when its status is one of the codes or its first digit one of the
	// classes (4 for 4xx). Both empty: every status.
	StatusCodes   []int
	StatusClasses []int
	// ErrorsOnly keeps the failed requests (status >= 400 or an error).
	ErrorsOnly bool
	// MinDuration keeps the requests that lasted at least this long.
	MinDuration time.Duration
	// Local subscribes to the events of this node only (a peer streaming
	// its events to the node of the subscriber).
	Local bool
}

// Subscription is a live subscription. It ends with the context given to
// Subscribe: Events is then closed.
type Subscription interface {
	// Events delivers the events. It is a pure accessor.
	Events() <-chan Event
	// Dropped counts the events lost because the subscriber did not keep
	// up, on this node and on the peers. It is a pure accessor.
	Dropped() uint64
}

// Delivery is one message of the stream of a peer: an event, or (Event
// nil) the count of the events the peer dropped since its last message.
type Delivery struct {
	Event   *Event
	Dropped uint64
}

// Peer is the trace of another node (internode RPC).
type Peer interface {
	// NodeID returns the "host:port" of the peer. It is a pure accessor.
	NodeID() string
	// Trace streams the events of the peer selected by f (Local set) to
	// fn until ctx ends, fn fails or the stream breaks.
	Trace(ctx context.Context, f Filter, fn func(Delivery) error) error
}

// Service is the live trace of the requests.
type Service interface {
	// Enabled reports whether someone subscribed on this node: the
	// publishers skip building the events otherwise. It is a pure
	// accessor.
	Enabled() bool
	// Publish delivers e to the matching subscribers of this node, never
	// blocking: a subscriber whose buffer is full loses it (counted). The
	// query of e is redacted again here.
	Publish(ctx context.Context, e Event)
	// Subscribe returns the events selected by f: of this node and,
	// unless f.Local, of the peers (reconnected when their stream breaks)
	// until ctx ends. An invalid filter is domain.ErrInvalidArgument.
	Subscribe(ctx context.Context, f Filter) (Subscription, error)
}

// SpanKind is the role of a span.
type SpanKind int

// Kinds of the spans.
const (
	SpanInternal SpanKind = iota
	// SpanServer is a request served by this node.
	SpanServer
	// SpanClient is a call made by this node.
	SpanClient
)

// Attribute is an attribute of a span: Value is a string, a bool, an
// integer or a float (anything else is recorded as its text).
type Attribute struct {
	Key   string
	Value any
}

// SpanOptions configure a new span.
type SpanOptions struct {
	Kind       SpanKind
	Attributes []Attribute
}

// Span is a span being recorded. Its methods only change the span in
// memory (End hands it to the batching exporter): the documented
// exception to the context rule.
type Span interface {
	// SetName renames the span (the operation is known after the routing).
	SetName(name string)
	// SetAttributes adds attributes.
	SetAttributes(attrs ...Attribute)
	// SetError marks the span failed with a description.
	SetError(description string)
	// End ends the span.
	End()
}

// Telemetry is the distributed tracing of the node (OpenTelemetry).
type Telemetry interface {
	// Start does nothing; Stop flushes the spans and stops the exporter.
	lifecycle.Runner
	// Enabled reports whether spans are recorded. It is a pure accessor.
	Enabled() bool
	// StartSpan starts a span, child of the span of ctx, and returns the
	// context carrying it.
	StartSpan(ctx context.Context, name string, opts SpanOptions) (context.Context, Span)
	// Extract returns ctx with the remote parent of the W3C trace context
	// headers of h (traceparent, tracestate), ctx itself without one.
	Extract(ctx context.Context, h http.Header) context.Context
	// Inject writes the trace context of the span of ctx into h.
	Inject(ctx context.Context, h http.Header)
}
