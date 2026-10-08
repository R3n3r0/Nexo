// Package metrics is the metrics registry of a node. The contract is
// library-neutral: services and transports create their instruments through
// the typed factories of Registry and expose them with Registry.Handler; the
// metrics library (Prometheus) never leaves this package (docs/12 §12.2
// rule 9).
package metrics

import (
	"context"
	"net/http"
)

// Namespace is the prefix of every Nexo metric.
const Namespace = "nexo"

// Registry creates metric instruments and serves them over HTTP.
//
// The factories are idempotent: asking again for a metric with the same
// subsystem, name and label names returns the instrument already
// registered, so that two listeners (S3 and console) can share one
// definition and tell themselves apart with a label. A definition that
// clashes with an existing one (same name, different kind or labels) fails
// with domain.ErrInvalidArgument.
type Registry interface {
	// Counter returns the counter vector described by def.
	Counter(ctx context.Context, def Definition) (Counter, error)
	// Gauge returns the gauge vector described by def.
	Gauge(ctx context.Context, def Definition) (Gauge, error)
	// Histogram returns the histogram vector described by def; without
	// def.Buckets it uses latency buckets from 1ms to 30s.
	Histogram(ctx context.Context, def Definition) (Histogram, error)
	// Handler returns the HTTP handler serving every registered metric in
	// the Prometheus text exposition format. It is a pure accessor (no I/O,
	// never blocks): the documented exception to the context rule.
	Handler() http.Handler
}

// Definition describes a metric vector.
type Definition struct {
	// Subsystem and Name form the metric name nexo_<subsystem>_<name>.
	Subsystem string
	Name      string
	// Help is the description shown by the exposition format.
	Help string
	// Labels are the names of the variable labels; every update passes one
	// value per label, in this order.
	Labels []string
	// Buckets are the histogram upper bounds (histograms only); empty
	// selects the default latency buckets, in seconds.
	Buckets []float64
}

// Counter is a monotonically increasing metric vector. Updates with a
// number of label values that does not match the definition are dropped
// (and logged), never panic.
type Counter interface {
	// Add adds delta (>= 0) to the counter identified by labelValues.
	Add(ctx context.Context, delta float64, labelValues ...string)
}

// Gauge is a metric vector that goes up and down.
type Gauge interface {
	// Add adds delta (possibly negative) to the gauge identified by labelValues.
	Add(ctx context.Context, delta float64, labelValues ...string)
	// Set sets the gauge identified by labelValues to v.
	Set(ctx context.Context, v float64, labelValues ...string)
}

// Histogram samples observations into buckets.
type Histogram interface {
	// Observe records v in the histogram identified by labelValues.
	Observe(ctx context.Context, v float64, labelValues ...string)
}
