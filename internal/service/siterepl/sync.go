package siterepl

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/replication"
)

// Start implements lifecycle.Runner: the rounds run every interval on the
// leader, and sooner when asked.
func (s *service) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel, s.done = cancel, make(chan struct{})
	go func() {
		defer close(s.done)
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-s.trigger:
			}
			if s.deps.Leader != nil && !s.deps.Leader.IsLeader() {
				continue
			}
			if err := s.runRound(ctx); err != nil && ctx.Err() == nil {
				s.deps.Logger.WarnContext(ctx, "siterepl: round failed", "err", err)
			}
		}
	}()
	return nil
}

// Stop implements lifecycle.Runner.
func (s *service) Stop(ctx context.Context) error {
	if s.cancel == nil {
		return nil
	}
	s.cancel()
	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// kick asks for a round soon.
func (s *service) kick() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Sync implements Service.
func (s *service) Sync(ctx context.Context) error {
	if cur, err := s.config(ctx); err != nil {
		return err
	} else if cur == nil {
		return fmt.Errorf("%w: this site is in no site replication group", domain.ErrNotFound)
	}
	s.kick()
	return nil
}

// runRound synchronizes this site with every other one, then makes the
// replication rules of the buckets right.
func (s *service) runRound(ctx context.Context) error {
	s.round.Lock()
	defer s.round.Unlock()
	cur, err := s.config(ctx)
	if err != nil || cur == nil {
		return err
	}
	_, peers := s.self(cur)
	var st statusDoc
	if _, err := s.readJSON(ctx, statusPath, &st); err != nil {
		return err
	}
	if st.Peers == nil {
		st.Peers = map[string]PeerStatus{}
	}
	s.mu.Lock()
	if len(s.resyncs) == 0 && len(st.Resyncs) > 0 {
		s.resyncs = st.Resyncs
	}
	s.mu.Unlock()
	items, err := s.local(ctx)
	if err != nil {
		return err
	}
	now := s.deps.Clock.Now().UTC()
	for _, p := range peers {
		ps := st.Peers[p.DeploymentID]
		ps.Name, ps.DeploymentID = p.Name, p.DeploymentID
		pushed, pulled, pending, err := s.syncPeer(ctx, p, items)
		ps.Pushed += int64(pushed)
		ps.Pulled += int64(pulled)
		ps.Pending = pending
		if err != nil {
			ps.LastError, ps.LastErrorAt = err.Error(), now
			s.logOnce(ctx, p.Name, "", err)
		} else {
			ps.LastSync, ps.LastError = now, ""
			s.logOnce(ctx, p.Name, "", nil)
		}
		st.Peers[p.DeploymentID] = ps
		if pulled > 0 {
			if items, err = s.local(ctx); err != nil {
				return err
			}
		}
	}
	st.Entries = len(items)
	s.ensureBuckets(ctx, cur)
	s.mu.Lock()
	st.Resyncs = map[string][]string{}
	for b, arns := range s.resyncs {
		st.Resyncs[b] = append([]string(nil), arns...)
	}
	s.mu.Unlock()
	return s.writeJSON(ctx, statusPath, st)
}

// logOnce logs an error of a peer when it changes.
func (s *service) logOnce(ctx context.Context, peer, key string, err error) {
	id := peer + "\x00" + key
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	s.mu.Lock()
	last := s.logged[id]
	if msg == "" {
		delete(s.logged, id)
	} else {
		s.logged[id] = msg
	}
	s.mu.Unlock()
	if msg != "" && msg != last {
		s.deps.Logger.WarnContext(ctx, "siterepl: synchronization with a site failed", "site", peer, "entry", key, "err", err)
	}
}

// batchSize bounds the entries of one call to another site.
const batchSize = 200

// change is a decision of the merge.
type change struct {
	key  string
	push bool
	// hash is the hash both sides have once it is applied ("" for a
	// deletion).
	hash string
}

// merge decides, per entry, what to send and what to take, from the
// entries here, there and in the base.
func merge(local map[string]item, remote map[string]Entry, base map[string]string, here, there string) (newBase map[string]string, changes []change) {
	newBase = make(map[string]string, len(base))
	keys := map[string]bool{}
	for k := range local {
		keys[k] = true
	}
	for k := range remote {
		keys[k] = true
	}
	for k := range base {
		keys[k] = true
	}
	for k := range keys {
		l, lok := local[k]
		r, rok := remote[k]
		lh, rh := "", ""
		if lok {
			lh = l.Hash
		}
		if rok {
			rh = r.Hash
		}
		if lh == rh {
			if lh != "" {
				newBase[k] = lh
			}
			continue
		}
		bh, bok := base[k]
		if bok {
			newBase[k] = bh
		}
		lChanged, rChanged := lh != bh, rh != bh
		var push bool
		switch {
		case lChanged && !rChanged:
			push = true
		case rChanged && !lChanged:
			push = false
		case !lok:
			// Deleted here, changed there: the change wins.
			push = false
		case !rok:
			push = true
		case !l.ModTime.Equal(r.ModTime):
			push = l.ModTime.After(r.ModTime)
		default:
			push = here < there
		}
		if push {
			changes = append(changes, change{key: k, push: true, hash: lh})
		} else {
			changes = append(changes, change{key: k, hash: rh})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].key < changes[j].key })
	return newBase, changes
}

// syncPeer synchronizes this site with one other.
func (s *service) syncPeer(ctx context.Context, site Site, local map[string]item) (pushed, pulled, pending int, err error) {
	peer, err := s.deps.Dialer.Dial(ctx, site)
	if err != nil {
		return 0, 0, 0, err
	}
	remote, err := peer.State(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("state: %w", err)
	}
	base := map[string]string{}
	if _, err := s.readJSON(ctx, basePath(site.DeploymentID), &base); err != nil {
		return 0, 0, 0, err
	}
	newBase, changes := merge(local, remote, base, s.deps.DeploymentID, site.DeploymentID)
	var sends []wireValue
	var takes []string
	want := map[string]string{}
	for _, c := range changes {
		want[c.key] = c.hash
		if c.push {
			if it, ok := local[c.key]; ok {
				sends = append(sends, wireValue{Key: c.key, Value: it.value})
			} else {
				sends = append(sends, wireValue{Key: c.key, Delete: true})
			}
		} else {
			takes = append(takes, c.key)
		}
	}
	record := func(key string, failure string) {
		if failure != "" {
			pending++
			s.logOnce(ctx, site.Name, key, errors.New(failure))
			return
		}
		s.logOnce(ctx, site.Name, key, nil)
		if h := want[key]; h != "" {
			newBase[key] = h
		} else {
			delete(newBase, key)
		}
	}
	var errs []error
	for start := 0; start < len(sends); start += batchSize {
		batch := sends[start:min(start+batchSize, len(sends))]
		sealed, err := s.seal(ctx, batch)
		if err != nil {
			return pushed, pulled, pending, err
		}
		results, err := peer.Apply(ctx, sealed)
		if err != nil {
			errs = append(errs, fmt.Errorf("apply there: %w", err))
			pending += len(batch)
			continue
		}
		for _, r := range results {
			record(r.Key, r.Error)
			if r.Error == "" {
				pushed++
			}
		}
	}
	for start := 0; start < len(takes); start += batchSize {
		batch := takes[start:min(start+batchSize, len(takes))]
		sealed, err := peer.Items(ctx, batch)
		var values []wireValue
		if err == nil {
			values, err = s.open(ctx, sealed)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("entries: %w", err))
			pending += len(batch)
			continue
		}
		for _, r := range s.applyLocal(ctx, values) {
			record(r.Key, r.Error)
			if r.Error == "" {
				pulled++
			}
		}
	}
	if err := s.writeJSON(ctx, basePath(site.DeploymentID), newBase); err != nil {
		errs = append(errs, err)
	}
	if pending > 0 && len(errs) == 0 {
		errs = append(errs, fmt.Errorf("%d changes not applied yet (retried)", pending))
	}
	return pushed, pulled, pending, errors.Join(errs...)
}

// --- bucket replication -----------------------------------------------------------------

// siteRulePrefix names the bucket replication rules of the sites.
const siteRulePrefix = "site-"

func siteSignature(peers []Site) string {
	parts := make([]string, 0, len(peers))
	for _, p := range peers {
		parts = append(parts, p.Name+"="+p.Endpoint)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// due reports whether the rules of a bucket must be checked.
func (s *service) due(name, signature string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.signature != signature {
		s.signature, s.checked = signature, map[string]time.Time{}
	}
	last, ok := s.checked[name]
	return !ok || now.Sub(last) >= s.recheck
}

// ruleIDOf names the rule of a peer.
func ruleIDOf(peer Site) string { return siteRulePrefix + peer.Name }

// ensureBuckets gives every bucket versioning and a replication rule to
// the same bucket of every other site, and starts the resyncs of the new
// rules one at a time per bucket. A rule whose remote bucket is not ready
// yet is added at a later round.
func (s *service) ensureBuckets(ctx context.Context, cur *configDoc) {
	_, peers := s.self(cur)
	signature := siteSignature(peers)
	buckets, err := s.deps.Buckets.List(ctx, bucket.ListInput{})
	if err != nil {
		s.logOnce(ctx, "", "buckets", err)
		return
	}
	now := s.deps.Clock.Now()
	for _, b := range buckets {
		if !s.due(b.Name, signature, now) {
			continue
		}
		complete, err := s.ensureBucket(ctx, b.Name, peers)
		s.logOnce(ctx, "", keyBucket+b.Name, err)
		if err == nil && complete {
			s.mu.Lock()
			s.checked[b.Name] = now
			s.mu.Unlock()
		}
	}
	s.startResyncs(ctx)
}

// ensureBucket makes the rules of one bucket right; complete is false
// when a remote bucket was not ready.
func (s *service) ensureBucket(ctx context.Context, name string, peers []Site) (complete bool, err error) {
	v, err := s.deps.Buckets.GetVersioning(ctx, name)
	if err != nil {
		return false, err
	}
	if v != domain.VersioningEnabled {
		if err := s.deps.Buckets.SetVersioning(ctx, name, domain.VersioningEnabled); err != nil {
			return false, fmt.Errorf("versioning: %w", err)
		}
	}
	targets, err := s.deps.Replication.ListTargets(ctx, name)
	if err != nil {
		return false, err
	}
	cfg, err := s.currentRules(ctx, name)
	if err != nil {
		return false, err
	}
	var kept []domain.ReplicationRule
	existing := map[string]domain.ReplicationRule{}
	maxPriority := 0
	for _, r := range cfg.Rules {
		maxPriority = max(maxPriority, r.Priority)
		if strings.HasPrefix(r.ID, siteRulePrefix) {
			existing[r.ID] = r
		} else {
			kept = append(kept, r)
		}
	}
	complete = true
	var added []string
	enabled := &domain.ReplicationToggle{Status: domain.ReplicationEnabled}
	empty := ""
	for _, p := range peers {
		arn := ""
		for _, t := range targets {
			if t.Bucket == name && strings.EqualFold(strings.TrimRight(t.Endpoint, "/"), p.Endpoint) {
				arn = t.ARN
			}
		}
		if arn == "" {
			t, err := s.deps.Replication.SetTarget(ctx, name, replication.TargetInput{Endpoint: p.Endpoint, Bucket: name,
				AccessKey: s.deps.RootUser, SecretKey: s.deps.RootPassword, Label: "site " + p.Name})
			if err != nil {
				// The bucket is not there yet: next round.
				complete = false
				continue
			}
			arn = t.ARN
		}
		rule, ok := existing[ruleIDOf(p)]
		if !ok || rule.Destination.Bucket != arn {
			if !ok {
				maxPriority++
				rule.Priority = maxPriority
			}
			rule = domain.ReplicationRule{ID: ruleIDOf(p), Priority: rule.Priority, Status: domain.ReplicationEnabled,
				Filter: &domain.ReplicationFilter{Prefix: &empty}, DeleteMarkerReplication: enabled, DeleteReplication: enabled,
				ExistingObjectReplication: enabled, Destination: domain.ReplicationRuleDestination{Bucket: arn}}
			added = append(added, arn)
		}
		delete(existing, ruleIDOf(p))
		kept = append(kept, rule)
	}
	stale := len(existing) > 0
	if len(added) == 0 && !stale {
		return complete, nil
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Priority < kept[j].Priority })
	if err := s.saveRules(ctx, name, kept); err != nil {
		return false, err
	}
	for _, r := range existing {
		if err := s.deps.Replication.RemoveTarget(ctx, name, r.Destination.Bucket); err != nil && !errors.Is(err, domain.ErrNotFound) {
			s.logOnce(ctx, "", keyBucket+name+"/target", err)
		}
	}
	if len(added) > 0 {
		s.mu.Lock()
		s.resyncs[name] = append(s.resyncs[name], added...)
		s.mu.Unlock()
		s.deps.Logger.InfoContext(ctx, "siterepl: bucket replicated to the other sites", "bucket", name, "rules", len(added))
	}
	return complete, nil
}

// currentRules returns the replication configuration of a bucket (empty
// when none).
func (s *service) currentRules(ctx context.Context, name string) (*domain.ReplicationConfig, error) {
	doc, err := s.deps.Buckets.GetReplication(ctx, name)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) || isNoReplication(err) {
			return &domain.ReplicationConfig{}, nil
		}
		return nil, err
	}
	if len(doc) == 0 {
		return &domain.ReplicationConfig{}, nil
	}
	return domain.ParseReplicationConfig(doc, nil)
}

// saveRules stores the rules of a bucket (none removes the
// configuration).
func (s *service) saveRules(ctx context.Context, name string, rules []domain.ReplicationRule) error {
	if len(rules) == 0 {
		err := s.deps.Buckets.DeleteReplication(ctx, name)
		if err != nil && (errors.Is(err, domain.ErrNotFound) || isNoReplication(err)) {
			return nil
		}
		return err
	}
	doc, err := (&domain.ReplicationConfig{Rules: rules}).Encode()
	if err != nil {
		return err
	}
	if doc, err = s.deps.Replication.ValidateConfig(ctx, name, doc); err != nil {
		return err
	}
	return s.deps.Buckets.SetReplication(ctx, name, doc)
}

// startResyncs starts, per bucket, the next resync of a new rule once no
// resync of the bucket runs.
func (s *service) startResyncs(ctx context.Context) {
	s.mu.Lock()
	pending := make(map[string][]string, len(s.resyncs))
	for b, arns := range s.resyncs {
		pending[b] = append([]string(nil), arns...)
	}
	s.mu.Unlock()
	for name, arns := range pending {
		if len(arns) == 0 {
			s.dropResync(name, "")
			continue
		}
		st, err := s.deps.Replication.Status(ctx, name)
		if err != nil {
			if errors.Is(err, domain.ErrBucketNotFound) {
				s.dropResync(name, "")
			}
			continue
		}
		if st.Resync != nil && st.Resync.State == replication.ResyncRunning {
			continue
		}
		if _, err := s.deps.Replication.StartResync(ctx, name, arns[0]); err != nil {
			s.logOnce(ctx, "", keyBucket+name+"/resync", err)
			if errors.Is(err, domain.ErrBucketNotFound) || errors.Is(err, domain.ErrInvalidArgument) {
				s.dropResync(name, arns[0])
			}
			continue
		}
		s.dropResync(name, arns[0])
	}
}

// dropResync removes a resync to start ("" drops the bucket).
func (s *service) dropResync(name, arn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if arn == "" {
		delete(s.resyncs, name)
		return
	}
	arns := s.resyncs[name]
	for i, a := range arns {
		if a == arn {
			arns = append(arns[:i:i], arns[i+1:]...)
			break
		}
	}
	if len(arns) == 0 {
		delete(s.resyncs, name)
	} else {
		s.resyncs[name] = arns
	}
}

// removeSiteRules removes the rules and the targets of the sites from
// every bucket (the group is dissolved).
func (s *service) removeSiteRules(ctx context.Context) {
	buckets, err := s.deps.Buckets.List(ctx, bucket.ListInput{})
	if err != nil {
		s.deps.Logger.WarnContext(ctx, "siterepl: the replication rules of the sites were not removed", "err", err)
		return
	}
	for _, b := range buckets {
		cfg, err := s.currentRules(ctx, b.Name)
		if err != nil {
			continue
		}
		var kept, removed []domain.ReplicationRule
		for _, r := range cfg.Rules {
			if strings.HasPrefix(r.ID, siteRulePrefix) {
				removed = append(removed, r)
			} else {
				kept = append(kept, r)
			}
		}
		if len(removed) == 0 {
			continue
		}
		if err := s.saveRules(ctx, b.Name, kept); err != nil {
			s.deps.Logger.WarnContext(ctx, "siterepl: the replication rules of the sites were not removed", "bucket", b.Name, "err", err)
			continue
		}
		for _, r := range removed {
			_ = s.deps.Replication.RemoveTarget(ctx, b.Name, r.Destination.Bucket)
		}
	}
}

// isNoReplication reports the error of a bucket without a replication
// configuration.
func isNoReplication(err error) bool { return errors.Is(err, domain.ErrPolicyNotFound) }
