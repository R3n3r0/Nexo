package distribution

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	"github.com/r3n3r0/nexo/internal/service/match"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// Locations of the documents in the system store, and the configuration
// lock of the writes.
const (
	distributionsDir = "distributions"
	keyGroupsDir     = "keygroups"
	docSuffix        = ".json"
	configLock       = "nexo-config/distributions"
)

// service implements Service.
type service struct {
	deps Deps
	opts options

	// table is the routing table, keys the key groups: swapped
	// atomically, read without locks.
	table atomic.Pointer[table]
	keys  atomic.Pointer[keySet]

	// reloadMu serializes the reloads and the local applications of a
	// change, so that an older state never replaces a newer one.
	reloadMu sync.Mutex

	lifeMu      sync.Mutex
	unsubscribe []func()
}

var _ Service = (*service)(nil)

// New returns the distribution service. The store, the locker and the
// notifier are required (domain.ErrInvalidArgument). The routing table is
// empty until Start (or Reload) loads the distributions.
func New(deps Deps, opts ...Option) (Service, error) {
	switch {
	case deps.Store == nil:
		return nil, fmt.Errorf("%w: distribution: the system store is required", domain.ErrInvalidArgument)
	case deps.Locks == nil:
		return nil, fmt.Errorf("%w: distribution: the locker is required", domain.ErrInvalidArgument)
	case deps.Notifier == nil:
		return nil, fmt.Errorf("%w: distribution: the notifier is required", domain.ErrInvalidArgument)
	}
	if deps.Matcher == nil {
		deps.Matcher = match.New(match.Deps{})
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	s := &service{deps: deps, opts: o}
	s.table.Store(newTable(nil))
	s.keys.Store(newKeySet(nil))
	return s, nil
}

// Start implements lifecycle.Runner: it subscribes to the invalidations of
// the peers and loads the distributions and key groups. A store without
// quorum does not fail the start: the table stays empty and the next
// resynchronization of the notifier loads it.
func (s *service) Start(ctx context.Context) error {
	s.lifeMu.Lock()
	if len(s.unsubscribe) == 0 {
		h := cluster.HandlerFunc(func(ctx context.Context, _ cluster.Event) error { return s.Reload(ctx) })
		s.unsubscribe = append(s.unsubscribe,
			s.deps.Notifier.Subscribe(EventDistribution, h),
			s.deps.Notifier.Subscribe(EventKeyGroup, h))
	}
	s.lifeMu.Unlock()
	if err := s.Reload(ctx); err != nil {
		s.deps.Logger.WarnContext(ctx, "distribution: initial load failed, retried on the next resynchronization", "err", err)
	}
	return nil
}

// Stop implements lifecycle.Runner.
func (s *service) Stop(context.Context) error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	for _, u := range s.unsubscribe {
		u()
	}
	s.unsubscribe = nil
	return nil
}

// Reload implements Service.
func (s *service) Reload(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	ds, err := s.loadDistributions(ctx)
	if err != nil {
		return err
	}
	groups, err := s.loadKeyGroups(ctx)
	if err != nil {
		return err
	}
	for _, d := range ds {
		for _, a := range d.Aliases {
			if why := s.reservedAlias(a, d.Origin.Bucket); why != "" {
				s.deps.Logger.WarnContext(ctx, "distribution: alias not routed", "distribution", d.ID, "alias", a, "why", why)
			}
		}
	}
	s.table.Store(newTable(ds, s.reservedAlias))
	s.keys.Store(newKeySet(groups))
	return nil
}

// loadDistributions reads every distribution of the store; a document that
// cannot be decoded is logged and skipped.
func (s *service) loadDistributions(ctx context.Context) ([]*Distribution, error) {
	docs, err := s.deps.Store.List(ctx, distributionsDir)
	if err != nil {
		return nil, fmt.Errorf("distribution: list: %w", err)
	}
	out := make([]*Distribution, 0, len(docs))
	for name, data := range docs {
		if !strings.HasSuffix(name, docSuffix) {
			continue
		}
		d, err := decodeDistribution(data)
		if err != nil {
			s.deps.Logger.WarnContext(ctx, "distribution: skipping an unreadable document", "document", name, "err", err)
			continue
		}
		out = append(out, &d)
	}
	slices.SortFunc(out, func(a, b *Distribution) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// decodeDistribution decodes a stored distribution and computes its ETag.
func decodeDistribution(data []byte) (Distribution, error) {
	var d Distribution
	if err := json.Unmarshal(data, &d); err != nil {
		return d, err
	}
	if !validID(d.ID) {
		return d, fmt.Errorf("invalid id %q", d.ID)
	}
	d.ETag = etagOf(data)
	return d, nil
}

// etagOf identifies a stored document.
func etagOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// docPath is the path of a document of dir.
func docPath(dir, id string) string { return dir + "/" + id + docSuffix }

// lockConfig takes the configuration lock of the distributions and key
// groups (every node: distributed in a cluster).
func (s *service) lockConfig(ctx context.Context) (func(), error) {
	if err := s.deps.Locks.Lock(ctx, configLock, s.opts.lockTimeout); err != nil {
		return nil, fmt.Errorf("distribution: configuration lock: %w", err)
	}
	return func() { _ = s.deps.Locks.Unlock(context.WithoutCancel(ctx), configLock) }, nil
}

// broadcast tells the peers that a document changed; a peer missed is
// retried by the notifier, never an error of the write.
func (s *service) broadcast(ctx context.Context, kind cluster.EventKind, id string) {
	if err := s.deps.Notifier.Broadcast(context.WithoutCancel(ctx), cluster.Event{Kind: kind, Name: id}); err != nil {
		s.deps.Logger.WarnContext(ctx, "distribution: change not delivered to every peer (retried in the background)",
			"kind", string(kind), "id", id, "err", err)
	}
}

// apply reloads the local state after a write of this node.
func (s *service) apply(ctx context.Context) {
	if err := s.Reload(ctx); err != nil {
		s.deps.Logger.WarnContext(ctx, "distribution: reload after a write failed", "err", err)
	}
}

// Validate implements Service.
func (s *service) Validate(ctx context.Context, d Distribution) (Distribution, error) {
	n, err := s.normalize(ctx, d)
	if err != nil {
		return n, err
	}
	ds, err := s.loadDistributions(ctx)
	if err != nil {
		return n, err
	}
	groups, err := s.loadKeyGroups(ctx)
	if err != nil {
		return n, err
	}
	return n, s.checkReferences(n, ds, groups)
}

// checkReferences checks d against the stored distributions and key
// groups: alias uniqueness, existing key groups.
func (s *service) checkReferences(d Distribution, ds []*Distribution, groups []*KeyGroup) error {
	owner := map[string]string{}
	for _, other := range ds {
		if other.ID == d.ID {
			continue
		}
		for _, a := range other.Aliases {
			owner[a] = other.ID
		}
	}
	for _, a := range d.Aliases {
		if id, taken := owner[a]; taken {
			return fmt.Errorf("%w: %s is an alias of %s", ErrAliasConflict, a, id)
		}
	}
	known := map[string]bool{}
	for _, g := range groups {
		known[g.ID] = true
	}
	for _, b := range append([]Behavior{d.DefaultBehavior}, d.Behaviors...) {
		if b.Signing != nil && !known[b.Signing.KeyGroup] {
			return fmt.Errorf("%w: signing.keyGroup %q does not exist", ErrInvalid, b.Signing.KeyGroup)
		}
	}
	return nil
}

// Create implements Service.
func (s *service) Create(ctx context.Context, d Distribution) (Distribution, error) {
	return s.save(ctx, d, true, "")
}

// Update implements Service.
func (s *service) Update(ctx context.Context, d Distribution, ifMatch string) (Distribution, error) {
	return s.save(ctx, d, false, ifMatch)
}

// save validates and stores a distribution under the configuration lock.
func (s *service) save(ctx context.Context, d Distribution, create bool, ifMatch string) (Distribution, error) {
	n, err := s.normalize(ctx, d)
	if err != nil {
		return Distribution{}, err
	}
	unlock, err := s.lockConfig(ctx)
	if err != nil {
		return Distribution{}, err
	}
	defer unlock()
	ds, err := s.loadDistributions(ctx)
	if err != nil {
		return Distribution{}, err
	}
	groups, err := s.loadKeyGroups(ctx)
	if err != nil {
		return Distribution{}, err
	}
	var current *Distribution
	for _, other := range ds {
		if other.ID == n.ID {
			current = other
		}
	}
	now := s.deps.Clock.Now().UTC()
	switch {
	case create && current != nil:
		return Distribution{}, fmt.Errorf("%w: distribution %s", ErrAlreadyExists, n.ID)
	case create && len(ds) >= s.opts.maxDistributions:
		return Distribution{}, fmt.Errorf("%w: at most %d distributions", ErrInvalid, s.opts.maxDistributions)
	case !create && current == nil:
		return Distribution{}, fmt.Errorf("%w: %s", ErrDistributionNotFound, n.ID)
	case !create && ifMatch != "" && strings.Trim(ifMatch, `"`) != current.ETag:
		return Distribution{}, fmt.Errorf("distribution %s changed (ETag %s): %w", n.ID, current.ETag, domain.ErrPreconditionFailed)
	}
	if err := s.checkReferences(n, ds, groups); err != nil {
		return Distribution{}, err
	}
	n.CreatedAt = now
	if current != nil {
		n.CreatedAt = current.CreatedAt
	}
	n.UpdatedAt = now
	stored, err := s.put(ctx, n)
	if err != nil {
		return Distribution{}, err
	}
	s.apply(ctx)
	s.broadcast(ctx, EventDistribution, n.ID)
	s.deps.Logger.InfoContext(ctx, "distribution saved", "id", n.ID, "aliases", n.Aliases, "mode", string(n.Mode), "created", create)
	return stored, nil
}

// put writes a distribution and returns it with its ETag.
func (s *service) put(ctx context.Context, d Distribution) (Distribution, error) {
	d.ETag = ""
	data, err := json.Marshal(d)
	if err != nil {
		return Distribution{}, fmt.Errorf("distribution: encode %s: %w", d.ID, err)
	}
	if err := s.deps.Store.Put(ctx, docPath(distributionsDir, d.ID), data); err != nil {
		return Distribution{}, fmt.Errorf("distribution: store %s: %w", d.ID, err)
	}
	d.ETag = etagOf(data)
	return d, nil
}

// Get implements Service: it reads the store, so that a caller sees its
// own writes on any node.
func (s *service) Get(ctx context.Context, id string) (Distribution, error) {
	if !validID(id) {
		return Distribution{}, fmt.Errorf("%w: %q", ErrDistributionNotFound, id)
	}
	data, err := s.deps.Store.Get(ctx, docPath(distributionsDir, id))
	if errors.Is(err, sysstore.ErrNotFound) {
		return Distribution{}, fmt.Errorf("%w: %s", ErrDistributionNotFound, id)
	}
	if err != nil {
		return Distribution{}, fmt.Errorf("distribution: get %s: %w", id, err)
	}
	return decodeDistribution(data)
}

// List implements Service.
func (s *service) List(ctx context.Context) ([]Distribution, error) {
	ds, err := s.loadDistributions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Distribution, 0, len(ds))
	for _, d := range ds {
		out = append(out, *d)
	}
	return out, nil
}

// Delete implements Service.
func (s *service) Delete(ctx context.Context, id string) error {
	if !validID(id) {
		return fmt.Errorf("%w: %q", ErrDistributionNotFound, id)
	}
	unlock, err := s.lockConfig(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	if err := s.deps.Store.Delete(ctx, docPath(distributionsDir, id)); err != nil {
		return fmt.Errorf("distribution: delete %s: %w", id, err)
	}
	s.apply(ctx)
	s.broadcast(ctx, EventDistribution, id)
	s.deps.Logger.InfoContext(ctx, "distribution deleted", "id", id)
	return nil
}

// SetEnabled implements Service.
func (s *service) SetEnabled(ctx context.Context, id string, enabled bool) (Distribution, error) {
	d, err := s.Get(ctx, id)
	if err != nil {
		return Distribution{}, err
	}
	d.Enabled = enabled
	return s.Update(ctx, d, d.ETag)
}

// RotateOriginSecret implements Service.
func (s *service) RotateOriginSecret(ctx context.Context, in RotateSecretInput) (string, Distribution, error) {
	d, err := s.Get(ctx, in.ID)
	if err != nil {
		return "", Distribution{}, err
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", Distribution{}, fmt.Errorf("distribution: generate secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	sec := d.Restrictions.OriginSecretHeader
	if sec == nil {
		sec = &OriginSecret{Name: DefaultOriginSecretHeader}
	}
	if in.Header != "" {
		sec.Name = in.Header
	}
	keep := in.Keep
	if keep <= 0 && !in.DropPrevious {
		keep = 1
	}
	if in.DropPrevious {
		keep = 0
	}
	previous := sec.Values
	if len(previous) > keep {
		previous = previous[len(previous)-keep:]
	}
	// The newest secret comes last: a later rotation keeps the most
	// recent previous ones.
	sec.Values = append(slices.Clone(previous), secret)
	sec.Value = ""
	d.Restrictions.OriginSecretHeader = sec
	out, err := s.Update(ctx, d, d.ETag)
	if err != nil {
		return "", Distribution{}, err
	}
	return secret, out, nil
}

// Test implements Service.
func (s *service) Test(ctx context.Context, in TestInput) (TestResult, error) {
	u, err := parseTestURL(in.URL)
	if err != nil {
		return TestResult{}, err
	}
	route := s.Resolve(ctx, u.Host)
	res := TestResult{Route: route, BehaviorIndex: -1}
	if route.Kind != RouteDistribution {
		return res, nil
	}
	c, err := compileDistribution(ctx, s.deps.Matcher, route.Distribution)
	if err != nil {
		return res, err
	}
	method := strings.ToUpper(in.Method)
	if method == "" {
		method = http.MethodGet
	}
	secure := u.Scheme == "https"
	p := c.plan(ctx, planRequest{
		host: u.Host, method: method, path: u.Path, escapedPath: u.EscapedPath(), rawQuery: u.RawQuery,
		secure: secure, clientIP: in.ClientIP, header: http.Header{},
	})
	res.BehaviorIndex = p.behaviorIndex
	res.Behavior = p.behavior
	if p.admission {
		// Refused before the path is looked at (origin secret, address,
		// disabled): the behavior of the path is still reported, so that
		// the signing of a URL of a distribution behind proxies finds
		// its key group.
		path := u.Path
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		res.BehaviorIndex, res.Behavior = c.behaviorFor(ctx, path)
	}
	res.Bucket = c.d.Origin.Bucket
	res.Key = p.key
	res.RedirectStatus, res.RedirectLocation = p.redirectStatus, p.redirectLocation
	res.RefusalStatus, res.Refusal = p.refusalStatus, p.refusal
	res.ResponseHeaders = c.responseHeaders(p.behavior)
	return res, nil
}
