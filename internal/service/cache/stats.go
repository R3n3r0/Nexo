package cache

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/service/metrics"
)

// maxTrackedObjects bounds the paths counted for the top objects.
const maxTrackedObjects = 4096

// requestRecord is an answer of a distribution served through the cache.
type requestRecord struct {
	dist, host, path string
	result           Result
	fromCache        bool
	status           int
	bytes            int64
	elapsed          time.Duration
}

// hostCounters are the counters of a host.
type hostCounters struct {
	requests    map[Result]int64
	bytesCache  int64
	bytesOrigin int64
}

// objectCounters count the requests of a path.
type objectCounters struct {
	requests, misses int64
}

// statsState are the statistics of this node.
type statsState struct {
	mu      sync.Mutex
	hosts   map[[2]string]*hostCounters
	objects map[[3]string]*objectCounters
}

// cacheMetrics are the instruments of docs/11 §11.8 (nil without
// registry).
type cacheMetrics struct {
	requests      metrics.Counter
	bytes         metrics.Counter
	saved         metrics.Counter
	duration      metrics.Histogram
	hitRatio      metrics.Gauge
	size          metrics.Gauge
	entries       metrics.Gauge
	evictions     metrics.Counter
	invalidations metrics.Counter
	originReads   metrics.Counter
	webhooks      metrics.Counter
}

// newMetrics registers the metrics of the cache.
func newMetrics(ctx context.Context, r metrics.Registry) (*cacheMetrics, error) {
	if r == nil {
		return nil, nil
	}
	m := &cacheMetrics{}
	var err error
	counter := func(dst *metrics.Counter, name, help string, labels ...string) {
		if err == nil {
			*dst, err = r.Counter(ctx, metrics.Definition{Subsystem: "cache", Name: name, Help: help, Labels: labels})
		}
	}
	gauge := func(dst *metrics.Gauge, name, help string, labels ...string) {
		if err == nil {
			*dst, err = r.Gauge(ctx, metrics.Definition{Subsystem: "cache", Name: name, Help: help, Labels: labels})
		}
	}
	counter(&m.requests, "requests_total", "Requests of the distributions by cache result.", "distribution", "host", "result")
	counter(&m.bytes, "bytes_served_total", "Bytes served by the distributions, from the cache or from the origin.", "distribution", "host", "source")
	counter(&m.saved, "origin_bytes_saved_total", "Bytes served from the cache instead of the origin.", "distribution", "host")
	gauge(&m.hitRatio, "hit_ratio", "Share of the cacheable requests served from the cache.", "distribution", "host")
	gauge(&m.size, "size_bytes", "Bytes cached, by host and tier.", "host", "tier")
	gauge(&m.entries, "entries", "Entries of the cache of this node.")
	counter(&m.evictions, "evictions_total", "Entries evicted to stay within the sizes and quotas.", "tier")
	counter(&m.invalidations, "invalidations_total", "Invalidations applied by this node.", "type")
	counter(&m.originReads, "origin_reads_total", "Reads of the origin made by the cache (fills and revalidations).")
	counter(&m.webhooks, "webhook_calls_total", "Calls of the purge webhooks.", "type", "outcome")
	if err == nil {
		m.duration, err = r.Histogram(ctx, metrics.Definition{Subsystem: "cache", Name: "request_duration_seconds",
			Help: "Latency of the requests served through the cache, by result (HIT vs MISS).", Labels: []string{"result"}})
	}
	if err != nil {
		return nil, fmt.Errorf("cache metrics: %w", err)
	}
	return m, nil
}

// recordRequest counts an answer.
func (s *service) recordRequest(ctx context.Context, r requestRecord) {
	s.stats.mu.Lock()
	k := [2]string{r.dist, r.host}
	h := s.stats.hosts[k]
	if h == nil {
		h = &hostCounters{requests: map[Result]int64{}}
		s.stats.hosts[k] = h
	}
	h.requests[r.result]++
	if r.fromCache {
		h.bytesCache += r.bytes
	} else {
		h.bytesOrigin += r.bytes
	}
	ratio := hitRatio(h.requests)
	ok := [3]string{r.dist, r.host, r.path}
	o := s.stats.objects[ok]
	if o == nil {
		if len(s.stats.objects) >= maxTrackedObjects {
			s.pruneObjectsLocked()
		}
		o = &objectCounters{}
		s.stats.objects[ok] = o
	}
	o.requests++
	if r.result == ResultMiss || r.result == ResultExpired {
		o.misses++
	}
	s.stats.mu.Unlock()
	m := s.metrics
	if m == nil {
		return
	}
	m.requests.Add(ctx, 1, r.dist, r.host, string(r.result))
	source := "origin"
	if r.fromCache {
		source = "cache"
		m.saved.Add(ctx, float64(r.bytes), r.dist, r.host)
	}
	m.bytes.Add(ctx, float64(r.bytes), r.dist, r.host, source)
	m.hitRatio.Set(ctx, ratio, r.dist, r.host)
	m.duration.Observe(ctx, r.elapsed.Seconds(), string(r.result))
}

// pruneObjectsLocked forgets the least requested half of the paths.
func (s *service) pruneObjectsLocked() {
	counts := make([]int64, 0, len(s.stats.objects))
	for _, o := range s.stats.objects {
		counts = append(counts, o.requests)
	}
	slices.Sort(counts)
	cut := counts[len(counts)/2]
	for k, o := range s.stats.objects {
		if o.requests <= cut {
			delete(s.stats.objects, k)
		}
	}
}

// hitRatio is the share of the cacheable requests served from the cache.
func hitRatio(r map[Result]int64) float64 {
	hits := r[ResultHit] + r[ResultStale] + r[ResultRevalidated]
	total := hits + r[ResultMiss] + r[ResultExpired]
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}

// countOrigin counts a read of the origin.
func (s *service) countOrigin() {
	s.originReads.Add(1)
	if s.metrics != nil {
		s.metrics.originReads.Add(context.Background(), 1)
	}
}

// countInvalidation counts an invalidation.
func (s *service) countInvalidation(ctx context.Context, kind string) {
	s.invalidations.Add(1)
	if s.metrics != nil {
		s.metrics.invalidations.Add(ctx, 1, kind)
	}
}

// countEviction counts an eviction.
func (s *service) countEviction(t tier) {
	s.evictions.Add(1)
	if s.metrics != nil {
		s.metrics.evictions.Add(context.Background(), 1, string(t))
	}
}

// webhookCalls counts the calls of the webhooks.
func (s *service) webhookCalls(ctx context.Context, typ, outcome string) {
	if s.metrics != nil {
		s.metrics.webhooks.Add(ctx, 1, typ, outcome)
	}
}

// updateGauges publishes the sizes of the cache.
func (s *service) updateGauges(ctx context.Context) {
	if s.metrics == nil {
		return
	}
	u := s.store.usage()
	s.metrics.entries.Set(ctx, float64(u.entries))
	for k, h := range u.hosts {
		host := k[1]
		if host == "" {
			host = "distribution:" + k[0]
		}
		s.metrics.size.Set(ctx, float64(h.memory), host, string(tierMemory))
		s.metrics.size.Set(ctx, float64(h.disk), host, string(tierDisk))
	}
}

// NodeStats implements Node.
func (s *service) NodeStats(_ context.Context, q StatsQuery) (NodeStats, error) {
	top := q.Top
	if top <= 0 {
		top = 10
	}
	u := s.store.usage()
	out := NodeStats{
		Node: s.deps.NodeID, Online: true, Mode: s.mode(), Entries: u.entries, MemorySize: Size(u.memory), MemoryLimit: Size(s.opts.memorySize),
		DiskSize: Size(u.disk), Evictions: s.evictions.Load(), Invalidations: s.invalidations.Load(), Flushes: s.flushes.Load(),
		OriginReads: s.originReads.Load(), WebhookQueue: s.webhookQueueSize(), Synced: s.trusted(),
	}
	if s.opts.dir != "" {
		out.DiskLimit = Size(s.opts.diskSize)
	}
	out.Requests = map[Result]int64{}
	match := func(dist, host string) bool {
		return (q.Distribution == "" || q.Distribution == dist) && (q.Host == "" || q.Host == host)
	}
	s.stats.mu.Lock()
	hosts := map[[2]string]*HostStats{}
	for k, h := range s.stats.hosts {
		if !match(k[0], k[1]) {
			continue
		}
		hs := &HostStats{Distribution: k[0], Host: k[1], Counters: Counters{Requests: maps.Clone(h.requests), BytesCache: h.bytesCache,
			BytesOrigin: h.bytesOrigin, HitRatio: hitRatio(h.requests)}}
		hosts[k] = hs
		for r, n := range h.requests {
			out.Requests[r] += n
		}
		out.BytesCache += h.bytesCache
		out.BytesOrigin += h.bytesOrigin
	}
	var objs []ObjectStat
	for k, o := range s.stats.objects {
		if match(k[0], k[1]) {
			objs = append(objs, ObjectStat{Distribution: k[0], Host: k[1], Path: k[2], Requests: o.requests, Misses: o.misses})
		}
	}
	s.stats.mu.Unlock()
	for k, h := range u.hosts {
		if !match(k[0], k[1]) {
			continue
		}
		hs := hosts[k]
		if hs == nil {
			hs = &HostStats{Distribution: k[0], Host: k[1], Counters: Counters{Requests: map[Result]int64{}}}
			hosts[k] = hs
		}
		hs.Entries, hs.MemorySize, hs.DiskSize = h.entries, Size(h.memory), Size(h.disk)
	}
	out.HitRatio = hitRatio(out.Requests)
	for _, k := range slices.SortedFunc(maps.Keys(hosts), func(a, b [2]string) int {
		if c := strings.Compare(a[0], b[0]); c != 0 {
			return c
		}
		return strings.Compare(a[1], b[1])
	}) {
		out.Hosts = append(out.Hosts, *hosts[k])
	}
	out.TopObjects = topObjects(objs, top, func(o ObjectStat) int64 { return o.Requests })
	out.TopMisses = topObjects(objs, top, func(o ObjectStat) int64 { return o.Misses })
	return out, nil
}

// topObjects returns the n objects with the largest value.
func topObjects(objs []ObjectStat, n int, value func(ObjectStat) int64) []ObjectStat {
	sorted := slices.Clone(objs)
	slices.SortFunc(sorted, func(a, b ObjectStat) int {
		if va, vb := value(a), value(b); va != vb {
			if va > vb {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Path, b.Path)
	})
	var out []ObjectStat
	for _, o := range sorted {
		if len(out) == n || value(o) == 0 {
			break
		}
		out = append(out, o)
	}
	return out
}

// Stats implements Operations: the statistics of every node, summed.
func (s *service) Stats(ctx context.Context, q StatsQuery) (StatsReport, error) {
	local, err := s.NodeStats(ctx, q)
	if err != nil {
		return StatsReport{}, err
	}
	nodes := make([]NodeStats, 1+len(s.deps.Peers))
	nodes[0] = local
	var wg sync.WaitGroup
	for i, p := range s.deps.Peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ns, err := p.NodeStats(ctx, q)
			if err != nil {
				ns = NodeStats{Node: p.NodeID(), Error: err.Error()}
			}
			nodes[i+1] = ns
		}()
	}
	wg.Wait()
	rep := StatsReport{Nodes: nodes, Counters: Counters{Requests: map[Result]int64{}}}
	hosts := map[[2]string]*HostStats{}
	objects := map[[3]string]*ObjectStat{}
	for _, n := range nodes {
		for r, c := range n.Requests {
			rep.Requests[r] += c
		}
		rep.BytesCache += n.BytesCache
		rep.BytesOrigin += n.BytesOrigin
		rep.Entries += n.Entries
		rep.MemorySize += n.MemorySize
		rep.DiskSize += n.DiskSize
		for _, h := range n.Hosts {
			k := [2]string{h.Distribution, h.Host}
			agg := hosts[k]
			if agg == nil {
				agg = &HostStats{Distribution: h.Distribution, Host: h.Host, Counters: Counters{Requests: map[Result]int64{}}}
				hosts[k] = agg
			}
			for r, c := range h.Requests {
				agg.Requests[r] += c
			}
			agg.BytesCache += h.BytesCache
			agg.BytesOrigin += h.BytesOrigin
			agg.Entries += h.Entries
			agg.MemorySize += h.MemorySize
			agg.DiskSize += h.DiskSize
		}
		for _, list := range [][]ObjectStat{n.TopObjects, n.TopMisses} {
			for _, o := range list {
				k := [3]string{o.Distribution, o.Host, o.Path}
				if agg := objects[k]; agg != nil {
					agg.Requests = max(agg.Requests, o.Requests)
					agg.Misses = max(agg.Misses, o.Misses)
					continue
				}
				cp := o
				objects[k] = &cp
			}
		}
	}
	rep.HitRatio = hitRatio(rep.Requests)
	for _, k := range slices.SortedFunc(maps.Keys(hosts), func(a, b [2]string) int {
		if c := strings.Compare(a[0], b[0]); c != 0 {
			return c
		}
		return strings.Compare(a[1], b[1])
	}) {
		h := hosts[k]
		h.HitRatio = hitRatio(h.Requests)
		rep.Hosts = append(rep.Hosts, *h)
	}
	objs := make([]ObjectStat, 0, len(objects))
	for _, o := range objects {
		objs = append(objs, *o)
	}
	top := q.Top
	if top <= 0 {
		top = 10
	}
	rep.TopObjects = topObjects(objs, top, func(o ObjectStat) int64 { return o.Requests })
	rep.TopMisses = topObjects(objs, top, func(o ObjectStat) int64 { return o.Misses })
	return rep, nil
}

// webhookQueueSize is the number of purges waiting.
func (s *service) webhookQueueSize() int {
	if s.webhooks == nil {
		return 0
	}
	return s.webhooks.size()
}
