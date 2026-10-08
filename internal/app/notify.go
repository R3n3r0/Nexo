package app

import (
	"context"
	"log/slog"

	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	configsvc "github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// The decorators of this file keep the caches of the nodes of a cluster
// coherent (docs/03 §3.9): every write of a shared document (IAM, named
// policies, dynamic configuration, bucket metadata, share links, session
// revocations) is followed by a synchronous broadcast of the matching
// cluster.Event, and every node invalidates the state an event names
// (subscribe). In the single-node modes the notifier is local: the
// broadcasts are no-ops.

// broadcaster sends the invalidation events of the writes. A broadcast
// that misses a peer does not fail the write (it succeeded): the notifier
// retries the delivery in the background.
type broadcaster struct {
	n      cluster.Notifier
	logger *slog.Logger
}

func (b broadcaster) send(ctx context.Context, kind cluster.EventKind, name string) {
	if err := b.n.Broadcast(context.WithoutCancel(ctx), cluster.Event{Kind: kind, Name: name}); err != nil {
		b.logger.WarnContext(ctx, "cluster: invalidation not delivered to every peer (retried in the background)",
			"kind", string(kind), "name", name, "err", err)
	}
}

// iamEventKind is the event of a write in an IAM store namespace.
func iamEventKind(ns iam.Namespace) cluster.EventKind {
	switch ns {
	case iam.NamespaceShares:
		return cluster.EventShare
	case iam.NamespaceSessions:
		return cluster.EventSessionRevocation
	default:
		return cluster.EventIAM
	}
}

// notifyingIAMStore broadcasts the writes of the IAM store (users,
// groups, service accounts, STS credentials, share links, session
// revocations).
type notifyingIAMStore struct {
	iam.Store
	b broadcaster
}

var _ iam.Store = notifyingIAMStore{}

// Put implements iam.Store.
func (s notifyingIAMStore) Put(ctx context.Context, ns iam.Namespace, key string, value []byte) error {
	if err := s.Store.Put(ctx, ns, key, value); err != nil {
		return err
	}
	s.b.send(ctx, iamEventKind(ns), "")
	return nil
}

// Delete implements iam.Store.
func (s notifyingIAMStore) Delete(ctx context.Context, ns iam.Namespace, key string) error {
	if err := s.Store.Delete(ctx, ns, key); err != nil {
		return err
	}
	s.b.send(ctx, iamEventKind(ns), "")
	return nil
}

// notifyingPolicyStore broadcasts the writes of the named policies.
type notifyingPolicyStore struct {
	policy.SharedStore
	b broadcaster
}

var _ policy.SharedStore = notifyingPolicyStore{}

// Set implements policy.PolicyStore.
func (s notifyingPolicyStore) Set(ctx context.Context, p policy.Named) error {
	if err := s.SharedStore.Set(ctx, p); err != nil {
		return err
	}
	s.b.send(ctx, cluster.EventPolicy, p.Name)
	return nil
}

// Delete implements policy.PolicyStore.
func (s notifyingPolicyStore) Delete(ctx context.Context, name string) error {
	if err := s.SharedStore.Delete(ctx, name); err != nil {
		return err
	}
	s.b.send(ctx, cluster.EventPolicy, name)
	return nil
}

// SetAttached implements policy.PolicyStore.
func (s notifyingPolicyStore) SetAttached(ctx context.Context, e policy.Entity, names []string) error {
	if err := s.SharedStore.SetAttached(ctx, e, names); err != nil {
		return err
	}
	s.b.send(ctx, cluster.EventPolicy, "")
	return nil
}

// notifyingConfigStore broadcasts the writes of the dynamic configuration.
type notifyingConfigStore struct {
	configsvc.Store
	b broadcaster
}

var _ configsvc.Store = notifyingConfigStore{}

// Save implements config.Store.
func (s notifyingConfigStore) Save(ctx context.Context, all map[string]configsvc.KV) error {
	if err := s.Store.Save(ctx, all); err != nil {
		return err
	}
	s.b.send(ctx, cluster.EventConfig, "")
	return nil
}

// notifyingBucketStore broadcasts the writes of the bucket metadata.
type notifyingBucketStore struct {
	bucket.MetadataStore
	b broadcaster
}

var _ bucket.MetadataStore = notifyingBucketStore{}

// Save implements bucket.MetadataStore.
func (s notifyingBucketStore) Save(ctx context.Context, name string, m *bucket.Metadata) error {
	if err := s.MetadataStore.Save(ctx, name, m); err != nil {
		return err
	}
	s.b.send(ctx, cluster.EventBucketMetadata, name)
	return nil
}

// Delete implements bucket.MetadataStore.
func (s notifyingBucketStore) Delete(ctx context.Context, name string) error {
	if err := s.MetadataStore.Delete(ctx, name); err != nil {
		return err
	}
	s.b.send(ctx, cluster.EventBucketMetadata, name)
	return nil
}

// invalidators are the caches a node drops when a peer changed the state
// they hold.
type invalidators struct {
	identities iam.Service
	// policies is nil when the named policies are not shared (SNSD).
	policies policy.SharedStore
	config   configsvc.Service
	buckets  bucket.MetadataStore
	logger   *slog.Logger
}

// subscribe registers the invalidations on the notifier and returns the
// function that unregisters them. The share links and the session
// revocations are read through on every use: their events need no
// handler.
func subscribe(n cluster.Notifier, inv invalidators) func() {
	var unsubs []func()
	add := func(kind cluster.EventKind, fn func(ctx context.Context, e cluster.Event) error) {
		unsubs = append(unsubs, n.Subscribe(kind, cluster.HandlerFunc(fn)))
	}
	add(cluster.EventIAM, func(ctx context.Context, _ cluster.Event) error {
		inv.identities.Invalidate(ctx)
		return nil
	})
	if inv.policies != nil {
		add(cluster.EventPolicy, func(ctx context.Context, _ cluster.Event) error {
			inv.policies.Invalidate(ctx)
			return nil
		})
	}
	add(cluster.EventConfig, func(ctx context.Context, _ cluster.Event) error {
		return inv.config.Reload(ctx)
	})
	add(cluster.EventBucketMetadata, func(ctx context.Context, e cluster.Event) error {
		name := e.Name
		if e.Kind == cluster.EventResync {
			name = ""
		}
		inv.buckets.Invalidate(ctx, name)
		return nil
	})
	return func() {
		for _, u := range unsubs {
			u()
		}
	}
}
