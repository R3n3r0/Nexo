package cluster_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	clustermock "github.com/r3n3r0/nexo/internal/service/cluster/mock"
	"github.com/r3n3r0/nexo/internal/service/lock"
)

// recorder counts the leadership notifications of a node.
type recorder struct {
	elected, revoked atomic.Int32
	mu               sync.Mutex
	term             context.Context
}

func (r *recorder) listener() cluster.LeaderListener {
	return cluster.LeaderFuncs{
		Elected: func(ctx context.Context) {
			r.mu.Lock()
			r.term = ctx
			r.mu.Unlock()
			r.elected.Add(1)
		},
		Revoked: func(context.Context) { r.revoked.Add(1) },
	}
}

func (r *recorder) currentTerm() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.term
}

// lockServers returns n in-memory lock servers with a short lease.
func lockServers(n int) []lock.Server {
	out := make([]lock.Server, n)
	for i := range out {
		out[i] = lock.NewServer(lock.ServerDeps{Logger: discard()}, lock.WithStaleAfter(500*time.Millisecond))
	}
	return out
}

func newLeader(t *testing.T, servers []lock.Server, id string) cluster.Leader {
	t.Helper()
	locker, err := lock.NewDistributed(lock.DistributedDeps{Servers: servers, Owner: id, Logger: discard()},
		lock.WithRefreshInterval(50*time.Millisecond), lock.WithRetryInterval(5*time.Millisecond, 20*time.Millisecond),
		lock.WithCallTimeout(200*time.Millisecond))
	require.NoError(t, err)
	l, err := cluster.NewLeader(cluster.LeaderDeps{Locker: locker, NodeID: id, Logger: discard()},
		cluster.WithElectionInterval(50*time.Millisecond), cluster.WithLeaderAcquireTimeout(50*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Stop(context.Background()) })
	return l
}

func leaders(ls []cluster.Leader) []int {
	var out []int
	for i, l := range ls {
		if l.IsLeader() {
			out = append(out, i)
		}
	}
	return out
}

// TestLeaderSingleton: among competing nodes exactly one leads; when it
// stops, another takes over (TC-14 at the unit level).
func TestLeaderSingleton(t *testing.T) {
	ctx := context.Background()
	servers := lockServers(3)
	var nodes []cluster.Leader
	recs := make([]*recorder, 3)
	for i := range 3 {
		recs[i] = &recorder{}
		l := newLeader(t, servers, fmt.Sprintf("n%d", i))
		l.Subscribe(recs[i].listener())
		nodes = append(nodes, l)
		require.NoError(t, l.Start(ctx))
	}
	require.Eventually(t, func() bool { return len(leaders(nodes)) == 1 }, 5*time.Second, 5*time.Millisecond)
	// Leadership is stable: never two leaders.
	for range 20 {
		assert.LessOrEqual(t, len(leaders(nodes)), 1)
		time.Sleep(5 * time.Millisecond)
	}
	first := leaders(nodes)[0]
	assert.Equal(t, int32(1), recs[first].elected.Load())
	term := recs[first].currentTerm()
	require.NoError(t, term.Err())

	require.NoError(t, nodes[first].Stop(ctx))
	assert.False(t, nodes[first].IsLeader())
	assert.Error(t, term.Err(), "the term of the former leader is over")
	assert.Equal(t, int32(1), recs[first].revoked.Load())
	require.Eventually(t, func() bool {
		l := leaders(nodes)
		return len(l) == 1 && l[0] != first
	}, 5*time.Second, 5*time.Millisecond, "failover")
}

// TestLeaderLosesTheLock: an administrator clears the leader lock; the
// leader notices it at the next refresh, steps down, and a leader is
// elected again.
func TestLeaderLosesTheLock(t *testing.T) {
	ctx := context.Background()
	servers := lockServers(3)
	l := newLeader(t, servers, "n1")
	rec := &recorder{}
	l.Subscribe(rec.listener())
	require.NoError(t, l.Start(ctx))
	require.Eventually(t, l.IsLeader, 5*time.Second, 5*time.Millisecond)

	manager := lock.NewManager(lock.ManagerDeps{Servers: servers, Logger: discard()})
	require.NoError(t, manager.ForceUnlock(ctx, lock.Resource(domain.SystemBucket, cluster.LeaderResource)))
	require.Eventually(t, func() bool { return rec.revoked.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool { return rec.elected.Load() == 2 && l.IsLeader() }, 5*time.Second, 5*time.Millisecond)
}

// TestLeaderLateSubscriber: a listener registered while the node leads is
// notified at once; an unsubscribed one is not notified anymore.
func TestLeaderLateSubscriber(t *testing.T) {
	ctx := context.Background()
	l := newLeader(t, lockServers(1), "n1")
	require.NoError(t, l.Start(ctx))
	require.NoError(t, l.Start(ctx), "Start is idempotent")
	require.Eventually(t, l.IsLeader, 5*time.Second, 5*time.Millisecond)

	ln := clustermock.NewLeaderListener(t)
	ln.EXPECT().OnElected(mock.Anything).Once()
	unsubscribe := l.Subscribe(ln)
	unsubscribe()
	require.NoError(t, l.Stop(ctx))
	require.NoError(t, l.Stop(ctx))
}

func TestLocalLeader(t *testing.T) {
	ctx := context.Background()
	l := cluster.NewLocalLeader(cluster.LeaderDeps{})
	ln := clustermock.NewLeaderListener(t)
	var term context.Context
	ln.EXPECT().OnElected(mock.Anything).Run(func(ctx context.Context) { term = ctx }).Once()
	ln.EXPECT().OnRevoked(mock.Anything).Once()
	l.Subscribe(ln)
	assert.False(t, l.IsLeader())
	require.NoError(t, l.Start(ctx))
	assert.True(t, l.IsLeader(), "a single node always leads")
	require.NoError(t, term.Err())

	late := &recorder{}
	l.Subscribe(late.listener())
	assert.Equal(t, int32(1), late.elected.Load())

	require.NoError(t, l.Stop(ctx))
	assert.False(t, l.IsLeader())
	assert.Error(t, term.Err())
	assert.Equal(t, int32(1), late.revoked.Load())
}

func TestNewLeaderValidation(t *testing.T) {
	_, err := cluster.NewLeader(cluster.LeaderDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	// LeaderFuncs tolerates missing callbacks.
	cluster.LeaderFuncs{}.OnElected(context.Background())
	cluster.LeaderFuncs{}.OnRevoked(context.Background())
}
