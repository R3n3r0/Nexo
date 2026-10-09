package commands

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// adminClusterExportRecord is the JSON record of admin cluster export
// --output.
type adminClusterExportRecord struct {
	Alias string `json:"alias"`
	File  string `json:"file"`
	Bytes int    `json:"bytes"`
}

// adminClusterImportRecord is the JSON record of admin cluster import.
type adminClusterImportRecord struct {
	Alias string `json:"alias"`
	File  string `json:"file"`
	nexoadmin.ClusterImportResult
}

// lockRecord is the JSON record of a namespace lock.
type lockRecord struct {
	Alias string `json:"alias"`
	nexoadmin.Lock
}

// newAdminClusterCmd returns admin cluster (C-ADM-13): the export and the
// import of the state of a deployment besides its objects, to migrate it
// to another mode with "nexoctl mirror" (docs/08 §8.2) or back it up.
func newAdminClusterCmd(e *env) *cobra.Command {
	cmd := groupCmd("cluster", "Export and import the state of a deployment (IAM, policies, configuration, bucket settings)")
	cmd.Long = `Export and import the state of a deployment besides its objects: users, groups,
access keys and their policies, the named policies, the dynamic configuration and the settings
of every bucket (versioning, object lock, policy, lifecycle, encryption, quota, tags, CORS,
notifications), in one zip archive.

To migrate a deployment (e.g. single drive to cluster), export the source and import the
archive into the target first (it creates the buckets with their versioning), then copy the
objects with every version with "nexoctl mirror --preserve". The IAM part of the archive is
encrypted with the root credentials of the source: the target must use the same ones. The
archive holds the secrets of the configuration: protect it.`
	cmd.AddCommand(newAdminClusterExportCmd(e), newAdminClusterImportCmd(e))
	return cmd
}

func newAdminClusterExportCmd(e *env) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:               "export [flags] ALIAS",
		Short:             "Export the state of the deployment as a zip archive (stdout or --output)",
		Example:           "  nexoctl admin cluster export local --output backup.zip\n  nexoctl admin cluster export local > backup.zip",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			data, err := client.ExportCluster(ctx)
			if err != nil {
				return err
			}
			if output == "" || output == "-" {
				_, err = e.out.Stdout().Write(data)
				return err
			}
			if err := os.WriteFile(output, data, 0o600); err != nil {
				return err
			}
			return e.out.Info(adminClusterExportRecord{Alias: alias, File: output, Bytes: len(data)},
				fmt.Sprintf("Exported the state of %q to %s (%s).", alias, output, cli.HumanSize(int64(len(data)))))
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "write the archive to this file (mode 0600)")
	return cmd
}

func newAdminClusterImportCmd(e *env) *cobra.Command {
	var (
		mode   string
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "import [flags] ALIAS FILE",
		Short: "Import an archive of \"admin cluster export\" (FILE, or - for stdin)",
		Long: `Import an archive of "admin cluster export". The whole archive is validated first;
--dry-run stops there and shows what the import would do. --mode merge (default) keeps
what the archive does not hold; --mode replace also replaces the whole configuration and
removes the users, groups and named policies the archive does not hold (buckets are never
removed).`,
		Example:           "  nexoctl admin cluster import cluster backup.zip --dry-run\n  nexoctl admin cluster import cluster backup.zip",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAdminPolicyFile(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if mode != "merge" && mode != "replace" {
				return cli.UsageError("--mode must be merge or replace, not %q", mode)
			}
			data, err := e.readDocument(args[1])
			if err != nil {
				return err
			}
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			res, err := client.ImportCluster(ctx, data, nexoadmin.ImportClusterOptions{Mode: mode, DryRun: dryRun})
			if err != nil {
				return fmt.Errorf("import %s: %w", args[1], err)
			}
			verb := "Imported"
			if res.DryRun {
				verb = "Would import"
			}
			lines := []string{fmt.Sprintf("%s into %q (%s): %d user(s), %d group(s), %d polic(ies), %d configuration subsystem(s), %d bucket(s).",
				verb, alias, res.Mode, res.Users, res.Groups, res.Policies, res.ConfigSubsystems, res.Buckets)}
			if len(res.BucketsCreated) > 0 {
				lines = append(lines, "Buckets created: "+strings.Join(res.BucketsCreated, ", "))
			}
			if len(res.Removed) > 0 {
				lines = append(lines, "Removed: "+strings.Join(res.Removed, ", "))
			}
			return e.out.Result(adminClusterImportRecord{Alias: alias, File: args[1], ClusterImportResult: *res}, strings.Join(lines, "\n"))
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "merge", "merge (keep what the archive does not hold) or replace")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate the archive and show what would change, without importing")
	return cmd
}

// newAdminLockCmd returns admin lock (docs/03 §3.6): the diagnostics of
// the namespace locks of a cluster.
func newAdminLockCmd(e *env) *cobra.Command {
	cmd := groupCmd("lock", "List and release the namespace locks of a cluster")
	cmd.AddCommand(newAdminLockListCmd(e), newAdminLockClearCmd(e))
	return cmd
}

func newAdminLockListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS",
		Aliases:           []string{"ls"},
		Short:             "List the locks held across the lock servers",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			locks, err := client.ListLocks(ctx)
			if err != nil {
				return err
			}
			if len(locks) > 0 {
				if err := e.out.Text(fmt.Sprintf("%-40s %-6s %-22s %-8s %s", "RESOURCE", "TYPE", "OWNER", "SERVERS", "SINCE")); err != nil {
					return err
				}
			}
			for _, l := range locks {
				text := fmt.Sprintf("%s %-6s %-22s %-8s %s", e.out.Paint(cli.ColorCyan, fmt.Sprintf("%-40s", l.Resource)), l.Type, l.Owner,
					fmt.Sprintf("%d/%d", l.Servers, l.Total), l.Since.UTC().Format(time.RFC3339))
				if err := e.out.Result(lockRecord{Alias: alias, Lock: l}, text); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newAdminLockClearCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "clear ALIAS RESOURCE...",
		Short: "Release locks on every lock server, whoever holds them",
		Long: `Release the locks of the given resources ("bucket/key") on every lock server: the
diagnostics of a stuck lock. An operation holding a released lock loses it at its next lease
renewal and stops before its commit.`,
		Example:           "  nexoctl admin lock clear cluster photos/2024/a.jpg",
		Args:              minArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			if err := client.ForceUnlock(ctx, args[1:]...); err != nil {
				return err
			}
			return e.out.Info(adminActionRecord{Alias: alias, Type: "lock", Action: "released", Names: args[1:]},
				fmt.Sprintf("Released %s on %q.", strings.Join(args[1:], ", "), alias))
		},
	}
}
