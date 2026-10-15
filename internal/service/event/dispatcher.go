package event

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/config"
)

// Option configures the dispatcher.
type Option func(*options)

type options struct {
	queueSize   int
	sendTimeout time.Duration
	region      string
	queueRoot   string
	retryMin    time.Duration
	retryMax    time.Duration
	owner       string
}

func defaultOptions() options {
	return options{
		queueSize:   10000,
		sendTimeout: 10 * time.Second,
		retryMin:    250 * time.Millisecond,
		retryMax:    5 * time.Second,
		owner:       "nexo",
	}
}

// WithQueueSize sets the capacity of the in-memory queue between the
// publishers and the router (default 10000): beyond it Publish fails.
func WithQueueSize(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.queueSize = n
		}
	}
}

// WithSendTimeout bounds each delivery attempt (default 10s).
func WithSendTimeout(t time.Duration) Option {
	return func(o *options) {
		if t > 0 {
			o.sendTimeout = t
		}
	}
}

// WithRegion sets the region of the deployment: the target ARNs of the
// bucket configurations must name it (or none).
func WithRegion(region string) Option {
	return func(o *options) { o.region = strings.TrimSpace(region) }
}

// WithQueueRoot keeps the queues of the targets in <root>/<type>-<id>, or
// <root>/queue_dir/<queue_dir> for a target with a queue_dir (on a local
// drive of the node): without it they are kept in memory.
func WithQueueRoot(dir string) Option {
	return func(o *options) { o.queueRoot = dir }
}

// WithRetryBackoff sets the delays between two delivery attempts to an
// offline target: min doubles up to max (default 250ms to 5s).
func WithRetryBackoff(minDelay, maxDelay time.Duration) Option {
	return func(o *options) {
		if minDelay > 0 {
			o.retryMin = minDelay
		}
		if maxDelay >= o.retryMin {
			o.retryMax = maxDelay
		}
	}
}

// WithOwner sets the principal reported as the owner of the buckets
// (s3.bucket.ownerIdentity; default "nexo").
func WithOwner(id string) Option {
	return func(o *options) {
		if id != "" {
			o.owner = id
		}
	}
}

// worker delivers the queue of one target, in order.
type worker struct {
	cfg    TargetConfig
	target Target
	store  queueStore
	wake   chan struct{}
	stop   chan struct{}
	done   chan struct{}
	// running reports a started delivery goroutine.
	running bool
	// state is the outcome of the last delivery (TargetsStatus).
	state deliveryState
}

// deliveryState is the outcome of the last delivery to a target, updated
// by its sender and by the test deliveries.
type deliveryState struct {
	mu      sync.Mutex
	failed  bool
	lastErr string
}

// record stores the outcome of a delivery.
func (s *deliveryState) record(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.failed, s.lastErr = true, err.Error()
		return
	}
	s.failed, s.lastErr = false, ""
}

// get returns whether the target is online and the last error.
func (s *deliveryState) get() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.failed, s.lastErr
}

// signal wakes the delivery goroutine up.
func (w *worker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// cachedRules is the parsed notification configuration of a bucket.
type cachedRules struct {
	doc []byte
	cfg *BucketConfig
}

// routeTo is a delivery decided by the router.
type routeTo struct {
	id       TargetID
	configID string
}

// dispatcher routes the published events to the queues of the targets and
// runs one delivery goroutine per target.
type dispatcher struct {
	deps Deps
	opts options

	mu      sync.RWMutex
	targets map[TargetID]*worker
	// stores are the queues by directory, shared by the successive
	// workers of a target.
	stores map[string]queueStore
	// fromConfig are the targets loaded from the configuration.
	fromConfig map[TargetID]TargetConfig
	queue      chan Event
	started    bool
	closed     bool
	routerDone chan struct{}
	// runCtx is canceled at Stop: it bounds the sends and the watcher.
	runCtx    context.Context
	cancelRun context.CancelFunc
	watchDone chan struct{}

	// configMu serializes the reloads of the configured targets.
	configMu sync.Mutex

	rulesMu sync.Mutex
	rules   map[string]cachedRules
}

var _ Dispatcher = (*dispatcher)(nil)

// NewDispatcher returns the dispatcher. Call Start before publishing.
func NewDispatcher(deps Deps, opts ...Option) Dispatcher {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Factory == nil {
		deps.Factory = NewTargetFactory(FactoryDeps{Logger: deps.Logger})
	}
	o := defaultOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return &dispatcher{
		deps: deps, opts: o,
		targets:    map[TargetID]*worker{},
		stores:     map[string]queueStore{},
		fromConfig: map[TargetID]TargetConfig{},
		rules:      map[string]cachedRules{},
	}
}

// Start implements lifecycle.Runner: it loads the configured targets,
// starts their delivery (the events queued before a restart first) and
// the router.
func (d *dispatcher) Start(ctx context.Context) error {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return errors.New("event: dispatcher already started")
	}
	d.queue = make(chan Event, d.opts.queueSize)
	d.routerDone = make(chan struct{})
	d.runCtx, d.cancelRun = context.WithCancel(context.WithoutCancel(ctx))
	d.started, d.closed = true, false
	for _, w := range d.targets {
		d.startWorker(w)
	}
	go d.route(d.runCtx, d.queue, d.routerDone)
	var watchDone chan struct{}
	if d.deps.Config != nil {
		watchDone = make(chan struct{})
		d.watchDone = watchDone
	}
	runCtx := d.runCtx
	d.mu.Unlock()

	if watchDone != nil {
		changes := d.deps.Config.Watch(runCtx)
		d.reloadConfig(ctx)
		go d.watch(runCtx, changes, watchDone)
	}
	return nil
}

// Stop implements lifecycle.Runner: it stops accepting events, moves the
// events of the in-memory queue to the target queues, stops the deliveries
// (the queued events are delivered after the next start) and closes the
// targets.
func (d *dispatcher) Stop(ctx context.Context) error {
	d.mu.Lock()
	if !d.started || d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	close(d.queue)
	routerDone, watchDone := d.routerDone, d.watchDone
	d.mu.Unlock()

	var err error
	select {
	case <-routerDone:
	case <-ctx.Done():
		err = fmt.Errorf("event: drain queue: %w", ctx.Err())
	}
	d.cancelRun()
	if watchDone != nil {
		select {
		case <-watchDone:
		case <-ctx.Done():
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for _, w := range d.targets {
		if werr := d.stopWorker(ctx, w); werr != nil && err == nil {
			err = werr
		}
	}
	for id, w := range d.targets {
		if cerr := w.target.Close(ctx); cerr != nil {
			err = errors.Join(err, fmt.Errorf("event: close target %s: %w", id, cerr))
		}
	}
	d.started = false
	d.watchDone = nil
	return err
}

// Publish implements Service.
func (d *dispatcher) Publish(ctx context.Context, e Event) error {
	if e.EventName.IsInternal() {
		// For the hooks of the object layer only.
		return nil
	}
	e = complete(ctx, e, d.opts.owner)
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.started || d.closed {
		return fmt.Errorf("%w: the event dispatcher is not running", domain.ErrConflict)
	}
	select {
	case d.queue <- e:
		return nil
	default:
		d.deps.Logger.WarnContext(ctx, "event: queue full, event dropped", "event", e.EventName, "bucket", e.S3.Bucket.Name, "key", e.S3.Object.Key)
		return fmt.Errorf("event: queue full: %w", domain.ErrStorageFull)
	}
}

// SetTarget implements Service.
func (d *dispatcher) SetTarget(ctx context.Context, cfg TargetConfig) error {
	if cfg.ID == "" {
		return fmt.Errorf("%w: target id is required", domain.ErrInvalidArgument)
	}
	t, err := d.deps.Factory.New(ctx, cfg)
	if err != nil {
		return fmt.Errorf("event: build target %s: %w", cfg.TargetID(), err)
	}
	store, err := d.storeFor(cfg)
	if err != nil {
		_ = t.Close(ctx)
		return err
	}
	w := &worker{cfg: cfg, target: t, store: store}
	d.mu.Lock()
	defer d.mu.Unlock()
	if old, ok := d.targets[cfg.TargetID()]; ok {
		if err := d.stopWorker(ctx, old); err != nil {
			d.deps.Logger.WarnContext(ctx, "event: replaced target still sending", "target", cfg.TargetID().String(), "err", err)
		}
		if cerr := old.target.Close(ctx); cerr != nil {
			d.deps.Logger.WarnContext(ctx, "event: close replaced target", "target", cfg.TargetID().String(), "err", cerr)
		}
	}
	d.targets[cfg.TargetID()] = w
	if d.started && !d.closed {
		d.startWorker(w)
	}
	return nil
}

// customQueueDir is the directory of the queue root holding the queues of
// the targets with a queue_dir.
const customQueueDir = "queue_dir"

// storeFor returns the queue of a target: a directory of the queue root
// (<root>/queue_dir/<QueueDir>, or <root>/<type>-<id> by default), shared
// with the earlier workers of the target, or memory without a queue root.
func (d *dispatcher) storeFor(cfg TargetConfig) (queueStore, error) {
	limit := cfg.QueueLimit
	if limit == 0 {
		limit = DefaultQueueLimit
	}
	if err := ValidateQueueDir(cfg.QueueDir); err != nil {
		return nil, err
	}
	var dir string
	switch {
	case d.opts.queueRoot == "":
		if cfg.QueueDir != "" {
			d.deps.Logger.Warn("event: queue_dir ignored without a local drive, the queue is kept in memory",
				"target", cfg.TargetID().String())
		}
	case cfg.QueueDir != "":
		// queue_dir is confined to the queue root (ValidateQueueDir), apart
		// from the default queues of the targets.
		dir = filepath.Join(d.opts.queueRoot, customQueueDir, filepath.Clean(cfg.QueueDir))
	default:
		dir = filepath.Join(d.opts.queueRoot, cfg.Type+"-"+safeName(cfg.ID))
	}
	key := "mem:" + cfg.TargetID().String()
	if dir != "" {
		key = filepath.Clean(dir)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.stores[key]; ok {
		return s, nil
	}
	var s queueStore
	if dir == "" {
		s = newMemQueue(limit)
	} else {
		q, err := openDirQueue(dir, limit)
		if err != nil {
			return nil, err
		}
		s = q
	}
	d.stores[key] = s
	return s, nil
}

// safeName keeps the characters of a target id that are safe in a file
// name.
func safeName(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, id)
}

// GetTarget implements Service.
func (d *dispatcher) GetTarget(_ context.Context, id TargetID) (TargetConfig, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	w, ok := d.targets[id]
	if !ok {
		return TargetConfig{}, fmt.Errorf("%w: event target %s", domain.ErrNotFound, id)
	}
	return w.cfg, nil
}

// ListTargets implements Service.
func (d *dispatcher) ListTargets(context.Context) ([]TargetConfig, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]TargetConfig, 0, len(d.targets))
	for _, w := range d.targets {
		out = append(out, w.cfg)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Type < out[j].Type
	})
	return out, nil
}

// RemoveTarget implements Service. The queue of the target is kept: a
// target configured again on the same queue delivers it.
func (d *dispatcher) RemoveTarget(ctx context.Context, id TargetID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	w, ok := d.targets[id]
	if !ok {
		return fmt.Errorf("%w: event target %s", domain.ErrNotFound, id)
	}
	delete(d.targets, id)
	err := d.stopWorker(ctx, w)
	if cerr := w.target.Close(ctx); cerr != nil {
		err = errors.Join(err, fmt.Errorf("event: close target %s: %w", id, cerr))
	}
	return err
}

// TargetsStatus implements Service.
func (d *dispatcher) TargetsStatus(ctx context.Context) ([]TargetStatus, error) {
	d.mu.RLock()
	workers := make([]*worker, 0, len(d.targets))
	for _, w := range d.targets {
		workers = append(workers, w)
	}
	d.mu.RUnlock()
	out := make([]TargetStatus, 0, len(workers))
	for _, w := range workers {
		online, lastErr := w.state.get()
		out = append(out, TargetStatus{Config: w.cfg, Online: online, Pending: len(w.store.Keys(ctx)), LastError: lastErr})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Config.ID != out[j].Config.ID {
			return out[i].Config.ID < out[j].Config.ID
		}
		return out[i].Config.Type < out[j].Config.Type
	})
	return out, nil
}

// TestTarget implements Service.
func (d *dispatcher) TestTarget(ctx context.Context, id TargetID) (TestResult, error) {
	d.mu.RLock()
	w, ok := d.targets[id]
	d.mu.RUnlock()
	if !ok {
		return TestResult{}, fmt.Errorf("%w: event target %s", domain.ErrNotFound, id)
	}
	e := complete(ctx, NewEvent(TestEvent, d.opts.region, time.Now()), d.opts.owner)
	sctx, cancel := context.WithTimeout(ctx, d.opts.sendTimeout)
	defer cancel()
	err := w.target.Send(sctx, e.record(ConfigID))
	w.state.record(err)
	if err != nil {
		d.deps.Logger.InfoContext(ctx, "event: test delivery failed", "target", id.String(), "err", err)
		return TestResult{Error: err.Error()}, nil
	}
	return TestResult{Delivered: true}, nil
}

// ValidateBucketConfig implements Service.
func (d *dispatcher) ValidateBucketConfig(_ context.Context, doc []byte) ([]byte, error) {
	cfg, err := ParseBucketConfig(doc, d.opts.region)
	if err != nil {
		return nil, err
	}
	if err := checkOverlaps(cfg.Rules); err != nil {
		return nil, err
	}
	d.mu.RLock()
	for _, r := range cfg.Rules {
		if _, ok := d.targets[r.ARN.TargetID()]; !ok {
			d.mu.RUnlock()
			return nil, invalidDestination(r.ARN.String())
		}
	}
	d.mu.RUnlock()
	return EncodeBucketConfig(cfg)
}

// --- routing ------------------------------------------------------------------

// route moves the published events to the queues of their targets, in
// order, until the queue is closed.
func (d *dispatcher) route(ctx context.Context, queue <-chan Event, done chan<- struct{}) {
	defer close(done)
	for e := range queue {
		d.dispatch(context.WithoutCancel(ctx), e)
	}
}

// dispatch queues one event for each of its targets.
func (d *dispatcher) dispatch(ctx context.Context, e Event) {
	routes := d.routes(ctx, e)
	if len(routes) == 0 {
		return
	}
	d.mu.RLock()
	workers := make([]*worker, 0, len(routes))
	configIDs := make([]string, 0, len(routes))
	for _, r := range routes {
		w, ok := d.targets[r.id]
		if !ok || !w.cfg.Enabled {
			d.deps.Logger.Debug("event: no enabled target for a route", "target", r.id.String(), "event", e.EventName)
			continue
		}
		workers = append(workers, w)
		configIDs = append(configIDs, r.configID)
	}
	d.mu.RUnlock()
	for i, w := range workers {
		if err := w.store.Put(ctx, e.record(configIDs[i])); err != nil {
			d.deps.Logger.Warn("event: cannot queue the event, dropped", "target", w.cfg.TargetID().String(), "event", e.EventName,
				"bucket", e.S3.Bucket.Name, "key", e.S3.Object.Key, "err", err)
			continue
		}
		w.signal()
	}
}

// routes returns the targets of an event: those of the matching rules of
// the bucket configuration, then those whose filter matches; a target is
// reached once.
func (d *dispatcher) routes(ctx context.Context, e Event) []routeTo {
	var out []routeTo
	seen := map[TargetID]bool{}
	if cfg := d.bucketRules(ctx, e.S3.Bucket.Name); cfg != nil {
		for _, r := range cfg.Rules {
			id := r.ARN.TargetID()
			if !seen[id] && r.Matches(e) {
				seen[id] = true
				out = append(out, routeTo{id: id, configID: r.ID})
			}
		}
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	for id, w := range d.targets {
		if !seen[id] && !w.cfg.Filter.IsZero() && w.cfg.Filter.Matches(e) {
			seen[id] = true
			out = append(out, routeTo{id: id, configID: ConfigID})
		}
	}
	return out
}

// bucketRules returns the parsed notification configuration of a bucket,
// nil when it has none (or an invalid one).
func (d *dispatcher) bucketRules(ctx context.Context, bucket string) *BucketConfig {
	if d.deps.Rules == nil || bucket == "" {
		return nil
	}
	doc, err := d.deps.Rules.NotificationConfig(ctx, bucket)
	if err != nil {
		if !errors.Is(err, domain.ErrBucketNotFound) {
			d.deps.Logger.Warn("event: notification configuration unavailable", "bucket", bucket, "err", err)
		}
		return nil
	}
	if len(bytes.TrimSpace(doc)) == 0 {
		return nil
	}
	d.rulesMu.Lock()
	defer d.rulesMu.Unlock()
	if c, ok := d.rules[bucket]; ok && bytes.Equal(c.doc, doc) {
		return c.cfg
	}
	cfg, err := ParseBucketConfig(doc, "")
	if err != nil {
		d.deps.Logger.Warn("event: invalid notification configuration ignored", "bucket", bucket, "err", err)
		cfg = nil
	}
	d.rules[bucket] = cachedRules{doc: bytes.Clone(doc), cfg: cfg}
	return cfg
}

// --- delivery -----------------------------------------------------------------

// startWorker starts the delivery goroutine of a target (d.mu held).
func (d *dispatcher) startWorker(w *worker) {
	if w.running {
		return
	}
	w.wake = make(chan struct{}, 1)
	w.stop = make(chan struct{})
	w.done = make(chan struct{})
	w.running = true
	go d.deliver(d.runCtx, w)
}

// stopWorker stops the delivery goroutine of a target and waits for it
// (bounded by ctx).
func (d *dispatcher) stopWorker(ctx context.Context, w *worker) error {
	if !w.running {
		return nil
	}
	w.running = false
	close(w.stop)
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("event: stop target %s: %w", w.cfg.TargetID(), ctx.Err())
	}
}

// deliver sends the queue of a target, oldest first. A failed send keeps
// the event at the head of the queue and is tried again after a backoff
// (doubling up to the maximum), so that the order is preserved.
func (d *dispatcher) deliver(ctx context.Context, w *worker) {
	defer close(w.done)
	id := w.cfg.TargetID().String()
	delay := time.Duration(0)
	offline := false
	for {
		keys := w.store.Keys(ctx)
		if len(keys) == 0 {
			select {
			case <-w.wake:
				continue
			case <-w.stop:
				return
			}
		}
		failed := false
		for _, key := range keys {
			select {
			case <-w.stop:
				return
			default:
			}
			e, err := w.store.Get(ctx, key)
			if err != nil {
				d.deps.Logger.Warn("event: unreadable queued event dropped", "target", id, "key", key, "err", err)
				_ = w.store.Delete(ctx, key)
				continue
			}
			sctx, cancel := context.WithTimeout(ctx, d.opts.sendTimeout)
			err = w.target.Send(sctx, e)
			cancel()
			if ctx.Err() == nil {
				w.state.record(err)
			}
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if !offline {
					d.deps.Logger.Warn("event: target offline, events kept in its queue", "target", id, "err", err)
				} else {
					d.deps.Logger.Debug("event: delivery failed", "target", id, "event", e.EventName, "err", err)
				}
				offline = true
				failed = true
				break
			}
			if offline {
				d.deps.Logger.Info("event: target back online", "target", id)
				offline = false
			}
			delay = 0
			if err := w.store.Delete(ctx, key); err != nil {
				d.deps.Logger.Warn("event: cannot remove a delivered event", "target", id, "key", key, "err", err)
			}
		}
		if !failed {
			continue
		}
		delay = nextDelay(delay, d.opts.retryMin, d.opts.retryMax)
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-w.stop:
			t.Stop()
			return
		}
	}
}

// nextDelay doubles the retry delay between minDelay and maxDelay.
func nextDelay(cur, minDelay, maxDelay time.Duration) time.Duration {
	if cur < minDelay {
		return minDelay
	}
	return min(cur*2, maxDelay)
}

// --- configured targets -------------------------------------------------------

// watch reloads the configured targets when a notify subsystem changes.
func (d *dispatcher) watch(ctx context.Context, changes <-chan config.Change, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case c, ok := <-changes:
			if !ok {
				return
			}
			if c.Subsystem == "" || IsTargetSubsystem(c.Subsystem) {
				d.reloadConfig(ctx)
			}
		}
	}
}

// reloadConfig applies the notify_* subsystems: the enabled targets are
// created or replaced when their configuration changed, the others are
// removed (their queue is kept).
func (d *dispatcher) reloadConfig(ctx context.Context) {
	d.configMu.Lock()
	defer d.configMu.Unlock()
	subsystems, err := d.deps.Config.List(ctx)
	if err != nil {
		d.deps.Logger.Warn("event: cannot list the configured targets", "err", err)
		return
	}
	want := map[TargetID]TargetConfig{}
	for _, s := range subsystems {
		if !IsTargetSubsystem(s) {
			continue
		}
		kv, err := d.deps.Config.Get(ctx, s)
		if err != nil {
			d.deps.Logger.Warn("event: cannot read a target configuration", "subsystem", s, "err", err)
			continue
		}
		cfg, err := ParseTargetConfig(s, kv)
		if err != nil {
			d.deps.Logger.Warn("event: invalid target configuration ignored", "subsystem", s, "err", err)
			continue
		}
		if cfg.Enabled {
			want[cfg.TargetID()] = cfg
		}
	}
	for id, cfg := range want {
		if cur, ok := d.fromConfig[id]; ok && reflect.DeepEqual(cur, cfg) {
			continue
		}
		if err := d.SetTarget(ctx, cfg); err != nil {
			d.deps.Logger.Warn("event: cannot configure a target", "target", id.String(), "err", err)
			continue
		}
		d.fromConfig[id] = cfg
		d.deps.Logger.Info("event: target configured", "target", id.String(), "arn", id.ARN(d.opts.region))
	}
	for id := range d.fromConfig {
		if _, ok := want[id]; ok {
			continue
		}
		delete(d.fromConfig, id)
		if err := d.RemoveTarget(ctx, id); err != nil && !errors.Is(err, domain.ErrNotFound) {
			d.deps.Logger.Warn("event: cannot remove a target", "target", id.String(), "err", err)
		}
	}
}
