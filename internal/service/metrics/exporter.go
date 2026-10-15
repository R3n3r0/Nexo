package metrics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/scanner"
)

// ExporterDeps are the dependencies of the exporter.
type ExporterDeps struct {
	// Registry holds the families of this node (required).
	Registry Registry
	// NodeID names this node in the label server ("host:port" of a
	// cluster node, "local" otherwise).
	NodeID string
	// Peers are the other nodes of a cluster (none on a single node).
	Peers []Peer
	// Cluster describes the deployment for the cluster families (nil:
	// none).
	Cluster ClusterInfo
	// Usage provides the data usage of the buckets computed by the
	// scanner, cluster wide (nil: none).
	Usage  scanner.Service
	Logger *slog.Logger
}

// ExporterOption configures the exporter.
type ExporterOption func(*exporterOptions)

type exporterOptions struct {
	peerTimeout time.Duration
}

// WithPeerTimeout bounds the collection of the families of a peer
// (default 5s): a slow node is left out of the scrape.
func WithPeerTimeout(d time.Duration) ExporterOption {
	return func(o *exporterOptions) {
		if d > 0 {
			o.peerTimeout = d
		}
	}
}

// LabelServer is the label naming the node of a series.
const LabelServer = "server"

// bucketPrefix is the prefix of the families of the bucket scope.
const bucketPrefix = Namespace + "_bucket_"

// clusterPrefix is the prefix of the cluster-wide families, computed by
// the exporter (never registered).
const clusterPrefix = Namespace + "_cluster_"

type exporter struct {
	deps ExporterDeps
	opts exporterOptions
}

var _ Exporter = (*exporter)(nil)

// NewExporter returns the exporter of the scrape endpoints. A missing
// registry is a wiring error (domain.ErrInvalidArgument).
func NewExporter(deps ExporterDeps, opts ...ExporterOption) (Exporter, error) {
	if deps.Registry == nil {
		return nil, fmt.Errorf("%w: metrics: the exporter needs the registry", domain.ErrInvalidArgument)
	}
	if deps.NodeID == "" {
		deps.NodeID = "local"
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := exporterOptions{peerTimeout: 5 * time.Second}
	for _, opt := range opts {
		opt(&o)
	}
	return &exporter{deps: deps, opts: o}, nil
}

// Local implements Exporter.
func (e *exporter) Local(ctx context.Context, scope Scope) ([]Family, error) {
	if err := validScope(scope); err != nil {
		return nil, err
	}
	all, err := e.deps.Registry.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Family, 0, len(all))
	for _, f := range all {
		bucket := strings.HasPrefix(f.Name, bucketPrefix)
		if bucket == (scope == ScopeBucket) {
			out = append(out, f)
		}
	}
	return out, nil
}

// Gather implements Exporter.
func (e *exporter) Gather(ctx context.Context, scope Scope) ([]Family, error) {
	local, err := e.Local(ctx, scope)
	if err != nil {
		return nil, err
	}
	switch scope {
	case ScopeNode:
		return withLabel(local, LabelServer, e.deps.NodeID), nil
	case ScopeBucket:
		sources := append([][]Family{local}, e.peers(ctx, scope)...)
		return append(mergeFamilies(true, sources...), e.usageFamilies(ctx)...), nil
	default:
		sources := [][]Family{withLabel(local, LabelServer, e.deps.NodeID)}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, p := range e.deps.Peers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				fams, err := e.peerFamilies(ctx, p, ScopeNode)
				if err != nil {
					return
				}
				mu.Lock()
				sources = append(sources, withLabel(fams, LabelServer, p.NodeID()))
				mu.Unlock()
			}()
		}
		wg.Wait()
		return append(mergeFamilies(false, sources...), e.clusterFamilies(ctx)...), nil
	}
}

// peers returns the families of scope of every reachable peer.
func (e *exporter) peers(ctx context.Context, scope Scope) [][]Family {
	out := make([][]Family, len(e.deps.Peers))
	var wg sync.WaitGroup
	for i, p := range e.deps.Peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fams, err := e.peerFamilies(ctx, p, scope)
			if err == nil {
				out[i] = fams
			}
		}()
	}
	wg.Wait()
	return out
}

// peerFamilies collects the families of a peer within the peer timeout.
func (e *exporter) peerFamilies(ctx context.Context, p Peer, scope Scope) ([]Family, error) {
	ctx, cancel := context.WithTimeout(ctx, e.opts.peerTimeout)
	defer cancel()
	fams, err := p.Families(ctx, scope)
	if err != nil {
		e.deps.Logger.DebugContext(ctx, "metrics: peer families unavailable", "peer", p.NodeID(), "scope", string(scope), "err", err)
		return nil, err
	}
	return fams, nil
}

// WriteText implements Exporter.
func (e *exporter) WriteText(ctx context.Context, w io.Writer, families []Family) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeText(w, families)
}

func validScope(scope Scope) error {
	switch scope {
	case ScopeNode, ScopeCluster, ScopeBucket:
		return nil
	}
	return fmt.Errorf("%w: metrics: unknown scope %q", domain.ErrInvalidArgument, scope)
}

// gaugeFamily builds a gauge family of the cluster or bucket scope.
type familyBuilder struct {
	fams  []Family
	index map[string]int
}

func newFamilyBuilder() *familyBuilder { return &familyBuilder{index: map[string]int{}} }

// add adds a sample to the family name (created with help and kind).
func (b *familyBuilder) add(name, help string, kind Kind, v float64, labels ...string) {
	i, ok := b.index[name]
	if !ok {
		i = len(b.fams)
		b.index[name] = i
		b.fams = append(b.fams, Family{Name: name, Help: help, Kind: kind})
	}
	s := Sample{Labels: map[string]string{}, Value: v}
	for k := 0; k+1 < len(labels); k += 2 {
		s.Labels[labels[k]] = labels[k+1]
	}
	b.fams[i].Samples = append(b.fams[i].Samples, s)
}

// clusterFamilies computes the cluster-wide families from the server
// information: nodes, drives, erasure sets by state, raw and usable
// capacity, quorums and data usage.
func (e *exporter) clusterFamilies(ctx context.Context) []Family {
	if e.deps.Cluster == nil {
		return nil
	}
	info, err := e.deps.Cluster.ServerInfo(ctx)
	if err != nil || info == nil {
		e.deps.Logger.WarnContext(ctx, "metrics: cluster information unavailable", "err", err)
		return nil
	}
	b := newFamilyBuilder()
	var nodesOn, nodesOff, drivesOn, drivesOff, drivesHealing, drives float64
	var rawTotal, rawFree, rawUsed float64
	poolRaw := map[int][2]float64{}
	for _, n := range info.Nodes {
		if n.State == domain.StateOffline {
			nodesOff++
		} else {
			nodesOn++
		}
		for _, d := range n.Disks {
			drives++
			switch d.State {
			case domain.StateOffline, domain.StateUnknown:
				drivesOff++
				continue
			case domain.StateHealing:
				drivesHealing++
			}
			drivesOn++
			rawTotal += float64(d.Total)
			rawFree += float64(d.Free)
			rawUsed += float64(d.Used)
			pr := poolRaw[d.PoolIdx]
			pr[0] += float64(d.Total)
			pr[1] += float64(d.Free)
			poolRaw[d.PoolIdx] = pr
		}
	}
	b.add(clusterPrefix+"nodes_online", "Nodes of the deployment that answer.", KindGauge, nodesOn)
	b.add(clusterPrefix+"nodes_offline", "Nodes of the deployment that cannot be reached.", KindGauge, nodesOff)
	b.add(clusterPrefix+"drives_online", "Drives of the deployment that answer (healing ones included).", KindGauge, drivesOn)
	b.add(clusterPrefix+"drives_offline", "Drives of the deployment that cannot be reached.", KindGauge, drivesOff)
	b.add(clusterPrefix+"drives_healing", "Drives of the deployment being repopulated.", KindGauge, drivesHealing)
	b.add(clusterPrefix+"drives_total", "Drives of the deployment.", KindGauge, drives)
	b.add(clusterPrefix+"capacity_raw_total_bytes", "Raw capacity of the online drives.", KindGauge, rawTotal)
	b.add(clusterPrefix+"capacity_raw_free_bytes", "Raw free space of the online drives.", KindGauge, rawFree)
	b.add(clusterPrefix+"capacity_raw_used_bytes", "Raw used space of the online drives.", KindGauge, rawUsed)
	var usableTotal, usableFree float64
	for _, p := range info.Pools {
		pool := strconv.Itoa(p.Index)
		var healthy, degraded, readOnly, unavailable float64
		for _, s := range p.ErasureSets {
			switch s.State {
			case domain.SetStateOK:
				healthy++
			case domain.SetStateDegraded:
				degraded++
			case domain.SetStateReadOnly:
				readOnly++
			default:
				unavailable++
			}
		}
		b.add(clusterPrefix+"sets_healthy", "Erasure sets with every drive online, by pool.", KindGauge, healthy, "pool", pool)
		b.add(clusterPrefix+"sets_degraded", "Erasure sets missing drives that keep their write quorum, by pool.", KindGauge, degraded, "pool", pool)
		b.add(clusterPrefix+"sets_read_only", "Erasure sets that keep only their read quorum, by pool.", KindGauge, readOnly, "pool", pool)
		b.add(clusterPrefix+"sets_unavailable", "Erasure sets below their read quorum, by pool.", KindGauge, unavailable, "pool", pool)
		b.add(clusterPrefix+"write_quorum", "Drives of a set needed to write, by pool.", KindGauge, float64(p.WriteQuorum), "pool", pool)
		b.add(clusterPrefix+"read_quorum", "Drives of a set needed to read, by pool.", KindGauge, float64(p.ReadQuorum), "pool", pool)
		if p.DrivesPerSet > 0 {
			ratio := float64(p.DrivesPerSet-p.Parity) / float64(p.DrivesPerSet)
			usableTotal += poolRaw[p.Index][0] * ratio
			usableFree += poolRaw[p.Index][1] * ratio
		}
	}
	if len(info.Pools) == 0 {
		// A single drive: the raw capacity is the usable one.
		usableTotal, usableFree = rawTotal, rawFree
	}
	b.add(clusterPrefix+"capacity_usable_total_bytes", "Usable capacity of the online drives (raw capacity without the parity).", KindGauge, usableTotal)
	b.add(clusterPrefix+"capacity_usable_free_bytes", "Usable free space of the online drives.", KindGauge, usableFree)
	b.add(clusterPrefix+"buckets", "Buckets of the deployment (scanner).", KindGauge, float64(info.Buckets))
	b.add(clusterPrefix+"objects", "Objects of the deployment (scanner).", KindGauge, float64(info.Objects))
	b.add(clusterPrefix+"usage_bytes", "Logical bytes stored (scanner).", KindGauge, float64(info.Usage))
	return b.fams
}

// usageFamilies computes the data usage families of the buckets.
func (e *exporter) usageFamilies(ctx context.Context) []Family {
	if e.deps.Usage == nil {
		return nil
	}
	usage, err := e.deps.Usage.Usage(ctx)
	if err != nil {
		// No cycle completed yet (domain.ErrNotFound): no usage families.
		return nil
	}
	b := newFamilyBuilder()
	for name, u := range usage.Buckets {
		b.add(bucketPrefix+"usage_total_bytes", "Logical bytes of every version of the bucket (scanner).", KindGauge, float64(u.Size), "bucket", name)
		b.add(bucketPrefix+"objects", "Live objects of the bucket (scanner).", KindGauge, float64(u.Objects), "bucket", name)
		b.add(bucketPrefix+"versions", "Noncurrent versions of the bucket (scanner).", KindGauge, float64(u.Versions), "bucket", name)
		b.add(bucketPrefix+"delete_markers", "Delete markers of the bucket (scanner).", KindGauge, float64(u.DeleteMarkers), "bucket", name)
	}
	if !usage.LastUpdate.IsZero() {
		b.add(bucketPrefix+"usage_last_update_timestamp_seconds", "When the scanner cycle that computed the usage ended.", KindGauge, float64(usage.LastUpdate.Unix()))
	}
	return b.fams
}
