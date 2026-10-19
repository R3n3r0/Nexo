package middleware

import (
	"context"
	"net/http"
	"runtime"
	"sync/atomic"
)

// ActivityDeps are the dependencies of the activity meter (none: the type
// keeps the constructor shape of docs/12 §12.3).
type ActivityDeps struct{}

// ActivityOption configures the activity meter.
type ActivityOption func(*Activity)

// WithActivityCapacity sets the requests in progress at which the load is
// 1 (default GOMAXPROCS).
func WithActivityCapacity(n int) ActivityOption {
	return func(a *Activity) {
		if n > 0 {
			a.capacity = n
		}
	}
}

// Activity counts the requests in progress of a listener and reports its
// load to the background work that yields to the API (the scanner): the
// peak of the requests in progress since the previous sample, over the
// capacity, at most 1. The peak keeps a burst of short requests visible
// to a sampler that would otherwise often see none in progress.
type Activity struct {
	capacity int
	inflight atomic.Int64
	peak     atomic.Int64
}

var _ Middleware = (*Activity)(nil)

// NewActivity returns an activity meter.
func NewActivity(_ ActivityDeps, opts ...ActivityOption) *Activity {
	a := &Activity{capacity: runtime.GOMAXPROCS(0)}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Wrap implements Middleware.
func (a *Activity) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := a.inflight.Add(1)
		for {
			p := a.peak.Load()
			if n <= p || a.peak.CompareAndSwap(p, n) {
				break
			}
		}
		defer a.inflight.Add(-1)
		next.ServeHTTP(w, r)
	})
}

// Load returns the load since the previous call, from 0 (idle) to 1.
func (a *Activity) Load(context.Context) float64 {
	n := max(a.peak.Swap(a.inflight.Load()), a.inflight.Load())
	return min(float64(n)/float64(a.capacity), 1)
}
