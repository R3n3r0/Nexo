package lock_test

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/lock"
	lockmock "github.com/r3n3r0/nexo/internal/service/lock/mock"
)

// link is the view one locker has of one lock server: the generated mock
// forwards to the real in-memory server unless the link is cut (partition,
// crashed node) or slowed down (latency injection).
type link struct {
	down    atomic.Bool
	latency atomic.Int64
}

func (l *link) guard(ctx context.Context) error {
	if d := time.Duration(l.latency.Load()); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if l.down.Load() {
		return fmt.Errorf("%w: link cut", domain.ErrNodeOffline)
	}
	return nil
}

func linkedServer(t *testing.T, real lock.Server, l *link) lock.Server {
	t.Helper()
	m := lockmock.NewServer(t)
	call := func(fn func(context.Context, lock.Args) (bool, error)) func(context.Context, lock.Args) (bool, error) {
		return func(ctx context.Context, a lock.Args) (bool, error) {
			if err := l.guard(ctx); err != nil {
				return false, err
			}
			return fn(ctx, a)
		}
	}
	m.EXPECT().Lock(mock.Anything, mock.Anything).RunAndReturn(call(real.Lock)).Maybe()
	m.EXPECT().RLock(mock.Anything, mock.Anything).RunAndReturn(call(real.RLock)).Maybe()
	m.EXPECT().Unlock(mock.Anything, mock.Anything).RunAndReturn(call(real.Unlock)).Maybe()
	m.EXPECT().RUnlock(mock.Anything, mock.Anything).RunAndReturn(call(real.RUnlock)).Maybe()
	m.EXPECT().Refresh(mock.Anything, mock.Anything).RunAndReturn(call(real.Refresh)).Maybe()
	m.EXPECT().ForceUnlock(mock.Anything, mock.Anything).RunAndReturn(call(real.ForceUnlock)).Maybe()
	m.EXPECT().List(mock.Anything).RunAndReturn(func(ctx context.Context) ([]lock.Info, error) {
		if err := l.guard(ctx); err != nil {
			return nil, err
		}
		return real.List(ctx)
	}).Maybe()
	return m
}

// cluster is a set of in-memory lock servers (one per node) and the lockers
// of the nodes, each reaching every server through its own links.
type cluster struct {
	servers []lock.Server
	clk     clock.Clock
}

func newCluster(t *testing.T, n int, clk clock.Clock, opts ...lock.ServerOption) *cluster {
	t.Helper()
	c := &cluster{clk: clk}
	for range n {
		c.servers = append(c.servers, lock.NewServer(lock.ServerDeps{Clock: clk, Logger: slog.New(slog.DiscardHandler)}, opts...))
	}
	return c
}

// locker returns a distributed locker over links to every server.
func (c *cluster) locker(t *testing.T, owner string, opts ...lock.DistributedOption) (lock.Locker, []*link) {
	t.Helper()
	links := make([]*link, len(c.servers))
	servers := make([]lock.Server, len(c.servers))
	for i, s := range c.servers {
		links[i] = &link{}
		servers[i] = linkedServer(t, s, links[i])
	}
	base := []lock.DistributedOption{
		lock.WithRetryInterval(5*time.Millisecond, 20*time.Millisecond),
		lock.WithCallTimeout(500 * time.Millisecond),
	}
	l, err := lock.NewDistributed(lock.DistributedDeps{Servers: servers, Owner: owner, Logger: slog.New(slog.DiscardHandler)},
		append(base, opts...)...)
	require.NoError(t, err)
	return l, links
}

func entries(t *testing.T, s lock.Server) []lock.Info {
	t.Helper()
	infos, err := s.List(context.Background())
	require.NoError(t, err)
	return infos
}

func TestQuorums(t *testing.T) {
	tests := []struct{ n, write, read int }{
		{1, 1, 1}, {2, 2, 1}, {3, 2, 2}, {4, 3, 2}, {5, 3, 3}, {6, 4, 3}, {8, 5, 4}, {16, 9, 8},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.n), func(t *testing.T) {
			assert.Equal(t, tt.write, lock.WriteQuorum(tt.n))
			assert.Equal(t, tt.read, lock.ReadQuorum(tt.n))
			// A writer and a reader can never both reach their quorum.
			assert.Greater(t, tt.write+tt.read, tt.n)
			assert.Greater(t, 2*tt.write, tt.n, "two writers can never both reach the quorum")
		})
	}
}

func TestNewDistributedValidation(t *testing.T) {
	_, err := lock.NewDistributed(lock.DistributedDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = lock.NewDistributed(lock.DistributedDeps{Servers: []lock.Server{nil}})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// covers: S-STO-07
func TestDistributedLockerSemantics(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, 4, nil)
	a, _ := c.locker(t, "a")
	b, _ := c.locker(t, "b")

	require.NoError(t, a.Lock(ctx, "b/o", time.Second))
	assert.ErrorIs(t, b.Lock(ctx, "b/o", 50*time.Millisecond), domain.ErrLockTimeout)
	assert.ErrorIs(t, b.RLock(ctx, "b/o", 50*time.Millisecond), domain.ErrLockTimeout)
	require.NoError(t, b.Lock(ctx, "b/other", time.Second), "resources are independent")
	require.NoError(t, b.Unlock(ctx, "b/other"))
	require.NoError(t, a.Unlock(ctx, "b/o"))
	for _, s := range c.servers {
		assert.Empty(t, entries(t, s), "the release reaches every server")
	}

	// Read locks are shared, across lockers and within one locker.
	require.NoError(t, a.RLock(ctx, "r", time.Second))
	require.NoError(t, a.RLock(ctx, "r", time.Second))
	require.NoError(t, b.RLock(ctx, "r", time.Second))
	assert.ErrorIs(t, b.Lock(ctx, "r", 50*time.Millisecond), domain.ErrLockTimeout)
	require.NoError(t, a.RUnlock(ctx, "r"))
	require.NoError(t, a.RUnlock(ctx, "r"))
	require.NoError(t, b.RUnlock(ctx, "r"))
	require.NoError(t, b.Lock(ctx, "r", time.Second))
	require.NoError(t, b.Unlock(ctx, "r"))

	// Releasing what is not held is an error, like the local locker.
	assert.ErrorIs(t, a.Unlock(ctx, "none"), domain.ErrInvalidArgument)
	assert.ErrorIs(t, a.RUnlock(ctx, "none"), domain.ErrInvalidArgument)

	// Contexts: already canceled, canceled while waiting.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	assert.ErrorIs(t, a.Lock(canceled, "c", time.Second), context.Canceled)
	require.NoError(t, a.Lock(ctx, "c", time.Second))
	waitCtx, cancelWait := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancelWait()
	assert.ErrorIs(t, b.Lock(waitCtx, "c", 0), context.DeadlineExceeded, "no timeout: wait for the context")
	require.NoError(t, a.Unlock(ctx, "c"))

	_, err := a.NewNSLock(ctx, "", "").GetLock(ctx, time.Second)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestDistributedOfflineServers checks the tolerance of the quorums: a
// write lock needs n/2+1 servers, a read lock n/2 when n is even.
func TestDistributedOfflineServers(t *testing.T) {
	tests := []struct {
		n, offline    int
		write, rwrite bool
	}{
		{n: 4, offline: 0, write: true, rwrite: true},
		{n: 4, offline: 1, write: true, rwrite: true},
		{n: 4, offline: 2, write: false, rwrite: true},
		{n: 4, offline: 3, write: false, rwrite: false},
		{n: 5, offline: 2, write: true, rwrite: true},
		{n: 5, offline: 3, write: false, rwrite: false},
		{n: 1, offline: 0, write: true, rwrite: true},
		{n: 1, offline: 1, write: false, rwrite: false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d servers %d offline", tt.n, tt.offline), func(t *testing.T) {
			ctx := context.Background()
			c := newCluster(t, tt.n, nil)
			l, links := c.locker(t, "a")
			for i := range tt.offline {
				links[i].down.Store(true)
			}
			err := l.Lock(ctx, "w", 100*time.Millisecond)
			if tt.write {
				require.NoError(t, err)
				require.NoError(t, l.Unlock(ctx, "w"))
			} else {
				assert.ErrorIs(t, err, domain.ErrLockTimeout)
			}
			err = l.RLock(ctx, "r", 100*time.Millisecond)
			if tt.rwrite {
				require.NoError(t, err)
				require.NoError(t, l.RUnlock(ctx, "r"))
			} else {
				assert.ErrorIs(t, err, domain.ErrLockTimeout)
			}
			for _, link := range links {
				link.down.Store(false)
			}
			for i, s := range c.servers {
				assert.Empty(t, entries(t, s), "failed attempts leave nothing on server %d", i)
			}
		})
	}
}

// TestDistributedRefreshLosesQuorum: when the renewal no longer reaches the
// quorum, the lock context is canceled so that the operation aborts.
//
// covers: S-STO-07
func TestDistributedRefreshLosesQuorum(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, 4, nil)
	l, links := c.locker(t, "a", lock.WithRefreshInterval(20*time.Millisecond))
	lc, err := l.NewNSLock(ctx, "b", "o").GetLock(ctx, time.Second)
	require.NoError(t, err)

	// One server lost: the quorum (3 of 4) still holds.
	links[0].down.Store(true)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, lc.Context().Err(), "3 of 4 servers keep the lock")

	links[1].down.Store(true)
	select {
	case <-lc.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the lock context was not canceled after losing the quorum")
	}
	lc.Unlock(ctx)
	lc.Unlock(ctx) // idempotent
}

// TestDistributedCrashedHolder is TS-U-14 / TC-09 at the unit level: a node
// crashes while holding a write lock (it never refreshes nor releases it);
// the servers expire the orphan after the stale timeout and the other
// writers resume.
//
// covers: S-STO-07
func TestDistributedCrashedHolder(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Now())
	c := newCluster(t, 4, clk, lock.WithStaleAfter(time.Minute))
	crashed, crashedLinks := c.locker(t, "crashed", lock.WithRefreshInterval(10*time.Millisecond))
	survivor, _ := c.locker(t, "survivor")

	lc, err := crashed.NewNSLock(ctx, "b", "o").GetLock(ctx, time.Second)
	require.NoError(t, err)
	// The node dies: no refresh nor release reaches the servers anymore.
	for _, l := range crashedLinks {
		l.down.Store(true)
	}
	<-lc.Context().Done()

	assert.ErrorIs(t, survivor.Lock(ctx, "b/o", 100*time.Millisecond), domain.ErrLockTimeout,
		"the orphan is held until its lease expires")
	holding := 0
	for _, s := range c.servers {
		for _, e := range entries(t, s) {
			assert.Equal(t, "crashed", e.Owner)
			holding++
		}
	}
	assert.GreaterOrEqual(t, holding, lock.WriteQuorum(4), "a quorum of servers keeps the orphan")

	clk.Advance(61 * time.Second)
	require.NoError(t, survivor.Lock(ctx, "b/o", time.Second), "the orphan expired")
	require.NoError(t, survivor.Unlock(ctx, "b/o"))
	lc.Unlock(ctx)
}

// TestDistributedPartition: with the servers split 2+2, no side can take a
// write lock (no split brain); read locks need only n/2 servers.
//
// covers: S-STO-07
func TestDistributedPartition(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, 4, nil)
	left, leftLinks := c.locker(t, "left")
	right, rightLinks := c.locker(t, "right")
	leftLinks[2].down.Store(true)
	leftLinks[3].down.Store(true)
	rightLinks[0].down.Store(true)
	rightLinks[1].down.Store(true)

	var wg sync.WaitGroup
	for _, l := range []lock.Locker{left, right} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.ErrorIs(t, l.Lock(ctx, "k", 150*time.Millisecond), domain.ErrLockTimeout)
		}()
	}
	wg.Wait()
	require.NoError(t, left.RLock(ctx, "k", time.Second))
	require.NoError(t, right.RLock(ctx, "k", time.Second))
	require.NoError(t, left.RUnlock(ctx, "k"))
	require.NoError(t, right.RUnlock(ctx, "k"))

	// The partition heals: writes resume.
	for _, l := range append(leftLinks, rightLinks...) {
		l.down.Store(false)
	}
	require.NoError(t, left.Lock(ctx, "k", time.Second))
	require.NoError(t, left.Unlock(ctx, "k"))
}

// TestDistributedMutualExclusion runs concurrent writers and readers from
// three nodes and checks that a writer is always alone.
//
// covers: S-STO-07
func TestDistributedMutualExclusion(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, 3, nil)
	var lockers []lock.Locker
	for i := range 3 {
		l, _ := c.locker(t, fmt.Sprintf("n%d", i))
		lockers = append(lockers, l)
	}
	var writers, readers, maxReaders atomic.Int32
	counter := 0
	var wg sync.WaitGroup
	for w := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := lockers[w%len(lockers)]
			for range 5 {
				if w%3 == 0 {
					lc, err := l.NewNSLock(ctx, "b", "shared").GetRLock(ctx, 10*time.Second)
					if !assert.NoError(t, err) {
						return
					}
					n := readers.Add(1)
					for cur := maxReaders.Load(); n > cur && !maxReaders.CompareAndSwap(cur, n); cur = maxReaders.Load() {
					}
					assert.Zero(t, writers.Load(), "a reader never overlaps a writer")
					time.Sleep(time.Millisecond)
					readers.Add(-1)
					lc.Unlock(ctx)
					continue
				}
				lc, err := l.NewNSLock(ctx, "b", "shared").GetLock(ctx, 10*time.Second)
				if !assert.NoError(t, err) {
					return
				}
				assert.Equal(t, int32(1), writers.Add(1), "a writer is alone")
				assert.Zero(t, readers.Load())
				counter++
				writers.Add(-1)
				lc.Unlock(ctx)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 16*5, counter)
	for _, s := range c.servers {
		assert.Empty(t, entries(t, s))
	}
}

// TestDistributedSlowServer: a slow server does not delay the acquisition
// beyond the quorum, and its late grant is released with the lock.
func TestDistributedSlowServer(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, 4, nil)
	l, links := c.locker(t, "a")
	links[3].latency.Store(int64(150 * time.Millisecond))
	start := time.Now()
	lc, err := l.NewNSLock(ctx, "b", "o").GetLock(ctx, time.Second)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 140*time.Millisecond, "the quorum of fast servers is enough")
	lc.Unlock(ctx)
	for i, s := range c.servers {
		assert.Empty(t, entries(t, s), "server %d holds nothing after the release", i)
	}

	// Simulated latency on every link: operations still complete.
	for _, link := range links {
		link.latency.Store(int64(20 * time.Millisecond))
	}
	require.NoError(t, l.Lock(ctx, "lat", time.Second))
	require.NoError(t, l.Unlock(ctx, "lat"))
}

func TestManagerListAndForceUnlock(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, 4, nil)
	l, _ := c.locker(t, "node-a", lock.WithRefreshInterval(20*time.Millisecond))
	m := lock.NewManager(lock.ManagerDeps{Servers: c.servers, Logger: slog.New(slog.DiscardHandler)})

	lc, err := l.NewNSLock(ctx, "b", "held").GetLock(ctx, time.Second)
	require.NoError(t, err)
	require.NoError(t, l.RLock(ctx, "b/read", time.Second))
	// The acquisition returns at the quorum; the last grants follow.
	var list []lock.Status
	require.Eventually(t, func() bool {
		var err error
		list, err = m.List(ctx)
		return err == nil && len(list) == 2 && list[0].Servers == 4 && list[1].Servers == 4
	}, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, "b/held", list[0].Resource)
	assert.True(t, list[0].Writer)
	assert.Equal(t, "node-a", list[0].Owner)
	assert.Equal(t, 4, list[0].Servers)
	assert.Equal(t, 4, list[0].Total)
	assert.Equal(t, 3, list[0].Quorum)
	assert.Equal(t, "b/read", list[1].Resource)
	assert.False(t, list[1].Writer)

	// An administrator clears the lock: the holder loses it at the next
	// refresh and its operation is canceled.
	require.NoError(t, m.ForceUnlock(ctx, "b/held"))
	select {
	case <-lc.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the holder did not notice the forced unlock")
	}
	lc.Unlock(ctx)
	require.NoError(t, l.RUnlock(ctx, "b/read"))
	list, err = m.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
	assert.ErrorIs(t, m.ForceUnlock(ctx), domain.ErrInvalidArgument)

	// Without servers (single-node modes) the manager is empty.
	empty := lock.NewManager(lock.ManagerDeps{})
	list, err = empty.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestManagerUnreachableServers(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, 2, nil)
	links := []*link{{}, {}}
	servers := []lock.Server{linkedServer(t, c.servers[0], links[0]), linkedServer(t, c.servers[1], links[1])}
	m := lock.NewManager(lock.ManagerDeps{Servers: servers, Logger: slog.New(slog.DiscardHandler)}, lock.WithManagerTimeout(time.Second))
	ok, err := c.servers[0].Lock(ctx, args("u1", "x"))
	require.NoError(t, err)
	require.True(t, ok)

	links[1].down.Store(true)
	list, err := m.List(ctx)
	require.NoError(t, err, "one server answered")
	require.Len(t, list, 1)
	assert.Equal(t, 1, list[0].Servers)
	assert.ErrorIs(t, m.ForceUnlock(ctx, "x"), domain.ErrNodeOffline)

	links[0].down.Store(true)
	_, err = m.List(ctx)
	assert.ErrorIs(t, err, domain.ErrNodeOffline)
}
