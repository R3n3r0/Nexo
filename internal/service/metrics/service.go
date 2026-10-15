// Package metrics is the metrics registry of a node. The contract is
// library-neutral: services and transports create their instruments through
// the typed factories of Registry and expose them with Registry.Handler; the
// metrics library (Prometheus) never leaves this package (docs/12 §12.2
// rule 9).
//
// Besides the registry the package holds:
//
//   - Exporter: the scrape endpoints /nexo/metrics/v1/{cluster,node,bucket}
//     (docs/07 §7.4): the families of this node, of every node of the
//     cluster (collected through the internode RPC, Peer) with the
//     cluster-wide families (nodes, drives, erasure sets, capacity), and
//     the families of the buckets;
//   - History: the low-resolution ring buffer of the key series of the
//     node (one point every 10 s for 24 h) that backs the charts of the
//     console (GET /api/v1/metrics/query), aggregated over the cluster;
//   - the decorators that measure the services they wrap without the
//     services knowing (WrapDrive, WrapLocker, WrapKMS, WrapTargetFactory,
//     WrapEvents, WrapLifecycle, NewRoundTripper) and RegisterSources, the
//     collectors read at scrape time (drives, healing queue, scanner,
//     notification targets, KMS, peers).
//
// Cardinality is bounded on purpose: no object key ever becomes a label,
// buckets only label the bucket scope (with a cap on the distinct values).
package metrics

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/lifecycle"
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
	// Observe registers fn as the source of the counter or gauge vector
	// def: fn is called at every collection (scrape, snapshot) with a
	// context bounded by a short timeout, and its observations are the
	// values of the vector at that time. It suits the values another
	// service already keeps (drive usage, queue lengths). Registering the
	// same name twice fails with domain.ErrInvalidArgument.
	Observe(ctx context.Context, def Definition, kind Kind, fn ObserveFunc) error
	// Snapshot returns the current value of every registered family,
	// sorted by name (the observed sources are called).
	Snapshot(ctx context.Context) ([]Family, error)
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

// Kind is the type of a metric family.
type Kind string

// Kinds of the families.
const (
	KindCounter   Kind = "counter"
	KindGauge     Kind = "gauge"
	KindHistogram Kind = "histogram"
	// KindSummary is a summary of the runtime collectors (quantiles).
	KindSummary Kind = "summary"
	// KindUntyped is a family whose type is unknown.
	KindUntyped Kind = "untyped"
)

// Observation is one value of an observed vector (Registry.Observe):
// LabelValues has one value per label of the definition, in order.
type Observation struct {
	LabelValues []string
	Value       float64
}

// ObserveFunc returns the current values of an observed vector.
type ObserveFunc func(ctx context.Context) []Observation

// Family is the snapshot of a metric family in a library-neutral form: the
// unit the exporter aggregates, the ring buffer samples and the internode
// RPC carries.
type Family struct {
	// Name is the complete metric name (nexo_s3_requests_total).
	Name string
	Help string
	Kind Kind
	// Samples are the series of the family.
	Samples []Sample
}

// Sample is one series of a family: its labels and its value (counters,
// gauges, untyped) or its distribution (histograms, summaries).
type Sample struct {
	Labels map[string]string
	Value  float64
	// Count and Sum describe a histogram or a summary; Buckets are the
	// cumulative buckets of a histogram, sorted by upper bound (the +Inf
	// bucket is Count), Quantiles the quantiles of a summary.
	Count     uint64
	Sum       float64
	Buckets   []Bucket
	Quantiles []Quantile
}

// Bucket is a cumulative histogram bucket.
type Bucket struct {
	UpperBound float64
	Count      uint64
}

// Quantile is a quantile of a summary.
type Quantile struct {
	Quantile float64
	Value    float64
}

// Scope selects the families of a scrape endpoint.
type Scope string

// Scopes of the scrape endpoints (/nexo/metrics/v1/<scope>).
const (
	// ScopeNode is every family measured by the node (API, drives, healing,
	// scanner, RPC, locks, KMS, notifications, cache, runtime), with the
	// label server naming the node.
	ScopeNode Scope = "node"
	// ScopeCluster is the node scope of every node of the deployment
	// (collected through the internode RPC), plus the cluster-wide
	// families (nexo_cluster_*: nodes, drives, erasure sets, capacity).
	ScopeCluster Scope = "cluster"
	// ScopeBucket is the families labeled by bucket (nexo_bucket_*): the
	// requests and traffic of the buckets summed over the nodes and the
	// data usage of the scanner.
	ScopeBucket Scope = "bucket"
)

// Exporter gathers the families of the scrape endpoints and encodes them.
type Exporter interface {
	// Local returns the families of scope measured by this node:
	// ScopeNode (and ScopeCluster) the node families, ScopeBucket the
	// families labeled by bucket (without the usage, which is cluster
	// wide). The internode RPC serves it to the peers.
	Local(ctx context.Context, scope Scope) ([]Family, error)
	// Gather returns the families of a scrape of scope: the node scope of
	// this node; the node scope of every node with the cluster families;
	// the bucket families summed over the nodes with the data usage. The
	// peers that cannot be reached are left out (nexo_cluster_nodes_offline
	// counts them).
	Gather(ctx context.Context, scope Scope) ([]Family, error)
	// WriteText encodes families in the Prometheus text exposition format
	// (version 0.0.4, TextContentType).
	WriteText(ctx context.Context, w io.Writer, families []Family) error
}

// TextContentType is the content type of the text exposition format.
const TextContentType = "text/plain; version=0.0.4; charset=utf-8"

// Peer is the metrics side of another node (internode RPC).
type Peer interface {
	// NodeID returns the "host:port" of the peer. It is a pure accessor.
	NodeID() string
	// Families returns Exporter.Local of the peer.
	Families(ctx context.Context, scope Scope) ([]Family, error)
	// History returns the series of the ring buffer of the peer
	// (History.Query with Local set).
	History(ctx context.Context, q HistoryQuery) (NodeSeries, error)
}

// ClusterInfo describes the deployment for the cluster families
// (admin.Service implements it).
type ClusterInfo interface {
	ServerInfo(ctx context.Context) (*domain.ServerInfo, error)
}

// SeriesInfo describes a series of the ring buffer.
type SeriesInfo struct {
	// Name identifies the series in the queries (s3_requests).
	Name        string `json:"name"`
	Description string `json:"description"`
	// Unit is the unit of the values (req/s, bytes, seconds...).
	Unit string `json:"unit"`
	// Aggregation tells how the values of the nodes combine into the
	// cluster series: "sum", "avg" or "max".
	Aggregation string `json:"aggregation"`
}

// HistoryQuery selects the points of a series.
type HistoryQuery struct {
	// Metric is the name of the series (History.Metrics).
	Metric string
	// Start and End bound the points (End defaults to now, Start to Range
	// before End; at most the retention).
	Start time.Time
	End   time.Time
	// Range is the span of the points when Start is zero (default one
	// hour).
	Range time.Duration
	// Step is the distance between two points (a multiple of the
	// resolution, default the smallest giving at most 360 points).
	Step time.Duration
	// Nodes restricts the nodes (empty: every node).
	Nodes []string
	// Local answers with the points of this node only (the peers ask it).
	Local bool
}

// NodeSeries are the points of one node: Values[i] is the value at
// Start+i*Step, NaN where no sample exists (the node was down, or the
// buffer does not go that far back).
type NodeSeries struct {
	Node   string
	Values []float64
	// Error tells why the points of a node could not be read.
	Error string
}

// HistoryResult is the answer of a query.
type HistoryResult struct {
	Metric SeriesInfo
	Start  time.Time
	End    time.Time
	Step   time.Duration
	// Nodes are the series of every node, this node first.
	Nodes []NodeSeries
	// Cluster is the aggregation of the nodes (NaN where no node has a
	// value).
	Cluster []float64
}

// History keeps the key series of the node in a low-resolution ring
// buffer (default one point every 10 s for 24 h, sampled from the
// registry) and answers the queries of the console, aggregated over the
// peers. It runs a sampler (lifecycle.Runner).
type History interface {
	lifecycle.Runner
	// Metrics lists the series the buffer keeps.
	Metrics(ctx context.Context) []SeriesInfo
	// Query returns the points of a series (domain.ErrNotFound for an
	// unknown series, domain.ErrInvalidArgument for an invalid range).
	Query(ctx context.Context, q HistoryQuery) (*HistoryResult, error)
	// Sample records one point of every series now (the runner does it
	// every resolution; the tests call it).
	Sample(ctx context.Context) error
}
