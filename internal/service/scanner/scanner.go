package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// scanner is the background scanner.
type scanner struct {
	deps Deps
	opts options

	// scanMu serializes the cycles.
	scanMu sync.Mutex

	mu       sync.Mutex
	usage    *Usage
	readAt   time.Time
	cycle    uint64
	last     CycleStats
	scanning bool
	active   bool
	// loops cancels the running cycle loops (one per leadership term).
	loops map[int]context.CancelFunc
	next  int

	runMu       sync.Mutex
	started     bool
	stopping    bool
	unsubscribe func()
	wg          sync.WaitGroup
}

var _ Service = (*scanner)(nil)

// noLifecycle is the default Lifecycle hook: it does nothing.
type noLifecycle struct{}

var _ Lifecycle = noLifecycle{}

// Apply implements Lifecycle.
func (noLifecycle) Apply(context.Context, domain.ObjectInfo) (LifecycleAction, error) {
	return LifecycleNone, nil
}

// New returns the scanner. Objects is required (a wiring error
// otherwise, domain.ErrInvalidArgument).
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.Objects == nil {
		return nil, fmt.Errorf("%w: scanner: the object layer is required", domain.ErrInvalidArgument)
	}
	if deps.Lifecycle == nil {
		deps.Lifecycle = noLifecycle{}
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	return &scanner{deps: deps, opts: o, loops: map[int]context.CancelFunc{}}, nil
}

// leaderListener adapts the scanner to cluster.LeaderListener.
type leaderListener struct{ s *scanner }

var _ cluster.LeaderListener = leaderListener{}

// OnElected implements cluster.LeaderListener: the cycles run until the
// leadership ends.
func (l leaderListener) OnElected(ctx context.Context) { l.s.activate(ctx) }

// OnRevoked implements cluster.LeaderListener: the context of OnElected
// is canceled, the loop ends by itself.
func (l leaderListener) OnRevoked(context.Context) {}

// Start implements lifecycle.Runner.
func (s *scanner) Start(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.started {
		return nil
	}
	s.mu.Lock()
	s.stopping = false
	s.mu.Unlock()
	if u, err := s.readUsage(ctx); err == nil {
		s.mu.Lock()
		s.cycle = u.Cycle
		s.mu.Unlock()
	}
	s.started = true
	if s.deps.Leader != nil {
		s.unsubscribe = s.deps.Leader.Subscribe(leaderListener{s: s})
		return nil
	}
	s.activate(context.WithoutCancel(ctx))
	return nil
}

// Stop implements lifecycle.Runner.
func (s *scanner) Stop(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if !s.started {
		return nil
	}
	if s.unsubscribe != nil {
		s.unsubscribe()
		s.unsubscribe = nil
	}
	s.mu.Lock()
	s.stopping = true
	for _, cancel := range s.loops {
		cancel()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("scanner: stop: %w", ctx.Err())
	}
	s.started = false
	return nil
}

// activate starts the cycle loop of a term (the leadership, or the life of
// the scanner without election).
func (s *scanner) activate(ctx context.Context) {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	id := s.next
	s.next++
	s.loops[id] = cancel
	s.active = true
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.loops, id)
			s.active = len(s.loops) > 0
			s.mu.Unlock()
		}()
		s.loop(ctx)
	}()
}

// loop runs the cycles until ctx ends.
func (s *scanner) loop(ctx context.Context) {
	s.deps.Logger.Info("scanner: active", "start_delay", s.opts.startDelay.String())
	if !sleep(ctx, s.opts.startDelay) {
		return
	}
	for {
		stats, err := s.Scan(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			s.deps.Logger.Warn("scanner: cycle failed", "cycle", stats.Cycle, "err", err)
		default:
			s.deps.Logger.Debug("scanner: cycle done", "cycle", stats.Cycle, "buckets", stats.Buckets, "objects", stats.Objects,
				"heal_checked", stats.HealChecked, "heal_repaired", stats.HealRepaired, "uploads_cleaned", stats.UploadsCleaned,
				"took", stats.Finished.Sub(stats.Started).String())
		}
		cycle, _ := s.settings(ctx)
		if !sleep(ctx, cycle) {
			return
		}
	}
}

// sleep waits for d or the end of ctx; it reports whether d elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// settings returns the pause between cycles and the speed, from the
// dynamic configuration when set, the options otherwise.
func (s *scanner) settings(ctx context.Context) (time.Duration, string) {
	cycle, speed := s.opts.cycle, s.opts.speed
	if s.deps.Config == nil {
		return cycle, speed
	}
	kv, err := s.deps.Config.Get(ctx, SubsystemScanner)
	if err != nil {
		return cycle, speed
	}
	if v := strings.TrimSpace(kv[KeyCycle]); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cycle = d
		} else {
			s.deps.Logger.Warn("scanner: invalid cycle setting, using the default", "value", v, "default", cycle.String())
		}
	}
	if v := strings.TrimSpace(kv[KeySpeed]); v != "" {
		if _, _, err := parseSpeed(v); err == nil {
			speed = strings.ToLower(v)
		} else {
			s.deps.Logger.Warn("scanner: invalid speed setting, using the default", "value", v, "default", speed)
		}
	}
	return cycle, speed
}

// Status implements Service.
func (s *scanner) Status(context.Context) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Active: s.active, Scanning: s.scanning, Cycle: s.cycle, Last: s.last}
}

// Usage implements Service.
func (s *scanner) Usage(ctx context.Context) (Usage, error) {
	now := s.deps.Clock.Now()
	s.mu.Lock()
	cached, fresh := s.usage, s.active || now.Sub(s.readAt) < s.opts.usageTTL
	s.mu.Unlock()
	if cached != nil && fresh {
		return cloneUsage(*cached), nil
	}
	u, err := s.readUsage(ctx)
	if err != nil {
		if cached != nil {
			return cloneUsage(*cached), nil
		}
		return Usage{}, err
	}
	s.mu.Lock()
	if s.usage == nil || !u.LastUpdate.Before(s.usage.LastUpdate) {
		s.usage = &u
	}
	s.readAt = now
	out := cloneUsage(*s.usage)
	s.mu.Unlock()
	return out, nil
}

// cloneUsage returns a deep copy of u.
func cloneUsage(u Usage) Usage {
	out := Usage{LastUpdate: u.LastUpdate, Cycle: u.Cycle, Buckets: make(map[string]BucketUsage, len(u.Buckets))}
	for name, b := range u.Buckets {
		cp := b
		if b.Prefixes != nil {
			cp.Prefixes = make(map[string]Counts, len(b.Prefixes))
			for p, c := range b.Prefixes {
				cp.Prefixes[p] = c
			}
		}
		out.Buckets[name] = cp
	}
	return out
}

// readUsage returns the newest usage.json of the drives.
func (s *scanner) readUsage(ctx context.Context) (Usage, error) {
	var best Usage
	found := false
	for _, d := range s.deps.Drives {
		if d == nil || !d.IsOnline() {
			continue
		}
		data, err := d.ReadAll(ctx, domain.SystemBucket, UsageFile)
		if err != nil {
			continue
		}
		var u Usage
		if err := json.Unmarshal(data, &u); err != nil {
			s.deps.Logger.Warn("scanner: invalid usage document, ignored", "drive", d.Endpoint().String(), "err", err)
			continue
		}
		if !found || u.LastUpdate.After(best.LastUpdate) {
			best, found = u, true
		}
	}
	if !found {
		return Usage{}, fmt.Errorf("%w: scanner: no data usage computed yet", domain.ErrNotFound)
	}
	if best.Buckets == nil {
		best.Buckets = map[string]BucketUsage{}
	}
	return best, nil
}

// saveUsage writes usage.json on every online drive.
func (s *scanner) saveUsage(ctx context.Context, u Usage) error {
	if len(s.deps.Drives) == 0 {
		return nil
	}
	data, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("scanner: encode usage: %w", err)
	}
	var errs []error
	written := 0
	for _, d := range s.deps.Drives {
		if d == nil || !d.IsOnline() {
			continue
		}
		if err := d.WriteAll(ctx, domain.SystemBucket, UsageFile, data); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.Endpoint(), err))
			continue
		}
		written++
	}
	if written == 0 {
		return fmt.Errorf("scanner: save usage: %w", errors.Join(append(errs, domain.ErrDiskNotFound)...))
	}
	return nil
}

// Scan implements Service.
func (s *scanner) Scan(ctx context.Context) (CycleStats, error) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	_, speed := s.settings(ctx)
	factor, maxSleep, _ := parseSpeed(speed)
	s.mu.Lock()
	s.cycle++
	s.scanning = true
	c := &cycle{
		s: s, factor: factor, maxSleep: maxSleep,
		stats: CycleStats{Cycle: s.cycle, Started: s.deps.Clock.Now().UTC()},
		usage: Usage{Cycle: s.cycle, Buckets: map[string]BucketUsage{}},
	}
	s.mu.Unlock()
	c.stats.Deep = s.opts.deepEvery > 0 && c.stats.Cycle%s.opts.deepEvery == 0
	err := c.run(ctx)
	now := s.deps.Clock.Now().UTC()
	c.stats.Finished = now
	s.mu.Lock()
	s.scanning = false
	if err == nil {
		c.usage.LastUpdate = now
		s.usage = &c.usage
		s.readAt = now
		s.last = c.stats
	}
	s.mu.Unlock()
	if err != nil {
		return c.stats, err
	}
	if err := s.saveUsage(ctx, c.usage); err != nil {
		s.deps.Logger.Warn("scanner: usage not saved", "err", err)
	}
	return c.stats, nil
}

// cycle is the state of one scan.
type cycle struct {
	s        *scanner
	factor   float64
	maxSleep time.Duration
	stats    CycleStats
	usage    Usage
	// sampled counts the keys since the last healed one.
	sampled uint64
}

// run scans every bucket, then cleans the stale uploads.
func (c *cycle) run(ctx context.Context) error {
	buckets, err := c.s.deps.Objects.ListBuckets(ctx)
	if err != nil {
		return fmt.Errorf("scanner: list buckets: %w", err)
	}
	for _, b := range buckets {
		if err := c.scanBucket(ctx, b.Name); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, domain.ErrBucketNotFound) {
				continue // removed meanwhile
			}
			return err
		}
		c.stats.Buckets++
	}
	if c.s.opts.staleUploads > 0 {
		n, err := c.s.deps.Objects.CleanupStaleUploads(ctx, "", c.s.opts.staleUploads)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.s.deps.Logger.Warn("scanner: stale uploads cleanup", "err", err)
		}
		c.stats.UploadsCleaned = n
	}
	return nil
}

// scanBucket pages through the versions of a bucket.
func (c *cycle) scanBucket(ctx context.Context, bucket string) error {
	usage := BucketUsage{}
	in := domain.ListObjectsInput{Bucket: bucket, MaxKeys: c.s.opts.pageSize, IncludeVersions: true}
	for {
		c.pause(ctx)
		started := time.Now()
		page, err := c.s.deps.Objects.ListObjectVersions(ctx, in)
		if err != nil {
			return fmt.Errorf("scanner: list %s: %w", bucket, err)
		}
		for _, obj := range page.Objects {
			c.version(ctx, &usage, obj)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		c.throttle(ctx, time.Since(started))
		if !page.IsTruncated {
			break
		}
		if page.NextMarker == "" && page.NextVersionIDMarker == "" {
			break
		}
		in.Marker, in.VersionIDMarker = page.NextMarker, page.NextVersionIDMarker
	}
	c.usage.Buckets[bucket] = usage
	return nil
}

// version accounts one version, hands it to the lifecycle hook and heals
// the sampled keys.
func (c *cycle) version(ctx context.Context, usage *BucketUsage, obj domain.ObjectInfo) {
	c.stats.Versions++
	action, err := c.s.deps.Lifecycle.Apply(ctx, obj)
	if err != nil {
		c.s.deps.Logger.Warn("scanner: lifecycle", "bucket", obj.Bucket, "key", obj.Key, "version_id", obj.VersionID, "err", err)
	}
	if action != LifecycleNone {
		c.stats.LifecycleActions++
	}
	if obj.IsLatest {
		c.stats.Objects++
		c.sample(ctx, obj)
	}
	if action == LifecycleDeleted {
		return
	}
	var delta Counts
	switch {
	case obj.IsDeleteMarker:
		delta.DeleteMarkers = 1
	case obj.IsLatest:
		delta.Objects = 1
	default:
		delta.Versions = 1
	}
	if !obj.IsDeleteMarker && obj.Size > 0 {
		delta.Size = uint64(obj.Size)
	}
	usage.add(delta)
	prefix := ""
	if i := strings.Index(obj.Key, "/"); i >= 0 {
		prefix = obj.Key[:i+1]
	}
	if usage.Prefixes == nil {
		usage.Prefixes = map[string]Counts{}
	}
	cur, ok := usage.Prefixes[prefix]
	if !ok && len(usage.Prefixes) >= c.s.opts.maxPrefixes {
		usage.PrefixesTruncated = true
		return
	}
	cur.add(delta)
	usage.Prefixes[prefix] = cur
}

func (c *Counts) add(d Counts) {
	c.Objects += d.Objects
	c.Versions += d.Versions
	c.DeleteMarkers += d.DeleteMarkers
	c.Size += d.Size
}

// sample heals one key every healEvery (every version, deep on the deep
// cycles).
func (c *cycle) sample(ctx context.Context, obj domain.ObjectInfo) {
	if c.s.deps.Healer == nil || c.s.opts.healEvery == 0 {
		return
	}
	c.sampled++
	if c.sampled < c.s.opts.healEvery {
		return
	}
	c.sampled = 0
	started := time.Now()
	c.stats.HealChecked++
	res, err := c.s.deps.Healer.HealObject(ctx, obj.Bucket, obj.Key, "", object.HealOptions{AllVersions: true, Deep: c.stats.Deep})
	switch {
	case err != nil && ctx.Err() == nil && !errors.Is(err, domain.ErrObjectNotFound):
		c.stats.HealFailed++
		c.s.deps.Logger.Warn("scanner: heal of a sampled object failed", "bucket", obj.Bucket, "key", obj.Key, "err", err)
	case err == nil && res.Healed > 0:
		c.stats.HealRepaired++
		c.s.deps.Logger.Info("scanner: sampled object healed", "bucket", obj.Bucket, "key", obj.Key, "drives", res.Healed)
	}
	c.throttle(ctx, time.Since(started))
}

// throttle sleeps in proportion to the work just done.
func (c *cycle) throttle(ctx context.Context, work time.Duration) {
	if c.factor <= 0 || work <= 0 {
		return
	}
	d := min(time.Duration(float64(work)*c.factor), c.maxSleep)
	if d <= 0 {
		return
	}
	started := time.Now()
	sleep(ctx, d)
	c.stats.Slept += time.Since(started)
}

// pause waits while the API load is at or above the threshold.
func (c *cycle) pause(ctx context.Context) {
	if c.s.deps.Load == nil {
		return
	}
	started := time.Now()
	for c.s.deps.Load.Load(ctx) >= c.s.opts.loadThreshold {
		if !sleep(ctx, c.s.opts.pauseCheck) {
			break
		}
	}
	c.stats.Paused += time.Since(started)
}
