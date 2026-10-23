package pools_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/pools"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// planner returns the service of the fixture with the layout of its
// command line.
func (f *fixture) planner(t *testing.T, args []string) pools.Service {
	t.Helper()
	topo := topology.New(topology.Deps{Logger: slog.New(slog.DiscardHandler)})
	layout, err := topo.Parse(context.Background(), args)
	require.NoError(t, err)
	ids := make([]string, 0, len(layout.Pools))
	for _, p := range layout.Pools {
		ids = append(ids, p.Args[0])
	}
	svc, err := pools.New(pools.Deps{Objects: f.objects, Mover: f.mover, Docs: f.docs, PoolIDs: ids, Topology: topo, Layout: layout,
		Logger: slog.New(slog.DiscardHandler)}, pools.WithPollInterval(20*time.Millisecond), pools.WithRetryDelay(0))
	require.NoError(t, err)
	return svc
}

// TestPlanExpansion (S-STO-08): a new pool is appended after the existing
// ones with its sets and parity, as every node would parse the command
// line; invalid pools are refused before any node restarts.
//
// covers: S-STO-08
func TestPlanExpansion(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	svc := f.planner(t, f.ids)

	plan, err := svc.Plan(ctx, pools.PlanInput{Add: []string{" /p4/d{1...8} "}})
	require.NoError(t, err)
	assert.Equal(t, domain.ModeSNMD, plan.Mode)
	assert.Equal(t, []string{"/p1/d{1...4}", "/p2/d{1...4}", "/p3/d{1...4}", "/p4/d{1...8}"}, plan.Args)
	require.Len(t, plan.Pools, 4)
	assert.Equal(t, 0, plan.Pools[0].Current)
	assert.Equal(t, pools.PlannedPool{Index: 3, Current: -1, Arg: "/p4/d{1...8}", Nodes: []string{"local"}, Drives: 8, Sets: 1,
		DrivesPerSet: 8, Parity: plan.Pools[3].Parity}, plan.Pools[3])
	assert.Positive(t, plan.Pools[3].Parity)
	assert.Empty(t, plan.NewNodes, "a single node")

	for name, add := range map[string]string{
		"duplicate drive":  "/p1/d{1...4}",
		"mixed kinds":      "http://nexo{5...8}:9000/d{1...4}",
		"no erasure set":   "/p4/d{1...17}",
		"two arguments":    "/p4/a{1...4} /p4/b{1...4}",
		"without ellipses": "/p4/d1",
		"relative drive":   "p4/d{1...4}",
		"empty pool":       " ",
	} {
		_, err := svc.Plan(ctx, pools.PlanInput{Add: []string{add}})
		require.ErrorIs(t, err, domain.ErrInvalidArgument, name)
	}
	_, err = svc.Plan(ctx, pools.PlanInput{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument, "nothing to change")
}

// TestPlanRemoval (S-STO-09): a pool leaves the command line only once its
// decommission is complete; the pools after it move up.
//
// covers: S-STO-09
func TestPlanRemoval(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	svc := f.planner(t, f.ids)

	_, err := svc.Plan(ctx, pools.PlanInput{Remove: []int{1}})
	require.ErrorIs(t, err, domain.ErrInvalidArgument, "not decommissioned")
	_, err = svc.Plan(ctx, pools.PlanInput{Remove: []int{7}})
	require.ErrorIs(t, err, domain.ErrInvalidArgument, "no such pool")

	require.NoError(t, svc.Start(ctx))
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	require.NoError(t, svc.Decommission(ctx, 1))
	waitJob(t, svc, func(st pools.Status) *pools.Job { return st.Pools[1].Decommission }, pools.JobComplete)

	plan, err := svc.Plan(ctx, pools.PlanInput{Remove: []int{1}, Add: []string{"/p4/d{1...4}"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"/p1/d{1...4}", "/p3/d{1...4}", "/p4/d{1...4}"}, plan.Args)
	assert.Equal(t, []string{"/p2/d{1...4}"}, plan.Removed)
	assert.Equal(t, []int{0, 2, -1}, []int{plan.Pools[0].Current, plan.Pools[1].Current, plan.Pools[2].Current})
}

// TestPlanNeedsLayout: without the layout (a node built without it) and
// with a pool written without ellipses, nothing is planned.
func TestPlanNeedsLayout(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	_, err := f.service(t, nil).Plan(ctx, pools.PlanInput{Add: []string{"/p4/d{1...4}"}})
	require.ErrorIs(t, err, domain.ErrNotImplemented)

	svc := f.planner(t, []string{"/p1/d1", "/p1/d2", "/p1/d3", "/p1/d4"})
	_, err = svc.Plan(ctx, pools.PlanInput{Add: []string{"/p4/d{1...4}"}})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	assert.ErrorContains(t, err, "one argument with ellipses")
}
