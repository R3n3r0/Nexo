package commands

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// tierRecord is the JSON record of a tier.
type tierRecord struct {
	Alias string `json:"alias"`
	nexoadmin.Tier
}

// newAdminTierCmd returns admin tier (S-STO-12).
func newAdminTierCmd(e *env) *cobra.Command {
	cmd := groupCmd("tier", "Manage the remote tiers of the lifecycle transitions")
	cmd.Long = `Manage the remote tiers: buckets of other S3 servers (Nexo, a cloud provider) that the
Transition actions of the bucket lifecycle move the data of old versions to. The name of a tier is
the storage class the transitions name (nexoctl ilm rule add --transition-days 30 --transition-tier COLD).

A transitioned version keeps its metadata here and is read from the tier, transparently; its
storage class becomes the name of the tier. A copy of the object onto itself brings its data back.
The objects of the tier that no version refers to any more are removed by a periodic sweep. A tier
that holds data cannot be removed.

The remote bucket is given as REMOTE_ALIAS/BUCKET: the URL and the credentials of that alias are
stored, sealed, by the server (read, write, list and delete in the bucket).`
	cmd.AddCommand(newAdminTierAddCmd(e), newAdminTierLsCmd(e), newAdminTierEditCmd(e), newAdminTierRmCmd(e))
	return cmd
}

func newAdminTierAddCmd(e *env) *cobra.Command {
	var prefix, class string
	cmd := &cobra.Command{
		Use:   "add ALIAS NAME REMOTE_ALIAS/BUCKET",
		Short: "Add a remote tier (its bucket is checked with the credentials of the remote alias)",
		Example: `  nexoctl admin tier add myminio COLD archive/cold-data --prefix nexo/
  nexoctl admin tier add myminio GLACIER-IR aws/archive --storage-class GLACIER_IR`,
		Args:              exactArgs(3),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			remote, err := e.replicaRemote(ctx, args[2])
			if err != nil {
				return err
			}
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			t, err := client.AddTier(ctx, nexoadmin.TierInput{Name: args[1], Endpoint: remote.Endpoint, Bucket: remote.Bucket, Prefix: prefix,
				Region: remote.Region, StorageClass: class, AccessKey: remote.AccessKey, SecretKey: remote.SecretKey})
			if err != nil {
				return err
			}
			return e.out.Info(tierRecord{Alias: alias, Tier: *t},
				fmt.Sprintf("Tier %s added: %s/%s%s.", t.Name, t.Endpoint, t.Bucket, prefixSuffix(t.Prefix)))
		},
	}
	cmd.Flags().StringVar(&prefix, "prefix", "", "prefix of the objects in the remote bucket (ending with '/')")
	cmd.Flags().StringVar(&class, "storage-class", "", "storage class of the remote objects (readable at once)")
	return cmd
}

func prefixSuffix(p string) string {
	if p == "" {
		return ""
	}
	return "/" + strings.TrimSuffix(p, "/")
}

func newAdminTierLsCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "ls ALIAS",
		Aliases:           []string{"list"},
		Short:             "List the tiers",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			tiers, err := client.ListTiers(ctx)
			if err != nil {
				return err
			}
			if e.out.JSON() {
				for _, t := range tiers {
					if err := e.out.Result(tierRecord{Alias: alias, Tier: t}, ""); err != nil {
						return err
					}
				}
				return nil
			}
			if len(tiers) == 0 {
				return e.out.Result(nil, fmt.Sprintf("%q has no tier.", alias))
			}
			lines := []string{fmt.Sprintf("%-16s %-40s %-24s %-14s %s", "NAME", "ENDPOINT", "BUCKET", "CLASS", "ACCESS KEY")}
			for _, t := range tiers {
				class := t.StorageClass
				if class == "" {
					class = "-"
				}
				lines = append(lines, fmt.Sprintf("%-16s %-40s %-24s %-14s %s", t.Name, t.Endpoint, t.Bucket+prefixSuffix(t.Prefix), class, t.AccessKey))
			}
			return e.out.Result(nil, strings.Join(lines, "\n"))
		},
	}
}

func newAdminTierEditCmd(e *env) *cobra.Command {
	var class string
	cmd := &cobra.Command{
		Use:   "edit ALIAS NAME REMOTE_ALIAS",
		Short: "Replace the credentials of a tier with those of the remote alias",
		Long: `Replace the credentials (and the region) of a tier with those of REMOTE_ALIAS, checked first;
the endpoint, the bucket and the prefix of a tier never change.`,
		Example:           "  nexoctl admin tier edit myminio COLD archive-rotated",
		Args:              exactArgs(3),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			aliases, err := e.loadAliases(ctx)
			if err != nil {
				return err
			}
			a, ok := aliases[strings.TrimSuffix(args[2], "/")]
			if !ok {
				return &cli.AliasNotFoundError{Name: args[2]}
			}
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			t, err := client.EditTier(ctx, args[1], nexoadmin.TierInput{Region: a.Region, StorageClass: class, AccessKey: a.AccessKey, SecretKey: a.SecretKey})
			if err != nil {
				return err
			}
			return e.out.Info(tierRecord{Alias: alias, Tier: *t}, fmt.Sprintf("Credentials of tier %s replaced.", t.Name))
		},
	}
	cmd.Flags().StringVar(&class, "storage-class", "", "new storage class of the remote objects")
	return cmd
}

func newAdminTierRmCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "rm ALIAS NAME",
		Aliases:           []string{"remove"},
		Short:             "Remove a tier that holds no data",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			if err := client.RemoveTier(ctx, args[1]); err != nil {
				return err
			}
			return e.out.Info(adminActionRecord{Alias: alias, Type: "tier", Name: args[1], Action: "removed"},
				fmt.Sprintf("Tier %s removed.", args[1]))
		},
	}
}
