package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// Defaults of the bootstrap.
const (
	defaultBackoffMin       = 250 * time.Millisecond
	defaultBackoffMax       = 5 * time.Second
	defaultBootstrapTimeout = 5 * time.Second
	// relogInterval repeats an unchanged "waiting for" message.
	relogInterval = 30 * time.Second
	// settleRounds is how many consecutive rounds a local drive must be
	// seen empty in an existing deployment before it is formatted as a
	// replacement: the node formatting a fresh deployment may still be
	// writing it.
	settleRounds = 2
	// formatAttempts bounds the writes of the format of a fresh pool to
	// one drive, formatRetryPause the pause before the first retry (it
	// grows linearly).
	formatAttempts   = 3
	formatRetryPause = 200 * time.Millisecond
)

// BootstrapDeps are the dependencies of the bootstrappers.
type BootstrapDeps struct {
	// Layout is the topology of the deployment (required).
	Layout *topology.Layout
	// Topology builds and checks the format.json documents (required).
	Topology topology.Service
	// Drives are the drives of the layout, matched by endpoint: the local
	// drives and, in a distributed deployment, the clients of the drives
	// of the other nodes. Every endpoint of the layout needs one.
	Drives []storage.StorageAPI
	// Peers are the bootstrap clients of the other nodes.
	Peers []BootstrapPeer
	// Formatter formats the drives of a single-node deployment (required
	// by NewLocalBootstrapper).
	Formatter storage.Formatter
	// IDs generates the deployment id of a fresh deployment (default
	// UUID v4).
	IDs topology.IDGenerator
	// Binder, when set, is bound to the deployment as soon as the
	// bootstrap knows its id (distributed deployments).
	Binder DeploymentBinder
	Clock  clock.Clock
	Logger *slog.Logger
}

// BootstrapOption configures the bootstrappers.
type BootstrapOption func(*bootstrapOptions)

type bootstrapOptions struct {
	backoffMin, backoffMax time.Duration
	timeout                time.Duration
	version                string
	skew                   time.Duration
	kmsFingerprint         string
}

// WithBootstrapBackoff sets the bounds of the pause between two rounds
// (default 250ms-5s).
func WithBootstrapBackoff(lo, hi time.Duration) BootstrapOption {
	return func(o *bootstrapOptions) {
		if lo > 0 && hi >= lo {
			o.backoffMin, o.backoffMax = lo, hi
		}
	}
}

// WithBootstrapTimeout bounds each call to a peer or a drive (default 5s).
func WithBootstrapTimeout(d time.Duration) BootstrapOption {
	return func(o *bootstrapOptions) {
		if d > 0 {
			o.timeout = d
		}
	}
}

// WithBootstrapVersion sets the build version this node announces and
// compares (default "dev", compatible with every version).
func WithBootstrapVersion(v string) BootstrapOption {
	return func(o *bootstrapOptions) {
		if v != "" {
			o.version = v
		}
	}
}

// WithBootstrapKMSFingerprint sets the fingerprint of the KMS
// configuration this node announces and compares (kms.Fingerprint; empty,
// the default, without KMS).
func WithBootstrapKMSFingerprint(fp string) BootstrapOption {
	return func(o *bootstrapOptions) { o.kmsFingerprint = fp }
}

// WithBootstrapSkewThreshold sets the clock offset of a peer reported at
// startup (default 2s).
func WithBootstrapSkewThreshold(d time.Duration) BootstrapOption {
	return func(o *bootstrapOptions) {
		if d > 0 {
			o.skew = d
		}
	}
}

func newBootstrapOptions(opts []BootstrapOption) bootstrapOptions {
	o := bootstrapOptions{backoffMin: defaultBackoffMin, backoffMax: defaultBackoffMax, timeout: defaultBootstrapTimeout,
		version: "dev", skew: DefaultSkewThreshold}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

func (deps *BootstrapDeps) defaults() error {
	if deps.Layout == nil || deps.Topology == nil {
		return fmt.Errorf("%w: cluster: the bootstrap needs the layout and the topology service", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.IDs == nil {
		deps.IDs = topology.NewUUIDGenerator()
	}
	return nil
}

// FormattedDeploymentID returns the deployment id recorded in the format
// of the given drives (the local drives of a node), "" when none is
// formatted. A node whose drives are formatted binds its internode
// authentication to it before it accepts any call (DeploymentBinder);
// drives that disagree are the business of the bootstrap, which refuses
// them: the first readable format wins here.
func FormattedDeploymentID(ctx context.Context, drives []storage.StorageAPI) string {
	for _, d := range drives {
		if d == nil {
			continue
		}
		data, err := d.ReadAll(ctx, domain.SystemBucket, storage.FormatFile)
		if err != nil {
			continue
		}
		if f, err := topology.UnmarshalFormat(data); err == nil && f.ID != "" {
			return f.ID
		}
	}
	return ""
}

// bootstrapper is the startup sequence of a node of a distributed
// deployment.
type bootstrapper struct {
	deps   BootstrapDeps
	opts   bootstrapOptions
	nodeID string
	drives map[string]storage.StorageAPI

	mu           sync.Mutex
	deploymentID string
}

var _ Bootstrapper = (*bootstrapper)(nil)

// NewBootstrapper returns the bootstrapper of a node of a distributed
// deployment.
func NewBootstrapper(deps BootstrapDeps, opts ...BootstrapOption) (Bootstrapper, error) {
	if err := deps.defaults(); err != nil {
		return nil, err
	}
	local, ok := LocalNode(NodesOf(deps.Layout))
	if !ok {
		return nil, fmt.Errorf("%w: cluster: no node of the layout is local", domain.ErrInvalidLayout)
	}
	b := &bootstrapper{deps: deps, opts: newBootstrapOptions(opts), nodeID: local.ID, drives: make(map[string]storage.StorageAPI)}
	for _, d := range deps.Drives {
		if d != nil {
			b.drives[d.Endpoint().String()] = d
		}
	}
	var missing []string
	for _, ep := range deps.Layout.Endpoints() {
		if _, ok := b.drives[ep.String()]; !ok {
			missing = append(missing, ep.String())
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: cluster: no drive for endpoints %s", domain.ErrInvalidArgument, strings.Join(missing, ", "))
	}
	return b, nil
}

// LocalConfig implements Bootstrapper.
func (b *bootstrapper) LocalConfig(ctx context.Context) (NodeConfig, error) {
	return localConfig(ctx, b.nodeID, b.deps, b.opts, b.currentID())
}

func localConfig(ctx context.Context, nodeID string, deps BootstrapDeps, opts bootstrapOptions, deploymentID string) (NodeConfig, error) {
	if err := ctx.Err(); err != nil {
		return NodeConfig{}, err
	}
	return NodeConfig{
		NodeID: nodeID, TopologyHash: deps.Layout.Hash(), Mode: deps.Layout.Mode, Version: opts.version,
		DeploymentID: deploymentID, KMSFingerprint: opts.kmsFingerprint, Time: deps.Clock.Now(),
	}, nil
}

// kmsLabel shows a KMS fingerprint in the messages ("none" without KMS).
func kmsLabel(fp string) string {
	if fp == "" {
		return "none"
	}
	return shortHash(fp)
}

func (b *bootstrapper) currentID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deploymentID
}

// Bootstrap implements Bootstrapper.
func (b *bootstrapper) Bootstrap(ctx context.Context) (BootstrapResult, error) {
	start := b.deps.Clock.Now()
	b.deps.Logger.Info("cluster: bootstrap", "self", b.nodeID, "nodes", len(b.deps.Layout.Nodes),
		"drives", b.deps.Layout.DriveCount(), "topology", shortHash(b.deps.Layout.Hash()))
	if err := b.verifyPeers(ctx); err != nil {
		return BootstrapResult{}, err
	}
	res, err := b.waitFormats(ctx)
	if err != nil {
		return BootstrapResult{}, err
	}
	b.mu.Lock()
	b.deploymentID = res.DeploymentID
	b.mu.Unlock()
	b.deps.Logger.Info("cluster: bootstrap complete", "self", b.nodeID, "deployment_id", res.DeploymentID,
		"healing", len(res.Healing), "offline", len(res.Offline), "elapsed", b.deps.Clock.Now().Sub(start).Round(time.Millisecond))
	return res, nil
}

// waiter paces the rounds of a phase and its "waiting for" messages.
type waiter struct {
	b        *bootstrapper
	delay    time.Duration
	last     string
	lastTime time.Time
}

func (b *bootstrapper) newWaiter() *waiter { return &waiter{b: b, delay: b.opts.backoffMin} }

// wait logs what the phase waits for (when it changed, or periodically)
// and sleeps until the next round.
func (w *waiter) wait(ctx context.Context, msg string, attrs ...slog.Attr) error {
	now := w.b.deps.Clock.Now()
	if msg != w.last || now.Sub(w.lastTime) >= relogInterval {
		w.b.deps.Logger.LogAttrs(ctx, slog.LevelWarn, msg, attrs...)
		w.last, w.lastTime = msg, now
	}
	timer := time.NewTimer(w.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("cluster: bootstrap interrupted (%s): %w", msg, ctx.Err())
	case <-timer.C:
	}
	w.delay = min(w.delay*2, w.b.opts.backoffMax)
	return nil
}

// verifyPeers waits until a quorum of the nodes (this one included) runs
// the same topology, compatible versions and the same KMS configuration. A
// node at odds with the majority of the deployment refuses to start.
func (b *bootstrapper) verifyPeers(ctx context.Context) error {
	peers := b.deps.Peers
	total := len(peers) + 1
	need := total/2 + 1
	ours, err := b.LocalConfig(ctx)
	if err != nil {
		return err
	}
	w := b.newWaiter()
	warnedSkew := map[string]bool{}
	for {
		type answer struct {
			cfg        NodeConfig
			err        error
			sent, recv time.Time
		}
		answers := make([]answer, len(peers))
		var wg sync.WaitGroup
		for i, p := range peers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				callCtx, cancel := context.WithTimeout(ctx, b.opts.timeout)
				defer cancel()
				sent := b.deps.Clock.Now()
				cfg, err := p.Config(callCtx)
				answers[i] = answer{cfg: cfg, err: err, sent: sent, recv: b.deps.Clock.Now()}
			}()
		}
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("cluster: bootstrap interrupted while verifying the peers: %w", err)
		}
		verified := 1
		var offline, topoDiff, versionDiff, kmsDiff []string
		for i, a := range answers {
			id := peers[i].NodeID()
			switch {
			case a.err != nil:
				offline = append(offline, id)
			case a.cfg.TopologyHash != ours.TopologyHash || a.cfg.Mode != ours.Mode:
				topoDiff = append(topoDiff, fmt.Sprintf("%s (topology %s, mode %s)", id, shortHash(a.cfg.TopologyHash), a.cfg.Mode))
			case !domain.CompatibleVersions(a.cfg.Version, ours.Version):
				versionDiff = append(versionDiff, fmt.Sprintf("%s (version %s)", id, a.cfg.Version))
			case a.cfg.KMSFingerprint != ours.KMSFingerprint:
				kmsDiff = append(kmsDiff, fmt.Sprintf("%s (kms %s)", id, kmsLabel(a.cfg.KMSFingerprint)))
			default:
				verified++
				rtt := a.recv.Sub(a.sent)
				offset := a.cfg.Time.Sub(a.sent.Add(rtt / 2))
				if (offset > b.opts.skew || offset < -b.opts.skew) && !warnedSkew[id] {
					warnedSkew[id] = true
					b.deps.Logger.Warn("cluster: clock skew between nodes", "peer", id, "offset", offset.Round(time.Millisecond),
						"threshold", b.opts.skew, "hint", "synchronize the clocks of the nodes (NTP)")
				}
			}
		}
		all := slices.Concat(topoDiff, versionDiff, kmsDiff)
		if differ := len(all); differ > total/2 {
			sentinel := ErrTopologyMismatch
			switch {
			case len(versionDiff) > len(topoDiff) && len(versionDiff) >= len(kmsDiff):
				sentinel = ErrIncompatibleVersion
			case len(kmsDiff) > len(topoDiff) && len(kmsDiff) > len(versionDiff):
				sentinel = ErrKMSMismatch
			}
			return fmt.Errorf("%w: this node %s (topology %s, mode %s, version %s, kms %s) disagrees with %d of the %d nodes: %s; start every node with the same server arguments, release and KMS configuration",
				sentinel, b.nodeID, shortHash(ours.TopologyHash), ours.Mode, ours.Version, kmsLabel(ours.KMSFingerprint), differ, total, strings.Join(all, ", "))
		}
		if verified >= need {
			if differ := all; len(differ) > 0 {
				b.deps.Logger.Error("cluster: nodes started with another configuration are left out", "nodes", differ)
			}
			b.deps.Logger.Info("cluster: peers verified", "verified", verified, "nodes", total, "offline", offline)
			return nil
		}
		msg := fmt.Sprintf("cluster: waiting for the peers: %d of %d nodes verified, %d needed", verified, total, need)
		if err := w.wait(ctx, msg, slog.Any("offline", offline), slog.Any("different_topology", topoDiff),
			slog.Any("incompatible_version", versionDiff)); err != nil {
			return err
		}
	}
}

// driveFormat is what a round read from one drive.
type driveFormat struct {
	ep  domain.Endpoint
	doc *topology.FormatV1
	// unformatted reports a reachable drive without format.json.
	unformatted bool
	err         error
}

func (d driveFormat) reachable() bool { return d.doc != nil || d.unformatted }

// waitFormats reads the formats until every erasure set has a read quorum
// of formatted drives, formatting a fresh deployment (first node only) and
// the empty local drives of an existing one on the way.
func (b *bootstrapper) waitFormats(ctx context.Context) (BootstrapResult, error) {
	layout := b.deps.Layout
	first := layout.Pools[0].Endpoints[0]
	empty := map[string]int{}
	healing := map[string]domain.Endpoint{}
	w := b.newWaiter()
	for {
		states, err := b.readFormats(ctx)
		if err != nil {
			return BootstrapResult{}, err
		}
		deploymentID, refs, err := b.references(ctx, states)
		if err != nil {
			return BootstrapResult{}, err
		}
		if deploymentID == "" && first.IsLocal {
			deploymentID = b.deps.IDs.NewID()
		}
		if deploymentID != "" && b.deps.Binder != nil {
			// From now on the callers of other deployments are refused,
			// and the formats this node writes on its peers carry the id
			// its tokens claim.
			b.deps.Binder.SetDeploymentID(ctx, deploymentID)
		}
		wrote := false
		var waiting []string
		for p, pool := range layout.Pools {
			poolStates := states[p]
			switch {
			case refs[p] == nil && !first.IsLocal:
				waiting = append(waiting, fmt.Sprintf("pool %d: the node of %s formats the fresh deployment", p+1, first))
			case refs[p] == nil:
				if gaps := setGaps(pool, poolStates, writeQuorum(pool), driveFormat.reachable); len(gaps) > 0 {
					waiting = append(waiting, fmt.Sprintf("pool %d (fresh): %s", p+1, strings.Join(gaps, "; ")))
					continue
				}
				n, err := b.formatPool(ctx, pool, poolStates, deploymentID)
				if err != nil {
					return BootstrapResult{}, err
				}
				if n == 0 {
					waiting = append(waiting, fmt.Sprintf("pool %d (fresh): no drive accepted its format.json", p+1))
				}
				wrote = wrote || n > 0
			default:
				n, err := b.formatReplacements(ctx, pool, poolStates, refs[p], empty, healing)
				if err != nil {
					return BootstrapResult{}, err
				}
				wrote = wrote || n > 0
				formatted := func(d driveFormat) bool { return d.doc != nil }
				if gaps := setGaps(pool, poolStates, readQuorum(pool), formatted); len(gaps) > 0 {
					waiting = append(waiting, fmt.Sprintf("pool %d: %s", p+1, strings.Join(gaps, "; ")))
				}
			}
		}
		if wrote {
			continue // read back what was written
		}
		if len(waiting) == 0 && len(empty) == 0 {
			return b.result(deploymentID, states, healing), nil
		}
		if len(waiting) == 0 {
			// Quorum reached, but local empty drives are settling before
			// they are formatted as replacements.
			if err := w.wait(ctx, "cluster: empty local drives in an existing deployment, formatting them", slog.Int("drives", len(empty))); err != nil {
				return BootstrapResult{}, err
			}
			continue
		}
		if err := w.wait(ctx, "cluster: waiting for the drives", slog.Any("missing", waiting)); err != nil {
			return BootstrapResult{}, err
		}
	}
}

// readFormats reads format.json from every drive of the layout, per pool
// in endpoint order.
func (b *bootstrapper) readFormats(ctx context.Context) ([][]driveFormat, error) {
	out := make([][]driveFormat, len(b.deps.Layout.Pools))
	var wg sync.WaitGroup
	for p, pool := range b.deps.Layout.Pools {
		out[p] = make([]driveFormat, len(pool.Endpoints))
		for i, ep := range pool.Endpoints {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out[p][i] = b.readFormat(ctx, ep)
			}()
		}
	}
	wg.Wait()
	return out, ctx.Err()
}

func (b *bootstrapper) readFormat(ctx context.Context, ep domain.Endpoint) driveFormat {
	callCtx, cancel := context.WithTimeout(ctx, b.opts.timeout)
	defer cancel()
	data, err := b.drives[ep.String()].ReadAll(callCtx, domain.SystemBucket, storage.FormatFile)
	switch {
	case errors.Is(err, domain.ErrObjectNotFound):
		return driveFormat{ep: ep, unformatted: true}
	case err != nil:
		return driveFormat{ep: ep, err: err}
	}
	doc, err := topology.UnmarshalFormat(data)
	if err != nil {
		b.deps.Logger.Error("cluster: unreadable format.json, the drive is left out", "drive", ep.String(), "err", err)
		return driveFormat{ep: ep, err: err}
	}
	return driveFormat{ep: ep, doc: doc}
}

// references checks the formats of every pool (same deployment, same sets,
// every drive at its position) and returns the deployment id and the
// reference document of each pool (nil for a pool without any format).
func (b *bootstrapper) references(ctx context.Context, states [][]driveFormat) (string, []*topology.FormatV1, error) {
	refs := make([]*topology.FormatV1, len(states))
	deploymentID := ""
	for p, pool := range b.deps.Layout.Pools {
		docs := make([]*topology.FormatV1, len(states[p]))
		found := false
		for i, s := range states[p] {
			docs[i] = s.doc
			found = found || s.doc != nil
		}
		if !found {
			continue
		}
		ref, err := b.deps.Topology.CheckFormats(ctx, b.deps.Layout, p, docs)
		if err != nil {
			return "", nil, fmt.Errorf("cluster: pool %d: %w", p+1, err)
		}
		for i, doc := range docs {
			if doc == nil {
				continue
			}
			set, disk, err := doc.Position()
			if err != nil {
				return "", nil, fmt.Errorf("cluster: drive %s: %w", pool.Endpoints[i], err)
			}
			if set != i/pool.SetDriveCount || disk != i%pool.SetDriveCount {
				return "", nil, fmt.Errorf("%w: drive %s is formatted for set %d disk %d but is given as set %d disk %d",
					domain.ErrFormatMismatch, pool.Endpoints[i], set+1, disk+1, i/pool.SetDriveCount+1, i%pool.SetDriveCount+1)
			}
		}
		if deploymentID != "" && ref.ID != deploymentID {
			return "", nil, fmt.Errorf("%w: pool %d belongs to deployment %s, another pool to %s", domain.ErrFormatMismatch, p+1, ref.ID, deploymentID)
		}
		deploymentID = ref.ID
		refs[p] = ref
	}
	return deploymentID, refs, nil
}

// formatPool writes the documents of a fresh pool to its reachable drives,
// local and remote, and returns how many it wrote.
func (b *bootstrapper) formatPool(ctx context.Context, pool topology.Pool, states []driveFormat, deploymentID string) (int, error) {
	docs, err := b.deps.Topology.NewFormats(ctx, b.deps.Layout, pool.Index, deploymentID)
	if err != nil {
		return 0, fmt.Errorf("cluster: format pool %d: %w", pool.Index+1, err)
	}
	var wg sync.WaitGroup
	written := make([]bool, len(states))
	for i, s := range states {
		if !s.unformatted {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A write that fails (a remote drive slow to answer under load)
			// is retried: a drive of a fresh pool left unformatted would be
			// taken by its node for a replaced drive, and healed for nothing.
			var err error
			for attempt := range formatAttempts {
				if err = b.writeFormat(ctx, s.ep, docs[i]); err == nil {
					written[i] = true
					return
				}
				if ctx.Err() != nil {
					return
				}
				if attempt < formatAttempts-1 {
					t := time.NewTimer(time.Duration(attempt+1) * formatRetryPause)
					select {
					case <-ctx.Done():
						t.Stop()
						return
					case <-t.C:
					}
				}
			}
			b.deps.Logger.Warn("cluster: cannot format drive", "drive", s.ep.String(), "attempts", formatAttempts, "err", err)
		}()
	}
	wg.Wait()
	n := 0
	for _, ok := range written {
		if ok {
			n++
		}
	}
	b.deps.Logger.Info("cluster: formatted fresh pool", "pool", pool.Index+1, "deployment_id", deploymentID,
		"drives", n, "of", len(states), "sets", pool.SetCount, "set_drives", pool.SetDriveCount, "parity", pool.Parity)
	return n, ctx.Err()
}

// formatReplacements formats the local drives found empty in a formatted
// pool (replaced or wiped drives) with the document of their position and
// marks them for healing. It returns how many it wrote.
func (b *bootstrapper) formatReplacements(ctx context.Context, pool topology.Pool, states []driveFormat, ref *topology.FormatV1,
	empty map[string]int, healing map[string]domain.Endpoint,
) (int, error) {
	n := 0
	for i, s := range states {
		key := s.ep.String()
		if !s.unformatted || !s.ep.IsLocal {
			delete(empty, key)
			continue
		}
		empty[key]++
		if empty[key] < settleRounds {
			continue
		}
		set, disk := i/pool.SetDriveCount, i%pool.SetDriveCount
		doc := ref.Clone()
		doc.Erasure.This = ref.Erasure.Sets[set][disk]
		if err := b.writeFormat(ctx, s.ep, doc); err != nil {
			b.deps.Logger.Warn("cluster: cannot format replacement drive", "drive", key, "err", err)
			continue
		}
		if !b.setHoldsData(ctx, pool, states, set) {
			// The set holds nothing yet (a fresh deployment whose first
			// format did not reach this drive): there is nothing to heal.
			delete(empty, key)
			n++
			b.deps.Logger.Info("cluster: empty drive of a deployment without data formatted", "drive", key,
				"pool", pool.Index+1, "set", set+1, "disk", disk+1)
			continue
		}
		now := b.deps.Clock.Now()
		tracker, err := json.Marshal(storage.HealingTracker{
			ID: doc.Erasure.This, PoolIndex: pool.Index, SetIndex: set, DiskIndex: disk, Path: s.ep.Path,
			Endpoint: key, Started: now, LastUpdate: now,
		})
		if err != nil {
			return n, fmt.Errorf("cluster: encode healing tracker: %w", err)
		}
		if err := b.drives[key].WriteAll(ctx, domain.SystemBucket, storage.HealingFile, tracker); err != nil {
			b.deps.Logger.Warn("cluster: cannot mark drive for healing", "drive", key, "err", err)
		}
		healing[key] = s.ep
		delete(empty, key)
		n++
		b.deps.Logger.Warn("cluster: empty drive in an existing deployment formatted and marked for healing",
			"drive", key, "pool", pool.Index+1, "set", set+1, "disk", disk+1)
	}
	return n, ctx.Err()
}

// setHoldsData reports whether the formatted drives of set hold data a
// replaced drive must receive: a bucket, or a system document (bucket
// metadata, configuration, IAM). When a drive cannot tell, it holds data.
func (b *bootstrapper) setHoldsData(ctx context.Context, pool topology.Pool, states []driveFormat, set int) bool {
	callCtx, cancel := context.WithTimeout(ctx, b.opts.timeout)
	defer cancel()
	checked := 0
	for i := set * pool.SetDriveCount; i < (set+1)*pool.SetDriveCount && i < len(states); i++ {
		if states[i].doc == nil {
			continue
		}
		d := b.drives[states[i].ep.String()]
		if d == nil {
			continue
		}
		vols, err := d.ListVols(callCtx)
		if err != nil {
			return true
		}
		for _, v := range vols {
			if !strings.HasPrefix(v.Name, ".") {
				return true
			}
		}
		for _, dir := range []string{storage.BucketsDir, storage.ConfigDir} {
			infos, err := d.StatInfoFile(callCtx, domain.SystemBucket, dir+"/*", true)
			if err != nil || len(infos) > 0 {
				return true
			}
		}
		checked++
	}
	return checked == 0
}

func (b *bootstrapper) writeFormat(ctx context.Context, ep domain.Endpoint, doc *topology.FormatV1) error {
	data, err := doc.Marshal()
	if err != nil {
		return fmt.Errorf("cluster: encode format.json of %s: %w", ep, err)
	}
	callCtx, cancel := context.WithTimeout(ctx, b.opts.timeout)
	defer cancel()
	return b.drives[ep.String()].WriteAll(callCtx, domain.SystemBucket, storage.FormatFile, data)
}

func (b *bootstrapper) result(deploymentID string, states [][]driveFormat, healing map[string]domain.Endpoint) BootstrapResult {
	res := BootstrapResult{DeploymentID: deploymentID}
	for _, pool := range states {
		for _, s := range pool {
			if s.doc == nil {
				res.Offline = append(res.Offline, s.ep)
			}
		}
	}
	for _, ep := range healing {
		res.Healing = append(res.Healing, ep)
	}
	slices.SortFunc(res.Healing, func(a, b domain.Endpoint) int { return strings.Compare(a.String(), b.String()) })
	return res
}

// setGaps describes the sets of pool with fewer than need drives
// satisfying ok, and what their missing drives are.
func setGaps(pool topology.Pool, states []driveFormat, need int, ok func(driveFormat) bool) []string {
	var gaps []string
	for s := 0; s < pool.SetCount; s++ {
		have := 0
		var missing []string
		for _, st := range states[s*pool.SetDriveCount : (s+1)*pool.SetDriveCount] {
			switch {
			case ok(st):
				have++
			case st.err != nil:
				missing = append(missing, st.ep.String()+" (unreachable)")
			case st.unformatted:
				missing = append(missing, st.ep.String()+" (unformatted)")
			default:
				missing = append(missing, st.ep.String())
			}
		}
		if have < need {
			gaps = append(gaps, fmt.Sprintf("set %d has %d of %d drives, needs %d, missing %s",
				s+1, have, pool.SetDriveCount, need, strings.Join(missing, ", ")))
		}
	}
	return gaps
}

// readQuorum is the number of drives of a set needed to read
// (topology.SetQuorums).
func readQuorum(p topology.Pool) int {
	r, _ := topology.SetQuorums(p.SetDriveCount, p.Parity)
	return r
}

// writeQuorum is the number of drives of a set needed to write
// (topology.SetQuorums).
func writeQuorum(p topology.Pool) int {
	_, w := topology.SetQuorums(p.SetDriveCount, p.Parity)
	return w
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// localBootstrapper is the startup of a single-node deployment: no peers,
// every drive is local and the formatter makes them consistent.
type localBootstrapper struct {
	deps BootstrapDeps
	opts bootstrapOptions

	mu           sync.Mutex
	deploymentID string
}

var _ Bootstrapper = (*localBootstrapper)(nil)

// NewLocalBootstrapper returns the bootstrapper of a single-node
// deployment (SNSD, SNMD): it formats or checks the local drives with the
// storage formatter.
func NewLocalBootstrapper(deps BootstrapDeps, opts ...BootstrapOption) (Bootstrapper, error) {
	if err := deps.defaults(); err != nil {
		return nil, err
	}
	if deps.Formatter == nil {
		return nil, fmt.Errorf("%w: cluster: the local bootstrap needs the drive formatter", domain.ErrInvalidArgument)
	}
	return &localBootstrapper{deps: deps, opts: newBootstrapOptions(opts)}, nil
}

// LocalConfig implements Bootstrapper.
func (b *localBootstrapper) LocalConfig(ctx context.Context) (NodeConfig, error) {
	b.mu.Lock()
	id := b.deploymentID
	b.mu.Unlock()
	return localConfig(ctx, LocalNodeID, b.deps, b.opts, id)
}

// Bootstrap implements Bootstrapper.
func (b *localBootstrapper) Bootstrap(ctx context.Context) (BootstrapResult, error) {
	id, err := b.deps.Formatter.EnsureFormats(ctx, b.deps.Layout, b.deps.Drives)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("cluster: %w", err)
	}
	b.mu.Lock()
	b.deploymentID = id
	b.mu.Unlock()
	res := BootstrapResult{DeploymentID: id}
	for _, d := range b.deps.Drives {
		if !d.IsOnline() {
			res.Offline = append(res.Offline, d.Endpoint())
		}
	}
	return res, nil
}
