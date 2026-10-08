package heal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// mrfVersion is the version of the persisted queue document.
const mrfVersion = 1

// mrfDocument is the content of MRFFile.
type mrfDocument struct {
	Version int     `json:"version"`
	Saved   string  `json:"saved"`
	Entries []Entry `json:"entries"`
}

// queueEntry is an entry with its lease state.
type queueEntry struct {
	Entry
	leased bool
	// again records a notification received during the lease.
	again bool
}

// queue is the in-memory MRF queue persisted on the local drives.
type queue struct {
	deps QueueDeps
	opts queueOptions

	mu      sync.Mutex
	entries map[string]*queueEntry
	stats   QueueStats
	dirty   bool
	// wake is signaled (without blocking) when an entry becomes due.
	wake chan struct{}

	runMu   sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
}

var _ Queue = (*queue)(nil)

// NewQueue returns the MRF queue. It accepts notifications at once; Start
// loads the entries persisted by the previous run and starts the periodic
// persistence.
func NewQueue(deps QueueDeps, opts ...QueueOption) Queue {
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := defaultQueueOptions()
	for _, opt := range opts {
		opt(&o)
	}
	return &queue{deps: deps, opts: o, entries: map[string]*queueEntry{}, wake: make(chan struct{}, 1)}
}

func entryKey(bucket, object, versionID string) string {
	return bucket + "\x00" + object + "\x00" + versionID
}

func (q *queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// NotifyHeal implements object.HealNotifier: it never blocks on I/O.
func (q *queue) NotifyHeal(_ context.Context, bucket, object, versionID string) {
	if bucket == "" || bucket == domain.SystemBucket {
		return
	}
	now := q.deps.Clock.Now()
	q.mu.Lock()
	defer q.mu.Unlock()
	key := entryKey(bucket, object, versionID)
	if e, ok := q.entries[key]; ok {
		// Already queued: it is healed when due. During a lease the heal
		// in progress may predate the damage: heal once more.
		q.stats.Merged++
		if e.leased {
			e.again = true
		}
		return
	}
	if len(q.entries) >= q.opts.size {
		q.stats.Dropped++
		if q.stats.Dropped == 1 || q.stats.Dropped%1000 == 0 {
			q.deps.Logger.Warn("heal: MRF queue full, notification dropped", "bucket", bucket, "object", object,
				"size", q.opts.size, "dropped", q.stats.Dropped)
		}
		return
	}
	q.entries[key] = &queueEntry{Entry: Entry{Bucket: bucket, Object: object, VersionID: versionID, Queued: now, NotBefore: now}}
	q.stats.Queued++
	q.dirty = true
	q.signal()
}

// Next implements Queue.
func (q *queue) Next(ctx context.Context) (Entry, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Entry{}, err
		}
		e, wait := q.lease()
		if e != nil {
			return *e, nil
		}
		var (
			timer *time.Timer
			fire  <-chan time.Time
		)
		if wait > 0 {
			timer = time.NewTimer(wait)
			fire = timer.C
		}
		select {
		case <-ctx.Done():
		case <-q.wake:
		case <-fire:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// lease leases the due entry queued first; without one it returns how
// long until the next one is due (0: none queued).
func (q *queue) lease() (*Entry, time.Duration) {
	now := q.deps.Clock.Now()
	q.mu.Lock()
	defer q.mu.Unlock()
	var best *queueEntry
	var next time.Time
	for _, e := range q.entries {
		if e.leased {
			continue
		}
		if e.NotBefore.After(now) {
			if next.IsZero() || e.NotBefore.Before(next) {
				next = e.NotBefore
			}
			continue
		}
		if best == nil || e.Queued.Before(best.Queued) ||
			(e.Queued.Equal(best.Queued) && entryKey(e.Bucket, e.Object, e.VersionID) < entryKey(best.Bucket, best.Object, best.VersionID)) {
			best = e
		}
	}
	if best != nil {
		best.leased = true
		out := best.Entry
		// Another due entry may be waiting: let the next waiter look.
		q.signal()
		return &out, 0
	}
	if next.IsZero() {
		return nil, 0
	}
	return nil, next.Sub(now)
}

// Done implements Queue.
func (q *queue) Done(_ context.Context, e Entry, retry bool) {
	now := q.deps.Clock.Now()
	q.mu.Lock()
	defer q.mu.Unlock()
	key := entryKey(e.Bucket, e.Object, e.VersionID)
	cur, ok := q.entries[key]
	if !ok || !cur.leased {
		return
	}
	cur.leased = false
	q.dirty = true
	switch {
	case cur.again:
		// Notified again during the lease: heal once more, now.
		cur.again = false
		cur.Attempts = 0
		cur.NotBefore = now
		if retry {
			q.stats.Retried++
		} else {
			q.stats.Healed++
		}
	case !retry:
		delete(q.entries, key)
		q.stats.Healed++
	default:
		cur.Attempts++
		if q.opts.maxAttempts > 0 && cur.Attempts >= q.opts.maxAttempts {
			delete(q.entries, key)
			q.stats.Dropped++
			q.deps.Logger.Warn("heal: MRF entry abandoned after too many attempts", "bucket", e.Bucket, "object", e.Object,
				"version_id", e.VersionID, "attempts", cur.Attempts)
			return
		}
		cur.NotBefore = now.Add(q.backoff(cur.Attempts))
		q.stats.Retried++
	}
	q.signal()
}

// backoff returns the delay before attempt n+1: minBackoff doubled n-1
// times, capped at maxBackoff.
func (q *queue) backoff(attempts int) time.Duration {
	d := q.opts.minBackoff
	for i := 1; i < attempts && d < q.opts.maxBackoff; i++ {
		d *= 2
	}
	return min(d, q.opts.maxBackoff)
}

// Stats implements Queue.
func (q *queue) Stats(context.Context) QueueStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	st := q.stats
	for _, e := range q.entries {
		if e.leased {
			st.Leased++
		} else {
			st.Pending++
		}
	}
	return st
}

// Start implements lifecycle.Runner: it loads the persisted entries and
// starts the periodic persistence.
func (q *queue) Start(ctx context.Context) error {
	q.runMu.Lock()
	defer q.runMu.Unlock()
	if q.started {
		return nil
	}
	q.load(ctx)
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	q.cancel, q.done, q.started = cancel, make(chan struct{}), true
	go q.persistLoop(runCtx)
	return nil
}

// Stop implements lifecycle.Runner: it stops the periodic persistence and
// persists the queue a last time.
func (q *queue) Stop(ctx context.Context) error {
	q.runMu.Lock()
	defer q.runMu.Unlock()
	if !q.started {
		return nil
	}
	q.cancel()
	select {
	case <-q.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	q.started = false
	return q.persist(ctx, true)
}

func (q *queue) persistLoop(ctx context.Context) {
	defer close(q.done)
	t := time.NewTicker(q.opts.persistInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := q.persist(ctx, false); err != nil && ctx.Err() == nil {
				q.deps.Logger.Warn("heal: cannot persist the MRF queue", "err", err)
			}
		}
	}
}

// snapshot returns the entries to persist, oldest first, and clears the
// dirty flag.
func (q *queue) snapshot(force bool) ([]Entry, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.dirty && !force {
		return nil, false
	}
	q.dirty = false
	out := make([]Entry, 0, len(q.entries))
	for _, e := range q.entries {
		out = append(out, e.Entry)
	}
	slices.SortFunc(out, func(a, b Entry) int {
		if c := a.Queued.Compare(b.Queued); c != 0 {
			return c
		}
		return strings.Compare(entryKey(a.Bucket, a.Object, a.VersionID), entryKey(b.Bucket, b.Object, b.VersionID))
	})
	return out, true
}

// persist writes the queue to every online local drive; it succeeds when
// one drive at least holds it (or no drive is configured).
func (q *queue) persist(ctx context.Context, force bool) error {
	if len(q.deps.Drives) == 0 {
		return nil
	}
	entries, changed := q.snapshot(force)
	if !changed {
		return nil
	}
	data, err := json.Marshal(mrfDocument{Version: mrfVersion, Saved: q.deps.Clock.Now().UTC().Format(time.RFC3339Nano), Entries: entries})
	if err != nil {
		return fmt.Errorf("heal: encode MRF queue: %w", err)
	}
	var errs []error
	written := 0
	for _, d := range q.deps.Drives {
		if d == nil || !d.IsOnline() {
			continue
		}
		if err := d.WriteAll(ctx, domain.SystemBucket, MRFFile, data); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.Endpoint(), err))
			continue
		}
		written++
	}
	if written == 0 && len(errs) > 0 {
		q.mu.Lock()
		q.dirty = true
		q.mu.Unlock()
		return fmt.Errorf("heal: persist MRF queue: %w", errors.Join(errs...))
	}
	return nil
}

// load merges the entries persisted on the local drives into the queue:
// due at once, attempts kept.
func (q *queue) load(ctx context.Context) {
	now := q.deps.Clock.Now()
	loaded := 0
	for _, d := range q.deps.Drives {
		if d == nil || !d.IsOnline() {
			continue
		}
		data, err := d.ReadAll(ctx, domain.SystemBucket, MRFFile)
		if err != nil {
			if !errors.Is(err, domain.ErrObjectNotFound) && !errors.Is(err, domain.ErrBucketNotFound) {
				q.deps.Logger.Warn("heal: cannot read the persisted MRF queue", "drive", d.Endpoint().String(), "err", err)
			}
			continue
		}
		var doc mrfDocument
		if err := json.Unmarshal(data, &doc); err != nil || doc.Version != mrfVersion {
			q.deps.Logger.Warn("heal: invalid persisted MRF queue, ignored", "drive", d.Endpoint().String(), "err", err)
			continue
		}
		q.mu.Lock()
		for _, e := range doc.Entries {
			key := entryKey(e.Bucket, e.Object, e.VersionID)
			if _, ok := q.entries[key]; ok || len(q.entries) >= q.opts.size || e.Bucket == "" {
				continue
			}
			e.NotBefore = now
			q.entries[key] = &queueEntry{Entry: e}
			loaded++
		}
		q.mu.Unlock()
	}
	if loaded > 0 {
		q.deps.Logger.Info("heal: MRF queue restored", "entries", loaded)
		q.signal()
	}
}
