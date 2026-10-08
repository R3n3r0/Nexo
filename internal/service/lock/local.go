package lock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Deps are the dependencies of the local locker (none for now).
type Deps struct{}

// Option configures the local locker.
type Option func(*local)

// local is an in-memory reader/writer locker keyed by resource name. Each
// resource holds a small state record; waiters block on a channel that is
// closed (and replaced) whenever the state changes, so that acquisition
// honors both the timeout and the caller's context. Writers are preferred,
// like sync.RWMutex: once a writer waits, new readers queue behind it, so
// that a steady stream of readers cannot starve a write (locks are not
// reentrant: a holder must not take the same read lock again).
type local struct {
	mu        sync.Mutex
	resources map[string]*entry
}

type entry struct {
	writer  bool
	readers int
	// waiters is the number of goroutines waiting for the entry.
	waiters int
	// writersWaiting is the number of waiters that want the write lock;
	// while it is positive no new reader is admitted.
	writersWaiting int
	// changed is closed every time the state changes.
	changed chan struct{}
}

var _ Locker = (*local)(nil)

// NewLocal returns an in-memory Locker suitable for single-node modes.
func NewLocal(_ Deps, opts ...Option) Locker {
	l := &local{resources: make(map[string]*entry)}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Lock implements Locker.
func (l *local) Lock(ctx context.Context, resource string, timeout time.Duration) error {
	return l.acquire(ctx, resource, timeout, true)
}

// RLock implements Locker.
func (l *local) RLock(ctx context.Context, resource string, timeout time.Duration) error {
	return l.acquire(ctx, resource, timeout, false)
}

// Unlock implements Locker.
func (l *local) Unlock(_ context.Context, resource string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.resources[resource]
	if !ok || !e.writer {
		return fmt.Errorf("%w: unlock of resource %q not write-locked", domain.ErrInvalidArgument, resource)
	}
	e.writer = false
	l.notifyLocked(resource, e)
	return nil
}

// RUnlock implements Locker.
func (l *local) RUnlock(_ context.Context, resource string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.resources[resource]
	if !ok || e.readers == 0 {
		return fmt.Errorf("%w: runlock of resource %q not read-locked", domain.ErrInvalidArgument, resource)
	}
	e.readers--
	l.notifyLocked(resource, e)
	return nil
}

// NewNSLock implements Locker.
func (l *local) NewNSLock(_ context.Context, volume, path string) NSLock {
	return &nsLock{locker: l, resource: Resource(volume, path)}
}

func (l *local) acquire(ctx context.Context, resource string, timeout time.Duration, write bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	// queued reports that this writer is counted in e.writersWaiting,
	// which keeps e in the map until the writer acquires or gives up.
	queued := false
	for {
		l.mu.Lock()
		e, ok := l.resources[resource]
		if !ok {
			e = &entry{changed: make(chan struct{})}
			l.resources[resource] = e
		}
		if e.canAcquire(write) {
			if write {
				e.writer = true
				if queued {
					e.writersWaiting--
				}
			} else {
				e.readers++
			}
			l.mu.Unlock()
			return nil
		}
		if write && !queued {
			e.writersWaiting++
			queued = true
		}
		e.waiters++
		wait := e.changed
		l.mu.Unlock()

		select {
		case <-wait:
			l.mu.Lock()
			e.waiters--
			l.mu.Unlock()
		case <-deadline:
			l.release(resource, e, queued)
			return fmt.Errorf("%w: resource %q after %s", domain.ErrLockTimeout, resource, timeout)
		case <-ctx.Done():
			l.release(resource, e, queued)
			return ctx.Err()
		}
	}
}

// release drops a waiter that gave up and removes the entry when it is
// unused. A writer that gave up wakes the readers it was holding back.
func (l *local) release(resource string, e *entry, queuedWriter bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.waiters--
	if queuedWriter {
		e.writersWaiting--
		l.notifyLocked(resource, e)
		return
	}
	l.cleanupLocked(resource, e)
}

func (l *local) notifyLocked(resource string, e *entry) {
	close(e.changed)
	e.changed = make(chan struct{})
	l.cleanupLocked(resource, e)
}

func (l *local) cleanupLocked(resource string, e *entry) {
	if !e.writer && e.readers == 0 && e.waiters == 0 && e.writersWaiting == 0 {
		if cur, ok := l.resources[resource]; ok && cur == e {
			delete(l.resources, resource)
		}
	}
}

// canAcquire reports whether the lock can be taken now. Readers also wait
// while a writer waits for the lock (writer preference).
func (e *entry) canAcquire(write bool) bool {
	if write {
		return !e.writer && e.readers == 0
	}
	return !e.writer && e.writersWaiting == 0
}

// nsLock is the NSLock handle over a local resource.
type nsLock struct {
	locker   *local
	resource string
}

var _ NSLock = (*nsLock)(nil)

// GetLock implements NSLock.
func (n *nsLock) GetLock(ctx context.Context, timeout time.Duration) (LockContext, error) {
	if err := n.locker.Lock(ctx, n.resource, timeout); err != nil {
		return nil, err
	}
	return newLockContext(ctx, func(ctx context.Context) { _ = n.locker.Unlock(ctx, n.resource) }), nil
}

// GetRLock implements NSLock.
func (n *nsLock) GetRLock(ctx context.Context, timeout time.Duration) (LockContext, error) {
	if err := n.locker.RLock(ctx, n.resource, timeout); err != nil {
		return nil, err
	}
	return newLockContext(ctx, func(ctx context.Context) { _ = n.locker.RUnlock(ctx, n.resource) }), nil
}

type lockContext struct {
	ctx     context.Context
	cancel  context.CancelFunc
	release func(ctx context.Context)
	once    sync.Once
}

func newLockContext(parent context.Context, release func(ctx context.Context)) *lockContext {
	ctx, cancel := context.WithCancel(parent)
	return &lockContext{ctx: ctx, cancel: cancel, release: release}
}

// Context implements LockContext.
func (c *lockContext) Context() context.Context { return c.ctx }

// Unlock implements LockContext.
func (c *lockContext) Unlock(ctx context.Context) {
	c.once.Do(func() {
		c.release(context.WithoutCancel(ctx))
		c.cancel()
	})
}
