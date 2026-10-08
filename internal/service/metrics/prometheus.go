package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Deps are the dependencies of the registry (none).
type Deps struct{}

// Option configures the registry.
type Option func(*options)

type options struct {
	runtimeCollectors bool
}

// WithoutRuntimeCollectors skips the Go and process collectors (tests).
func WithoutRuntimeCollectors() Option {
	return func(o *options) { o.runtimeCollectors = false }
}

// registry is the Registry implementation: a Prometheus registry owned by
// the service, so that the composition root never handles the external type
// directly.
type registry struct {
	*prometheus.Registry
}

var _ Registry = (*registry)(nil)

// New returns a fresh registry with the Go runtime and process collectors
// registered.
func New(_ Deps, opts ...Option) Registry {
	o := options{runtimeCollectors: true}
	for _, opt := range opts {
		opt(&o)
	}
	reg := prometheus.NewRegistry()
	if o.runtimeCollectors {
		reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	}
	return &registry{Registry: reg}
}
