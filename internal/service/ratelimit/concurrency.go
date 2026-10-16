package ratelimit

import (
	"context"
	"sync"
	"time"
)

// slots are the S3 requests a node serves at once (api.requests_max): the
// requests beyond the limit wait in arrival order. The limit is the one
// of the last acquisition: a change applies to the next request. The
// requests admitted while there was no limit are not counted.
type slots struct {
	mu      sync.Mutex
	inUse   int
	max     int
	waiters []chan struct{}
}

// resize sets the limit (0: none) and admits the waiters it leaves room
// for.
func (s *slots) resize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.max = n
	s.grantLocked()
}

// grantLocked admits the first waiters while there is room.
func (s *slots) grantLocked() {
	for len(s.waiters) > 0 && (s.max <= 0 || s.inUse < s.max) {
		ch := s.waiters[0]
		s.waiters[0] = nil
		s.waiters = s.waiters[1:]
		s.inUse++
		close(ch)
	}
}

// acquire takes a slot of limit, waiting up to deadline (a timer of the
// wall clock: the wait is real time).
func (s *slots) acquire(ctx context.Context, limit int, deadline time.Duration) (Release, Decision, error) {
	s.mu.Lock()
	s.max = limit
	s.grantLocked()
	if len(s.waiters) == 0 && s.inUse < limit {
		s.inUse++
		s.mu.Unlock()
		return s.releaser(), Decision{Allowed: true}, nil
	}
	refused := Decision{Kind: KindConcurrency, RetryAfter: minRetryAfter}
	if deadline <= 0 {
		s.mu.Unlock()
		return func() {}, refused, nil
	}
	ch := make(chan struct{})
	s.waiters = append(s.waiters, ch)
	s.mu.Unlock()

	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case <-ch:
		return s.releaser(), Decision{Allowed: true}, nil
	case <-timer.C:
		if s.abandon(ch) {
			return func() {}, refused, nil
		}
		// Granted while the timer fired.
		return s.releaser(), Decision{Allowed: true}, nil
	case <-ctx.Done():
		if !s.abandon(ch) {
			s.release()
		}
		return func() {}, refused, context.Cause(ctx)
	}
}

// abandon removes a waiter; false when it was granted a slot meanwhile.
func (s *slots) abandon(ch chan struct{}) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, w := range s.waiters {
		if w == ch {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			return true
		}
	}
	return false
}

// release frees a slot and admits the next waiter.
func (s *slots) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inUse--
	s.grantLocked()
}

// releaser returns the Release of a slot: once only.
func (s *slots) releaser() Release {
	var once sync.Once
	return func() { once.Do(s.release) }
}
