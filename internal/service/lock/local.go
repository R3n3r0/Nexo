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
// honors both the timeout and the caller's context.
type local struct {
	mu        sync.Mutex
	resources map[string]*entry
}

type entry struct {
	writer  bool
	readers int
	// waiters is the number of goroutines waiting for the entry.
	waiters int
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
			} else {
				e.readers++
			}
			l.mu.Unlock()
			return nil
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
			l.release(resource, e)
			return fmt.Errorf("%w: resource %q after %s", domain.ErrLockTimeout, resource, timeout)
		case <-ctx.Done():
			l.release(resource, e)
			return ctx.Err()
		}
	}
}

// release drops a waiter and removes the entry when it is unused.
func (l *local) release(resource string, e *entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.waiters--
	l.cleanupLocked(resource, e)
}

func (l *local) notifyLocked(resource string, e *entry) {
	close(e.changed)
	e.changed = make(chan struct{})
	l.cleanupLocked(resource, e)
}

func (l *local) cleanupLocked(resource string, e *entry) {
	if !e.writer && e.readers == 0 && e.waiters == 0 {
		if cur, ok := l.resources[resource]; ok && cur == e {
			delete(l.resources, resource)
		}
	}
}

func (e *entry) canAcquire(write bool) bool {
	if write {
		return !e.writer && e.readers == 0
	}
	return !e.writer
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
	return newLockContext(ctx, func() { _ = n.locker.Unlock(context.Background(), n.resource) }), nil
}

// GetRLock implements NSLock.
func (n *nsLock) GetRLock(ctx context.Context, timeout time.Duration) (LockContext, error) {
	if err := n.locker.RLock(ctx, n.resource, timeout); err != nil {
		return nil, err
	}
	return newLockContext(ctx, func() { _ = n.locker.RUnlock(context.Background(), n.resource) }), nil
}

type lockContext struct {
	ctx     context.Context
	cancel  context.CancelFunc
	release func()
	once    sync.Once
}

func newLockContext(parent context.Context, release func()) *lockContext {
	ctx, cancel := context.WithCancel(parent)
	return &lockContext{ctx: ctx, cancel: cancel, release: release}
}

// Context implements LockContext.
func (c *lockContext) Context() context.Context { return c.ctx }

// Unlock implements LockContext.
func (c *lockContext) Unlock() {
	c.once.Do(func() {
		c.release()
		c.cancel()
	})
}
