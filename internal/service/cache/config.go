package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// EventConfig is the kind of the cluster invalidations of the cache
// configuration (global settings, policies, distribution settings).
const EventConfig cluster.EventKind = "cache-config"

// Locations of the documents in the system store.
const (
	globalDoc        = "cache/global.json"
	policiesDir      = "cache/policies"
	distributionsDir = "cache/distributions"
	invalidationsDir = "cache/invalidations"
	warmDir          = "cache/warm"
	docSuffix        = ".json"
	configLock       = "nexo-config/cache"
	// globalScope is the history directory of the global purges ('~' is
	// not allowed in the ids of the distributions).
	globalScope = "~global"
)

// secretMask replaces a secret in the documents returned.
const secretMask = ""

// --- snapshot ---------------------------------------------------------------------

// compiledDist is the cache configuration of a distribution ready for the
// requests.
type compiledDist struct {
	cfg DistributionConfig
	// exact maps the exact domains to their index, wild lists the
	// wildcard ones, the longest suffix first.
	exact     map[string]int
	wild      []int
	behaviors pathPatterns
	warm      pathPatterns
	// fp identifies the settings that change what is cached.
	fp string
}

// domainFor returns the index of the domain settings of host (-1: none).
func (c *compiledDist) domainFor(host string) int {
	if i, ok := c.exact[host]; ok {
		return i
	}
	for _, i := range c.wild {
		if hostMatches(c.cfg.Domains[i].Host, host) {
			return i
		}
	}
	return -1
}

// behaviorFor returns the index of the first behavior matching path (-1:
// none).
func (c *compiledDist) behaviorFor(ctx context.Context, path string) int {
	return c.behaviors.first(ctx, path)
}

// snapshot is the cache configuration of the cluster, swapped atomically
// on every change.
type snapshot struct {
	global   GlobalConfig
	policies map[string]Policy
	dists    map[string]*compiledDist
	// memo caches the resolutions by distribution, domain and behavior.
	memo sync.Map
}

// resolveFor returns the effective policy of a path of a host of a
// distribution.
func (sn *snapshot) resolveFor(ctx context.Context, id, host, path string) *resolution {
	cd := sn.dists[id]
	di, bi := -1, -1
	if cd != nil {
		di, bi = cd.domainFor(host), cd.behaviorFor(ctx, path)
	}
	key := id + "\x00" + strconv.Itoa(di) + "\x00" + strconv.Itoa(bi)
	if v, ok := sn.memo.Load(key); ok {
		return v.(*resolution)
	}
	levels := []levelSettings{{level: LevelGlobal, s: &sn.global.Cache}}
	r := &resolution{}
	if cd != nil {
		levels = append(levels, levelSettings{level: LevelDistribution, s: &cd.cfg.Cache})
		if q := cd.cfg.Cache.Quota; q != nil {
			r.distQuota = int64(*q)
		}
		if di >= 0 {
			d := &cd.cfg.Domains[di]
			levels = append(levels, levelSettings{level: LevelDomain, s: &d.Cache})
			r.domainKey = d.Host
			if q := d.Cache.Quota; q != nil {
				r.domainQuota = int64(*q)
			}
		}
		if bi >= 0 {
			levels = append(levels, levelSettings{level: LevelBehavior, s: &cd.cfg.Behaviors[bi].Cache})
		}
	}
	r.eff = resolve(levels, sn.policies)
	r.eff.Behavior = bi
	r.eff.Domain = r.domainKey
	r.eff.DistributionQuota, r.eff.DomainQuota = Size(r.distQuota), Size(r.domainQuota)
	r.p = paramsOf(r.eff.Policy, r.eff.Enabled, r.eff.AutoInvalidate)
	sn.memo.Store(key, r)
	return r
}

// distReferences reports whether a distribution configuration names the
// policy.
func distReferences(cfg DistributionConfig, name string) bool {
	if cfg.Cache.Policy == name {
		return true
	}
	for _, d := range cfg.Domains {
		if d.Cache.Policy == name {
			return true
		}
	}
	for _, b := range cfg.Behaviors {
		if b.Cache.Policy == name {
			return true
		}
	}
	return false
}

// fingerprint identifies the settings of a configuration that change what
// is cached (not the quotas, webhooks and warm-up patterns).
func fingerprint(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// cacheSettingsOf strips the settings that do not change the entries.
func cacheSettingsOf(s Settings) Settings {
	s.Quota, s.PurgeWebhooks = nil, nil
	return s
}

// distFingerprint is the fingerprint of a distribution configuration.
func distFingerprint(cfg DistributionConfig) string {
	type domainFP struct {
		Host  string
		Cache Settings
	}
	type behaviorFP struct {
		Pattern distribution.PathPattern
		Cache   Settings
	}
	v := struct {
		Cache     Settings
		Domains   []domainFP
		Behaviors []behaviorFP
	}{Cache: cacheSettingsOf(cfg.Cache)}
	for _, d := range cfg.Domains {
		v.Domains = append(v.Domains, domainFP{Host: d.Host, Cache: cacheSettingsOf(d.Cache)})
	}
	for _, b := range cfg.Behaviors {
		v.Behaviors = append(v.Behaviors, behaviorFP{Pattern: b.PathPattern, Cache: cacheSettingsOf(b.Cache)})
	}
	return fingerprint(v)
}

// compileDist compiles the configuration of a distribution.
func (s *service) compileDist(ctx context.Context, cfg DistributionConfig) (*compiledDist, error) {
	c := &compiledDist{cfg: cfg, exact: map[string]int{}, fp: distFingerprint(cfg)}
	for i, d := range cfg.Domains {
		if strings.HasPrefix(d.Host, "*.") {
			c.wild = append(c.wild, i)
		} else {
			c.exact[d.Host] = i
		}
	}
	sort.SliceStable(c.wild, func(a, b int) bool { return len(cfg.Domains[c.wild[a]].Host) > len(cfg.Domains[c.wild[b]].Host) })
	patterns := make([]distribution.PathPattern, 0, len(cfg.Behaviors))
	for _, b := range cfg.Behaviors {
		patterns = append(patterns, b.PathPattern)
	}
	var err error
	if c.behaviors, err = compilePatterns(ctx, s.deps.Matcher, patterns, "behaviors"); err != nil {
		return nil, err
	}
	if c.warm, err = compilePatterns(ctx, s.deps.Matcher, cfg.WarmOnWrite, "warmOnWrite"); err != nil {
		return nil, err
	}
	return c, nil
}

// --- documents ----------------------------------------------------------------------

// etagOf identifies a stored document.
func etagOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

func docPath(dir, name string) string { return dir + "/" + name + docSuffix }

// lockConfig takes the configuration lock of the cache.
func (s *service) lockConfig(ctx context.Context) (func(), error) {
	if err := s.deps.Locks.Lock(ctx, configLock, s.opts.lockTimeout); err != nil {
		return nil, fmt.Errorf("cache: configuration lock: %w", err)
	}
	return func() { _ = s.deps.Locks.Unlock(context.WithoutCancel(ctx), configLock) }, nil
}

// loadPolicies reads the defined policies (and adds the built-in ones).
func (s *service) loadPolicies(ctx context.Context) (map[string]Policy, error) {
	out := builtinPolicies()
	docs, err := s.deps.Store.List(ctx, policiesDir)
	if err != nil {
		return nil, fmt.Errorf("cache: list policies: %w", err)
	}
	for name, data := range docs {
		if !strings.HasSuffix(name, docSuffix) {
			continue
		}
		var p Policy
		if err := json.Unmarshal(data, &p); err != nil || !validName(p.Name) {
			s.deps.Logger.WarnContext(ctx, "cache: skipping an unreadable policy", "document", name, "err", err)
			continue
		}
		if _, builtin := out[p.Name]; builtin {
			continue
		}
		p.PolicySpec = complete(p.PolicySpec)
		p.ETag = etagOf(data)
		out[p.Name] = p
	}
	return out, nil
}

// loadGlobal reads the global settings (the zero settings when missing).
func (s *service) loadGlobal(ctx context.Context) (GlobalConfig, error) {
	data, err := s.deps.Store.Get(ctx, globalDoc)
	if errors.Is(err, sysstore.ErrNotFound) {
		return GlobalConfig{}, nil
	}
	if err != nil {
		return GlobalConfig{}, fmt.Errorf("cache: read the global settings: %w", err)
	}
	var g GlobalConfig
	if err := json.Unmarshal(data, &g); err != nil {
		return GlobalConfig{}, fmt.Errorf("cache: decode the global settings: %w", err)
	}
	g.ETag = etagOf(data)
	return g, nil
}

// loadDistConfigs reads the configurations of the distributions.
func (s *service) loadDistConfigs(ctx context.Context) (map[string]DistributionConfig, error) {
	docs, err := s.deps.Store.List(ctx, distributionsDir)
	if err != nil {
		return nil, fmt.Errorf("cache: list the distribution settings: %w", err)
	}
	out := make(map[string]DistributionConfig, len(docs))
	for name, data := range docs {
		if !strings.HasSuffix(name, docSuffix) {
			continue
		}
		var c DistributionConfig
		if err := json.Unmarshal(data, &c); err != nil || c.Distribution == "" {
			s.deps.Logger.WarnContext(ctx, "cache: skipping unreadable distribution settings", "document", name, "err", err)
			continue
		}
		c.ETag = etagOf(data)
		out[c.Distribution] = c
	}
	return out, nil
}

// loadDistConfig reads the configuration of a distribution (an empty one
// when missing).
func (s *service) loadDistConfig(ctx context.Context, id string) (DistributionConfig, error) {
	data, err := s.deps.Store.Get(ctx, docPath(distributionsDir, id))
	if errors.Is(err, sysstore.ErrNotFound) {
		return DistributionConfig{Distribution: id}, nil
	}
	if err != nil {
		return DistributionConfig{}, fmt.Errorf("cache: read the settings of %s: %w", id, err)
	}
	var c DistributionConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return DistributionConfig{}, fmt.Errorf("cache: decode the settings of %s: %w", id, err)
	}
	c.ETag = etagOf(data)
	return c, nil
}

// putDoc stores a document and returns its ETag.
func (s *service) putDoc(ctx context.Context, path string, v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("cache: encode %s: %w", path, err)
	}
	if err := s.deps.Store.Put(ctx, path, data); err != nil {
		return "", fmt.Errorf("cache: store %s: %w", path, err)
	}
	return etagOf(data), nil
}

// Reload reloads the configuration from the store and drops the entries
// whose settings changed.
func (s *service) Reload(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	policies, err := s.loadPolicies(ctx)
	if err != nil {
		return err
	}
	global, err := s.loadGlobal(ctx)
	if err != nil {
		return err
	}
	cfgs, err := s.loadDistConfigs(ctx)
	if err != nil {
		return err
	}
	sn := &snapshot{global: global, policies: policies, dists: make(map[string]*compiledDist, len(cfgs))}
	for id, cfg := range cfgs {
		c, err := s.compileDist(ctx, cfg)
		if err != nil {
			s.deps.Logger.WarnContext(ctx, "cache: skipping invalid distribution settings", "distribution", id, "err", err)
			continue
		}
		sn.dists[id] = c
	}
	old := s.snap.Swap(sn)
	s.purgeChanged(ctx, old, sn)
	return nil
}

// purgeChanged drops the entries whose settings differ between two
// snapshots: everything when the global settings changed, the entries of
// a distribution when its settings or a policy it names changed.
func (s *service) purgeChanged(ctx context.Context, old, cur *snapshot) {
	if old == nil {
		return
	}
	changed := map[string]bool{}
	for name, p := range cur.policies {
		if q, ok := old.policies[name]; !ok || q.ETag != p.ETag {
			changed[name] = true
		}
	}
	for name := range old.policies {
		if _, ok := cur.policies[name]; !ok {
			changed[name] = true
		}
	}
	if fingerprint(cacheSettingsOf(old.global.Cache)) != fingerprint(cacheSettingsOf(cur.global.Cache)) || changed[cur.global.Cache.Policy] {
		n := s.store.removeWhere(func(*entryMeta) bool { return true }, false)
		s.deps.Logger.InfoContext(ctx, "cache: global settings changed, entries dropped", "entries", n)
		return
	}
	ids := map[string]bool{}
	for id := range old.dists {
		ids[id] = true
	}
	for id := range cur.dists {
		ids[id] = true
	}
	for id := range ids {
		o, c := old.dists[id], cur.dists[id]
		drop := (o == nil) != (c == nil) || (o != nil && c != nil && o.fp != c.fp)
		if !drop && c != nil {
			for name := range changed {
				if distReferences(c.cfg, name) {
					drop = true
				}
			}
		}
		if drop {
			n := s.store.removeWhere(func(m *entryMeta) bool { return m.Distribution == id }, false)
			s.deps.Logger.InfoContext(ctx, "cache: settings changed, entries dropped", "distribution", id, "entries", n)
		}
	}
}

// broadcastConfig tells the peers that the configuration changed.
func (s *service) broadcastConfig(ctx context.Context, name string) {
	if err := s.deps.Notifier.Broadcast(context.WithoutCancel(ctx), cluster.Event{Kind: EventConfig, Name: name}); err != nil {
		s.deps.Logger.WarnContext(ctx, "cache: configuration change not delivered to every peer (retried)", "name", name, "err", err)
	}
}

// applyConfig reloads the local state after a write of this node and
// tells the peers.
func (s *service) applyConfig(ctx context.Context, name string) {
	if err := s.Reload(ctx); err != nil {
		s.deps.Logger.WarnContext(ctx, "cache: reload after a configuration change failed", "err", err)
	}
	s.broadcastConfig(ctx, name)
}

// --- Manager: policies -------------------------------------------------------------

// ListPolicies implements Manager.
func (s *service) ListPolicies(ctx context.Context) ([]Policy, error) {
	ps, err := s.loadPolicies(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Policy, 0, len(ps))
	for _, name := range slices.Sorted(maps.Keys(ps)) {
		out = append(out, ps[name])
	}
	return out, nil
}

// GetPolicy implements Manager.
func (s *service) GetPolicy(ctx context.Context, name string) (Policy, error) {
	if p, ok := builtinPolicies()[name]; ok {
		return p, nil
	}
	if !validName(name) {
		return Policy{}, fmt.Errorf("%w: %q", ErrPolicyNotFound, name)
	}
	data, err := s.deps.Store.Get(ctx, docPath(policiesDir, name))
	if errors.Is(err, sysstore.ErrNotFound) {
		return Policy{}, fmt.Errorf("%w: %s", ErrPolicyNotFound, name)
	}
	if err != nil {
		return Policy{}, fmt.Errorf("cache: read policy %s: %w", name, err)
	}
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return Policy{}, fmt.Errorf("cache: decode policy %s: %w", name, err)
	}
	p.PolicySpec = complete(p.PolicySpec)
	p.ETag = etagOf(data)
	return p, nil
}

// CreatePolicy implements Manager.
func (s *service) CreatePolicy(ctx context.Context, p Policy) (Policy, error) {
	return s.savePolicy(ctx, p, true, "")
}

// UpdatePolicy implements Manager.
func (s *service) UpdatePolicy(ctx context.Context, p Policy, ifMatch string) (Policy, error) {
	return s.savePolicy(ctx, p, false, ifMatch)
}

func (s *service) savePolicy(ctx context.Context, p Policy, create bool, ifMatch string) (Policy, error) {
	if _, builtin := builtinPolicies()[p.Name]; builtin {
		return Policy{}, fmt.Errorf("%w: %s", ErrBuiltinPolicy, p.Name)
	}
	p, err := validatePolicy(p)
	if err != nil {
		return Policy{}, err
	}
	unlock, err := s.lockConfig(ctx)
	if err != nil {
		return Policy{}, err
	}
	defer unlock()
	cur, err := s.GetPolicy(ctx, p.Name)
	exists := err == nil
	if err != nil && !errors.Is(err, ErrPolicyNotFound) {
		return Policy{}, err
	}
	switch {
	case create && exists:
		return Policy{}, fmt.Errorf("%w: policy %s", ErrAlreadyExists, p.Name)
	case !create && !exists:
		return Policy{}, fmt.Errorf("%w: %s", ErrPolicyNotFound, p.Name)
	case !create && ifMatch != "" && strings.Trim(ifMatch, `"`) != cur.ETag:
		return Policy{}, fmt.Errorf("policy %s changed (ETag %s): %w", p.Name, cur.ETag, domain.ErrPreconditionFailed)
	}
	p.UpdatedAt = s.deps.Clock.Now().UTC()
	p.ETag = ""
	if p.ETag, err = s.putDoc(ctx, docPath(policiesDir, p.Name), p); err != nil {
		return Policy{}, err
	}
	s.applyConfig(ctx, "policy:"+p.Name)
	s.deps.Logger.InfoContext(ctx, "cache policy saved", "policy", p.Name, "created", create)
	return p, nil
}

// DeletePolicy implements Manager.
func (s *service) DeletePolicy(ctx context.Context, name string) error {
	if _, builtin := builtinPolicies()[name]; builtin {
		return fmt.Errorf("%w: %s", ErrBuiltinPolicy, name)
	}
	unlock, err := s.lockConfig(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := s.GetPolicy(ctx, name); err != nil {
		return err
	}
	global, err := s.loadGlobal(ctx)
	if err != nil {
		return err
	}
	if global.Cache.Policy == name {
		return fmt.Errorf("%w: %s is the global policy", ErrPolicyInUse, name)
	}
	cfgs, err := s.loadDistConfigs(ctx)
	if err != nil {
		return err
	}
	for _, id := range slices.Sorted(maps.Keys(cfgs)) {
		if distReferences(cfgs[id], name) {
			return fmt.Errorf("%w: %s is used by distribution %s", ErrPolicyInUse, name, id)
		}
	}
	if err := s.deps.Store.Delete(ctx, docPath(policiesDir, name)); err != nil {
		return fmt.Errorf("cache: delete policy %s: %w", name, err)
	}
	s.applyConfig(ctx, "policy:"+name)
	return nil
}

// PreviewPolicy implements Manager.
func (s *service) PreviewPolicy(_ context.Context, p Policy) (PolicyPreview, error) {
	if p.Name == "" {
		p.Name = "preview"
	}
	if b, builtin := builtinPolicies()[p.Name]; builtin {
		return previewOf(b), nil
	}
	v, err := validatePolicy(p)
	if err != nil {
		return PolicyPreview{}, err
	}
	return previewOf(v), nil
}

// --- Manager: global settings -------------------------------------------------------

// GetGlobal implements Manager.
func (s *service) GetGlobal(ctx context.Context) (GlobalConfig, error) {
	g, err := s.loadGlobal(ctx)
	if err != nil {
		return GlobalConfig{}, err
	}
	return maskGlobal(g), nil
}

// maskGlobal hides the debug secret (references are shown).
func maskGlobal(g GlobalConfig) GlobalConfig {
	if g.Debug != nil {
		d := *g.Debug
		if _, ref := secretRef(d.Secret); !ref {
			d.Secret = secretMask
		}
		g.Debug = &d
	}
	return g
}

// UpdateGlobal implements Manager.
func (s *service) UpdateGlobal(ctx context.Context, g GlobalConfig, ifMatch string) (GlobalConfig, error) {
	policies, err := s.loadPolicies(ctx)
	if err != nil {
		return GlobalConfig{}, err
	}
	if g.Cache.Quota != nil || len(g.Cache.PurgeWebhooks) > 0 {
		return GlobalConfig{}, fmt.Errorf("%w: the global settings have no quota (--cache-size) and no webhooks", ErrInvalid)
	}
	if err := validateSettings("cache.", g.Cache, policies, nil); err != nil {
		return GlobalConfig{}, err
	}
	if d := g.Debug; d != nil && d.Header != "" && !validToken(d.Header) {
		return GlobalConfig{}, fmt.Errorf("%w: debug.header %q is not a header name", ErrInvalid, d.Header)
	}
	unlock, err := s.lockConfig(ctx)
	if err != nil {
		return GlobalConfig{}, err
	}
	defer unlock()
	cur, err := s.loadGlobal(ctx)
	if err != nil {
		return GlobalConfig{}, err
	}
	if ifMatch != "" && strings.Trim(ifMatch, `"`) != cur.ETag {
		return GlobalConfig{}, fmt.Errorf("cache global settings changed (ETag %s): %w", cur.ETag, domain.ErrPreconditionFailed)
	}
	if g.Debug != nil && g.Debug.Secret == secretMask && cur.Debug != nil {
		g.Debug.Secret = cur.Debug.Secret
	}
	g.UpdatedAt = s.deps.Clock.Now().UTC()
	g.ETag = ""
	if g.ETag, err = s.putDoc(ctx, globalDoc, g); err != nil {
		return GlobalConfig{}, err
	}
	s.applyConfig(ctx, "global")
	return maskGlobal(g), nil
}

// --- Manager: distributions --------------------------------------------------------

// GetDistribution implements Manager.
func (s *service) GetDistribution(ctx context.Context, id string) (DistributionConfig, error) {
	if _, err := s.deps.Distributions.Get(ctx, id); err != nil {
		return DistributionConfig{}, err
	}
	c, err := s.loadDistConfig(ctx, id)
	if err != nil {
		return DistributionConfig{}, err
	}
	return maskDist(c), nil
}

// maskDist hides the secrets of the webhooks (references are shown).
func maskDist(c DistributionConfig) DistributionConfig {
	c.Webhooks = slices.Clone(c.Webhooks)
	for i := range c.Webhooks {
		c.Webhooks[i] = maskWebhook(c.Webhooks[i])
	}
	return c
}

func maskWebhook(w Webhook) Webhook {
	if _, ref := secretRef(w.Token); !ref {
		w.Token = secretMask
	}
	if _, ref := secretRef(w.ClientSecret); !ref {
		w.ClientSecret = secretMask
	}
	return w
}

// UpdateDistribution implements Manager.
func (s *service) UpdateDistribution(ctx context.Context, cfg DistributionConfig, ifMatch string) (DistributionConfig, error) {
	d, err := s.deps.Distributions.Get(ctx, cfg.Distribution)
	if err != nil {
		return DistributionConfig{}, err
	}
	policies, err := s.loadPolicies(ctx)
	if err != nil {
		return DistributionConfig{}, err
	}
	unlock, err := s.lockConfig(ctx)
	if err != nil {
		return DistributionConfig{}, err
	}
	defer unlock()
	cur, err := s.loadDistConfig(ctx, cfg.Distribution)
	if err != nil {
		return DistributionConfig{}, err
	}
	if ifMatch != "" && strings.Trim(ifMatch, `"`) != cur.ETag {
		return DistributionConfig{}, fmt.Errorf("cache settings of %s changed (ETag %s): %w", cfg.Distribution, cur.ETag, domain.ErrPreconditionFailed)
	}
	if err := keepSecrets(&cfg, cur); err != nil {
		return DistributionConfig{}, err
	}
	if err := s.validateDistConfig(ctx, &cfg, d, policies); err != nil {
		return DistributionConfig{}, err
	}
	return s.storeDistConfig(ctx, cfg)
}

// storeDistConfig writes a validated configuration (configuration lock
// held).
func (s *service) storeDistConfig(ctx context.Context, cfg DistributionConfig) (DistributionConfig, error) {
	cfg.UpdatedAt = s.deps.Clock.Now().UTC()
	cfg.ETag = ""
	var err error
	if cfg.ETag, err = s.putDoc(ctx, docPath(distributionsDir, cfg.Distribution), cfg); err != nil {
		return DistributionConfig{}, err
	}
	s.applyConfig(ctx, "distribution:"+cfg.Distribution)
	return maskDist(cfg), nil
}

// keepSecrets keeps the stored secrets of the webhooks given without
// them (masked), only while the webhook keeps its destination: a secret is
// never sent to an endpoint other than the one it was entered for, so a
// webhook whose type, URL, zone or Akamai client changed needs its secrets
// again.
func keepSecrets(cfg *DistributionConfig, cur DistributionConfig) error {
	stored := map[string]Webhook{}
	for _, w := range cur.Webhooks {
		stored[w.ID] = w
	}
	for i, w := range cfg.Webhooks {
		old, ok := stored[w.ID]
		if !ok {
			continue
		}
		sameTarget := old.Type == w.Type && old.URL == w.URL && old.Zone == w.Zone && old.ClientToken == w.ClientToken &&
			old.Network == w.Network
		for _, f := range []struct {
			name     string
			now, was string
			dst      *string
		}{
			{"token", w.Token, old.Token, &cfg.Webhooks[i].Token},
			{"clientSecret", w.ClientSecret, old.ClientSecret, &cfg.Webhooks[i].ClientSecret},
		} {
			if f.now != secretMask || f.was == "" {
				continue
			}
			if !sameTarget {
				if _, ref := secretRef(f.was); !ref {
					return fmt.Errorf("%w: webhook %s: its type, URL or zone changed: enter its %s again", ErrInvalid, w.ID, f.name)
				}
				// A reference is shown, not masked: an empty value
				// drops it.
				continue
			}
			*f.dst = f.was
		}
	}
	return nil
}

// validateDistConfig checks and normalizes the configuration of
// distribution d.
func (s *service) validateDistConfig(ctx context.Context, cfg *DistributionConfig, d distribution.Distribution, policies map[string]Policy) error {
	webhooks := map[string]bool{}
	for i := range cfg.Webhooks {
		w := &cfg.Webhooks[i]
		if err := s.validateWebhook(w); err != nil {
			return fmt.Errorf("webhooks[%d]: %w", i, err)
		}
		if webhooks[w.ID] {
			return fmt.Errorf("%w: webhook %s defined twice", ErrInvalid, w.ID)
		}
		webhooks[w.ID] = true
		for j, h := range w.Domains {
			h = normalizeHost(h)
			if !domainOf(d, h) {
				return fmt.Errorf("%w: webhooks[%d].domains: %q is not a domain of %s", ErrInvalid, i, h, d.ID)
			}
			w.Domains[j] = h
		}
	}
	if err := validateSettings("cache.", cfg.Cache, policies, webhooks); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i := range cfg.Domains {
		ds := &cfg.Domains[i]
		ds.Host = normalizeHost(ds.Host)
		if !domainOf(d, ds.Host) {
			return fmt.Errorf("%w: domains[%d]: %q is not a domain of %s", ErrInvalid, i, ds.Host, d.ID)
		}
		if seen[ds.Host] {
			return fmt.Errorf("%w: domains[%d]: %s listed twice", ErrInvalid, i, ds.Host)
		}
		seen[ds.Host] = true
		if ds.Cache.AutoInvalidate != nil {
			return fmt.Errorf("%w: domains[%d]: autoInvalidate is a setting of the distribution", ErrInvalid, i)
		}
		if err := validateSettings(fmt.Sprintf("domains[%d].cache.", i), ds.Cache, policies, webhooks); err != nil {
			return err
		}
	}
	for i, b := range cfg.Behaviors {
		if b.Cache.Quota != nil || len(b.Cache.PurgeWebhooks) > 0 || b.Cache.AutoInvalidate != nil {
			return fmt.Errorf("%w: behaviors[%d]: quota, purgeWebhooks and autoInvalidate are not behavior settings", ErrInvalid, i)
		}
		if err := validateSettings(fmt.Sprintf("behaviors[%d].cache.", i), b.Cache, policies, webhooks); err != nil {
			return err
		}
	}
	if len(cfg.Cache.PurgeWebhooks) > 0 {
		return fmt.Errorf("%w: cache.purgeWebhooks is a domain setting (or list the domains of the webhook)", ErrInvalid)
	}
	if _, err := s.compileDist(ctx, *cfg); err != nil {
		return err
	}
	return nil
}

// domainOf reports whether host is a domain of the distribution: an
// alias, a wildcard alias, or a host a wildcard alias matches.
func domainOf(d distribution.Distribution, host string) bool {
	if host == "" {
		return false
	}
	for _, a := range d.Aliases {
		if a == host || hostMatches(a, host) {
			return true
		}
	}
	return false
}

// normalizeHost is the canonical host of the distributions: the cache
// compares the hosts exactly as the routing table does.
func normalizeHost(h string) string { return distribution.NormalizeHost(h) }

// DeleteDistribution implements Manager.
func (s *service) DeleteDistribution(ctx context.Context, id string) error {
	if !validName(id) {
		return fmt.Errorf("%w: %q", distribution.ErrDistributionNotFound, id)
	}
	unlock, err := s.lockConfig(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.deps.Store.Delete(ctx, docPath(distributionsDir, id)); err != nil {
		return fmt.Errorf("cache: delete the settings of %s: %w", id, err)
	}
	s.applyConfig(ctx, "distribution:"+id)
	s.store.removeWhere(func(m *entryMeta) bool { return m.Distribution == id }, false)
	return nil
}

// Effective implements Manager.
func (s *service) Effective(ctx context.Context, in EffectiveInput) (EffectivePolicy, error) {
	if _, err := s.deps.Distributions.Get(ctx, in.Distribution); err != nil {
		return EffectivePolicy{}, err
	}
	path := in.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return s.snap.Load().resolveFor(ctx, in.Distribution, normalizeHost(in.Host), path).eff, nil
}

// --- Manager: webhooks ---------------------------------------------------------------

// ListWebhooks implements Manager.
func (s *service) ListWebhooks(ctx context.Context, dist string) ([]Webhook, error) {
	c, err := s.GetDistribution(ctx, dist)
	if err != nil {
		return nil, err
	}
	return c.Webhooks, nil
}

// AddWebhook implements Manager.
func (s *service) AddWebhook(ctx context.Context, dist string, w Webhook) (Webhook, error) {
	d, err := s.deps.Distributions.Get(ctx, dist)
	if err != nil {
		return Webhook{}, err
	}
	policies, err := s.loadPolicies(ctx)
	if err != nil {
		return Webhook{}, err
	}
	unlock, err := s.lockConfig(ctx)
	if err != nil {
		return Webhook{}, err
	}
	defer unlock()
	cfg, err := s.loadDistConfig(ctx, dist)
	if err != nil {
		return Webhook{}, err
	}
	for _, existing := range cfg.Webhooks {
		if existing.ID == w.ID {
			return Webhook{}, fmt.Errorf("%w: webhook %s", ErrAlreadyExists, w.ID)
		}
	}
	cfg.Webhooks = append(cfg.Webhooks, w)
	if err := s.validateDistConfig(ctx, &cfg, d, policies); err != nil {
		return Webhook{}, err
	}
	out, err := s.storeDistConfig(ctx, cfg)
	if err != nil {
		return Webhook{}, err
	}
	return out.Webhooks[len(out.Webhooks)-1], nil
}

// DeleteWebhook implements Manager: the domains bound to it are unbound.
func (s *service) DeleteWebhook(ctx context.Context, dist, id string) error {
	if _, err := s.deps.Distributions.Get(ctx, dist); err != nil {
		return err
	}
	unlock, err := s.lockConfig(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := s.loadDistConfig(ctx, dist)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(cfg.Webhooks, func(w Webhook) bool { return w.ID == id })
	if i < 0 {
		return fmt.Errorf("%w: %s", ErrWebhookNotFound, id)
	}
	cfg.Webhooks = slices.Delete(cfg.Webhooks, i, i+1)
	for j := range cfg.Domains {
		cfg.Domains[j].Cache.PurgeWebhooks = slices.DeleteFunc(cfg.Domains[j].Cache.PurgeWebhooks, func(w string) bool { return w == id })
	}
	_, err = s.storeDistConfig(ctx, cfg)
	return err
}

// handleConfigEvent reloads the configuration on the invalidations of the
// peers (and on the resynchronizations).
func (s *service) handleConfigEvent(ctx context.Context, _ cluster.Event) error {
	return s.Reload(ctx)
}
