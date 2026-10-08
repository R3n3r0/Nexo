package session

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Defaults of the failed-login throttle: after DefaultMaxFailures failures
// of a client on an access key, or DefaultMaxClientFailures failures of a
// client on any access key (password spraying), within
// DefaultFailureWindow, the attempts are refused for DefaultLockout.
const (
	DefaultMaxFailures       = 5
	DefaultMaxClientFailures = 20
	DefaultFailureWindow     = 5 * time.Minute
	DefaultLockout           = time.Minute
	// DefaultThrottleEntries bounds the memory of each table of the
	// throttle (pairs and clients): beyond it the least recently failed
	// entries are forgotten.
	DefaultThrottleEntries = 4096
)

// ThrottledError is the error of a login refused because of too many
// failed attempts. It wraps domain.ErrTooManyRequests.
type ThrottledError struct {
	// RetryAfter is the remaining lockout.
	RetryAfter time.Duration
}

// Error implements error.
func (e *ThrottledError) Error() string {
	return fmt.Sprintf("too many failed login attempts, retry in %s", e.RetryAfter.Round(time.Second))
}

// Unwrap returns domain.ErrTooManyRequests.
func (e *ThrottledError) Unwrap() error { return domain.ErrTooManyRequests }

// WithLoginThrottle configures the failed-login throttle: maxFailures
// failures within window lock the (client, access key) pair out for
// lockout. maxFailures <= 0 disables the throttle (the per-client limit
// too).
func WithLoginThrottle(maxFailures int, window, lockout time.Duration) Option {
	return func(s *service) {
		limits := s.limits
		limits.maxFailures, limits.window, limits.lockout = maxFailures, window, lockout
		s.limits = limits
	}
}

// WithClientLoginThrottle sets the failures of a client on any access key
// that lock the client out (default DefaultMaxClientFailures); <= 0
// disables the per-client limit.
func WithClientLoginThrottle(maxFailures int) Option {
	return func(s *service) { s.limits.maxClientFailures = maxFailures }
}

// WithThrottleEntries bounds the entries of each table of the throttle
// (default DefaultThrottleEntries, at least 1).
func WithThrottleEntries(n int) Option {
	return func(s *service) {
		if n > 0 {
			s.limits.capacity = n
		}
	}
}

// throttleLimits are the settings of the throttle.
type throttleLimits struct {
	maxFailures       int
	maxClientFailures int
	window            time.Duration
	lockout           time.Duration
	capacity          int
}

func defaultThrottleLimits() throttleLimits {
	return throttleLimits{
		maxFailures: DefaultMaxFailures, maxClientFailures: DefaultMaxClientFailures,
		window: DefaultFailureWindow, lockout: DefaultLockout, capacity: DefaultThrottleEntries,
	}
}

// throttle counts the failed logins per (client, access key) pair and per
// client, and locks a pair or a whole client out after too many of them.
// Its memory is bounded: each table keeps at most capacity entries and
// forgets the least recently failed ones first. A nil throttle never
// throttles.
//
// The client is the address the transport derives (behind trusted
// proxies, the forwarded one): without it every client of a proxy would
// share one budget.
type throttle struct {
	lockout time.Duration

	mu      sync.Mutex
	pairs   *failureTable
	clients *failureTable
}

func newThrottle(l throttleLimits) *throttle {
	if l.maxFailures <= 0 {
		return nil
	}
	t := &throttle{lockout: l.lockout, pairs: newFailureTable(l.maxFailures, l.window, l.capacity)}
	if l.maxClientFailures > 0 {
		t.clients = newFailureTable(l.maxClientFailures, l.window, l.capacity)
	}
	return t
}

func throttleKey(client, accessKey string) string { return client + "\x00" + accessKey }

// check returns the remaining lockout of the pair or of the client, zero
// when it may try.
func (t *throttle) check(client, accessKey string, now time.Time) time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	wait := t.pairs.lockedFor(throttleKey(client, accessKey), now)
	if t.clients != nil && client != "" {
		wait = max(wait, t.clients.lockedFor(client, now))
	}
	return wait
}

// fail records a failed attempt and returns the lockout it starts (zero
// while the pair and the client stay under their limits).
func (t *throttle) fail(client, accessKey string, now time.Time) time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	locked := t.pairs.fail(throttleKey(client, accessKey), now, t.lockout)
	if t.clients != nil && client != "" {
		locked = max(locked, t.clients.fail(client, now, t.lockout))
	}
	return locked
}

// reset forgets the failures of a pair after a successful login. The
// failures of the client on other keys stay: a valid account of its own
// must not reset the budget of a password spraying.
func (t *throttle) reset(client, accessKey string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pairs.forget(throttleKey(client, accessKey))
}

// failureEntry are the recent failures of one key of a table.
type failureEntry struct {
	key         string
	failures    int
	first       time.Time
	lockedUntil time.Time
}

// failureTable counts the failures per key within a window, in least
// recently failed order, with at most capacity keys. The caller holds the
// lock of the throttle.
type failureTable struct {
	limit    int
	window   time.Duration
	capacity int
	entries  map[string]*list.Element
	// order holds *failureEntry, the most recently failed first.
	order *list.List
}

func newFailureTable(limit int, window time.Duration, capacity int) *failureTable {
	if capacity <= 0 {
		capacity = DefaultThrottleEntries
	}
	return &failureTable{limit: limit, window: window, capacity: capacity, entries: map[string]*list.Element{}, order: list.New()}
}

// lockedFor returns the remaining lockout of key.
func (t *failureTable) lockedFor(key string, now time.Time) time.Duration {
	el, ok := t.entries[key]
	if !ok {
		return 0
	}
	e := el.Value.(*failureEntry)
	if !now.Before(e.lockedUntil) {
		return 0
	}
	return e.lockedUntil.Sub(now)
}

// fail records a failure of key and returns the lockout it starts.
func (t *failureTable) fail(key string, now time.Time, lockout time.Duration) time.Duration {
	el, ok := t.entries[key]
	var e *failureEntry
	if ok {
		e = el.Value.(*failureEntry)
		t.order.MoveToFront(el)
		if now.Sub(e.first) > t.window || (!e.lockedUntil.IsZero() && !now.Before(e.lockedUntil)) {
			// A new window, or a new chance after a lockout.
			*e = failureEntry{key: key, first: now}
		}
	} else {
		for t.order.Len() >= t.capacity {
			oldest := t.order.Back()
			t.order.Remove(oldest)
			delete(t.entries, oldest.Value.(*failureEntry).key)
		}
		e = &failureEntry{key: key, first: now}
		t.entries[key] = t.order.PushFront(e)
	}
	e.failures++
	if e.failures < t.limit {
		return 0
	}
	e.lockedUntil = now.Add(lockout)
	return lockout
}

// forget drops key.
func (t *failureTable) forget(key string) {
	if el, ok := t.entries[key]; ok {
		t.order.Remove(el)
		delete(t.entries, key)
	}
}
