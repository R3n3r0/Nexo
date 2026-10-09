// Package lock defines the namespace locking used to serialize access to
// buckets and objects. NewLocal serves the single-node modes; the cluster
// mode uses NewDistributed (dsync, docs/03 §3.6): every node runs an
// in-memory lock Server (NewServer) and a lock is held when a quorum of the
// servers granted it, with a lease refreshed in the background.
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
	// the lock is released or lost; the cause of a lost lock
	// (context.Cause) wraps domain.ErrLockLost.
	Context() context.Context
	// Unlock releases the lock. It is idempotent. ctx carries the request
	// values (logging, tracing); the release is not aborted when ctx is
	// already canceled, so that a lock is never leaked by a client that
	// went away.
	Unlock(ctx context.Context)
}

// Args identifies one acquisition on the lock servers: every server that
// grants it records the same UID, so that the holder refreshes and releases
// exactly that acquisition.
type Args struct {
	// UID identifies the acquisition (one per Lock/RLock call).
	UID string
	// Resources are the names locked together (all or nothing).
	Resources []string
	// Owner is the node that requested the lock.
	Owner string
	// Source describes the caller for diagnostics.
	Source string
	// Quorum is the number of servers the acquisition needs.
	Quorum int
}

// Info describes a lock entry held by a server (diagnostics).
type Info struct {
	Resource string
	UID      string
	Owner    string
	Source   string
	// Writer reports a write lock; read locks may have several entries.
	Writer bool
	Quorum int
	// Since is the time the lock was granted, Refreshed the time of the
	// last refresh, both on the clock of the server.
	Since     time.Time
	Refreshed time.Time
}

// Status is a lock of the deployment as seen by the lock servers: one
// entry per acquisition (UID) with the number of servers holding it.
type Status struct {
	Info
	// Servers is the number of lock servers holding the acquisition and
	// Total the number of servers queried.
	Servers int
	Total   int
}

// Server is the in-memory lock server of one node (docs/03 §3.6). The
// distributed locker acquires a lock on a quorum of servers: each call
// answers whether this server granted (or found) the acquisition. Errors
// are reserved to failures (unreachable server, invalid arguments). Locks
// that are not refreshed for a while are orphans (their holder died) and
// expire on their own.
type Server interface {
	// Lock grants a write lock on every resource of args when none of them
	// is locked; it is idempotent for the same UID.
	Lock(ctx context.Context, args Args) (bool, error)
	// RLock grants a read lock when no resource of args is write-locked.
	RLock(ctx context.Context, args Args) (bool, error)
	// Unlock releases the write lock of args.UID.
	Unlock(ctx context.Context, args Args) (bool, error)
	// RUnlock releases the read lock of args.UID.
	RUnlock(ctx context.Context, args Args) (bool, error)
	// Refresh renews the entries of args.UID; false means the server no
	// longer holds them (expired or force-unlocked).
	Refresh(ctx context.Context, args Args) (bool, error)
	// ForceUnlock removes every entry of args.Resources (any UID), or of
	// args.UID when no resource is given (admin lock clear).
	ForceUnlock(ctx context.Context, args Args) (bool, error)
	// List returns the entries held by the server.
	List(ctx context.Context) ([]Info, error)
}

// Manager exposes the locks of a deployment for diagnostics (admin lock
// list/clear): it aggregates the entries of every lock server.
type Manager interface {
	// List returns the locks held across the servers, sorted by resource.
	List(ctx context.Context) ([]Status, error)
	// ForceUnlock releases the given resources on every server, whoever
	// holds them. Unreachable servers are reported in the error; their
	// entries expire on their own.
	ForceUnlock(ctx context.Context, resources ...string) error
}
