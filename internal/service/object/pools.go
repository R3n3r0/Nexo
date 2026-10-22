package object

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// erasureServerPools is the erasure-coded object layer (SNMD, cluster):
// server pools of erasure sets (docs/03 §3.1). Each object lives in one
// set of one pool: its pool is the one of its existing versions, or one
// chosen by free space for a new key; its set is SipHash(key) mod sets.
// Buckets exist in every set.
type erasureServerPools struct {
	*layer
	pools []*pool
}

var _ Service = (*erasureServerPools)(nil)

// noopHeal is the default HealNotifier: healing is not wired.
type noopHeal struct{}

var _ HealNotifier = (*noopHeal)(nil)

// NotifyHeal implements HealNotifier.
func (*noopHeal) NotifyHeal(context.Context, string, string, string) {}

// NewServerPools returns the erasure object layer over deps.Pools. Every
// set of a pool has the same number of drives (2..16); the parity of the
// STANDARD class is the pool's (or the default of its set size, or the
// WithStorageClass setting), REDUCED_REDUNDANCY uses its own (default 1).
func NewServerPools(deps PoolsDeps, opts ...Option) (Service, error) {
	if len(deps.Pools) == 0 {
		return nil, fmt.Errorf("%w: object: NewServerPools requires at least one pool", domain.ErrInvalidArgument)
	}
	if deps.Codecs == nil {
		return nil, fmt.Errorf("%w: object: NewServerPools requires the erasure codec factory", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Heal == nil {
		deps.Heal = &noopHeal{}
	}
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	codecs := deps.Codecs
	k0, k1 := sipKeys(deps.DeploymentID)
	pools := make([]*pool, 0, len(deps.Pools))
	var stores []store
	for pi, spec := range deps.Pools {
		if len(spec.Sets) == 0 {
			return nil, fmt.Errorf("%w: object: pool %d has no erasure set", domain.ErrInvalidArgument, pi+1)
		}
		size := len(spec.Sets[0])
		if size < topology.MinSetDriveCount || size > topology.MaxSetDriveCount {
			return nil, fmt.Errorf("%w: object: pool %d: erasure sets of %d drives (want %d..%d)",
				domain.ErrInvalidArgument, pi+1, size, topology.MinSetDriveCount, topology.MaxSetDriveCount)
		}
		classes, err := resolveStorageClasses(size, spec.Parity, o)
		if err != nil {
			return nil, fmt.Errorf("object: pool %d: %w", pi+1, err)
		}
		pl := &pool{idx: pi, k0: k0, k1: k1}
		pl.draining.Store(spec.Decommissioning)
		for si, drives := range spec.Sets {
			if len(drives) != size {
				return nil, fmt.Errorf("%w: object: pool %d: set %d has %d drives, set 1 has %d", domain.ErrInvalidArgument, pi+1, si+1, len(drives), size)
			}
			set := &erasureSet{
				poolIdx: pi, setIdx: si, drives: drives, codecs: codecs, classes: classes,
				blockSize: o.blockSize, heal: deps.Heal, logger: deps.Logger,
			}
			pl.sets = append(pl.sets, set)
			stores = append(stores, set)
		}
		pools = append(pools, pl)
	}
	l := newLayer(Deps{
		Buckets: deps.Buckets, Locks: deps.Locks, Events: deps.Events, Heal: deps.Heal, Crypto: deps.Crypto, Tiers: deps.Tiers,
		DeploymentID: deps.DeploymentID, IDs: deps.IDs, Clock: deps.Clock, Logger: deps.Logger,
	}, opts, stores, "erasure")
	l.place = &poolPlacement{pools: pools, spaceTTL: o.poolSpaceTTL, now: l.now, randN: rand.Uint64N}
	for _, pl := range pools {
		for _, set := range pl.sets {
			set.now = l.now
		}
	}
	return &erasureServerPools{layer: l, pools: pools}, nil
}

// NewErasure returns the erasure object layer of a single pool: the sets
// of deps.Sets with deps.Parity (1..N/2) parity shards. It is
// NewServerPools with one pool.
func NewErasure(deps Deps, opts ...Option) (Service, error) {
	if len(deps.Sets) == 0 {
		return nil, fmt.Errorf("%w: object: NewErasure requires at least one erasure set", domain.ErrInvalidArgument)
	}
	for i, drives := range deps.Sets {
		if n := len(drives); deps.Parity < 1 || deps.Parity > n/2 {
			return nil, fmt.Errorf("%w: object: set %d has %d drives and parity %d", domain.ErrInvalidArgument, i+1, n, deps.Parity)
		}
	}
	return NewServerPools(PoolsDeps{
		Pools:        []PoolSpec{{Sets: deps.Sets, Parity: deps.Parity}},
		DeploymentID: deps.DeploymentID,
		Locks:        deps.Locks,
		Heal:         deps.Heal,
		Buckets:      deps.Buckets,
		Events:       deps.Events,
		Codecs:       deps.Codecs,
		Crypto:       deps.Crypto,
		Tiers:        deps.Tiers,
		IDs:          deps.IDs,
		Clock:        deps.Clock,
		Logger:       deps.Logger,
	}, opts...)
}

// Health implements Service: every set must keep its write quorum and,
// with Maintenance, keep it once one more node is down (the node holding
// the most online drives of the set: a single-node deployment never can).
func (p *erasureServerPools) Health(_ context.Context, opts HealthOptions) HealthResult {
	res := HealthResult{Healthy: true}
	var problems []string
	for _, pl := range p.pools {
		for _, set := range pl.sets {
			online, total := set.Online()
			read, write := set.readQuorum(), set.writeQuorum()
			res.ReadQuorum, res.WriteQuorum = max(res.ReadQuorum, read), max(res.WriteQuorum, write)
			for _, d := range set.drives {
				if isOnline(d) {
					if h := d.Healing(); h != nil && !h.Finished {
						res.HealingDrives++
					}
				}
			}
			need := write
			if opts.Maintenance {
				need += onlinePerNode(set.drives)
			}
			if online < need {
				res.Healthy = false
				msg := fmt.Sprintf("pool %d set %d has %d/%d drives online, write quorum is %d", pl.idx+1, set.setIdx+1, online, total, write)
				if opts.Maintenance && online >= write {
					msg += fmt.Sprintf(", %d of them on one node", need-write)
				}
				problems = append(problems, msg)
			}
		}
	}
	res.Message = strings.Join(problems, "; ")
	return res
}

// onlinePerNode returns the largest number of online drives of one node
// among drives (the drives of a node share the host of their endpoint;
// the paths of a single node have none).
func onlinePerNode(drives []storage.StorageAPI) int {
	perNode := map[string]int{}
	most := 0
	for _, d := range drives {
		if !isOnline(d) {
			continue
		}
		host := d.Endpoint().Host
		perNode[host]++
		most = max(most, perNode[host])
	}
	return most
}
