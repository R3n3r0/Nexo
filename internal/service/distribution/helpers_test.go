package distribution_test

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"

	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	clustermock "github.com/r3n3r0/nexo/internal/service/cluster/mock"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	sysmock "github.com/r3n3r0/nexo/internal/service/sysstore/mock"
)

// memDocs is the shared state of the store mocks: the documents of a
// deployment, seen by every node of a test.
type memDocs struct {
	mu   sync.Mutex
	docs map[string][]byte
}

// newStore returns a store mock backed by docs.
func newStore(t testing.TB, docs *memDocs) *sysmock.Store {
	t.Helper()
	m := sysmock.NewStore(t)
	m.EXPECT().Get(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, p string) ([]byte, error) {
		docs.mu.Lock()
		defer docs.mu.Unlock()
		d, ok := docs.docs[p]
		if !ok {
			return nil, sysstore.ErrNotFound
		}
		return append([]byte(nil), d...), nil
	}).Maybe()
	m.EXPECT().Put(tmock.Anything, tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, p string, data []byte) error {
		docs.mu.Lock()
		defer docs.mu.Unlock()
		docs.docs[p] = append([]byte(nil), data...)
		return nil
	}).Maybe()
	m.EXPECT().Delete(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, p string) error {
		docs.mu.Lock()
		defer docs.mu.Unlock()
		delete(docs.docs, p)
		return nil
	}).Maybe()
	m.EXPECT().List(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, dir string) (map[string][]byte, error) {
		docs.mu.Lock()
		defer docs.mu.Unlock()
		out := map[string][]byte{}
		for p, d := range docs.docs {
			if rest, ok := strings.CutPrefix(p, dir+"/"); ok && !strings.Contains(rest, "/") {
				out[rest] = append([]byte(nil), d...)
			}
		}
		return out, nil
	}).Maybe()
	return m
}

// node is one service of a test deployment.
type node struct {
	svc      distribution.Service
	notifier cluster.Notifier
}

// newDeployment starts n distribution services on one store; the
// broadcasts of each node are delivered to the others, as the peer
// notifier of a cluster does.
func newDeployment(t testing.TB, n int, opts ...distribution.Option) []*node {
	t.Helper()
	docs := &memDocs{docs: map[string][]byte{}}
	locks := lock.NewLocal(lock.Deps{})
	nodes := make([]*node, n)
	for i := range nodes {
		nodes[i] = &node{notifier: cluster.NewLocalNotifier(cluster.NotifierDeps{NodeID: "n" + string(rune('1'+i)), Logger: slog.New(slog.DiscardHandler)})}
	}
	for i := range nodes {
		self := i
		peers := clusterNotifier(t, nodes[i].notifier, func(ctx context.Context, e cluster.Event) error {
			for j, other := range nodes {
				if j != self {
					if err := other.notifier.Deliver(ctx, e); err != nil {
						return err
					}
				}
			}
			return nil
		})
		svc, err := distribution.New(distribution.Deps{Store: newStore(t, docs), Locks: locks, Notifier: peers, Clock: clock.NewReal(),
			Logger: slog.New(slog.DiscardHandler)}, opts...)
		require.NoError(t, err)
		require.NoError(t, svc.Start(context.Background()))
		t.Cleanup(func() { _ = svc.Stop(context.Background()) })
		nodes[i].svc = svc
	}
	return nodes
}

// newService starts a single distribution service.
func newService(t testing.TB, opts ...distribution.Option) distribution.Service {
	t.Helper()
	return newDeployment(t, 1, opts...)[0].svc
}

// sample returns a valid website distribution.
func sample(id string, aliases ...string) distribution.Distribution {
	return distribution.Distribution{
		ID:      id,
		Enabled: true,
		Aliases: aliases,
		Origin:  distribution.Origin{Bucket: "web"},
		Mode:    distribution.ModeWebsite,
		DefaultBehavior: distribution.Behavior{
			Access:        distribution.AccessPublic,
			IndexDocument: "index.html",
		},
	}
}

// sortedAliases returns the aliases of the distributions, sorted.
func sortedAliases(ds []distribution.Distribution) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Aliases...)
	}
	sort.Strings(out)
	return out
}

// clusterNotifier returns a notifier mock whose subscriptions go to local
// and whose broadcasts call deliver (the peers of the node).
func clusterNotifier(t testing.TB, local cluster.Notifier, deliver func(ctx context.Context, e cluster.Event) error) *clustermock.Notifier {
	t.Helper()
	m := clustermock.NewNotifier(t)
	m.EXPECT().Subscribe(tmock.Anything, tmock.Anything).RunAndReturn(local.Subscribe).Maybe()
	m.EXPECT().Broadcast(tmock.Anything, tmock.Anything).RunAndReturn(deliver).Maybe()
	m.EXPECT().Deliver(tmock.Anything, tmock.Anything).RunAndReturn(local.Deliver).Maybe()
	return m
}
