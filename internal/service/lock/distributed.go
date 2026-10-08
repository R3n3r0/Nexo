package lock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Defaults of the distributed locker (docs/03 §3.6).
const (
	// DefaultRefreshInterval is the lease renewal period of a held lock.
	DefaultRefreshInterval = 10 * time.Second
	defaultCallTimeout     = 5 * time.Second
	defaultRetryMin        = 50 * time.Millisecond
	defaultRetryMax        = 500 * time.Millisecond
)

// DistributedDeps are the dependencies of the distributed locker.
type DistributedDeps struct {
	// Servers are the lock servers of the deployment, the local one
	// included (required, at least one). A lock is held when a quorum of
	// them granted it.
	Servers []Server
	// Owner identifies this node in the entries of the servers.
	Owner  string
	Logger *slog.Logger
}

// DistributedOption configures the distributed locker.
type DistributedOption func(*distOptions)

type distOptions struct {
	refreshInterval time.Duration
	callTimeout     time.Duration
	retryMin        time.Duration
	retryMax        time.Duration
}

// WithRefreshInterval sets the lease renewal period (default 10s). It must
// stay well below the stale timeout of the servers (1 min).
func WithRefreshInterval(d time.Duration) DistributedOption {
	return func(o *distOptions) {
		if d > 0 {
			o.refreshInterval = d
		}
	}
}

// WithCallTimeout bounds every call to a lock server (default 5s).
func WithCallTimeout(d time.Duration) DistributedOption {
	return func(o *distOptions) {
		if d > 0 {
			o.callTimeout = d
		}
	}
}

// WithRetryInterval sets the bounds of the random pause between two
// acquisition attempts (default 50ms-500ms).
func WithRetryInterval(lo, hi time.Duration) DistributedOption {
	return func(o *distOptions) {
		if lo > 0 && hi >= lo {
			o.retryMin, o.retryMax = lo, hi
		}
	}
}

// distributed is the dsync Locker: a lock is granted when at least
// n/2+1 servers grant it (read locks: n/2 when n is even, so that a reader
// and a writer can never both reach their quorum), retried with jitter
// until the timeout, renewed every refresh interval and lost (its context
// canceled) when a renewal no longer reaches the quorum.
type distributed struct {
	deps DistributedDeps
	opts distOptions

	mu sync.Mutex
	// writers and readers track the acquisitions made through the Locker
	// methods, which release by resource name.
	writers map[string]*acquisition
	readers map[string][]*acquisition
}

var _ Locker = (*distributed)(nil)

// NewDistributed returns the distributed (dsync) Locker over the lock
// servers of the deployment.
func NewDistributed(deps DistributedDeps, opts ...DistributedOption) (Locker, error) {
	if len(deps.Servers) == 0 {
		return nil, fmt.Errorf("%w: lock: the distributed locker needs at least one server", domain.ErrInvalidArgument)
	}
	for i, s := range deps.Servers {
		if s == nil {
			return nil, fmt.Errorf("%w: lock: server %d is nil", domain.ErrInvalidArgument, i)
		}
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := distOptions{
		refreshInterval: DefaultRefreshInterval, callTimeout: defaultCallTimeout,
		retryMin: defaultRetryMin, retryMax: defaultRetryMax,
	}
	for _, opt := range opts {
		opt(&o)
	}
	return &distributed{
		deps: deps, opts: o,
		writers: make(map[string]*acquisition),
		readers: make(map[string][]*acquisition),
	}, nil
}

// WriteQuorum returns the number of lock servers out of n that must grant a
// write lock: n/2+1.
func WriteQuorum(n int) int { return n/2 + 1 }

// ReadQuorum returns the number of lock servers out of n that must grant a
// read lock: n/2 when n is even (a writer holds n/2+1 servers, so a reader
// can never reach n/2 free ones), n/2+1 when n is odd.
func ReadQuorum(n int) int {
	if n%2 == 0 {
		return n / 2
	}
	return n/2 + 1
}

// Lock implements Locker.
func (d *distributed) Lock(ctx context.Context, resource string, timeout time.Duration) error {
	acq, err := d.acquire(ctx, resource, timeout, true)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.writers[resource] = acq
	d.mu.Unlock()
	return nil
}

// RLock implements Locker.
func (d *distributed) RLock(ctx context.Context, resource string, timeout time.Duration) error {
	acq, err := d.acquire(ctx, resource, timeout, false)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.readers[resource] = append(d.readers[resource], acq)
	d.mu.Unlock()
	return nil
}

// Unlock implements Locker.
func (d *distributed) Unlock(ctx context.Context, resource string) error {
	d.mu.Lock()
	acq, ok := d.writers[resource]
	delete(d.writers, resource)
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: unlock of resource %q not write-locked", domain.ErrInvalidArgument, resource)
	}
	acq.Unlock(ctx)
	return nil
}

// RUnlock implements Locker.
func (d *distributed) RUnlock(ctx context.Context, resource string) error {
	d.mu.Lock()
	held := d.readers[resource]
	if len(held) == 0 {
		d.mu.Unlock()
		return fmt.Errorf("%w: runlock of resource %q not read-locked", domain.ErrInvalidArgument, resource)
	}
	acq := held[len(held)-1]
	if len(held) == 1 {
		delete(d.readers, resource)
	} else {
		d.readers[resource] = held[:len(held)-1]
	}
	d.mu.Unlock()
	acq.Unlock(ctx)
	return nil
}

// NewNSLock implements Locker.
func (d *distributed) NewNSLock(_ context.Context, volume, path string) NSLock {
	return &distNSLock{d: d, resource: Resource(volume, path)}
}

// distNSLock is the NSLock handle of the distributed locker.
type distNSLock struct {
	d        *distributed
	resource string
}

var _ NSLock = (*distNSLock)(nil)

// GetLock implements NSLock.
func (n *distNSLock) GetLock(ctx context.Context, timeout time.Duration) (LockContext, error) {
	return n.d.acquire(ctx, n.resource, timeout, true)
}

// GetRLock implements NSLock.
func (n *distNSLock) GetRLock(ctx context.Context, timeout time.Duration) (LockContext, error) {
	return n.d.acquire(ctx, n.resource, timeout, false)
}

// acquire retries an acquisition on the quorum of the servers until it
// succeeds, the timeout expires (domain.ErrLockTimeout) or ctx ends.
func (d *distributed) acquire(ctx context.Context, resource string, timeout time.Duration, write bool) (*acquisition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if resource == "" {
		return nil, fmt.Errorf("%w: lock: empty resource name", domain.ErrInvalidArgument)
	}
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	quorum := d.quorum(write)
	// releases tracks the background releases of the failed attempts: an
	// acquisition that gives up waits for them, so that it leaves nothing
	// behind on the servers.
	var releases sync.WaitGroup
	fail := func(err error) (*acquisition, error) {
		releases.Wait()
		return nil, err
	}
	for {
		acq, err := d.tryAcquire(ctx, resource, write, quorum, deadline, &releases)
		if err != nil {
			return fail(err)
		}
		if acq != nil {
			acq.startRefresh()
			return acq, nil
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		pause := d.jitter()
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return fail(fmt.Errorf("%w: resource %q after %s (quorum %d of %d lock servers)",
					domain.ErrLockTimeout, resource, timeout, quorum, len(d.deps.Servers)))
			}
			pause = min(pause, remaining)
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fail(ctx.Err())
		case <-timer.C:
		}
	}
}

func (d *distributed) quorum(write bool) int {
	if write {
		return WriteQuorum(len(d.deps.Servers))
	}
	return ReadQuorum(len(d.deps.Servers))
}

func (d *distributed) jitter() time.Duration {
	lo, hi := d.opts.retryMin, d.opts.retryMax
	if hi <= lo {
		return lo
	}
	return lo + rand.N(hi-lo)
}

// grantResult is the answer of one server to an acquisition attempt.
type grantResult struct {
	idx     int
	granted bool
	err     error
}

// tryAcquire runs one acquisition attempt with a fresh UID: it returns the
// acquisition as soon as the quorum granted it, or nil when the quorum
// cannot be reached anymore (the grants obtained are then released).
// Calls still in flight are drained in the background; their grants belong
// to the acquisition and are released with it (failed attempts register
// their background release in releases).
func (d *distributed) tryAcquire(ctx context.Context, resource string, write bool, quorum int, deadline time.Time, releases *sync.WaitGroup) (*acquisition, error) {
	servers := d.deps.Servers
	args := Args{UID: uuid.NewString(), Resources: []string{resource}, Owner: d.deps.Owner, Quorum: quorum}
	callCtx, cancel := context.WithTimeout(ctx, d.opts.callTimeout)
	if !deadline.IsZero() && time.Until(deadline) < d.opts.callTimeout {
		cancel()
		callCtx, cancel = context.WithDeadline(ctx, deadline.Add(d.opts.callTimeout/10))
	}
	results := make(chan grantResult, len(servers))
	for i, s := range servers {
		go func() {
			var ok bool
			var err error
			if write {
				ok, err = s.Lock(callCtx, args)
			} else {
				ok, err = s.RLock(callCtx, args)
			}
			results <- grantResult{idx: i, granted: ok && err == nil, err: err}
		}()
	}
	granted := make([]bool, len(servers))
	count, pending := 0, len(servers)
	var errs []error
	for pending > 0 && count < quorum && count+pending >= quorum {
		r := <-results
		pending--
		if r.granted {
			granted[r.idx] = true
			count++
		} else if r.err != nil {
			errs = append(errs, r.err)
		}
	}
	acq := &acquisition{d: d, args: args, write: write, quorum: quorum, drained: make(chan struct{})}
	// The remaining answers are collected in the background; late grants
	// are recorded so that a failed attempt releases them too.
	go func() {
		defer cancel()
		defer close(acq.drained)
		for ; pending > 0; pending-- {
			<-results
		}
	}()
	if count >= quorum {
		acq.ctx, acq.cancel = context.WithCancel(ctx)
		return acq, nil
	}
	releases.Add(1)
	if err := ctx.Err(); err != nil {
		go func() {
			defer releases.Done()
			acq.releaseServers(context.WithoutCancel(ctx), "acquisition canceled")
		}()
		return nil, err
	}
	d.deps.Logger.LogAttrs(ctx, slog.LevelDebug, "lock: quorum not reached, retrying",
		slog.String("resource", resource), slog.Bool("write", write), slog.Int("granted", count),
		slog.Int("quorum", quorum), slog.Int("servers", len(servers)), slog.Any("errors", errors.Join(errs...)))
	// Release synchronously where the lock was granted, so that the next
	// attempt does not trip over this one; the servers still answering
	// are released once they did.
	acq.releaseOn(context.WithoutCancel(ctx), granted)
	go func() {
		defer releases.Done()
		acq.releaseServers(context.WithoutCancel(ctx), "acquisition failed")
	}()
	return nil, nil
}

// acquisition is a lock held on a quorum of servers. It implements
// LockContext: its context is canceled when the lock is released or lost.
type acquisition struct {
	d      *distributed
	args   Args
	write  bool
	quorum int

	ctx    context.Context
	cancel context.CancelFunc
	// drained is closed once every server answered the acquisition.
	drained chan struct{}

	// stop ends the renewal; refreshCtx bounds the renewal calls in
	// flight, so that a release never waits for a slow server.
	stop          chan struct{}
	refreshCtx    context.Context
	refreshCancel context.CancelFunc
	refreshW      sync.WaitGroup
	once          sync.Once
}

var _ LockContext = (*acquisition)(nil)

// Context implements LockContext.
func (a *acquisition) Context() context.Context { return a.ctx }

// Unlock implements LockContext: it stops the renewal and releases the
// lock on every server (the release survives the cancellation of ctx).
func (a *acquisition) Unlock(ctx context.Context) {
	a.once.Do(func() {
		close(a.stop)
		a.refreshCancel()
		a.refreshW.Wait()
		a.cancel()
		a.releaseServers(context.WithoutCancel(ctx), "released")
	})
}

func (a *acquisition) startRefresh() {
	a.stop = make(chan struct{})
	a.refreshCtx, a.refreshCancel = context.WithCancel(context.Background())
	a.refreshW.Add(1)
	go a.refreshLoop()
}

// refreshLoop renews the lease until the lock is released, and declares
// the lock lost when a renewal does not reach the quorum.
func (a *acquisition) refreshLoop() {
	defer a.refreshW.Done()
	ticker := time.NewTicker(a.d.opts.refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-ticker.C:
		}
		refreshed := a.refresh()
		select {
		case <-a.stop:
			return // released while refreshing
		default:
		}
		if refreshed >= a.quorum {
			continue
		}
		a.d.deps.Logger.Warn("lock: lost the quorum while refreshing, the operation is canceled",
			"resource", a.args.Resources, "uid", a.args.UID, "write", a.write,
			"refreshed", refreshed, "quorum", a.quorum, "servers", len(a.d.deps.Servers))
		a.cancel()
		go a.releaseServers(context.Background(), "lost")
		return
	}
}

// refresh renews the entries on every server and returns how many still
// held them.
func (a *acquisition) refresh() int {
	ctx, cancel := context.WithTimeout(a.refreshCtx, a.d.opts.callTimeout)
	defer cancel()
	results := make(chan bool, len(a.d.deps.Servers))
	for _, s := range a.d.deps.Servers {
		go func() {
			ok, err := s.Refresh(ctx, a.args)
			results <- ok && err == nil
		}()
	}
	n := 0
	for range a.d.deps.Servers {
		if <-results {
			n++
		}
	}
	return n
}

// releaseServers waits for the acquisition calls still in flight and
// releases the lock on every server.
func (a *acquisition) releaseServers(ctx context.Context, why string) {
	<-a.drained
	all := make([]bool, len(a.d.deps.Servers))
	for i := range all {
		all[i] = true
	}
	released := a.releaseOn(ctx, all)
	a.d.deps.Logger.LogAttrs(ctx, slog.LevelDebug, "lock: released",
		slog.Any("resource", a.args.Resources), slog.String("uid", a.args.UID), slog.String("why", why),
		slog.Int("servers", released))
}

// releaseOn releases the lock on the selected servers and returns how many
// of them held it.
func (a *acquisition) releaseOn(ctx context.Context, selected []bool) int {
	ctx, cancel := context.WithTimeout(ctx, a.d.opts.callTimeout)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	released := 0
	for i, s := range a.d.deps.Servers {
		if !selected[i] {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var ok bool
			var err error
			if a.write {
				ok, err = s.Unlock(ctx, a.args)
			} else {
				ok, err = s.RUnlock(ctx, a.args)
			}
			if ok && err == nil {
				mu.Lock()
				released++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return released
}
