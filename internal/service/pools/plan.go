package pools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// PlanInput is a change of the pools of the deployment: pools appended
// after the existing ones and pools removed once decommissioned.
type PlanInput struct {
	// Add are the arguments of the new pools, one argument with ellipses
	// per pool ("http://nexo{5...8}:9000/mnt/disk{1...4}").
	Add []string
	// Remove are the pools to remove (0-based): their decommission must be
	// complete.
	Remove []int
}

// PlannedPool is a pool of the planned topology.
type PlannedPool struct {
	// Index is the position of the pool in the planned command line
	// (0-based); Current its position today, -1 for a new pool.
	Index   int
	Current int
	// Arg is the command-line argument of the pool.
	Arg string
	// Nodes are the nodes of the pool ("host:port", "local" for the
	// drives of a single node).
	Nodes        []string
	Drives       int
	Sets         int
	DrivesPerSet int
	Parity       int
}

// Plan is the topology of the deployment after a change of its pools, as
// every node parses it at startup.
type Plan struct {
	Mode domain.Mode
	// Args are the endpoint arguments of every node, in order
	// (NEXO_ENDPOINTS, or the arguments of "nexo server").
	Args  []string
	Pools []PlannedPool
	// Removed are the arguments of the removed pools.
	Removed []string
	// Nodes are every node of the planned deployment; NewNodes the ones
	// that are not part of the deployment today.
	Nodes    []string
	NewNodes []string
}

// Plan implements Service.
func (s *service) Plan(ctx context.Context, in PlanInput) (Plan, error) {
	if s.deps.Topology == nil || s.deps.Layout == nil {
		return Plan{}, fmt.Errorf("%w: planning a topology needs the layout of the deployment", domain.ErrNotImplemented)
	}
	current := s.deps.Layout
	if len(in.Add) == 0 && len(in.Remove) == 0 {
		return Plan{}, fmt.Errorf("%w: add or remove at least one pool", domain.ErrInvalidArgument)
	}
	for _, p := range current.Pools {
		if len(p.Args) != 1 {
			return Plan{}, fmt.Errorf("%w: pool %d is written as %d arguments without ellipses: write it as one argument "+
				"with ellipses (e.g. http://nexo{1...4}:9000/mnt/disk{1...4}) on every node before changing the pools",
				domain.ErrInvalidArgument, p.Index+1, len(p.Args))
		}
	}
	removed := map[int]bool{}
	if len(in.Remove) > 0 {
		st, err := s.Status(ctx)
		if err != nil {
			return Plan{}, err
		}
		for _, i := range in.Remove {
			if err := s.checkPool(i); err != nil {
				return Plan{}, err
			}
			if j := st.Pools[i].Decommission; j == nil || j.State != JobComplete {
				return Plan{}, fmt.Errorf("%w: pool %d is not decommissioned: decommission it and wait for complete first", domain.ErrInvalidArgument, i+1)
			}
			removed[i] = true
		}
	}
	var args, removedArgs []string
	var origin []int
	for _, p := range current.Pools {
		if removed[p.Index] {
			removedArgs = append(removedArgs, p.Args[0])
			continue
		}
		args = append(args, p.Args[0])
		origin = append(origin, p.Index)
	}
	for _, a := range in.Add {
		a = strings.TrimSpace(a)
		if a == "" {
			return Plan{}, fmt.Errorf("%w: empty pool argument", domain.ErrInvalidArgument)
		}
		if len(strings.Fields(a)) != 1 {
			return Plan{}, fmt.Errorf("%w: %q: one argument with ellipses per pool", domain.ErrInvalidArgument, a)
		}
		args = append(args, a)
		origin = append(origin, -1)
	}
	if len(args) == 0 {
		return Plan{}, fmt.Errorf("%w: no pool would be left", domain.ErrInvalidArgument)
	}
	layout, err := s.deps.Topology.Parse(ctx, args)
	if err != nil {
		if errors.Is(err, topology.ErrNoLocalEndpoint) {
			return Plan{}, fmt.Errorf("%w: this node is not part of the planned topology: plan it on a node that stays", domain.ErrInvalidArgument)
		}
		return Plan{}, err
	}
	if len(layout.Pools) != len(args) {
		return Plan{}, fmt.Errorf("%w: every argument must contain ellipses, one pool per argument", domain.ErrInvalidArgument)
	}
	out := Plan{Mode: layout.Mode, Args: args, Removed: removedArgs}
	for i, p := range layout.Pools {
		pp := PlannedPool{Index: i, Current: origin[i], Arg: args[i], Drives: len(p.Endpoints), Sets: p.SetCount,
			DrivesPerSet: p.SetDriveCount, Parity: p.Parity}
		if pp.Current >= 0 {
			// The pools kept must keep their sets: the drives hold them.
			was := current.Pools[pp.Current]
			if was.SetCount != p.SetCount || was.SetDriveCount != p.SetDriveCount || was.Parity != p.Parity {
				return Plan{}, fmt.Errorf("%w: pool %d would change its erasure sets", domain.ErrInvalidLayout, pp.Current+1)
			}
		}
		pp.Nodes = poolNodes(p)
		out.Pools = append(out.Pools, pp)
	}
	known := map[string]bool{}
	for _, n := range current.Nodes {
		known[n.String()] = true
	}
	for _, n := range layout.Nodes {
		out.Nodes = append(out.Nodes, n.String())
		if !known[n.String()] {
			out.NewNodes = append(out.NewNodes, n.String())
		}
	}
	return out, nil
}

// poolNodes lists the nodes of a pool in order of first appearance.
func poolNodes(p topology.Pool) []string {
	var out []string
	for _, ep := range p.Endpoints {
		n := "local"
		if ep.IsURL() {
			n = ep.Host
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}
