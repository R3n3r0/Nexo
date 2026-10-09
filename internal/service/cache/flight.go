package cache

import "sync"

// flightGroup collapses the concurrent fills of a key into one origin
// read (docs/11 §11.3 "Request collapsing").
type flightGroup struct {
	mu sync.Mutex
	m  map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	val  piece
	err  error
}

// do runs fn once for the callers of key that overlap; shared reports a
// caller that waited for another one's call.
func (g *flightGroup) do(key string, fn func() (piece, error)) (piece, error, bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]*flightCall{}
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.val, c.err, true
	}
	c := &flightCall{done: make(chan struct{})}
	g.m[key] = c
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.m, key)
		g.mu.Unlock()
		close(c.done)
	}()
	c.val, c.err = fn()
	return c.val, c.err, false
}

// inFlight reports a call in progress for key.
func (g *flightGroup) inFlight(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.m[key]
	return ok
}
