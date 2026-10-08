package cluster

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Defaults of the info service.
const (
	// DefaultSkewThreshold is the clock offset above which a peer is
	// reported (docs/03 §3.11).
	DefaultSkewThreshold = 2 * time.Second
	defaultInfoTimeout   = 5 * time.Second
)

// InfoDeps are the dependencies of the info service.
type InfoDeps struct {
	// NodeID identifies this node ("local" by default).
	NodeID string
	// Nodes are the nodes of the deployment (NodesOf), used to report the
	// drives of the unreachable peers.
	Nodes []Node
	// Drives are the local drives of this node.
	Drives []storage.StorageAPI
	// Peers are the other nodes (none on a single node).
	Peers  []Peer
	Clock  clock.Clock
	Logger *slog.Logger
}

// InfoOption configures the info service.
type InfoOption func(*infoOptions)

type infoOptions struct {
	version, commit string
	skew            time.Duration
	timeout         time.Duration
}

// WithBuildInfo sets the version and commit this node reports.
func WithBuildInfo(version, commit string) InfoOption {
	return func(o *infoOptions) { o.version, o.commit = version, commit }
}

// WithSkewThreshold sets the clock offset reported as excessive (default
// 2s).
func WithSkewThreshold(d time.Duration) InfoOption {
	return func(o *infoOptions) {
		if d > 0 {
			o.skew = d
		}
	}
}

// WithInfoTimeout bounds the calls to each peer (default 5s).
func WithInfoTimeout(d time.Duration) InfoOption {
	return func(o *infoOptions) {
		if d > 0 {
			o.timeout = d
		}
	}
}

// info implements Info.
type info struct {
	deps    InfoDeps
	opts    infoOptions
	started time.Time
}

var _ Info = (*info)(nil)

// NewInfo returns the info service of a node; without peers it describes
// a single-node deployment.
func NewInfo(deps InfoDeps, opts ...InfoOption) Info {
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.NodeID == "" {
		deps.NodeID = LocalNodeID
	}
	o := infoOptions{version: "dev", skew: DefaultSkewThreshold, timeout: defaultInfoTimeout}
	for _, opt := range opts {
		opt(&o)
	}
	return &info{deps: deps, opts: o, started: deps.Clock.Now()}
}

// Ping implements Info.
func (i *info) Ping(ctx context.Context) (PingResult, error) {
	if err := ctx.Err(); err != nil {
		return PingResult{}, err
	}
	now := i.deps.Clock.Now()
	return PingResult{NodeID: i.deps.NodeID, Version: i.opts.version, Time: now, Uptime: now.Sub(i.started)}, nil
}

// Local implements Info.
func (i *info) Local(ctx context.Context) (domain.NodeInfo, error) {
	if err := ctx.Err(); err != nil {
		return domain.NodeInfo{}, err
	}
	out := domain.NodeInfo{
		Endpoint: i.deps.NodeID, State: domain.StateOnline, Version: i.opts.version, Commit: i.opts.commit,
		Uptime: i.deps.Clock.Now().Sub(i.started),
	}
	out.Disks = make([]domain.DiskInfo, len(i.deps.Drives))
	var wg sync.WaitGroup
	for n, d := range i.deps.Drives {
		wg.Add(1)
		go func() {
			defer wg.Done()
			di, err := d.DiskInfo(ctx, storage.DiskInfoOptions{})
			if err != nil {
				ep := d.Endpoint()
				di.Endpoint, di.PoolIdx, di.SetIdx, di.DiskIdx = ep.String(), ep.PoolIdx, ep.SetIdx, ep.DiskIdx
				di.State, di.Error = domain.StateOffline, err.Error()
			}
			out.Disks[n] = di
		}()
	}
	wg.Wait()
	return out, nil
}

// Nodes implements Info.
func (i *info) Nodes(ctx context.Context) ([]domain.NodeInfo, error) {
	local, err := i.Local(ctx)
	if err != nil {
		return nil, err
	}
	peers := make([]domain.NodeInfo, len(i.deps.Peers))
	var wg sync.WaitGroup
	for n, p := range i.deps.Peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(ctx, i.opts.timeout)
			defer cancel()
			ni, err := p.NodeInfo(callCtx)
			if err != nil {
				ni = i.offline(p.NodeID(), err)
			}
			peers[n] = ni
		}()
	}
	wg.Wait()
	// Topology order when the nodes are known, this node first.
	order := make(map[string]int, len(i.deps.Nodes))
	for n, node := range i.deps.Nodes {
		order[node.ID] = n
	}
	out := append([]domain.NodeInfo{local}, peers...)
	if len(order) > 0 {
		sortNodes(out[1:], order)
	}
	return out, ctx.Err()
}

// offline describes an unreachable peer with its drives.
func (i *info) offline(id string, err error) domain.NodeInfo {
	ni := domain.NodeInfo{Endpoint: id, State: domain.StateOffline}
	for _, node := range i.deps.Nodes {
		if node.ID != id {
			continue
		}
		for _, ep := range node.Endpoints {
			ni.Disks = append(ni.Disks, domain.DiskInfo{
				Endpoint: ep.String(), Path: ep.Path, PoolIdx: ep.PoolIdx, SetIdx: ep.SetIdx, DiskIdx: ep.DiskIdx,
				State: domain.StateOffline, Error: err.Error(),
			})
		}
	}
	return ni
}

func sortNodes(nodes []domain.NodeInfo, order map[string]int) {
	rank := func(id string) int {
		if r, ok := order[id]; ok {
			return r
		}
		return len(order)
	}
	for a := 1; a < len(nodes); a++ {
		for b := a; b > 0 && rank(nodes[b].Endpoint) < rank(nodes[b-1].Endpoint); b-- {
			nodes[b], nodes[b-1] = nodes[b-1], nodes[b]
		}
	}
}

// ClockSkew implements Info.
func (i *info) ClockSkew(ctx context.Context) ([]Skew, error) {
	out := make([]Skew, len(i.deps.Peers))
	var wg sync.WaitGroup
	for n, p := range i.deps.Peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(ctx, i.opts.timeout)
			defer cancel()
			sent := i.deps.Clock.Now()
			res, err := p.Ping(callCtx)
			received := i.deps.Clock.Now()
			s := Skew{NodeID: p.NodeID()}
			if err != nil {
				s.Err = err.Error()
				out[n] = s
				return
			}
			s.RTT = received.Sub(sent)
			s.Offset = res.Time.Sub(sent.Add(s.RTT / 2))
			s.Exceeded = s.Offset > i.opts.skew || s.Offset < -i.opts.skew
			if s.Exceeded {
				i.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "cluster: clock skew between nodes",
					slog.String("peer", s.NodeID), slog.Duration("offset", s.Offset), slog.Duration("threshold", i.opts.skew),
					slog.String("hint", "synchronize the clocks (NTP): SigV4 tolerates 15 minutes, expirations and lifecycle rules use the node clocks"))
			}
			out[n] = s
		}()
	}
	wg.Wait()
	return out, ctx.Err()
}
