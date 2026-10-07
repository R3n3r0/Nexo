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

// New returns a fresh Prometheus registry with the Go runtime and process
// collectors registered.
func New(_ Deps, opts ...Option) Registry {
	o := options{runtimeCollectors: true}
	for _, opt := range opts {
		opt(&o)
	}
	reg := prometheus.NewRegistry()
	if o.runtimeCollectors {
		reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	}
	return reg
}

var _ Registry = (*prometheus.Registry)(nil)
