package cluster_test

import (
	"context"
	"errors"
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
	"github.com/r3n3r0/nexo/internal/service/cluster"
	clustermock "github.com/r3n3r0/nexo/internal/service/cluster/mock"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakePeer is the state behind a generated Peer mock: whether it is
// reachable and the events it received.
type fakePeer struct {
	id     string
	online atomic.Bool
	fail   atomic.Bool
	mu     sync.Mutex
	got    []cluster.Event
}

func (p *fakePeer) events() []cluster.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]cluster.Event(nil), p.got...)
}

func newPeer(t *testing.T, id string) (*clustermock.Peer, *fakePeer) {
	t.Helper()
	state := &fakePeer{id: id}
	state.online.Store(true)
	m := clustermock.NewPeer(t)
	m.EXPECT().NodeID().Return(id).Maybe()
	m.EXPECT().IsOnline().RunAndReturn(state.online.Load).Maybe()
	m.EXPECT().Notify(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, e cluster.Event) error {
		if !state.online.Load() || state.fail.Load() {
			return domain.ErrNodeOffline
		}
		state.mu.Lock()
		state.got = append(state.got, e)
		state.mu.Unlock()
		return nil
	}).Maybe()
	return m, state
}

func TestNotifierBroadcast(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	p1, s1 := newPeer(t, "n2:9000")
	p2, s2 := newPeer(t, "n3:9000")
	n := cluster.NewNotifier(cluster.NotifierDeps{NodeID: "n1:9000", Peers: []cluster.Peer{p1, p2}, Clock: clock.NewFake(now), Logger: discard()})
	local := 0
	n.Subscribe(cluster.EventIAM, cluster.HandlerFunc(func(context.Context, cluster.Event) error { local++; return nil }))

	require.NoError(t, n.Broadcast(ctx, cluster.Event{Kind: cluster.EventIAM, Name: "alice"}))
	want := cluster.Event{Kind: cluster.EventIAM, Name: "alice", Origin: "n1:9000", Time: now}
	assert.Equal(t, []cluster.Event{want}, s1.events())
	assert.Equal(t, []cluster.Event{want}, s2.events())
	assert.Zero(t, local, "the broadcasting node is not notified")
	assert.ErrorIs(t, n.Broadcast(ctx, cluster.Event{}), domain.ErrInvalidArgument)
}

// TestNotifierRetriesMissedEvents: the events a peer missed are delivered
// once it is reachable again, deduplicated, and collapse into a resync
// beyond the bound.
func TestNotifierRetriesMissedEvents(t *testing.T) {
	ctx := context.Background()
	p1, s1 := newPeer(t, "n2:9000")
	p2, s2 := newPeer(t, "n3:9000")
	p3, s3 := newPeer(t, "n4:9000")
	n := cluster.NewNotifier(cluster.NotifierDeps{NodeID: "n1:9000", Peers: []cluster.Peer{p1, p2, p3}, Logger: discard()},
		cluster.WithRetryInterval(10*time.Millisecond), cluster.WithMaxPending(3))
	require.NoError(t, n.Start(ctx))
	t.Cleanup(func() { _ = n.Stop(context.Background()) })

	s2.online.Store(false) // unreachable: queued without a call
	s3.fail.Store(true)    // reachable but failing: queued after the call
	for _, name := range []string{"alice", "bob", "alice"} {
		err := n.Broadcast(ctx, cluster.Event{Kind: cluster.EventIAM, Name: name})
		require.Error(t, err)
		assert.ErrorIs(t, err, domain.ErrNodeOffline)
	}
	assert.Len(t, s1.events(), 3)
	assert.Empty(t, s2.events())

	s2.online.Store(true)
	require.Eventually(t, func() bool { return len(s2.events()) == 2 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, "alice", s2.events()[0].Name)
	assert.Equal(t, "bob", s2.events()[1].Name, "duplicates are delivered once, in order")

	// Too many missed events become one resync.
	for _, name := range []string{"c", "d", "e", "f"} {
		_ = n.Broadcast(ctx, cluster.Event{Kind: cluster.EventPolicy, Name: name})
	}
	s3.fail.Store(false)
	require.Eventually(t, func() bool { return len(s3.events()) == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, cluster.EventResync, s3.events()[0].Kind)
	time.Sleep(50 * time.Millisecond)
	assert.Len(t, s3.events(), 1, "nothing else is pending")
	require.NoError(t, n.Stop(ctx))
	require.NoError(t, n.Stop(ctx), "Stop is idempotent")
}

func TestNotifierDeliver(t *testing.T) {
	ctx := context.Background()
	n := cluster.NewLocalNotifier(cluster.NotifierDeps{Logger: discard()})
	var iam, policy []string
	unsubscribe := n.Subscribe(cluster.EventIAM, cluster.HandlerFunc(func(_ context.Context, e cluster.Event) error {
		iam = append(iam, string(e.Kind)+":"+e.Name)
		return nil
	}))
	boom := errors.New("reload failed")
	n.Subscribe(cluster.EventPolicy, cluster.HandlerFunc(func(_ context.Context, e cluster.Event) error {
		policy = append(policy, string(e.Kind)+":"+e.Name)
		return boom
	}))

	require.NoError(t, n.Deliver(ctx, cluster.Event{Kind: cluster.EventIAM, Name: "alice"}))
	assert.ErrorIs(t, n.Deliver(ctx, cluster.Event{Kind: cluster.EventPolicy, Name: "readonly"}), boom)
	require.NoError(t, n.Deliver(ctx, cluster.Event{Kind: cluster.EventShare, Name: "x"}), "no subscriber")
	assert.ErrorIs(t, n.Deliver(ctx, cluster.Event{Kind: cluster.EventResync}), boom, "a resync reaches every subscriber")
	assert.Equal(t, []string{"iam:alice", "resync:"}, iam)
	assert.Equal(t, []string{"policy:readonly", "resync:"}, policy)

	unsubscribe()
	require.NoError(t, n.Deliver(ctx, cluster.Event{Kind: cluster.EventIAM, Name: "bob"}))
	assert.Len(t, iam, 2)
	assert.ErrorIs(t, n.Deliver(ctx, cluster.Event{}), domain.ErrInvalidArgument)

	// A single node has nobody to notify.
	require.NoError(t, n.Broadcast(ctx, cluster.Event{Kind: cluster.EventConfig}))
	require.NoError(t, n.Start(ctx))
	require.NoError(t, n.Stop(ctx))
}

// TestNotifierSubscriberMock drives a generated Handler mock.
func TestNotifierSubscriberMock(t *testing.T) {
	ctx := context.Background()
	n := cluster.NewLocalNotifier(cluster.NotifierDeps{Logger: discard()})
	h := clustermock.NewHandler(t)
	e := cluster.Event{Kind: cluster.EventBucketMetadata, Name: "photos", Origin: "n2:9000"}
	h.EXPECT().HandleEvent(mock.Anything, e).Return(nil).Once()
	n.Subscribe(cluster.EventBucketMetadata, h)
	require.NoError(t, n.Deliver(ctx, e))
}
