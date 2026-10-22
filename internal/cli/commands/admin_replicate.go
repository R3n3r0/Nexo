package commands

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// siteReplicationRecord is the JSON record of admin replicate info and add.
type siteReplicationRecord struct {
	Alias string `json:"alias"`
	nexoadmin.SiteReplicationInfo
}

// newAdminReplicateCmd returns admin replicate (C-ADM-11, S-STO-11).
func newAdminReplicateCmd(e *env) *cobra.Command {
	cmd := groupCmd("replicate", "Keep several deployments in sync (site replication)")
	cmd.Long = `Group several Nexo deployments (sites) so that they stay in sync: their users, groups,
access keys, named policies and buckets with their settings are the same everywhere, and every
bucket replicates its objects to the same bucket of every other site (active-active, the most
recent version wins). A change made on any site reaches the others within seconds; a site that
was unreachable catches up when it comes back.

The sites must share the root credentials: they call each other's Admin API with them and seal
what they exchange with a key derived from them. The configuration, the KMS keys, the
notification targets and the distributions stay per site.`
	cmd.AddCommand(newAdminReplicateAddCmd(e), newAdminReplicateInfoCmd(e), newAdminReplicateRemoveCmd(e), newAdminReplicateSyncCmd(e))
	return cmd
}

func newAdminReplicateAddCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "add ALIAS ALIAS [ALIAS...]",
		Short: "Group the deployments of the aliases (the alias names name the sites)",
		Long: `Group the deployments of the aliases: the first one checks that it reaches every other
with its root credentials, saves the group on every site and starts a first round, which only adds
(nothing is deleted at the first round). The URL of each alias is the S3 endpoint of its site, its
Admin API URL the endpoint the sites call each other on: they must be reachable from every site.`,
		Example:           "  nexoctl admin replicate add site-a site-b site-c",
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			aliases, err := e.loadAliases(ctx)
			if err != nil {
				return err
			}
			var sites []nexoadmin.SiteReplicationSite
			seen := map[string]bool{}
			for _, name := range args {
				if seen[name] {
					return cli.UsageError("alias %q given twice", name)
				}
				seen[name] = true
				a, ok := aliases[name]
				if !ok {
					return &cli.AliasNotFoundError{Name: name}
				}
				admin, err := cli.AdminURL(a)
				if err != nil {
					return err
				}
				sites = append(sites, nexoadmin.SiteReplicationSite{Name: name, Endpoint: strings.TrimRight(a.URL, "/"), AdminEndpoint: admin})
			}
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			info, err := client.AddSiteReplication(ctx, sites)
			if err != nil {
				return err
			}
			return e.out.Info(siteReplicationRecord{Alias: alias, SiteReplicationInfo: *info},
				fmt.Sprintf("Sites %s grouped: they synchronize now (follow it with \"nexoctl admin replicate info %s\").",
					strings.Join(args, ", "), alias))
		},
	}
}

// siteState describes the synchronization with a site in one line.
func siteState(p nexoadmin.SiteReplicationPeerStatus) string {
	switch {
	case p.LastError != "":
		return "error: " + p.LastError
	case p.LastSync.IsZero():
		return "not synchronized yet"
	default:
		return "in sync at " + p.LastSync.UTC().Format(time.RFC3339)
	}
}

func newAdminReplicateInfoCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "info ALIAS",
		Aliases:           []string{"status"},
		Short:             "Show the sites of the group and their synchronization",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			info, err := client.GetSiteReplication(ctx)
			if err != nil {
				return err
			}
			if e.out.JSON() {
				return e.out.Result(siteReplicationRecord{Alias: alias, SiteReplicationInfo: *info}, "")
			}
			if !info.Enabled {
				return e.out.Result(nil, fmt.Sprintf("%q is in no site replication group.", alias))
			}
			lines := []string{fmt.Sprintf("Site %s (deployment %s), %d replicated entries.", info.Name, info.DeploymentID, info.Entries),
				fmt.Sprintf("%-16s %-40s %-40s %s", "SITE", "S3 ENDPOINT", "ADMIN ENDPOINT", "STATE")}
			peers := map[string]nexoadmin.SiteReplicationPeerStatus{}
			for _, p := range info.Peers {
				peers[p.DeploymentID] = p
			}
			for _, s := range info.Sites {
				state := "this site"
				if p, ok := peers[s.DeploymentID]; ok {
					state = siteState(p)
					if p.Pending > 0 {
						state += fmt.Sprintf(" (%d changes pending)", p.Pending)
					}
				}
				lines = append(lines, fmt.Sprintf("%-16s %-40s %-40s %s", s.Name, s.Endpoint, s.AdminEndpoint, state))
			}
			return e.out.Result(nil, strings.Join(lines, "\n"))
		},
	}
}

func newAdminReplicateRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:     "remove ALIAS",
		Aliases: []string{"rm"},
		Short:   "Dissolve the group of the site (on every site reached)",
		Long: `Dissolve the group: every site reached forgets it and the bucket replication rules it
added. Nothing that was replicated is deleted. A site that cannot be reached keeps the group:
run the command on it too.`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			if err := client.RemoveSiteReplication(ctx); err != nil {
				return err
			}
			return e.out.Info(adminActionRecord{Alias: alias, Type: "site-replication", Action: "removed"},
				fmt.Sprintf("Site replication of %q removed.", alias))
		},
	}
}

func newAdminReplicateSyncCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "sync ALIAS",
		Short:             "Run a synchronization round now",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			if err := client.SyncSiteReplication(ctx); err != nil {
				return err
			}
			return e.out.Info(adminActionRecord{Alias: alias, Type: "site-replication", Action: "sync"},
				fmt.Sprintf("Synchronization of %q asked.", alias))
		},
	}
}
