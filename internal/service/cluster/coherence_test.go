package cluster_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	clustermock "github.com/r3n3r0/nexo/internal/service/cluster/mock"
	configsvc "github.com/r3n3r0/nexo/internal/service/config"
	configmock "github.com/r3n3r0/nexo/internal/service/config/mock"
	"github.com/r3n3r0/nexo/internal/service/iam"
	iammock "github.com/r3n3r0/nexo/internal/service/iam/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
)

func TestNewCoherenceRequiresNotifier(t *testing.T) {
	_, err := cluster.NewCoherence(cluster.CoherenceDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestCoherenceBroadcastsWrites: every successful write of a shared store
// is followed by the broadcast of its event; a failed write broadcasts
// nothing, and a broadcast that misses a peer does not fail the write.
func TestCoherenceBroadcastsWrites(t *testing.T) {
	ctx := context.Background()
	failed := errors.New("disk full")
	tests := []struct {
		name  string
		write func(t *testing.T, c cluster.Coherence) error
		event cluster.Event
	}{
		{name: "iam user", event: cluster.Event{Kind: cluster.EventIAM}, write: func(t *testing.T, c cluster.Coherence) error {
			s := iammock.NewStore(t)
			s.EXPECT().Put(mock.Anything, iam.NamespaceUsers, "bob", []byte("{}")).Return(nil)
			return c.IAMStore(s).Put(ctx, iam.NamespaceUsers, "bob", []byte("{}"))
		}},
		{name: "share link", event: cluster.Event{Kind: cluster.EventShare}, write: func(t *testing.T, c cluster.Coherence) error {
			s := iammock.NewStore(t)
			s.EXPECT().Delete(mock.Anything, iam.NamespaceShares, "s1").Return(nil)
			return c.IAMStore(s).Delete(ctx, iam.NamespaceShares, "s1")
		}},
		{name: "session revocation", event: cluster.Event{Kind: cluster.EventSessionRevocation}, write: func(t *testing.T, c cluster.Coherence) error {
			s := iammock.NewStore(t)
			s.EXPECT().Put(mock.Anything, iam.NamespaceSessions, "j1", []byte("x")).Return(nil)
			return c.IAMStore(s).Put(ctx, iam.NamespaceSessions, "j1", []byte("x"))
		}},
		{name: "policy", event: cluster.Event{Kind: cluster.EventPolicy, Name: "readers"}, write: func(t *testing.T, c cluster.Coherence) error {
			s := policymock.NewSharedStore(t)
			s.EXPECT().Set(mock.Anything, policy.Named{Name: "readers"}).Return(nil)
			return c.PolicyStore(s).Set(ctx, policy.Named{Name: "readers"})
		}},
		{name: "policy removal", event: cluster.Event{Kind: cluster.EventPolicy, Name: "readers"}, write: func(t *testing.T, c cluster.Coherence) error {
			s := policymock.NewSharedStore(t)
			s.EXPECT().Delete(mock.Anything, "readers").Return(nil)
			return c.PolicyStore(s).Delete(ctx, "readers")
		}},
		{name: "policy attachment", event: cluster.Event{Kind: cluster.EventPolicy}, write: func(t *testing.T, c cluster.Coherence) error {
			s := policymock.NewSharedStore(t)
			e := policy.Entity{Kind: policy.EntityUser, Name: "bob"}
			s.EXPECT().SetAttached(mock.Anything, e, []string{"readers"}).Return(nil)
			return c.PolicyStore(s).SetAttached(ctx, e, []string{"readers"})
		}},
		{name: "config", event: cluster.Event{Kind: cluster.EventConfig}, write: func(t *testing.T, c cluster.Coherence) error {
			s := configmock.NewStore(t)
			s.EXPECT().Save(mock.Anything, map[string]configsvc.KV{}).Return(nil)
			return c.ConfigStore(s).Save(ctx, map[string]configsvc.KV{})
		}},
		{name: "bucket metadata", event: cluster.Event{Kind: cluster.EventBucketMetadata, Name: "photos"}, write: func(t *testing.T, c cluster.Coherence) error {
			s := bucketmock.NewMetadataStore(t)
			s.EXPECT().Save(mock.Anything, "photos", mock.Anything).Return(nil)
			return c.BucketStore(s).Save(ctx, "photos", nil)
		}},
		{name: "bucket removal", event: cluster.Event{Kind: cluster.EventBucketMetadata, Name: "photos"}, write: func(t *testing.T, c cluster.Coherence) error {
			s := bucketmock.NewMetadataStore(t)
			s.EXPECT().Delete(mock.Anything, "photos").Return(nil)
			return c.BucketStore(s).Delete(ctx, "photos")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := clustermock.NewNotifier(t)
			n.EXPECT().Broadcast(mock.Anything, tc.event).Return(errors.New("peer n3 unreachable")).Once()
			c, err := cluster.NewCoherence(cluster.CoherenceDeps{Notifier: n, Logger: discard()})
			require.NoError(t, err)
			assert.NoError(t, tc.write(t, c), "a peer missed does not fail the write")
		})
	}

	t.Run("failed write", func(t *testing.T) {
		n := clustermock.NewNotifier(t)
		c, err := cluster.NewCoherence(cluster.CoherenceDeps{Notifier: n, Logger: discard()})
		require.NoError(t, err)
		s := iammock.NewStore(t)
		s.EXPECT().Put(mock.Anything, iam.NamespaceUsers, "bob", []byte("{}")).Return(failed)
		assert.ErrorIs(t, c.IAMStore(s).Put(ctx, iam.NamespaceUsers, "bob", []byte("{}")), failed)
		n.AssertNotCalled(t, "Broadcast", mock.Anything, mock.Anything)
	})
}

// TestCoherenceSubscribe: the events of the peers drop the matching
// caches (every bucket on a resync), and unsubscribe unregisters them.
func TestCoherenceSubscribe(t *testing.T) {
	ctx := context.Background()
	n := clustermock.NewNotifier(t)
	handlers := map[cluster.EventKind]cluster.Handler{}
	unsubscribed := 0
	n.EXPECT().Subscribe(mock.Anything, mock.Anything).RunAndReturn(func(k cluster.EventKind, h cluster.Handler) func() {
		handlers[k] = h
		return func() { unsubscribed++ }
	})
	c, err := cluster.NewCoherence(cluster.CoherenceDeps{Notifier: n, Logger: discard()})
	require.NoError(t, err)

	identities := iammock.NewService(t)
	policies := policymock.NewSharedStore(t)
	config := configmock.NewService(t)
	buckets := bucketmock.NewMetadataStore(t)
	unsubscribe := c.Subscribe(cluster.Caches{IAM: identities, Policies: policies, Config: config, Buckets: buckets})
	require.Len(t, handlers, 4)

	identities.EXPECT().Invalidate(mock.Anything).Once()
	require.NoError(t, handlers[cluster.EventIAM].HandleEvent(ctx, cluster.Event{Kind: cluster.EventIAM}))
	policies.EXPECT().Invalidate(mock.Anything).Once()
	require.NoError(t, handlers[cluster.EventPolicy].HandleEvent(ctx, cluster.Event{Kind: cluster.EventPolicy, Name: "readers"}))
	reload := errors.New("store unreachable")
	config.EXPECT().Reload(mock.Anything).Return(reload).Once()
	assert.ErrorIs(t, handlers[cluster.EventConfig].HandleEvent(ctx, cluster.Event{Kind: cluster.EventConfig}), reload)
	buckets.EXPECT().Invalidate(mock.Anything, "photos").Once()
	require.NoError(t, handlers[cluster.EventBucketMetadata].HandleEvent(ctx, cluster.Event{Kind: cluster.EventBucketMetadata, Name: "photos"}))
	buckets.EXPECT().Invalidate(mock.Anything, "").Once()
	require.NoError(t, handlers[cluster.EventBucketMetadata].HandleEvent(ctx, cluster.Event{Kind: cluster.EventResync, Name: "ignored"}))

	unsubscribe()
	assert.Equal(t, 4, unsubscribed)

	// Without shared policies (SNSD) no policy handler is registered.
	handlers = map[cluster.EventKind]cluster.Handler{}
	c.Subscribe(cluster.Caches{IAM: identities, Config: config, Buckets: buckets})
	assert.NotContains(t, handlers, cluster.EventPolicy)
}
