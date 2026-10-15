package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Deps are the dependencies of the registry.
type Deps struct {
	// Logger reports dropped samples (default slog.Default()).
	Logger *slog.Logger
}

// Option configures the registry.
type Option func(*options)

type options struct {
	runtimeCollectors bool
	observeTimeout    time.Duration
}

// WithoutRuntimeCollectors skips the Go and process collectors (tests).
func WithoutRuntimeCollectors() Option {
	return func(o *options) { o.runtimeCollectors = false }
}

// WithObserveTimeout bounds every call of an observed source
// (Registry.Observe) during a collection (default 3s): a drive that hangs
// does not hang the scrape.
func WithObserveTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.observeTimeout = d
		}
	}
}

// registry is the Registry implementation: a Prometheus registry owned by
// the service. The Prometheus types never leave this package.
type registry struct {
	reg     *prometheus.Registry
	handler http.Handler
	logger  *slog.Logger
	opts    options
}

var _ Registry = (*registry)(nil)

// New returns a fresh registry with the Go runtime and process collectors
// registered.
func New(deps Deps, opts ...Option) Registry {
	o := options{runtimeCollectors: true, observeTimeout: 3 * time.Second}
	for _, opt := range opts {
		opt(&o)
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	reg := prometheus.NewRegistry()
	if o.runtimeCollectors {
		reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	}
	return &registry{
		reg:     reg,
		handler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError}),
		logger:  logger,
		opts:    o,
	}
}

// defaultLatencyBuckets are the histogram upper bounds, in seconds, used
// when a definition does not set its own.
func defaultLatencyBuckets() []float64 {
	return []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}
}

// Handler implements Registry.
func (r *registry) Handler() http.Handler { return r.handler }

// Counter implements Registry.
func (r *registry) Counter(_ context.Context, def Definition) (Counter, error) {
	if err := validate(def); err != nil {
		return nil, err
	}
	vec := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace, Subsystem: def.Subsystem, Name: def.Name, Help: def.Help,
	}, def.Labels)
	got, err := r.register(def, vec)
	if err != nil {
		return nil, err
	}
	cv, ok := got.(*prometheus.CounterVec)
	if !ok {
		return nil, kindClash(def)
	}
	return &counter{sample: r.sample(def), vec: cv}, nil
}

// Gauge implements Registry.
func (r *registry) Gauge(_ context.Context, def Definition) (Gauge, error) {
	if err := validate(def); err != nil {
		return nil, err
	}
	vec := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: Namespace, Subsystem: def.Subsystem, Name: def.Name, Help: def.Help,
	}, def.Labels)
	got, err := r.register(def, vec)
	if err != nil {
		return nil, err
	}
	gv, ok := got.(*prometheus.GaugeVec)
	if !ok {
		return nil, kindClash(def)
	}
	return &gauge{sample: r.sample(def), vec: gv}, nil
}

// Histogram implements Registry.
func (r *registry) Histogram(_ context.Context, def Definition) (Histogram, error) {
	if err := validate(def); err != nil {
		return nil, err
	}
	buckets := def.Buckets
	if len(buckets) == 0 {
		buckets = defaultLatencyBuckets()
	}
	// prometheus panics on these when the first child is created: reject
	// them up front.
	for i := 1; i < len(buckets); i++ {
		if buckets[i] <= buckets[i-1] {
			return nil, fmt.Errorf("%w: metrics: histogram %s: buckets must be strictly increasing", domain.ErrInvalidArgument, fqName(def))
		}
	}
	for _, l := range def.Labels {
		if l == "le" {
			return nil, fmt.Errorf("%w: metrics: histogram %s: label %q is reserved", domain.ErrInvalidArgument, fqName(def), l)
		}
	}
	vec := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: Namespace, Subsystem: def.Subsystem, Name: def.Name, Help: def.Help, Buckets: buckets,
	}, def.Labels)
	got, err := r.register(def, vec)
	if err != nil {
		return nil, err
	}
	hv, ok := got.(*prometheus.HistogramVec)
	if !ok {
		return nil, kindClash(def)
	}
	return &histogram{sample: r.sample(def), vec: hv}, nil
}

// register adds c to the registry, or returns the collector already
// registered under the same definition.
func (r *registry) register(def Definition, c prometheus.Collector) (prometheus.Collector, error) {
	err := r.reg.Register(c)
	if err == nil {
		return c, nil
	}
	var already prometheus.AlreadyRegisteredError
	if errors.As(err, &already) {
		return already.ExistingCollector, nil
	}
	return nil, fmt.Errorf("%w: metrics: register %s: %v", domain.ErrInvalidArgument, fqName(def), err)
}

func (r *registry) sample(def Definition) sampleInfo {
	return sampleInfo{name: fqName(def), logger: r.logger}
}

func validate(def Definition) error {
	if def.Name == "" {
		return fmt.Errorf("%w: metrics: metric name is required", domain.ErrInvalidArgument)
	}
	if def.Help == "" {
		return fmt.Errorf("%w: metrics: metric %s needs a help text", domain.ErrInvalidArgument, fqName(def))
	}
	if !validName(fqName(def), true) {
		return fmt.Errorf("%w: metrics: invalid metric name %q", domain.ErrInvalidArgument, fqName(def))
	}
	seen := make(map[string]bool, len(def.Labels))
	for _, l := range def.Labels {
		if !validName(l, false) || strings.HasPrefix(l, "__") || seen[l] {
			return fmt.Errorf("%w: metrics: metric %s: invalid or duplicate label %q", domain.ErrInvalidArgument, fqName(def), l)
		}
		seen[l] = true
	}
	return nil
}

// validName checks the classic Prometheus naming rules
// ([a-zA-Z_:][a-zA-Z0-9_:]* for metrics, no colon for labels), stricter
// than the UTF-8 names newer clients accept, so that every scraper and
// query language can address the series.
func validName(name string, metric bool) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		case r == ':' && metric:
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func kindClash(def Definition) error {
	return fmt.Errorf("%w: metrics: %s is already registered with another kind", domain.ErrInvalidArgument, fqName(def))
}

func fqName(def Definition) string {
	return prometheus.BuildFQName(Namespace, def.Subsystem, def.Name)
}

// sampleInfo reports updates that do not fit the definition.
type sampleInfo struct {
	name   string
	logger *slog.Logger
}

func (s sampleInfo) drop(ctx context.Context, err error) {
	s.logger.WarnContext(ctx, "metrics: sample dropped", "metric", s.name, "err", err)
}

type counter struct {
	sample sampleInfo
	vec    *prometheus.CounterVec
}

var _ Counter = (*counter)(nil)

// Add implements Counter.
func (c *counter) Add(ctx context.Context, delta float64, labelValues ...string) {
	if delta < 0 {
		c.sample.drop(ctx, fmt.Errorf("%w: negative counter increment %g", domain.ErrInvalidArgument, delta))
		return
	}
	m, err := c.vec.GetMetricWithLabelValues(labelValues...)
	if err != nil {
		c.sample.drop(ctx, err)
		return
	}
	m.Add(delta)
}

type gauge struct {
	sample sampleInfo
	vec    *prometheus.GaugeVec
}

var _ Gauge = (*gauge)(nil)

// Add implements Gauge.
func (g *gauge) Add(ctx context.Context, delta float64, labelValues ...string) {
	m, err := g.vec.GetMetricWithLabelValues(labelValues...)
	if err != nil {
		g.sample.drop(ctx, err)
		return
	}
	m.Add(delta)
}

// Set implements Gauge.
func (g *gauge) Set(ctx context.Context, v float64, labelValues ...string) {
	m, err := g.vec.GetMetricWithLabelValues(labelValues...)
	if err != nil {
		g.sample.drop(ctx, err)
		return
	}
	m.Set(v)
}

type histogram struct {
	sample sampleInfo
	vec    *prometheus.HistogramVec
}

var _ Histogram = (*histogram)(nil)

// Observe implements Histogram.
func (h *histogram) Observe(ctx context.Context, v float64, labelValues ...string) {
	m, err := h.vec.GetMetricWithLabelValues(labelValues...)
	if err != nil {
		h.sample.drop(ctx, err)
		return
	}
	m.Observe(v)
}

// Observe implements Registry.
func (r *registry) Observe(_ context.Context, def Definition, kind Kind, fn ObserveFunc) error {
	if err := validate(def); err != nil {
		return err
	}
	if fn == nil {
		return fmt.Errorf("%w: metrics: observed metric %s needs a source", domain.ErrInvalidArgument, fqName(def))
	}
	var vt prometheus.ValueType
	switch kind {
	case KindCounter:
		vt = prometheus.CounterValue
	case KindGauge:
		vt = prometheus.GaugeValue
	default:
		return fmt.Errorf("%w: metrics: observed metric %s must be a counter or a gauge", domain.ErrInvalidArgument, fqName(def))
	}
	c := &observedCollector{
		desc:    prometheus.NewDesc(fqName(def), def.Help, def.Labels, nil),
		kind:    vt,
		fn:      fn,
		labels:  len(def.Labels),
		sample:  r.sample(def),
		timeout: r.opts.observeTimeout,
	}
	if err := r.reg.Register(c); err != nil {
		return fmt.Errorf("%w: metrics: register %s: %v", domain.ErrInvalidArgument, fqName(def), err)
	}
	return nil
}

// observedCollector collects the values of an ObserveFunc at every
// collection.
type observedCollector struct {
	desc    *prometheus.Desc
	kind    prometheus.ValueType
	fn      ObserveFunc
	labels  int
	sample  sampleInfo
	timeout time.Duration
}

var _ prometheus.Collector = (*observedCollector)(nil)

// Describe implements prometheus.Collector.
func (c *observedCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

// Collect implements prometheus.Collector: the source runs with a bounded
// context (the library gives none); a series reported twice keeps its last
// value, a series with the wrong number of label values is dropped.
func (c *observedCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	obs := c.fn(ctx)
	last := make(map[string]int, len(obs))
	for i, o := range obs {
		last[strings.Join(o.LabelValues, "\xff")] = i
	}
	for i, o := range obs {
		if last[strings.Join(o.LabelValues, "\xff")] != i {
			continue
		}
		if len(o.LabelValues) != c.labels {
			c.sample.drop(ctx, fmt.Errorf("%w: %d label values for %d labels", domain.ErrInvalidArgument, len(o.LabelValues), c.labels))
			continue
		}
		m, err := prometheus.NewConstMetric(c.desc, c.kind, o.Value, o.LabelValues...)
		if err != nil {
			c.sample.drop(ctx, err)
			continue
		}
		ch <- m
	}
}

// Snapshot implements Registry. A collection error (a source failing) is
// logged: the families gathered are returned.
func (r *registry) Snapshot(ctx context.Context) ([]Family, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mfs, err := r.reg.Gather()
	if err != nil {
		r.logger.WarnContext(ctx, "metrics: partial collection", "err", err)
	}
	return fromProto(mfs), nil
}
