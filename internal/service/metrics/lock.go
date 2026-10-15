package metrics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/lock"
)

// lockBuckets are the bounds of the lock acquisition latency: a
// distributed acquisition is a round of RPC calls, a contended one waits.
func lockBuckets() []float64 {
	return []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}
}

// lockInstruments are the instruments of the locker decorator.
type lockInstruments struct {
	duration Histogram
	timeouts Counter
	errors   Counter
}

// WrapLocker returns l measured: the latency of every acquisition
// (nexo_lock_acquire_duration_seconds, by type read or write), the
// acquisitions that timed out and the ones that failed otherwise (lost
// quorum). A canceled acquisition (the client went away) is neither.
func WrapLocker(ctx context.Context, l lock.Locker, reg Registry) (lock.Locker, error) {
	if l == nil || reg == nil {
		return nil, fmt.Errorf("%w: metrics: the locker decorator needs a locker and a registry", domain.ErrInvalidArgument)
	}
	m := &lockInstruments{}
	var err error
	if m.duration, err = reg.Histogram(ctx, Definition{Subsystem: "lock", Name: "acquire_duration_seconds",
		Help: "Latency of the namespace lock acquisitions (dsync in a cluster), by type.", Labels: []string{"type"}, Buckets: lockBuckets()}); err != nil {
		return nil, fmt.Errorf("metrics: lock: %w", err)
	}
	if m.timeouts, err = reg.Counter(ctx, Definition{Subsystem: "lock", Name: "acquire_timeouts_total",
		Help: "Namespace lock acquisitions that timed out, by type.", Labels: []string{"type"}}); err != nil {
		return nil, fmt.Errorf("metrics: lock: %w", err)
	}
	if m.errors, err = reg.Counter(ctx, Definition{Subsystem: "lock", Name: "acquire_errors_total",
		Help: "Namespace lock acquisitions that failed for another reason than a timeout (quorum), by type.", Labels: []string{"type"}}); err != nil {
		return nil, fmt.Errorf("metrics: lock: %w", err)
	}
	return &measuredLocker{next: l, m: m}, nil
}

// Lock types (label type).
const (
	lockWrite = "write"
	lockRead  = "read"
)

// record records an acquisition of type typ started at start.
func (m *lockInstruments) record(ctx context.Context, typ string, start time.Time, err error) {
	switch {
	case err == nil:
		m.duration.Observe(ctx, time.Since(start).Seconds(), typ)
	case errors.Is(err, domain.ErrLockTimeout), errors.Is(err, context.DeadlineExceeded):
		m.timeouts.Add(ctx, 1, typ)
	case errors.Is(err, context.Canceled):
	default:
		m.errors.Add(ctx, 1, typ)
	}
}

type measuredLocker struct {
	next lock.Locker
	m    *lockInstruments
}

var _ lock.Locker = (*measuredLocker)(nil)

// Lock implements lock.Locker.
func (l *measuredLocker) Lock(ctx context.Context, resource string, timeout time.Duration) error {
	start := time.Now()
	err := l.next.Lock(ctx, resource, timeout)
	l.m.record(ctx, lockWrite, start, err)
	return err
}

// Unlock implements lock.Locker.
func (l *measuredLocker) Unlock(ctx context.Context, resource string) error {
	return l.next.Unlock(ctx, resource)
}

// RLock implements lock.Locker.
func (l *measuredLocker) RLock(ctx context.Context, resource string, timeout time.Duration) error {
	start := time.Now()
	err := l.next.RLock(ctx, resource, timeout)
	l.m.record(ctx, lockRead, start, err)
	return err
}

// RUnlock implements lock.Locker.
func (l *measuredLocker) RUnlock(ctx context.Context, resource string) error {
	return l.next.RUnlock(ctx, resource)
}

// NewNSLock implements lock.Locker.
func (l *measuredLocker) NewNSLock(ctx context.Context, volume, path string) lock.NSLock {
	return &measuredNSLock{next: l.next.NewNSLock(ctx, volume, path), m: l.m}
}

type measuredNSLock struct {
	next lock.NSLock
	m    *lockInstruments
}

var _ lock.NSLock = (*measuredNSLock)(nil)

// GetLock implements lock.NSLock.
func (n *measuredNSLock) GetLock(ctx context.Context, timeout time.Duration) (lock.LockContext, error) {
	start := time.Now()
	lc, err := n.next.GetLock(ctx, timeout)
	n.m.record(ctx, lockWrite, start, err)
	return lc, err
}

// GetRLock implements lock.NSLock.
func (n *measuredNSLock) GetRLock(ctx context.Context, timeout time.Duration) (lock.LockContext, error) {
	start := time.Now()
	lc, err := n.next.GetRLock(ctx, timeout)
	n.m.record(ctx, lockRead, start, err)
	return lc, err
}
