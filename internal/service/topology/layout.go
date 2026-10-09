package topology

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Layout is the validated deployment topology produced by Service.Parse.
type Layout struct {
	// Mode is the detected deployment mode.
	Mode domain.Mode
	// Pools lists the server pools in command line order.
	Pools []Pool
	// Nodes lists the distinct nodes in order of first appearance across pools.
	Nodes []Node
}

// Pool is a group of drives split into erasure sets of equal size.
type Pool struct {
	// Index is the zero-based position of the pool in the command line.
	Index int
	// Args are the original command line arguments that produced the pool.
	Args []string
	// Endpoints lists the drives of the pool in set order: the drives of set s
	// are Endpoints[s*SetDriveCount:(s+1)*SetDriveCount], sorted by DiskIdx.
	Endpoints []domain.Endpoint
	// NodeCount is the number of distinct nodes in the pool.
	NodeCount int
	// SetCount is the number of erasure sets.
	SetCount int
	// SetDriveCount is the number of drives per erasure set (1 only for SNSD).
	SetDriveCount int
	// Parity is the number of parity shards per erasure set (0 only for SNSD).
	Parity int
}

// Node is a server of the deployment with the drives it exposes.
type Node struct {
	// Host is the lower-cased host name or IP of the node; empty for the
	// implicit local node of a single-node deployment.
	Host string
	// Port is the TCP port of the node; zero for a single-node deployment.
	Port int
	// Local is true when the node is the one running this process.
	Local bool
	// Endpoints lists the drives of the node across all pools.
	Endpoints []domain.Endpoint
}

// String returns "host:port" for a network node and "local" for the implicit
// node of a single-node deployment.
func (n Node) String() string {
	if n.Host == "" && n.Port == 0 {
		return "local"
	}
	return net.JoinHostPort(n.Host, strconv.Itoa(n.Port))
}

// key returns the node identifier matching nodeKey of its endpoints.
func (n Node) key() string {
	if n.Host == "" && n.Port == 0 {
		return ""
	}
	return net.JoinHostPort(n.Host, strconv.Itoa(n.Port))
}

// DriveCount returns the number of drives in the pool.
func (p Pool) DriveCount() int { return len(p.Endpoints) }

// DataShards returns the number of data shards per erasure set.
func (p Pool) DataShards() int { return p.SetDriveCount - p.Parity }

// SetQuorums returns the read and write quorums of an erasure set of
// drives drives with parity parity (docs/03 §3.3): D = drives - parity
// drives to read, D to write, D+1 when D == P so that the two halves of a
// split set cannot both write. It is the one rule of the object layer,
// the bootstrap and the cluster information.
func SetQuorums(drives, parity int) (read, write int) {
	d := drives - parity
	if d == parity {
		return d, d + 1
	}
	return d, d
}

// Set returns the drives of erasure set i, ordered by DiskIdx. It returns nil
// when i is out of range.
func (p Pool) Set(i int) []domain.Endpoint {
	if i < 0 || i >= p.SetCount || p.SetDriveCount <= 0 {
		return nil
	}
	lo, hi := i*p.SetDriveCount, (i+1)*p.SetDriveCount
	if hi > len(p.Endpoints) {
		return nil
	}
	return p.Endpoints[lo:hi]
}

// Sets returns all the erasure sets of the pool.
func (p Pool) Sets() [][]domain.Endpoint {
	out := make([][]domain.Endpoint, 0, p.SetCount)
	for i := 0; i < p.SetCount; i++ {
		out = append(out, p.Set(i))
	}
	return out
}

// PoolInfos summarizes the erasure pools of the layout.
func (l *Layout) PoolInfos() []domain.PoolInfo {
	out := make([]domain.PoolInfo, 0, len(l.Pools))
	for _, p := range l.Pools {
		out = append(out, domain.PoolInfo{Index: p.Index, Sets: p.SetCount, DrivesPerSet: p.SetDriveCount, Parity: p.Parity})
	}
	return out
}

// DriveCount returns the total number of drives across all pools.
func (l *Layout) DriveCount() int {
	n := 0
	for _, p := range l.Pools {
		n += len(p.Endpoints)
	}
	return n
}

// Endpoints returns every drive of the deployment in pool and set order.
func (l *Layout) Endpoints() []domain.Endpoint {
	out := make([]domain.Endpoint, 0, l.DriveCount())
	for _, p := range l.Pools {
		out = append(out, p.Endpoints...)
	}
	return out
}

// LocalEndpoints returns the drives that belong to the local node.
func (l *Layout) LocalEndpoints() []domain.Endpoint {
	var out []domain.Endpoint
	for _, p := range l.Pools {
		for _, ep := range p.Endpoints {
			if ep.IsLocal {
				out = append(out, ep)
			}
		}
	}
	return out
}

// LocalNode returns the node running this process, if any.
func (l *Layout) LocalNode() (Node, bool) {
	for _, n := range l.Nodes {
		if n.Local {
			return n, true
		}
	}
	return Node{}, false
}

// String returns a multi-line human readable description for the logs.
func (l *Layout) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "mode=%s pools=%d nodes=%d drives=%d hash=%s\n",
		l.Mode, len(l.Pools), len(l.Nodes), l.DriveCount(), shortHash(l.Hash()))
	for _, p := range l.Pools {
		fmt.Fprintf(&sb, "pool %d: %d node(s) x %d drive(s), %d set(s) x %d drive(s), EC:%d (data %d)\n",
			p.Index+1, p.NodeCount, p.DriveCount()/max(p.NodeCount, 1), p.SetCount, p.SetDriveCount, p.Parity, p.DataShards())
		for s := 0; s < p.SetCount; s++ {
			fmt.Fprintf(&sb, "  set %d:", s+1)
			for _, ep := range p.Set(s) {
				sb.WriteByte(' ')
				sb.WriteString(ep.String())
				if ep.IsLocal && l.Mode == domain.ModeDistributed {
					sb.WriteString("(local)")
				}
			}
			sb.WriteByte('\n')
		}
	}
	for _, n := range l.Nodes {
		fmt.Fprintf(&sb, "node %s: %d drive(s)", n, len(n.Endpoints))
		if n.Local {
			sb.WriteString(" [local]")
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// canonical returns the canonical textual form of the topology. It depends only
// on the mode, the pools, their sets and their endpoints, never on which node
// is local, so that every node of a cluster computes the same value.
func (l *Layout) canonical() string {
	var sb strings.Builder
	sb.WriteString("nexo-topology/v1\n")
	fmt.Fprintf(&sb, "mode=%s\n", l.Mode)
	for _, p := range l.Pools {
		fmt.Fprintf(&sb, "pool=%d sets=%d drives=%d parity=%d\n", p.Index, p.SetCount, p.SetDriveCount, p.Parity)
		for _, ep := range p.Endpoints {
			fmt.Fprintf(&sb, "%d/%d %s\n", ep.SetIdx, ep.DiskIdx, ep.String())
		}
	}
	return sb.String()
}

// Hash returns the hex encoded SHA-256 of the canonical topology. Nodes compare
// it during bootstrap to make sure they were started with the same arguments.
func (l *Layout) Hash() string {
	sum := sha256.Sum256([]byte(l.canonical()))
	return hex.EncodeToString(sum[:])
}

// Validate checks the internal consistency of the layout: known mode, pools
// with complete erasure sets, admissible parity, correct placement indexes,
// endpoints compatible with the mode, a single URL scheme, no duplicates, sets
// spread uniformly across nodes and a node list that covers every endpoint
// exactly once.
func (l *Layout) Validate() error {
	if l == nil {
		return fmt.Errorf("%w: nil layout", ErrInvalidLayout)
	}
	if !validMode(l.Mode) {
		return fmt.Errorf("%w: unknown mode %q", ErrInvalidLayout, l.Mode)
	}
	if len(l.Pools) == 0 {
		return fmt.Errorf("%w: no pools", ErrInvalidLayout)
	}
	if l.Mode == domain.ModeSNSD && (len(l.Pools) != 1 || len(l.Pools[0].Endpoints) != 1) {
		return fmt.Errorf("%w: snsd requires exactly one pool with one drive", ErrInvalidLayout)
	}

	seen := make(map[string]struct{}, l.DriveCount())
	for i, p := range l.Pools {
		if err := l.validatePool(i, p, seen); err != nil {
			return err
		}
	}
	schemes := make(map[string]struct{})
	for _, ep := range l.Endpoints() {
		if ep.IsURL() {
			schemes[ep.Scheme] = struct{}{}
		}
	}
	if len(schemes) > 1 {
		return fmt.Errorf("%w: %w", ErrInvalidLayout, ErrMixedSchemes)
	}

	covered := make(map[string]struct{}, l.DriveCount())
	localNodes := 0
	for _, n := range l.Nodes {
		if len(n.Endpoints) == 0 {
			return fmt.Errorf("%w: node %s has no drives", ErrInvalidLayout, n)
		}
		if n.Local {
			localNodes++
		}
		for _, ep := range n.Endpoints {
			if nodeKey(ep) != n.key() {
				return fmt.Errorf("%w: endpoint %s listed under node %s", ErrInvalidLayout, ep, n)
			}
			if ep.IsLocal != n.Local {
				return fmt.Errorf("%w: endpoint %s locality differs from node %s", ErrInvalidLayout, ep, n)
			}
			key := ep.String()
			if _, dup := covered[key]; dup {
				return fmt.Errorf("%w: endpoint %s listed twice in nodes", ErrInvalidLayout, ep)
			}
			covered[key] = struct{}{}
		}
	}
	if len(covered) != len(seen) {
		return fmt.Errorf("%w: nodes cover %d drives, pools have %d", ErrInvalidLayout, len(covered), len(seen))
	}
	if localNodes > 1 {
		return fmt.Errorf("%w: %d nodes are marked local", ErrInvalidLayout, localNodes)
	}
	return nil
}

func (l *Layout) validatePool(i int, p Pool, seen map[string]struct{}) error {
	if p.Index != i {
		return fmt.Errorf("%w: pool at position %d has index %d", ErrInvalidLayout, i, p.Index)
	}
	if len(p.Endpoints) == 0 {
		return fmt.Errorf("%w: pool %d has no drives", ErrInvalidLayout, i)
	}
	if p.SetCount < 1 || p.SetDriveCount < 1 || p.SetCount*p.SetDriveCount != len(p.Endpoints) {
		return fmt.Errorf("%w: pool %d has %d drives but %d set(s) x %d", ErrInvalidLayout, i, len(p.Endpoints), p.SetCount, p.SetDriveCount)
	}
	if l.Mode == domain.ModeSNSD {
		if p.SetDriveCount != 1 {
			return fmt.Errorf("%w: snsd pool with %d drives per set", ErrInvalidLayout, p.SetDriveCount)
		}
	} else if p.SetDriveCount < MinSetDriveCount || p.SetDriveCount > MaxSetDriveCount {
		return fmt.Errorf("%w: pool %d set size %d outside [%d..%d]", ErrInvalidLayout, i, p.SetDriveCount, MinSetDriveCount, MaxSetDriveCount)
	}
	if err := validateParity(p.Parity, p.SetDriveCount); err != nil {
		return fmt.Errorf("%w: pool %d: %w", ErrInvalidLayout, i, err)
	}

	nodes := make(map[string]struct{})
	for j, ep := range p.Endpoints {
		if err := validateEndpoint(ep); err != nil {
			return fmt.Errorf("%w: pool %d: %w", ErrInvalidLayout, i, err)
		}
		if ep.IsURL() != (l.Mode == domain.ModeDistributed) {
			return fmt.Errorf("%w: endpoint %s is incompatible with mode %s", ErrInvalidLayout, ep, l.Mode)
		}
		if ep.PoolIdx != i || ep.SetIdx != j/p.SetDriveCount || ep.DiskIdx != j%p.SetDriveCount {
			return fmt.Errorf("%w: endpoint %s has placement %d/%d/%d, expected %d/%d/%d",
				ErrInvalidLayout, ep, ep.PoolIdx, ep.SetIdx, ep.DiskIdx, i, j/p.SetDriveCount, j%p.SetDriveCount)
		}
		key := ep.String()
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%w: %s", ErrDuplicateEndpoint, ep)
		}
		seen[key] = struct{}{}
		nodes[nodeKey(ep)] = struct{}{}
	}
	if p.NodeCount != len(nodes) {
		return fmt.Errorf("%w: pool %d declares %d nodes, found %d", ErrInvalidLayout, i, p.NodeCount, len(nodes))
	}
	for s := 0; s < p.SetCount; s++ {
		if err := validateSetSymmetry(p.Set(s), p.NodeCount); err != nil {
			return fmt.Errorf("%w: pool %d set %d: %v", ErrInvalidLayout, i, s, err)
		}
	}
	return nil
}

// validateSetSymmetry checks that a set takes the same number of drives from
// every node it touches and touches min(len(set), nodeCount) distinct nodes.
func validateSetSymmetry(set []domain.Endpoint, nodeCount int) error {
	perNode := make(map[string]int)
	for _, ep := range set {
		perNode[nodeKey(ep)]++
	}
	want := min(len(set), nodeCount)
	if len(perNode) != want {
		return fmt.Errorf("drives spread over %d node(s), expected %d", len(perNode), want)
	}
	count := -1
	for _, c := range perNode {
		if count == -1 {
			count = c
		} else if c != count {
			return fmt.Errorf("uneven number of drives per node")
		}
	}
	return nil
}

// validateEndpoint checks the fields of a single canonical endpoint.
func validateEndpoint(ep domain.Endpoint) error {
	if ep.Path == "" {
		return fmt.Errorf("%w: empty path", ErrInvalidEndpoint)
	}
	if !ep.IsURL() {
		if ep.Host != "" {
			return fmt.Errorf("%w: local path %q with host", ErrInvalidEndpoint, ep.Path)
		}
		if !strings.HasPrefix(ep.Path, "/") {
			return fmt.Errorf("%w: %q", ErrRelativePath, ep.Path)
		}
		if ep.Path == "/" {
			return ErrRootPath
		}
		return nil
	}
	if ep.Scheme != "http" && ep.Scheme != "https" {
		return fmt.Errorf("%w: %q", ErrInvalidScheme, ep.Scheme)
	}
	if ep.Hostname() == "" {
		return fmt.Errorf("%w: missing host in %s", ErrInvalidEndpoint, ep)
	}
	if port := endpointPort(ep); port < 1 || port > 65535 {
		return fmt.Errorf("%w: port %q out of range in %s", ErrInvalidEndpoint, ep.Port(), ep)
	}
	if !strings.HasPrefix(ep.Path, "/") {
		return fmt.Errorf("%w: %q", ErrRelativePath, ep.Path)
	}
	if ep.Path == "/" {
		return ErrRootPath
	}
	return nil
}
