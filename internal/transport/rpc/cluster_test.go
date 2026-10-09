package rpc_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/transport/rpc/rpctest"
)

// bootstrapAll bootstraps every node concurrently and returns their
// results.
func bootstrapAll(t *testing.T, ctx context.Context, c *rpctest.Cluster, nodes ...int) []cluster.BootstrapResult {
	t.Helper()
	if len(nodes) == 0 {
		for i := range c.Nodes {
			nodes = append(nodes, i)
		}
	}
	results := make([]cluster.BootstrapResult, len(c.Nodes))
	errs := make([]error, len(c.Nodes))
	var wg sync.WaitGroup
	for _, i := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = c.Nodes[i].Bootstrap(ctx)
		}()
	}
	wg.Wait()
	for _, i := range nodes {
		require.NoError(t, errs[i], "node %d", i+1)
	}
	return results
}

// TestClusterBootstrapRandomOrder is TC-01: the nodes start in random order
// with random delays; every node completes the bootstrap with the same
// deployment, formatted once by the node of the first endpoint.
func TestClusterBootstrapRandomOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := rpctest.New(t, rpctest.Options{})
	order := rand.Perm(len(c.Nodes))
	results := make([]cluster.BootstrapResult, len(c.Nodes))
	errs := make([]error, len(c.Nodes))
	var wg sync.WaitGroup
	for k, i := range order {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(k)*150*time.Millisecond + time.Duration(rand.IntN(100))*time.Millisecond)
			c.Nodes[i].Start()
			results[i], errs[i] = c.Nodes[i].Bootstrap(ctx)
		}()
	}
	wg.Wait()
	for i := range c.Nodes {
		require.NoError(t, errs[i], "node %d (start order %v)", i+1, order)
	}
	id := results[0].DeploymentID
	require.NotEmpty(t, id)
	for i, res := range results {
		assert.Equal(t, id, res.DeploymentID, "node %d", i+1)
		// The deployment is formatted as soon as a write quorum of drives
		// is reachable: the drives of a node started later join it as
		// replacements, marked for healing.
		for _, ep := range res.Healing {
			assert.Equal(t, c.Nodes[i].ID, ep.Host, "node %d heals its own drives only", i+1)
		}
	}
	// Every drive carries the format of the deployment, read through any
	// node (once its connections closed the circuits opened while the
	// other nodes were starting).
	for _, d := range c.Nodes[2].Drives {
		var data []byte
		require.Eventually(t, func() bool {
			var err error
			data, err = d.ReadAll(ctx, domain.SystemBucket, storage.FormatFile)
			return err == nil
		}, 10*time.Second, 10*time.Millisecond, d.Endpoint().String())
		assert.Contains(t, string(data), id)
	}
	cfg, err := c.Nodes[1].Bootstrapper.LocalConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, id, cfg.DeploymentID)
}

// TestClusterTopologyMismatch is TC-02: a node started with other server
// arguments refuses to start with a clear error; the other nodes form the
// deployment without it.
func TestClusterTopologyMismatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := rpctest.New(t, rpctest.Options{ArgsOf: func(n int, args []string) []string {
		if n == 3 { // node 4 is started with other drive directories
			for i, a := range args {
				args[i] = strings.Replace(a, "/n", "/other-n", 1)
			}
		}
		return args
	}})
	c.StartAll()
	_, err := c.Nodes[3].Bootstrap(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, cluster.ErrTopologyMismatch)
	assert.Contains(t, err.Error(), "start every node with the same server arguments")

	results := bootstrapAll(t, ctx, c, 0, 1, 2)
	assert.Equal(t, results[0].DeploymentID, results[1].DeploymentID)
	assert.Equal(t, results[0].DeploymentID, results[2].DeploymentID)
	assert.Len(t, results[0].Offline, 2, "the drives of node 4 are not part of the deployment")
}

// TestClusterInvalidationPropagates is TC-04 at the notifier level: an
// identity changed on node 2 is invalidated on every node when the change
// returns; a node that was down receives what it missed.
func TestClusterInvalidationPropagates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := rpctest.New(t, rpctest.Options{})
	c.StartAll()
	bootstrapAll(t, ctx, c)
	for _, n := range c.Nodes {
		require.NoError(t, n.Notifier.Start(ctx))
	}
	received := make([]atomic.Int32, len(c.Nodes))
	names := make([][]string, len(c.Nodes))
	var mu sync.Mutex
	for i, n := range c.Nodes {
		n.Notifier.Subscribe(cluster.EventIAM, cluster.HandlerFunc(func(_ context.Context, e cluster.Event) error {
			received[i].Add(1)
			mu.Lock()
			names[i] = append(names[i], e.Name+"@"+e.Origin)
			mu.Unlock()
			return nil
		}))
	}

	require.NoError(t, c.Nodes[1].Notifier.Broadcast(ctx, cluster.Event{Kind: cluster.EventIAM, Name: "alice"}))
	for i := range c.Nodes {
		want := int32(1)
		if i == 1 {
			want = 0 // the origin holds the new state already
		}
		assert.Equal(t, want, received[i].Load(), "node %d is up to date when the broadcast returns", i+1)
	}
	assert.Equal(t, []string{"alice@" + c.Nodes[1].ID}, names[0])

	// Node 4 is down while a policy is attached: it gets the event when
	// it is back.
	c.Nodes[3].Crash()
	err := c.Nodes[1].Notifier.Broadcast(ctx, cluster.Event{Kind: cluster.EventIAM, Name: "bob"})
	assert.ErrorIs(t, err, domain.ErrNodeOffline)
	assert.Equal(t, int32(2), received[0].Load())
	c.Nodes[3].Start()
	require.Eventually(t, func() bool { return received[3].Load() == 2 }, 10*time.Second, 10*time.Millisecond)
}

// TestClusterLeader is TC-14: exactly one node runs the singleton jobs;
// when it dies another takes over after the lease of its lock.
func TestClusterLeader(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := rpctest.New(t, rpctest.Options{LockStaleAfter: 600 * time.Millisecond, LockRefresh: 100 * time.Millisecond})
	c.StartAll()
	bootstrapAll(t, ctx, c)
	var running atomic.Int32
	var maxRunning atomic.Int32
	jobs := make([]atomic.Int32, len(c.Nodes))
	for i, n := range c.Nodes {
		n.Leader.Subscribe(cluster.LeaderFuncs{
			Elected: func(ctx context.Context) {
				jobs[i].Add(1)
				r := running.Add(1)
				for cur := maxRunning.Load(); r > cur && !maxRunning.CompareAndSwap(cur, r); cur = maxRunning.Load() {
				}
				go func() { <-ctx.Done(); running.Add(-1) }()
			},
		})
		require.NoError(t, n.Leader.Start(ctx))
	}
	leader := func() []int {
		var out []int
		for i, n := range c.Nodes {
			if n.Leader.IsLeader() {
				out = append(out, i)
			}
		}
		return out
	}
	require.Eventually(t, func() bool { return len(leader()) == 1 }, 10*time.Second, 10*time.Millisecond)
	first := leader()[0]
	for range 30 {
		require.LessOrEqual(t, len(leader()), 1, "never two leaders")
		time.Sleep(10 * time.Millisecond)
	}

	// The leader dies: another node leads after the lease.
	c.Nodes[first].Crash()
	require.Eventually(t, func() bool {
		l := leader()
		return len(l) == 1 && l[0] != first
	}, 15*time.Second, 10*time.Millisecond, "failover")
	assert.Equal(t, int32(1), maxRunning.Load(), "the singleton job never ran twice at once")
}

// TestClusterLockWithDeadNode is TC-09: a node crashes while holding a write
// lock; the lock expires and the other nodes write again; meanwhile the
// other locks keep working on the quorum of the 3 remaining nodes.
func TestClusterLockWithDeadNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := rpctest.New(t, rpctest.Options{LockStaleAfter: 800 * time.Millisecond})
	c.StartAll()
	bootstrapAll(t, ctx, c)

	held, err := c.Nodes[0].Locker.NewNSLock(ctx, "bucket", "object").GetLock(ctx, 5*time.Second)
	require.NoError(t, err)
	_, err = c.Nodes[1].Locker.NewNSLock(ctx, "bucket", "object").GetLock(ctx, 100*time.Millisecond)
	require.ErrorIs(t, err, domain.ErrLockTimeout, "held by node 1")
	list, err := c.Nodes[2].LockManager.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "bucket/object", list[0].Resource)
	assert.Equal(t, c.Nodes[0].ID, list[0].Owner)

	c.Nodes[0].Crash()
	start := time.Now()
	// Other objects are locked on the quorum of the remaining nodes.
	lc, err := c.Nodes[2].Locker.NewNSLock(ctx, "bucket", "other").GetLock(ctx, 5*time.Second)
	require.NoError(t, err)
	lc.Unlock(ctx)
	// The orphan lock expires and the writers resume.
	lc, err = c.Nodes[1].Locker.NewNSLock(ctx, "bucket", "object").GetLock(ctx, 10*time.Second)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
	lc.Unlock(ctx)
	<-held.Context().Done() // the crashed holder learnt it lost the lock
	held.Unlock(ctx)
}

// TestClusterPartition: with the nodes split 2+2 no side can take a write
// lock (no split brain); reads keep working on both sides.
func TestClusterPartition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := rpctest.New(t, rpctest.Options{})
	c.StartAll()
	bootstrapAll(t, ctx, c)
	c.Partition([]int{0, 1}, []int{2, 3})
	for _, i := range []int{0, 2} {
		_, err := c.Nodes[i].Locker.NewNSLock(ctx, "b", "k").GetLock(ctx, 300*time.Millisecond)
		assert.ErrorIs(t, err, domain.ErrLockTimeout, "node %d cannot write alone", i+1)
		lc, err := c.Nodes[i].Locker.NewNSLock(ctx, "b", "k").GetRLock(ctx, 2*time.Second)
		require.NoError(t, err, "node %d reads", i+1)
		lc.Unlock(ctx)
	}
	c.Heal()
	require.Eventually(t, func() bool {
		lc, err := c.Nodes[0].Locker.NewNSLock(ctx, "b", "k").GetLock(ctx, 500*time.Millisecond)
		if err != nil {
			return false
		}
		lc.Unlock(ctx)
		return true
	}, 10*time.Second, 50*time.Millisecond, "writes resume once the partition heals")
}

// TestClusterLatency: with 20 ms on every internode call the bootstrap,
// the locks and the remote drives keep working within their timeouts.
func TestClusterLatency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := rpctest.New(t, rpctest.Options{Latency: 20 * time.Millisecond})
	c.StartAll()
	bootstrapAll(t, ctx, c)
	start := time.Now()
	lc, err := c.Nodes[1].Locker.NewNSLock(ctx, "b", "k").GetLock(ctx, 5*time.Second)
	require.NoError(t, err)
	lc.Unlock(ctx)
	assert.Less(t, time.Since(start), 2*time.Second)

	// A drive of node 3 written and read from node 1.
	remote := c.Nodes[0].Drives[0]
	for _, d := range c.Nodes[0].Drives {
		if d.Endpoint().Host == c.Nodes[2].ID {
			remote = d
			break
		}
	}
	require.NoError(t, remote.MakeVol(ctx, "photos"))
	require.NoError(t, remote.WriteMetadata(ctx, "photos", "a.jpg", storage.FileInfo{ModTime: time.Now(), Size: 3, Data: []byte("abc")}))
	fi, err := c.Nodes[2].LocalDrives[0].ReadVersion(ctx, "photos", "a.jpg", "", storage.ReadOptions{ReadData: true})
	if err != nil { // the drive picked may be the second local drive of node 3
		fi, err = c.Nodes[2].LocalDrives[1].ReadVersion(ctx, "photos", "a.jpg", "", storage.ReadOptions{ReadData: true})
	}
	require.NoError(t, err)
	assert.Equal(t, "abc", string(fi.Data))
}

// TestClusterServerInfo: the aggregated server info reports every node and
// drive, and the drives of a dead node as offline; a node with a skewed
// clock is reported (TC-13).
func TestClusterServerInfo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	skewed := clock.NewFake(time.Now().Add(5 * time.Second))
	c := rpctest.New(t, rpctest.Options{Clocks: map[int]clock.Clock{2: skewed}})
	c.StartAll()
	bootstrapAll(t, ctx, c)

	nodes, err := c.Nodes[0].Info.Nodes(ctx)
	require.NoError(t, err)
	require.Len(t, nodes, 4)
	for i, n := range nodes {
		assert.Equal(t, c.Nodes[i].ID, n.Endpoint)
		assert.Equal(t, domain.StateOnline, n.State)
		assert.Equal(t, "v1.0.0", n.Version)
		require.Len(t, n.Disks, 2)
		for _, d := range n.Disks {
			assert.Equal(t, domain.StateOnline, d.State, d.Endpoint)
			assert.NotEmpty(t, d.ID, "formatted drive")
		}
	}

	skews, err := c.Nodes[0].Info.ClockSkew(ctx)
	require.NoError(t, err)
	for _, s := range skews {
		assert.Equal(t, s.NodeID == c.Nodes[2].ID, s.Exceeded, "%s offset %s", s.NodeID, s.Offset)
	}

	c.Nodes[3].Crash()
	nodes, err = c.Nodes[0].Info.Nodes(ctx)
	require.NoError(t, err)
	assert.Equal(t, domain.StateOffline, nodes[3].State)
	require.Len(t, nodes[3].Disks, 2)
	assert.Equal(t, domain.StateOffline, nodes[3].Disks[0].State)
}

// TestClusterRPCOnTheS3Port: the RPC server answers below its prefix and
// passes every other request to the S3 API of the same port.
func TestClusterRPCOnTheS3Port(t *testing.T) {
	c := rpctest.New(t, rpctest.Options{Nodes: 2})
	c.StartAll()
	resp, err := http.Get(fmt.Sprintf("http://%s/bucket/key", c.Nodes[0].ID))
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, rpctest.S3Placeholder, string(body))
	resp, err = http.Post(fmt.Sprintf("http://%s/nexo/rpc/v1/peer/ping", c.Nodes[0].ID), "application/x-msgpack", nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode, "the RPC is HTTP/2 only")
	h2c := &http.Client{Transport: &http2.Transport{AllowHTTP: true, DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}}
	resp, err = h2c.Post(fmt.Sprintf("http://%s/nexo/rpc/v1/peer/ping", c.Nodes[0].ID), "application/x-msgpack", nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "the RPC needs the internode token")
}
