package cache_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cache"
	cachemock "github.com/r3n3r0/nexo/internal/service/cache/mock"
)

// link is the network between two nodes of a test cluster: up, or cut;
// drop loses the invalidations only (an asymmetric fault).
type link struct {
	up   atomic.Bool
	drop atomic.Bool
	// stall makes the owner of a key hang on its pieces (a paused node).
	stall atomic.Bool
}

// testCluster is a cluster of cache services on one origin, connected by
// peer mocks that call the other services (the internode RPC).
type testCluster struct {
	nodes []*env
	// links[i][j] is the link from node i to node j.
	links [][]*link
}

// testLease is the trust lease of the test clusters: their fake clock
// stands still, so the lease only lapses when a test advances it; it also
// bounds the wait of a write that misses a peer.
const testLease = 50 * time.Millisecond

// newCluster starts n nodes in mode.
func newCluster(t *testing.T, n int, mode cache.Mode, opts ...cache.Option) *testCluster {
	t.Helper()
	c := &testCluster{nodes: make([]*env, n), links: make([][]*link, n)}
	for i := range n {
		c.links[i] = make([]*link, n)
		for j := range n {
			// Down until every node exists: a node starting calls its
			// peers.
			c.links[i][j] = &link{}
		}
	}
	shared := newEnv(t, envOptions{})
	for i := range n {
		var peers []cache.Peer
		for j := range n {
			if j != i {
				peers = append(peers, c.peer(t, i, j))
			}
		}
		c.nodes[i] = newEnv(t, envOptions{
			node: fmt.Sprintf("n%d", i+1), peers: peers, docs: shared.docs, origin: shared.origin, dists: shared.dists, clk: shared.clk,
			opts: append([]cache.Option{cache.WithMode(mode), cache.WithTrustLease(testLease)}, opts...),
		})
	}
	for i := range n {
		for j := range n {
			c.links[i][j].up.Store(true)
		}
	}
	for _, nd := range c.nodes {
		cache.SyncPeers(nd.svc)
	}
	return c
}

// peer is the mock of node j seen from node i.
func (c *testCluster) peer(t *testing.T, i, j int) cache.Peer {
	m := cachemock.NewPeer(t)
	l := c.links[i][j]
	offline := func() error {
		if !l.up.Load() {
			return fmt.Errorf("n%d: %w", j+1, domain.ErrNodeOffline)
		}
		return nil
	}
	target := func() cache.Service { return c.nodes[j].svc }
	m.EXPECT().NodeID().Return(fmt.Sprintf("n%d", j+1)).Maybe()
	m.EXPECT().IsOnline().RunAndReturn(l.up.Load).Maybe()
	m.EXPECT().ApplyInvalidations(tmock.Anything, tmock.Anything).RunAndReturn(func(ctx context.Context, b cache.InvalidationBatch) (cache.InvalidationAck, error) {
		if err := offline(); err != nil {
			return cache.InvalidationAck{}, err
		}
		if l.drop.Load() {
			return cache.InvalidationAck{}, fmt.Errorf("n%d: %w", j+1, domain.ErrNodeOffline)
		}
		return target().ApplyInvalidations(ctx, b)
	}).Maybe()
	m.EXPECT().SyncStatus(tmock.Anything, tmock.Anything).RunAndReturn(func(ctx context.Context, req cache.SyncRequest) (cache.SyncStatus, error) {
		if err := offline(); err != nil {
			return cache.SyncStatus{}, err
		}
		return target().SyncStatus(ctx, req)
	}).Maybe()
	m.EXPECT().FetchPiece(tmock.Anything, tmock.Anything).RunAndReturn(func(ctx context.Context, req cache.PieceRequest) (cache.PieceReply, error) {
		if err := offline(); err != nil {
			return cache.PieceReply{}, err
		}
		if l.stall.Load() {
			<-ctx.Done()
			return cache.PieceReply{}, ctx.Err()
		}
		return target().FetchPiece(ctx, req)
	}).Maybe()
	m.EXPECT().Presence(tmock.Anything, tmock.Anything).RunAndReturn(func(ctx context.Context, req cache.PresenceRequest) (cache.PresenceReply, error) {
		if err := offline(); err != nil {
			return cache.PresenceReply{}, err
		}
		return target().Presence(ctx, req)
	}).Maybe()
	m.EXPECT().NodeStats(tmock.Anything, tmock.Anything).RunAndReturn(func(ctx context.Context, q cache.StatsQuery) (cache.NodeStats, error) {
		if err := offline(); err != nil {
			return cache.NodeStats{}, err
		}
		return target().NodeStats(ctx, q)
	}).Maybe()
	m.EXPECT().StartWarm(tmock.Anything, tmock.Anything).RunAndReturn(func(ctx context.Context, job cache.WarmJob) error {
		if err := offline(); err != nil {
			return err
		}
		return target().StartWarm(ctx, job)
	}).Maybe()
	return m
}

// isolate cuts (or restores) every link of node i, both ways.
func (c *testCluster) isolate(i int, cut bool) {
	for j := range c.nodes {
		if j != i {
			c.links[i][j].up.Store(!cut)
			c.links[j][i].up.Store(!cut)
		}
	}
}

// TestClusterInvalidation is TCC-C-03 in the local mode: a write on node 1
// drops the entries of every node before it is acknowledged.
func TestClusterInvalidation(t *testing.T) {
	c := newCluster(t, 3, cache.ModeLocal)
	n1 := c.nodes[0]
	n1.origin.put("web", "index.html", []byte("v1"))
	for _, nd := range c.nodes {
		nd.get("www.example.com", "/index.html")
		require.Equal(t, "HIT", nd.get("www.example.com", "/index.html").xcache(), nd.node)
	}
	n1.origin.put("web", "index.html", []byte("v2"))
	require.NoError(t, n1.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "index.html"}))
	for _, nd := range c.nodes {
		r := nd.get("www.example.com", "/index.html")
		assert.Equal(t, "v2", string(r.body), nd.node)
	}
	// A manual invalidation reports every node.
	inv, err := n1.svc.Invalidate(context.Background(), cache.InvalidationRequest{Distribution: "web", All: true})
	require.NoError(t, err)
	assert.Equal(t, cache.InvalidationCompleted, inv.State)
	require.Len(t, inv.Nodes, 3)
	for _, ns := range inv.Nodes {
		assert.Equal(t, cache.NodeCompleted, ns.State, ns.Node)
	}
	for _, nd := range c.nodes {
		assert.Equal(t, "MISS", nd.get("www.example.com", "/index.html").xcache(), nd.node)
	}
}

// TestClusterIsolatedNode is TCC-C-04: a node cut off during an
// invalidation never serves the old content: its entries are revalidated
// while a peer is unreachable, the invalidations it missed are delivered
// when it comes back, and a node that lost some (overflow, or a gap seen
// by the comparison of the sequences) drops its cache.
func TestClusterIsolatedNode(t *testing.T) {
	c := newCluster(t, 3, cache.ModeLocal, cache.WithMaxPending(2))
	n1, n3 := c.nodes[0], c.nodes[2]
	n1.origin.put("web", "a.html", []byte("v1"))
	n1.origin.put("web", "b.html", []byte("b1"))
	for _, nd := range c.nodes {
		nd.get("www.example.com", "/a.html")
		nd.get("www.example.com", "/b.html")
	}
	require.True(t, cache.Trusted(n3.svc))
	c.isolate(2, true)
	n1.origin.put("web", "a.html", []byte("v2"))
	inv, err := n1.svc.Invalidate(context.Background(), cache.InvalidationRequest{Distribution: "web", Paths: []string{"/a.html"}})
	require.NoError(t, err)
	assert.Equal(t, cache.InvalidationInProgress, inv.State, "node 3 did not apply it")
	// Isolated, node 3 no longer trusts its entries: revalidated with
	// the origin, never the old content.
	assert.False(t, cache.Trusted(n3.svc))
	r := n3.get("www.example.com", "/a.html")
	assert.Equal(t, "v2", string(r.body))
	assert.Equal(t, "EXPIRED", r.xcache())
	// Back: the pending invalidation is delivered and recorded.
	c.isolate(2, false)
	cache.RetryLinks(n1.svc)
	cache.SyncPeers(n3.svc)
	require.True(t, cache.Trusted(n3.svc))
	got, err := n1.svc.GetInvalidation(context.Background(), "web", inv.ID)
	require.NoError(t, err)
	assert.Equal(t, cache.InvalidationCompleted, got.State)
	st, err := n3.svc.NodeStats(context.Background(), cache.StatsQuery{})
	require.NoError(t, err)
	assert.Zero(t, st.Flushes, "nothing lost: no flush")

	// Overflow: more invalidations than the queue holds collapse into a
	// flush of the node.
	c.isolate(2, true)
	for i := range 4 {
		err := n1.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: fmt.Sprintf("x%d", i)})
		require.ErrorIs(t, err, domain.ErrNodeOffline, "reported, the write is not failed by the hook")
	}
	c.isolate(2, false)
	cache.RetryLinks(n1.svc)
	st, err = n3.svc.NodeStats(context.Background(), cache.StatsQuery{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), st.Flushes)
	assert.Zero(t, cache.Entries(n3.svc))

	// Lost: the invalidations never reach node 3 (asymmetric fault)
	// while it trusts its peers. The first comparison of the sequences
	// finds node 1 ahead: node 3 stops trusting its entries at once (no
	// old content), and still behind at the second one it drops its cache.
	n3.get("www.example.com", "/b.html")
	require.Equal(t, "HIT", n3.get("www.example.com", "/b.html").xcache())
	c.links[0][2].drop.Store(true)
	n1.origin.put("web", "b.html", []byte("b2"))
	require.ErrorIs(t, n1.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "b.html"}), domain.ErrNodeOffline)
	c.links[0][2].drop.Store(false)
	// Forget the pending message: it is lost (the sender crashed).
	cache.SyncPeers(n3.svc)
	assert.False(t, cache.Trusted(n3.svc), "node 1 is ahead")
	r = n3.get("www.example.com", "/b.html")
	assert.Equal(t, "b2", string(r.body), "no old content after the first comparison")
	assert.NotEqual(t, "HIT", r.xcache())
	// A retry had its chance (the retry interval of the tests is an
	// hour): the message is lost.
	n3.clk.Advance(2 * time.Hour)
	cache.SyncPeers(n3.svc)
	st, err = n3.svc.NodeStats(context.Background(), cache.StatsQuery{})
	require.NoError(t, err)
	assert.Equal(t, int64(2), st.Flushes, "the lost invalidation drops the cache")
	assert.True(t, cache.Trusted(n3.svc))
	assert.Equal(t, "b2", string(n3.get("www.example.com", "/b.html").body))
}

// TestClusterTrustLease is TCC-C-04 for a node that does not notice it
// was cut off (a paused process, a one-way partition): the write that
// misses it waits for the trust lease of the node to lapse before it is
// acknowledged, and the node, whose lease expired meanwhile, revalidates
// its entries instead of serving the old content; the trust comes back
// once the missed invalidation is applied.
func TestClusterTrustLease(t *testing.T) {
	c := newCluster(t, 3, cache.ModeLocal)
	n1, n3 := c.nodes[0], c.nodes[2]
	n1.origin.put("web", "news.html", []byte("OLD CONTENT"))
	n3.get("www.example.com", "/news.html")
	require.Equal(t, "HIT", n3.get("www.example.com", "/news.html").xcache())

	// The invalidations to node 3 are lost; node 3 still sees its peers
	// online.
	c.links[0][2].drop.Store(true)
	n1.origin.put("web", "news.html", []byte("NEW CONTENT"))
	start := time.Now()
	err := n1.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "news.html"})
	require.ErrorIs(t, err, domain.ErrNodeOffline)
	assert.GreaterOrEqual(t, time.Since(start), testLease, "the write waits for the lease of node 3 to lapse")
	// The time the write waited, on the clock of the nodes.
	n1.clk.Advance(testLease)
	assert.False(t, cache.Trusted(n3.svc), "lease expired")
	r := n3.get("www.example.com", "/news.html")
	assert.Equal(t, "NEW CONTENT", string(r.body))
	assert.Equal(t, "EXPIRED", r.xcache())

	// A comparison finds node 1 ahead: not trusted until the message is
	// applied, then trusted again without a flush.
	cache.SyncPeers(n3.svc)
	assert.False(t, cache.Trusted(n3.svc))
	c.links[0][2].drop.Store(false)
	cache.RetryLinks(n1.svc)
	assert.True(t, cache.Trusted(n3.svc), "caught up within the lease of the last comparison")
	assert.Equal(t, "NEW CONTENT", string(n3.get("www.example.com", "/news.html").body))
	assert.Equal(t, "HIT", n3.get("www.example.com", "/news.html").xcache())
	st, err := n3.svc.NodeStats(context.Background(), cache.StatsQuery{})
	require.NoError(t, err)
	assert.Zero(t, st.Flushes)

	// A write that reaches every peer does not wait.
	start = time.Now()
	require.NoError(t, n1.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "other.html"}))
	assert.Less(t, time.Since(start), testLease)
}

// TestClusterSharedMode is TCC-C-01 and TCC-C-02: in the shared mode a key
// has one owner; the other nodes ask it, the cluster keeps one copy; an
// owner that goes away is skipped without errors.
func TestClusterSharedMode(t *testing.T) {
	c := newCluster(t, 4, cache.ModeShared)
	data := payload(32<<10, 4)
	c.nodes[0].origin.put("web", "video.mp4", data, textInfo("video/mp4"))
	var results []string
	for _, nd := range c.nodes {
		r := nd.get("www.example.com", "/video.mp4")
		require.NoError(t, r.err)
		require.Equal(t, data, r.body)
		results = append(results, r.xcache())
	}
	misses := 0
	for _, x := range results {
		if x == "MISS" {
			misses++
		}
	}
	assert.Equal(t, 1, misses, "one MISS, then HIT: %v", results)
	assert.Equal(t, int64(1), c.nodes[0].origin.gets.Load(), "one read of the origin")
	copies := 0
	for _, nd := range c.nodes {
		copies += cache.Entries(nd.svc)
	}
	assert.Equal(t, 1, copies, "one copy in the cluster")
	// The inspector names the owner.
	res, err := c.nodes[1].svc.Inspect(context.Background(), cache.InspectInput{URL: "http://www.example.com/video.mp4"})
	require.NoError(t, err)
	owners, present := 0, 0
	for _, np := range res.Nodes {
		if np.Owner {
			owners++
		}
		if np.Entry.Present {
			present++
		}
	}
	assert.Equal(t, 1, owners)
	assert.Equal(t, 1, present)
	// The owner stops: no error, the ring is recomputed.
	owner := -1
	for i, nd := range c.nodes {
		if cache.Entries(nd.svc) == 1 {
			owner = i
		}
	}
	require.GreaterOrEqual(t, owner, 0)
	c.isolate(owner, true)
	for i, nd := range c.nodes {
		if i == owner {
			continue
		}
		r := nd.get("www.example.com", "/video.mp4")
		require.NoError(t, r.err, nd.node)
		assert.Equal(t, data, r.body)
	}
	// Writes reach every node, the owners included.
	c.isolate(owner, false)
	data2 := payload(32<<10, 8)
	c.nodes[0].origin.put("web", "video.mp4", data2, textInfo("video/mp4"))
	require.NoError(t, c.nodes[2].svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "video.mp4"}))
	for _, nd := range c.nodes {
		assert.Equal(t, data2, nd.get("www.example.com", "/video.mp4").body, nd.node)
	}
}

// TestClusterSharedStalledOwner is TCC-C-02 with an owner that stalls
// (a paused process) instead of going offline: its piece is waited for a
// bounded time, then read from the origin without caching.
func TestClusterSharedStalledOwner(t *testing.T) {
	const fetchTimeout = 100 * time.Millisecond
	c := newCluster(t, 2, cache.ModeShared, cache.WithFetchTimeout(fetchTimeout))
	n1 := c.nodes[0]
	// A path owned by node 2.
	path := ""
	for i := range 64 {
		p := fmt.Sprintf("/f%d.txt", i)
		if cache.Owner(n1.svc, cache.KeyOf(ptr(cache.ParamsOf(cache.PolicySpec{})), "h:www.example.com", "web", p[1:], nil, "")) == "n2" {
			path = p
			break
		}
	}
	require.NotEmpty(t, path)
	n1.origin.put("web", path[1:], []byte("content"))
	c.links[0][1].stall.Store(true)
	start := time.Now()
	r := n1.get("www.example.com", path)
	elapsed := time.Since(start)
	require.NoError(t, r.err)
	assert.Equal(t, "content", string(r.body))
	assert.Less(t, elapsed, 5*time.Second, "bounded by the fetch timeout, not the RPC timeouts")
	assert.GreaterOrEqual(t, elapsed, fetchTimeout)
	assert.Zero(t, cache.Entries(n1.svc), "read from the origin without caching")
}

// TestClusterSharedLocal keeps the hot objects on the node that serves
// them too.
func TestClusterSharedLocal(t *testing.T) {
	c := newCluster(t, 3, cache.ModeSharedLocal)
	c.nodes[0].origin.put("web", "hot.js", []byte("hot"))
	for range 3 {
		for _, nd := range c.nodes {
			require.NoError(t, nd.get("www.example.com", "/hot.js").err)
		}
	}
	assert.Equal(t, int64(1), c.nodes[0].origin.gets.Load())
	for _, nd := range c.nodes {
		assert.Equal(t, "HIT", nd.get("www.example.com", "/hot.js").xcache(), nd.node)
	}
	c.nodes[0].origin.put("web", "hot.js", []byte("new"))
	require.NoError(t, c.nodes[1].svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "hot.js"}))
	for _, nd := range c.nodes {
		assert.Equal(t, "new", string(nd.get("www.example.com", "/hot.js").body), nd.node)
	}
}

// TestClusterStats sums the statistics of the nodes.
func TestClusterStats(t *testing.T) {
	c := newCluster(t, 3, cache.ModeLocal)
	c.nodes[0].origin.put("web", "a", []byte("a"))
	var wg sync.WaitGroup
	for _, nd := range c.nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nd.get("www.example.com", "/a")
			nd.get("www.example.com", "/a")
		}()
	}
	wg.Wait()
	rep, err := c.nodes[0].svc.Stats(context.Background(), cache.StatsQuery{Distribution: "web"})
	require.NoError(t, err)
	require.Len(t, rep.Nodes, 3)
	assert.Equal(t, int64(3), rep.Requests[cache.ResultMiss])
	assert.Equal(t, int64(3), rep.Requests[cache.ResultHit])
	assert.InDelta(t, 0.5, rep.HitRatio, 0.001)
	require.Len(t, rep.Hosts, 1)
	assert.Equal(t, "www.example.com", rep.Hosts[0].Host)
	assert.Equal(t, 3, rep.Hosts[0].Entries)
	require.NotEmpty(t, rep.TopObjects)
	assert.Equal(t, "/a", rep.TopObjects[0].Path)
	c.isolate(2, true)
	rep, err = c.nodes[0].svc.Stats(context.Background(), cache.StatsQuery{})
	require.NoError(t, err)
	assert.NotEmpty(t, rep.Nodes[2].Error)
}
