package commands

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// newAdminCmd returns the admin command group (docs/02 C-ADM-*): server
// information, IAM, configuration and service control through the Admin
// API of a Nexo server (pkg/nexoadmin, SigV4 with the credentials of the
// alias).
func newAdminCmd(e *env) *cobra.Command {
	cmd := groupCmd("admin", "Administer a Nexo server (Admin API)")
	cmd.Long = `Administer a Nexo server through its Admin API: server information and usage,
users, groups, policies, access keys, configuration and service control.

The Admin API is served by the console listener of the server: see --admin-url
of "nexoctl alias set". Every subcommand takes the alias as its first argument.`
	cmd.AddCommand(
		newAdminInfoCmd(e), newAdminUsageCmd(e), newAdminHealCmd(e),
		newAdminUserCmd(e), newAdminGroupCmd(e), newAdminPolicyCmd(e), newAdminAccessKeyCmd(e),
		newAdminConfigCmd(e), newAdminServiceCmd(e),
	)
	return cmd
}

func newAdminInfoCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "info ALIAS",
		Short:             "Show the server: version, mode, nodes and drives",
		Example:           "  nexoctl admin info local",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			alias, client, err := e.adminAlias(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			info, err := client.ServerInfo(cmd.Context())
			if err != nil {
				return err
			}
			return e.out.Result(adminInfoRecord{Alias: alias, ServerInfo: *info}, e.formatInfo(alias, info))
		},
	}
}

// formatInfo renders the server information: a summary, the pools and
// one block per node with its drives (offline ones highlighted).
func (e *env) formatInfo(alias string, info *nexoadmin.ServerInfo) string {
	var b strings.Builder
	p := e.out
	fmt.Fprintf(&b, "%s  Nexo %s (commit %s, built %s)\n", p.Paint(cli.ColorBold, alias), info.Version, orDash(info.Commit), orDash(info.BuildDate))
	fmt.Fprintf(&b, "Mode: %s  Region: %s  Deployment: %s\n", info.Mode, orDash(info.Region), orDash(info.DeploymentID))
	fmt.Fprintf(&b, "Uptime: %s  Buckets: %d  Objects: %d  Usage: %s\n",
		cli.HumanDuration(time.Duration(info.UptimeSeconds)*time.Second), info.Buckets, info.Objects, cli.HumanSize(int64(info.Usage)))
	for _, pool := range info.Pools {
		fmt.Fprintf(&b, "Pool %d: %d set(s) of %d drive(s), parity %d\n", pool.Index+1, pool.Sets, pool.DrivesPerSet, pool.Parity)
	}
	online, offline := 0, 0
	for _, n := range info.Nodes {
		if n.State == stateOnline {
			online++
		} else {
			offline++
		}
	}
	fmt.Fprintf(&b, "Nodes: %d online, %d offline", online, offline)
	for _, n := range info.Nodes {
		state := p.Paint(cli.ColorGreen, n.State)
		if n.State != stateOnline {
			state = p.Paint(cli.ColorRed, n.State)
		}
		uptime := "-"
		if n.UptimeSeconds > 0 {
			uptime = cli.HumanDuration(time.Duration(n.UptimeSeconds) * time.Second)
		}
		fmt.Fprintf(&b, "\n  %s  %s  uptime %s  %d drive(s)", n.Endpoint, state, uptime, len(n.Disks))
		for _, d := range n.Disks {
			dstate := p.Paint(cli.ColorGreen, d.State)
			if d.State != stateOnline {
				dstate = p.Paint(cli.ColorRed, d.State)
			}
			path := d.Path
			if path == "" {
				path = d.Endpoint
			}
			fmt.Fprintf(&b, "\n    %s  %s", path, dstate)
			if d.Total > 0 {
				fmt.Fprintf(&b, "  used %s of %s (free %s)", cli.HumanSize(int64(d.Used)), cli.HumanSize(int64(d.Total)), cli.HumanSize(int64(d.Free)))
			}
			if d.Healing {
				b.WriteString("  healing")
			}
			if d.Error != "" {
				b.WriteString("  error: " + d.Error)
			}
		}
	}
	return b.String()
}

func newAdminUsageCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "usage ALIAS",
		Aliases:           []string{"du"},
		Short:             "Show the data usage of the server and of each bucket",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			alias, client, err := e.adminAlias(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			du, err := client.DataUsage(cmd.Context())
			if err != nil {
				return err
			}
			if du.Buckets == nil {
				du.Buckets = map[string]nexoadmin.BucketUsage{}
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%-10s %10s %10s  %s", "SIZE", "OBJECTS", "VERSIONS", "BUCKET")
			for _, name := range cli.SortedKeys(du.Buckets) {
				u := du.Buckets[name]
				fmt.Fprintf(&b, "\n%-10s %10d %10d  %s", cli.HumanSize(int64(u.Size)), u.ObjectsCount, u.VersionsCount, name)
			}
			fmt.Fprintf(&b, "\n%-10s %10d %10d  %s", cli.HumanSize(int64(du.TotalSize)), du.ObjectsCount, du.VersionsCount,
				fmt.Sprintf("total (%d bucket(s))", du.BucketsCount))
			return e.out.Result(adminUsageRecord{Alias: alias, DataUsage: *du}, b.String())
		},
	}
}

func newAdminHealCmd(e *env) *cobra.Command {
	var recursive, dryRun bool
	cmd := &cobra.Command{
		Use:   "heal [flags] ALIAS[/BUCKET[/PREFIX]]",
		Short: "Heal the drives of the server (erasure-coded deployments)",
		Long: `Start a healing operation on the whole server, a bucket or a prefix and print its
outcome. With --dry-run the objects are checked and nothing is repaired.`,
		Example:           "  nexoctl admin heal --recursive --dry-run local/photos",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, err := e.remoteTarget(ctx, args[0], false)
			if err != nil {
				return err
			}
			client, err := e.admin(ctx, t.Alias)
			if err != nil {
				return err
			}
			res, err := client.Heal(ctx, nexoadmin.HealRequest{Bucket: t.Bucket, Prefix: t.Key, Recursive: recursive, DryRun: dryRun})
			if err != nil {
				return fmt.Errorf("heal %s: %w", t, err)
			}
			mode := ""
			if dryRun {
				mode = " (dry run)"
			}
			text := fmt.Sprintf("Heal %s%s: %s, scanned %d, healed %d, failed %d", res.ID, mode,
				map[bool]string{true: "finished", false: "running"}[res.Finished], res.Scanned, res.Healed, res.Failed)
			return e.out.Result(adminHealRecord{Alias: t.Alias, DryRun: dryRun, HealResult: *res}, text)
		},
	}
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "heal every object under the bucket or prefix")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "only report what would be healed")
	return cmd
}

func newAdminServiceCmd(e *env) *cobra.Command {
	cmd := groupCmd("service", "Restart or stop the server")
	cmd.AddCommand(
		newAdminServiceActionCmd(e, "restart", "Restart the server process (connections are dropped)", "restarted",
			func(ctx context.Context, c cli.AdminClient) error { return c.RestartService(ctx) }),
		newAdminServiceActionCmd(e, "stop", "Stop the server process (it must be started again by hand)", "stopped",
			func(ctx context.Context, c cli.AdminClient) error { return c.StopService(ctx) }),
	)
	return cmd
}

func newAdminServiceActionCmd(e *env, verb, short, done string, call func(context.Context, cli.AdminClient) error) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:               verb + " [flags] ALIAS",
		Short:             short,
		Long:              short + ".\n\nThe command asks for a confirmation on the terminal; --yes skips it (scripts).",
		Example:           fmt.Sprintf("  nexoctl admin service %s --yes local", verb),
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			if err := e.confirm(yes, fmt.Sprintf("%s the server of alias %q?", capitalize(verb), alias)); err != nil {
				return err
			}
			if err := call(ctx, client); err != nil {
				return fmt.Errorf("%s %s: %w", verb, alias, err)
			}
			return e.out.Info(adminActionRecord{Alias: alias, Type: kindService, Action: done},
				fmt.Sprintf("The server of %q is %s.", alias, map[string]string{"restarted": "restarting", "stopped": "stopping"}[done]))
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for a confirmation")
	return cmd
}

// confirm asks the user to confirm a destructive action on stdin unless
// yes is set; in JSON mode, or when stdin gives no "y"/"yes" answer, the
// action is refused with a usage error.
func (e *env) confirm(yes bool, question string) error {
	if yes {
		return nil
	}
	if e.out.JSON() {
		return cli.UsageError("confirmation required: add --yes")
	}
	_, _ = fmt.Fprintf(e.deps.Stderr, "%s [y/N] ", question)
	answer, err := bufio.NewReader(e.deps.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	_, _ = fmt.Fprintln(e.deps.Stderr)
	return errors.New("not confirmed: nothing done (add --yes to skip the question)")
}

// --- helpers ------------------------------------------------------------------

// stateOnline is the state of the reachable nodes and drives.
const stateOnline = "online"

// listAll collects every page of a list endpoint.
func listAll[T any](ctx context.Context, page func(ctx context.Context, cursor string) ([]T, string, error)) ([]T, error) {
	var out []T
	cursor := ""
	for {
		items, next, err := page(ctx, cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" || next == cursor {
			return out, nil
		}
		cursor = next
	}
}

func listUsers(ctx context.Context, c cli.AdminClient) ([]nexoadmin.User, error) {
	return listAll(ctx, func(ctx context.Context, cursor string) ([]nexoadmin.User, string, error) {
		p, err := c.ListUsers(ctx, nexoadmin.ListOptions{Cursor: cursor})
		if err != nil {
			return nil, "", err
		}
		return p.Items, p.NextCursor, nil
	})
}

func listGroups(ctx context.Context, c cli.AdminClient) ([]nexoadmin.Group, error) {
	return listAll(ctx, func(ctx context.Context, cursor string) ([]nexoadmin.Group, string, error) {
		p, err := c.ListGroups(ctx, nexoadmin.ListOptions{Cursor: cursor})
		if err != nil {
			return nil, "", err
		}
		return p.Items, p.NextCursor, nil
	})
}

func listPolicies(ctx context.Context, c cli.AdminClient) ([]nexoadmin.Policy, error) {
	return listAll(ctx, func(ctx context.Context, cursor string) ([]nexoadmin.Policy, string, error) {
		p, err := c.ListPolicies(ctx, nexoadmin.ListOptions{Cursor: cursor})
		if err != nil {
			return nil, "", err
		}
		return p.Items, p.NextCursor, nil
	})
}

func listAccessKeys(ctx context.Context, c cli.AdminClient, user string) ([]nexoadmin.AccessKey, error) {
	return listAll(ctx, func(ctx context.Context, cursor string) ([]nexoadmin.AccessKey, string, error) {
		p, err := c.ListAccessKeys(ctx, nexoadmin.ListAccessKeysOptions{ListOptions: nexoadmin.ListOptions{Cursor: cursor}, User: user})
		if err != nil {
			return nil, "", err
		}
		return p.Items, p.NextCursor, nil
	})
}

func listShares(ctx context.Context, c cli.AdminClient, owner, bucket string) ([]nexoadmin.Share, error) {
	return listAll(ctx, func(ctx context.Context, cursor string) ([]nexoadmin.Share, string, error) {
		p, err := c.ListShares(ctx, nexoadmin.ListSharesOptions{ListOptions: nexoadmin.ListOptions{Cursor: cursor}, Owner: owner, Bucket: bucket})
		if err != nil {
			return nil, "", err
		}
		return p.Items, p.NextCursor, nil
	})
}

// readDocument reads a JSON document from a file, or from stdin for "-".
func (e *env) readDocument(path string) ([]byte, error) {
	if path == "-" {
		data, err := io.ReadAll(e.deps.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		return data, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// joinOrDash joins names with commas ("-" when empty).
func joinOrDash(s []string) string {
	if len(s) == 0 {
		return "-"
	}
	out := append([]string(nil), s...)
	sort.Strings(out)
	return strings.Join(out, ",")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// splitList splits comma-separated values of repeated flags.
func splitList(values []string) []string {
	var out []string
	for _, v := range values {
		for _, item := range strings.Split(v, ",") {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}
	}
	return out
}

// --- completion -----------------------------------------------------------------

// completeAdmin completes the alias as first argument, then the names
// returned by list (users, groups, policies...).
func completeAdmin(e *env, list func(ctx context.Context, c cli.AdminClient) ([]string, error)) cobra.CompletionFunc {
	aliases := completeAliasNames(e)
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return aliases(cmd, args, toComplete)
		}
		return completeAdminNames(e, cmd, args[0], toComplete, list)
	}
}

// completeAdminFlag completes a flag with the names returned by list, on
// the alias of the first argument.
func completeAdminFlag(e *env, list func(ctx context.Context, c cli.AdminClient) ([]string, error)) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeAdminNames(e, cmd, args[0], toComplete, list)
	}
}

func completeAdminNames(e *env, cmd *cobra.Command, aliasArg, toComplete string, list func(ctx context.Context, c cli.AdminClient) ([]string, error)) ([]cobra.Completion, cobra.ShellCompDirective) {
	if _, err := e.completionAliases(cmd); err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	ctx, cancel := context.WithTimeout(cmdContext(cmd), completionTimeout)
	defer cancel()
	alias, _, _ := strings.Cut(aliasArg, "/")
	client, err := e.admin(ctx, alias)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	all, err := list(ctx, client)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	var out []string
	for _, n := range all {
		if strings.HasPrefix(n, toComplete) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, cobra.ShellCompDirectiveNoFileComp
}

func userNames(ctx context.Context, c cli.AdminClient) ([]string, error) {
	users, err := listUsers(ctx, c)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(users))
	for _, u := range users {
		out = append(out, u.AccessKey)
	}
	return out, nil
}

func groupNames(ctx context.Context, c cli.AdminClient) ([]string, error) {
	groups, err := listGroups(ctx, c)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.Name)
	}
	return out, nil
}

func policyNames(ctx context.Context, c cli.AdminClient) ([]string, error) {
	policies, err := listPolicies(ctx, c)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(policies))
	for _, p := range policies {
		out = append(out, p.Name)
	}
	return out, nil
}

func accessKeyNames(ctx context.Context, c cli.AdminClient) ([]string, error) {
	keys, err := listAccessKeys(ctx, c, "")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.AccessKey)
	}
	return out, nil
}

func shareIDs(ctx context.Context, c cli.AdminClient) ([]string, error) {
	shares, err := listShares(ctx, c, "", "")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(shares))
	for _, s := range shares {
		out = append(out, s.ID)
	}
	return out, nil
}

func configSubsystems(ctx context.Context, c cli.AdminClient) ([]string, error) {
	schema, err := c.ConfigSchema(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(schema))
	for _, s := range schema {
		out = append(out, s.Name)
	}
	return out, nil
}
