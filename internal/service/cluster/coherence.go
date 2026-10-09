package cluster

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	configsvc "github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// CoherenceDeps are the dependencies of the cache coherence.
type CoherenceDeps struct {
	// Notifier broadcasts the events and dispatches those of the peers
	// (required).
	Notifier Notifier
	Logger   *slog.Logger
}

// CoherenceOption configures the cache coherence (none yet: the type
// keeps the constructor shape of docs/12 §12.3).
type CoherenceOption func(*coherence)

type coherence struct {
	n      Notifier
	logger *slog.Logger
}

var _ Coherence = (*coherence)(nil)

// NewCoherence returns the cache coherence of the shared stores over the
// notifier.
func NewCoherence(deps CoherenceDeps, opts ...CoherenceOption) (Coherence, error) {
	if deps.Notifier == nil {
		return nil, fmt.Errorf("%w: cluster: the cache coherence requires the notifier", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	c := &coherence{n: deps.Notifier, logger: deps.Logger}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// send broadcasts the invalidation of a write; a peer missed is logged
// (the notifier retries it in the background), never an error of the
// write.
func (c *coherence) send(ctx context.Context, kind EventKind, name string) {
	if err := c.n.Broadcast(context.WithoutCancel(ctx), Event{Kind: kind, Name: name}); err != nil {
		c.logger.WarnContext(ctx, "cluster: invalidation not delivered to every peer (retried in the background)",
			"kind", string(kind), "name", name, "err", err)
	}
}

// IAMStore implements Coherence.
func (c *coherence) IAMStore(s iam.Store) iam.Store { return notifyingIAMStore{Store: s, c: c} }

// PolicyStore implements Coherence.
func (c *coherence) PolicyStore(s policy.SharedStore) policy.SharedStore {
	return notifyingPolicyStore{SharedStore: s, c: c}
}

// ConfigStore implements Coherence.
func (c *coherence) ConfigStore(s configsvc.Store) configsvc.Store {
	return notifyingConfigStore{Store: s, c: c}
}

// BucketStore implements Coherence.
func (c *coherence) BucketStore(s bucket.MetadataStore) bucket.MetadataStore {
	return notifyingBucketStore{MetadataStore: s, c: c}
}

// Subscribe implements Coherence.
func (c *coherence) Subscribe(caches Caches) func() {
	var unsubs []func()
	add := func(kind EventKind, fn func(ctx context.Context, e Event) error) {
		unsubs = append(unsubs, c.n.Subscribe(kind, HandlerFunc(fn)))
	}
	if caches.IAM != nil {
		add(EventIAM, func(ctx context.Context, _ Event) error {
			caches.IAM.Invalidate(ctx)
			return nil
		})
	}
	if caches.Policies != nil {
		add(EventPolicy, func(ctx context.Context, _ Event) error {
			caches.Policies.Invalidate(ctx)
			return nil
		})
	}
	if caches.Config != nil {
		add(EventConfig, func(ctx context.Context, _ Event) error {
			return caches.Config.Reload(ctx)
		})
	}
	if caches.Buckets != nil {
		add(EventBucketMetadata, func(ctx context.Context, e Event) error {
			name := e.Name
			if e.Kind == EventResync {
				name = ""
			}
			caches.Buckets.Invalidate(ctx, name)
			return nil
		})
	}
	return func() {
		for _, u := range unsubs {
			u()
		}
	}
}

// iamEventKind is the event of a write in an IAM store namespace.
func iamEventKind(ns iam.Namespace) EventKind {
	switch ns {
	case iam.NamespaceShares:
		return EventShare
	case iam.NamespaceSessions:
		return EventSessionRevocation
	default:
		return EventIAM
	}
}

// notifyingIAMStore broadcasts the writes of the IAM store.
type notifyingIAMStore struct {
	iam.Store
	c *coherence
}

// Put implements iam.Store.
func (s notifyingIAMStore) Put(ctx context.Context, ns iam.Namespace, key string, value []byte) error {
	if err := s.Store.Put(ctx, ns, key, value); err != nil {
		return err
	}
	s.c.send(ctx, iamEventKind(ns), "")
	return nil
}

// Delete implements iam.Store.
func (s notifyingIAMStore) Delete(ctx context.Context, ns iam.Namespace, key string) error {
	if err := s.Store.Delete(ctx, ns, key); err != nil {
		return err
	}
	s.c.send(ctx, iamEventKind(ns), "")
	return nil
}

// notifyingPolicyStore broadcasts the writes of the named policies.
type notifyingPolicyStore struct {
	policy.SharedStore
	c *coherence
}

// Set implements policy.PolicyStore.
func (s notifyingPolicyStore) Set(ctx context.Context, p policy.Named) error {
	if err := s.SharedStore.Set(ctx, p); err != nil {
		return err
	}
	s.c.send(ctx, EventPolicy, p.Name)
	return nil
}

// Delete implements policy.PolicyStore.
func (s notifyingPolicyStore) Delete(ctx context.Context, name string) error {
	if err := s.SharedStore.Delete(ctx, name); err != nil {
		return err
	}
	s.c.send(ctx, EventPolicy, name)
	return nil
}

// SetAttached implements policy.PolicyStore.
func (s notifyingPolicyStore) SetAttached(ctx context.Context, e policy.Entity, names []string) error {
	if err := s.SharedStore.SetAttached(ctx, e, names); err != nil {
		return err
	}
	s.c.send(ctx, EventPolicy, "")
	return nil
}

// notifyingConfigStore broadcasts the writes of the dynamic configuration.
type notifyingConfigStore struct {
	configsvc.Store
	c *coherence
}

// Save implements config.Store.
func (s notifyingConfigStore) Save(ctx context.Context, all map[string]configsvc.KV) error {
	if err := s.Store.Save(ctx, all); err != nil {
		return err
	}
	s.c.send(ctx, EventConfig, "")
	return nil
}

// notifyingBucketStore broadcasts the writes of the bucket metadata.
type notifyingBucketStore struct {
	bucket.MetadataStore
	c *coherence
}

// Save implements bucket.MetadataStore.
func (s notifyingBucketStore) Save(ctx context.Context, name string, m *bucket.Metadata) error {
	if err := s.MetadataStore.Save(ctx, name, m); err != nil {
		return err
	}
	s.c.send(ctx, EventBucketMetadata, name)
	return nil
}

// Delete implements bucket.MetadataStore.
func (s notifyingBucketStore) Delete(ctx context.Context, name string) error {
	if err := s.MetadataStore.Delete(ctx, name); err != nil {
		return err
	}
	s.c.send(ctx, EventBucketMetadata, name)
	return nil
}
