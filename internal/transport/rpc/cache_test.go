package rpc_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cache"
	cachemock "github.com/r3n3r0/nexo/internal/service/cache/mock"
	"github.com/r3n3r0/nexo/internal/transport/rpc"
)

// TestCacheOverRPC carries every internode call of the cache through the
// authenticated RPC routes; the errors keep their domain sentinel.
func TestCacheOverRPC(t *testing.T) {
	ctx := context.Background()
	node := cachemock.NewNode(t)
	node.EXPECT().ApplyInvalidations(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, b cache.InvalidationBatch) (cache.InvalidationAck, error) {
		if b.Origin == "" {
			return cache.InvalidationAck{}, fmt.Errorf("%w: invalidations without origin", cache.ErrInvalid)
		}
		return cache.InvalidationAck{Node: "n2", LastSeq: b.Messages[len(b.Messages)-1].Seq, Removed: map[string]int{"inv-1": 3}}, nil
	})
	node.EXPECT().SyncStatus(tmock.Anything).Return(cache.SyncStatus{Node: "n2", Epoch: "e1", Seq: 42}, nil)
	node.EXPECT().FetchPiece(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, req cache.PieceRequest) (cache.PieceReply, error) {
		if string(req.Spec) == `{"changed":true}` {
			return cache.PieceReply{}, fmt.Errorf("cache: object changed: %w", domain.ErrPreconditionFailed)
		}
		return cache.PieceReply{Node: "n2", Result: cache.ResultHit, Meta: []byte(`{"etag":"e"}`), Body: []byte("body bytes")}, nil
	})
	node.EXPECT().Presence(tmock.Anything, cache.PresenceRequest{Keys: []string{"k"}}).
		Return(cache.PresenceReply{Node: "n2", Entries: []cache.Presence{{Key: "k", Present: true, Tier: "memory", Size: 10}}}, nil)
	node.EXPECT().NodeStats(tmock.Anything, cache.StatsQuery{Distribution: "web"}).
		Return(cache.NodeStats{Node: "n2", Entries: 7, Counters: cache.Counters{Requests: map[cache.Result]int64{cache.ResultHit: 5}}}, nil)
	node.EXPECT().StartWarm(tmock.Anything, tmock.Anything).Return(fmt.Errorf("cache: %w", cache.ErrNotReady))

	routes, err := rpc.NewCacheRoutes(rpc.CacheRoutesDeps{Cache: node})
	require.NoError(t, err)
	addr, _ := startRPC(t, routes)
	peer, err := rpc.NewCacheClient(rpc.CacheClientDeps{Conn: dial(t, newAuth(t, "client:9000"), addr)})
	require.NoError(t, err)
	assert.Equal(t, addr, peer.NodeID())

	ack, err := peer.ApplyInvalidations(ctx, cache.InvalidationBatch{Origin: "n1", Epoch: "e0", Messages: []cache.InvalidationMessage{
		{Seq: 1, Change: &cache.ObjectChange{Bucket: "web", Key: "a"}}, {Seq: 2, ID: "inv-1", FlushAll: true},
	}})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), ack.LastSeq)
	assert.Equal(t, 3, ack.Removed["inv-1"])
	_, err = peer.ApplyInvalidations(ctx, cache.InvalidationBatch{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)

	st, err := peer.SyncStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, cache.SyncStatus{Node: "n2", Epoch: "e1", Seq: 42}, st)

	rep, err := peer.FetchPiece(ctx, cache.PieceRequest{Spec: []byte(`{"key":"k"}`)})
	require.NoError(t, err)
	assert.Equal(t, []byte("body bytes"), rep.Body)
	assert.Equal(t, cache.ResultHit, rep.Result)
	assert.JSONEq(t, `{"etag":"e"}`, string(rep.Meta))
	_, err = peer.FetchPiece(ctx, cache.PieceRequest{Spec: []byte(`{"changed":true}`)})
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)

	pr, err := peer.Presence(ctx, cache.PresenceRequest{Keys: []string{"k"}})
	require.NoError(t, err)
	require.Len(t, pr.Entries, 1)
	assert.True(t, pr.Entries[0].Present)
	assert.Equal(t, cache.Size(10), pr.Entries[0].Size)

	ns, err := peer.NodeStats(ctx, cache.StatsQuery{Distribution: "web"})
	require.NoError(t, err)
	assert.Equal(t, 7, ns.Entries)
	assert.Equal(t, int64(5), ns.Requests[cache.ResultHit])

	err = peer.StartWarm(ctx, cache.WarmJob{ID: "warm-1"})
	require.ErrorIs(t, err, domain.ErrNodeOffline, "not ready: the services of the node are not composed yet")

	// A caller outside of the deployment is refused.
	other, err := rpc.NewAuthenticator(rpc.AuthDeps{Logger: discard()}, rpc.WithCredentials(rootUser, "other-secret"), rpc.WithNodeID("intruder:9000"))
	require.NoError(t, err)
	intruder, err := rpc.NewCacheClient(rpc.CacheClientDeps{Conn: dial(t, other, addr)})
	require.NoError(t, err)
	_, err = intruder.SyncStatus(ctx)
	require.ErrorIs(t, err, domain.ErrUnauthenticated)

	_, err = rpc.NewCacheRoutes(rpc.CacheRoutesDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = rpc.NewCacheClient(rpc.CacheClientDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}
