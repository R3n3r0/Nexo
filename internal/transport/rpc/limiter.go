package rpc

import (
	"net"
	"sync"
	"time"
)

// Defaults of the authentication failure limiter of the RPC server.
const (
	defaultFailBurst  = 20
	defaultFailRefill = 3 * time.Second
	// failLogWindow is the minimum time between two warnings about the
	// same remote address and reason.
	failLogWindow = time.Minute
	// maxTrackedRemotes bounds the remote addresses the limiter tracks.
	maxTrackedRemotes = 4096
)

// failureLimiter is a token bucket of authentication failures per remote
// address: every failure takes a token, a token comes back every refill;
// an address without tokens is refused without verification (an oracle
// on the root password that answers slowly). It also paces the warnings
// of the refusals. A legitimate node never fails its authentication: the
// limiter only ever slows a misconfigured node or an attacker.
type failureLimiter struct {
	burst  int
	refill time.Duration

	mu      sync.Mutex
	buckets map[string]*failBucket
	logs    map[string]*logState
}

type failBucket struct {
	tokens float64
	at     time.Time
}

type logState struct {
	last       time.Time
	suppressed int
}

func newFailureLimiter(burst int, refill time.Duration) *failureLimiter {
	return &failureLimiter{burst: burst, refill: refill, buckets: map[string]*failBucket{}, logs: map[string]*logState{}}
}

// level refills the bucket of remote up to now and returns it (nil when
// the address is not tracked).
func (l *failureLimiter) level(remote string, now time.Time) *failBucket {
	b := l.buckets[remote]
	if b == nil {
		return nil
	}
	if elapsed := now.Sub(b.at); elapsed > 0 {
		b.tokens = min(float64(l.burst), b.tokens+float64(elapsed)/float64(l.refill))
		b.at = now
	}
	return b
}

// allow reports whether a call of remote may be verified.
func (l *failureLimiter) allow(remote string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.level(remote, now)
	return b == nil || b.tokens >= 1
}

// failed records an authentication failure of remote.
func (l *failureLimiter) failed(remote string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.level(remote, now)
	if b == nil {
		if len(l.buckets) >= maxTrackedRemotes {
			l.prune(now)
		}
		b = &failBucket{tokens: float64(l.burst), at: now}
		l.buckets[remote] = b
	}
	b.tokens = max(0, b.tokens-1)
}

// prune forgets the addresses whose bucket is full again (and, when that
// is not enough, every address: the limiter fails open rather than grow).
func (l *failureLimiter) prune(now time.Time) {
	for k := range l.buckets {
		if b := l.level(k, now); b.tokens >= float64(l.burst) {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= maxTrackedRemotes {
		clear(l.buckets)
	}
	for k, s := range l.logs {
		if now.Sub(s.last) > failLogWindow {
			delete(l.logs, k)
		}
	}
	if len(l.logs) >= maxTrackedRemotes {
		clear(l.logs)
	}
}

// shouldLog reports whether a refusal of key is to be logged now, with
// the number of refusals not logged since the previous warning.
func (l *failureLimiter) shouldLog(key string, now time.Time) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.logs[key]
	if s == nil {
		if len(l.logs) >= maxTrackedRemotes {
			l.prune(now)
		}
		l.logs[key] = &logState{last: now}
		return 0, true
	}
	if now.Sub(s.last) < failLogWindow {
		s.suppressed++
		return 0, false
	}
	n := s.suppressed
	s.last, s.suppressed = now, 0
	return n, true
}

// remoteHost returns the host of a remote address ("ip:port").
func remoteHost(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
