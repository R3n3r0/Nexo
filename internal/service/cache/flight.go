package cache

import "sync"

// flightJoins bounds how many calls of a key a caller waits for before it
// runs its own (the calls it found were outdated by writes).
const flightJoins = 2

// flightGroup collapses the concurrent fills of a key into one origin
// read (docs/11 §11.3 "Request collapsing").
//
// A call is generation-aware: it records the invalidation barrier of the
// store taken when it began, and a caller reuses its result only while
// the object was not invalidated since then, both when it joins and when
// the result arrives. A request that begins after a write (the 200 of a
// PUT, which runs the invalidation first) therefore never receives the
// content a call begun before the write read (TCC-05).
type flightGroup struct {
	st *store
	mu sync.Mutex
	m  map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	val  piece
	err  error
	// ref is the object the call reads, since the barrier taken when it
	// began.
	ref   objRef
	since uint64
}

// current reports a call whose object was not invalidated since it
// began.
func (g *flightGroup) current(c *flightCall) bool {
	return !g.st.invalidated(c.ref, c.since)
}

// do runs fn once for the callers of key (an object ref) that overlap;
// shared reports a caller that received another one's result.
func (g *flightGroup) do(key string, ref objRef, fn func() (piece, error)) (piece, error, bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]*flightCall{}
	}
	for range flightJoins {
		c, ok := g.m[key]
		if !ok || !g.current(c) {
			break
		}
		g.mu.Unlock()
		<-c.done
		if g.current(c) {
			return c.val, c.err, true
		}
		// The object changed while the call ran: its result may predate
		// the write.
		g.mu.Lock()
	}
	// A call outdated by a write is replaced: the later callers join this
	// one.
	c := &flightCall{done: make(chan struct{}), ref: ref, since: g.st.begin()}
	g.m[key] = c
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		if g.m[key] == c {
			delete(g.m, key)
		}
		g.mu.Unlock()
		g.st.end(c.since)
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
