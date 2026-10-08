// Package lock defines the namespace locking used to serialize access to
// buckets and objects. The local implementation serves single-node modes;
// the distributed one (dsync) arrives with the cluster mode.
package lock

import (
	"context"
	"time"
)

// Locker acquires read/write locks on named resources. Locks are not
// reentrant. Lock and RLock return domain.ErrLockTimeout when the lock
// cannot be acquired within timeout, or ctx.Err() when ctx ends first.
type Locker interface {
	Lock(ctx context.Context, resource string, timeout time.Duration) error
	Unlock(ctx context.Context, resource string) error
	RLock(ctx context.Context, resource string, timeout time.Duration) error
	RUnlock(ctx context.Context, resource string) error

	// NewNSLock returns a lock handle for the given volume/path, the
	// preferred way to lock objects. Nothing is acquired until GetLock or
	// GetRLock is called.
	NewNSLock(ctx context.Context, volume, path string) NSLock
}

// NSLock is a lock handle on one namespace entry (volume + path).
type NSLock interface {
	// GetLock acquires the write lock and returns a context that is
	// canceled if the lock is lost, plus the handle to release it.
	GetLock(ctx context.Context, timeout time.Duration) (LockContext, error)
	// GetRLock acquires the read lock.
	GetRLock(ctx context.Context, timeout time.Duration) (LockContext, error)
}

// LockContext is a held lock.
type LockContext interface {
	// Context is derived from the caller's context and is canceled when
	// the lock is released or lost.
	Context() context.Context
	// Unlock releases the lock. It is idempotent.
	Unlock()
}
