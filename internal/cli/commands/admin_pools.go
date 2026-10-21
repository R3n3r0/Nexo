package commands

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// poolRecord is the JSON record of a pool of admin decommission status.
type poolRecord struct {
	Alias string `json:"alias"`
	nexoadmin.PoolStatus
}

// rebalanceRecord is the JSON record of admin rebalance status.
type rebalanceRecord struct {
	Alias     string             `json:"alias"`
	Rebalance *nexoadmin.PoolJob `json:"rebalance"`
}

// newAdminDecommissionCmd returns admin decommission (C-ADM-10, S-STO-09).
func newAdminDecommissionCmd(e *env) *cobra.Command {
	cmd := groupCmd("decommission", "Drain a server pool so that it can be removed")
	cmd.Long = `Drain a server pool: it takes no new object, and every version of its objects moves to the
other pools, verified before the source copy is removed. Once the status reports "complete",
remove the pool from the command line of every node and restart them. The first pool, which
holds the system documents, cannot be decommissioned.`
	cmd.AddCommand(newAdminDecommissionStartCmd(e), newAdminDecommissionStatusCmd(e), newAdminDecommissionCancelCmd(e))
	return cmd
}

// parsePool parses a 1-based pool position.
func parsePool(arg string) (int, error) {
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 {
		return 0, cli.UsageError("invalid pool %q: give its position in the command line (1, 2, ...)", arg)
	}
	return n, nil
}

func newAdminDecommissionStartCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "start ALIAS POOL",
		Short:             "Start the decommission of a pool (its 1-based position in the command line)",
		Example:           "  nexoctl admin decommission start cluster 2",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			pool, err := parsePool(args[1])
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			if err := client.DecommissionPool(ctx, pool); err != nil {
				return err
			}
			return e.out.Info(adminActionRecord{Alias: alias, Type: "pool", Name: args[1], Action: "decommission started"},
				fmt.Sprintf("Decommission of pool %d started on %q: follow it with \"nexoctl admin decommission status %s\".", pool, alias, alias))
		},
	}
}

func newAdminDecommissionCancelCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "cancel ALIAS POOL",
		Short:             "Cancel the decommission of a pool (it takes new objects again)",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			pool, err := parsePool(args[1])
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			if err := client.CancelDecommission(ctx, pool); err != nil {
				return err
			}
			return e.out.Info(adminActionRecord{Alias: alias, Type: "pool", Name: args[1], Action: "decommission canceled"},
				fmt.Sprintf("Decommission of pool %d canceled on %q.", pool, alias))
		},
	}
}

// jobSummary describes the progress of a job in one line.
func jobSummary(j *nexoadmin.PoolJob) string {
	if j == nil {
		return "-"
	}
	parts := []string{j.State, fmt.Sprintf("%d objects (%s) moved", j.Objects, cli.HumanSize(j.Bytes))}
	if j.Pass > 1 {
		parts = append(parts, fmt.Sprintf("pass %d", j.Pass))
	}
	if j.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed, retried", j.Failed))
	}
	if j.PendingUploads > 0 {
		parts = append(parts, fmt.Sprintf("waiting for %d multipart uploads", j.PendingUploads))
	}
	if j.State == "running" && j.Bucket != "" {
		parts = append(parts, "at "+j.Bucket+"/"+j.Object)
	}
	if j.Error != "" && j.State != "complete" {
		parts = append(parts, "error: "+j.Error)
	}
	return strings.Join(parts, ", ")
}

func newAdminDecommissionStatusCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "status ALIAS",
		Short:             "Show the pools, their usage and their decommission",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			pools, err := client.ListPools(ctx)
			if err != nil {
				return err
			}
			if err := e.out.Text(fmt.Sprintf("%-5s %-9s %-10s %-10s %s", "POOL", "STATE", "USED", "TOTAL", "DECOMMISSION")); err != nil {
				return err
			}
			for _, p := range pools.Pools {
				state := "active"
				if p.Draining {
					state = "draining"
				}
				// #nosec G115 -- capacities fit in an int64
				used, total := int64(p.Total-p.Free), int64(p.Total)
				text := fmt.Sprintf("%-5d %-9s %-10s %-10s %s", p.Index, state, cli.HumanSize(used), cli.HumanSize(total), jobSummary(p.Decommission))
				if err := e.out.Result(poolRecord{Alias: alias, PoolStatus: p}, text); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// newAdminRebalanceCmd returns admin rebalance (C-ADM-10, S-STO-10).
func newAdminRebalanceCmd(e *env) *cobra.Command {
	cmd := groupCmd("rebalance", "Move objects from the fullest pools to the emptiest ones")
	cmd.AddCommand(
		&cobra.Command{
			Use:               "start ALIAS",
			Short:             "Start a rebalance between the pools",
			Args:              exactArgs(1),
			ValidArgsFunction: completeAliasNames(e),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx := cmd.Context()
				alias, client, err := e.adminAlias(ctx, args[0])
				if err != nil {
					return err
				}
				if err := client.StartRebalance(ctx); err != nil {
					return err
				}
				return e.out.Info(adminActionRecord{Alias: alias, Type: "rebalance", Action: "started"},
					fmt.Sprintf("Rebalance started on %q.", alias))
			},
		},
		&cobra.Command{
			Use:               "stop ALIAS",
			Short:             "Stop the rebalance",
			Args:              exactArgs(1),
			ValidArgsFunction: completeAliasNames(e),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx := cmd.Context()
				alias, client, err := e.adminAlias(ctx, args[0])
				if err != nil {
					return err
				}
				if err := client.StopRebalance(ctx); err != nil {
					return err
				}
				return e.out.Info(adminActionRecord{Alias: alias, Type: "rebalance", Action: "stopped"},
					fmt.Sprintf("Rebalance stopped on %q.", alias))
			},
		},
		&cobra.Command{
			Use:               "status ALIAS",
			Short:             "Show the rebalance",
			Args:              exactArgs(1),
			ValidArgsFunction: completeAliasNames(e),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx := cmd.Context()
				alias, client, err := e.adminAlias(ctx, args[0])
				if err != nil {
					return err
				}
				pools, err := client.ListPools(ctx)
				if err != nil {
					return err
				}
				text := "No rebalance was started."
				if pools.Rebalance != nil {
					text = "Rebalance: " + jobSummary(pools.Rebalance)
				}
				return e.out.Result(rebalanceRecord{Alias: alias, Rebalance: pools.Rebalance}, text)
			},
		},
	)
	return cmd
}
