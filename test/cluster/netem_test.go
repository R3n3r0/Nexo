//go:build cluster

package cluster_test

import (
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The links put a TCP proxy on every internode connection: bin/nexo,
// built with the test hooks, dials its peers through the address
// NEXO_TEST_DIAL_MAP gives (cmd/nexo/testhooks_nexotest.go). A link can
// be cut (a real partition: both sides keep running, docs/06 TC-07) or
// slowed (latency and lost segments, TC-08), with no Docker nor
// toxiproxy. The S3 traffic of the clients does not go through them.

// withLinks routes the internode connections of every node through the
// links of the cluster.
func withLinks() clusterOption {
	return func(c *cluster) { c.withLinks = true }
}

// startLinks starts a link for every ordered pair of nodes and gives
// every node its dial map.
func (c *cluster) startLinks() {
	n := len(c.nodes)
	c.links = make([][]*link, n)
	for i, from := range c.nodes {
		c.links[i] = make([]*link, n)
		var routes []string
		for j, to := range c.nodes {
			if i == j {
				continue
			}
			l := newLink(c.t, to.S3)
			c.links[i][j] = l
			routes = append(routes, to.S3+"="+l.addr())
		}
		from.env = append(from.env, "NEXO_TEST_DIAL_MAP="+strings.Join(routes, ","))
	}
}

// link returns the link of the connections of node i to node j (1-based).
func (c *cluster) link(i, j int) *link { return c.links[i-1][j-1] }

// partition cuts every link between the groups of nodes, both ways.
func (c *cluster) partition(groups ...[]int) {
	for a, ga := range groups {
		for b, gb := range groups {
			if a == b {
				continue
			}
			for _, i := range ga {
				for _, j := range gb {
					c.link(i, j).cut(true)
				}
			}
		}
	}
}

// heal restores every link and removes their delays.
func (c *cluster) heal() {
	for i, row := range c.links {
		for j, l := range row {
			if i != j {
				l.cut(false)
				l.slow(0, 0, 0)
			}
		}
	}
}

// slowNode delays every link from and to node i (TC-08).
func (c *cluster) slowNode(i int, latency time.Duration, loss float64, stall time.Duration) {
	for j := 1; j <= len(c.nodes); j++ {
		if j != i {
			c.link(i, j).slow(latency, loss, stall)
			c.link(j, i).slow(latency, loss, stall)
		}
	}
}

// link is a TCP proxy to one node. Cut, it closes its connections and
// holds the new ones open without carrying a byte (a black hole: the
// peers see timeouts, as in a real partition) until it is restored.
// Slowed, it delivers every chunk latency after it was read (a delay
// line: the throughput is kept), and with probability loss a chunk waits
// stall more, as a segment retransmitted after a loss would.
type link struct {
	t      *testing.T
	ln     net.Listener
	target string

	mu      sync.Mutex
	isCut   bool
	latency time.Duration
	loss    float64
	stall   time.Duration
	conns   map[net.Conn]struct{}
	held    map[net.Conn]struct{}
}

func newLink(t *testing.T, target string) *link {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	l := &link{t: t, ln: ln, target: target, conns: map[net.Conn]struct{}{}, held: map[net.Conn]struct{}{}}
	go l.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		l.mu.Lock()
		defer l.mu.Unlock()
		for c := range l.conns {
			_ = c.Close()
		}
		for c := range l.held {
			_ = c.Close()
		}
	})
	return l
}

func (l *link) addr() string { return l.ln.Addr().String() }

// cut cuts (or restores) the link.
func (l *link) cut(on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.isCut = on
	if on {
		for c := range l.conns {
			_ = c.Close()
		}
		return
	}
	for c := range l.held {
		_ = c.Close()
		delete(l.held, c)
	}
}

// slow sets the delays of the link (zero: none).
func (l *link) slow(latency time.Duration, loss float64, stall time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.latency, l.loss, l.stall = latency, loss, stall
}

// delay returns the delay of the next chunk.
func (l *link) delay() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	d := l.latency
	if l.loss > 0 && rand.Float64() < l.loss {
		d += l.stall
	}
	return d
}

func (l *link) serve() {
	for {
		in, err := l.ln.Accept()
		if err != nil {
			return
		}
		l.mu.Lock()
		if l.isCut {
			l.held[in] = struct{}{}
			l.mu.Unlock()
			continue
		}
		l.mu.Unlock()
		go l.carry(in)
	}
}

// carry proxies one connection to the target.
func (l *link) carry(in net.Conn) {
	out, err := net.DialTimeout("tcp", l.target, 5*time.Second)
	if err != nil {
		_ = in.Close()
		return
	}
	l.mu.Lock()
	if l.isCut {
		l.mu.Unlock()
		_ = in.Close()
		_ = out.Close()
		return
	}
	l.conns[in], l.conns[out] = struct{}{}, struct{}{}
	l.mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); l.pipe(out, in) }()
	go func() { defer wg.Done(); l.pipe(in, out) }()
	wg.Wait()
	l.mu.Lock()
	delete(l.conns, in)
	delete(l.conns, out)
	l.mu.Unlock()
}

// chunk is a piece of a stream and when it may be delivered.
type chunk struct {
	data []byte
	due  time.Time
}

// pipe copies src to dst through the delay line; when either side ends,
// both are closed.
func (l *link) pipe(dst, src net.Conn) {
	defer func() {
		_ = dst.Close()
		_ = src.Close()
	}()
	line := make(chan chunk, 256)
	go func() {
		defer close(line)
		buf := make([]byte, 64<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				line <- chunk{data: append([]byte(nil), buf[:n]...), due: time.Now().Add(l.delay())}
			}
			if err != nil {
				return
			}
		}
	}()
	var last time.Time
	for c := range line {
		// Delivered in order: a stalled chunk holds the next ones, as a
		// lost TCP segment does.
		if c.due.Before(last) {
			c.due = last
		}
		last = c.due
		if d := time.Until(c.due); d > 0 {
			time.Sleep(d)
		}
		if _, err := dst.Write(c.data); err != nil {
			// Drain the reader so that it ends with src.
			_ = src.Close()
			for range line {
			}
			return
		}
	}
}
