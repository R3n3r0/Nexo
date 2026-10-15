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
users, groups, policies, access keys, configuration, service control, the export and
import of the state of a deployment, the lock diagnostics, the KMS of the
server-side encryption, the notification targets, the live trace of the requests
and the live logs, the audit log, the metrics and their Prometheus configuration,
the runtime profiles and the speed tests.

The Admin API is served by the console listener of the server: see --admin-url
of "nexoctl alias set". Every subcommand takes the alias as its first argument.`
	cmd.AddCommand(
		newAdminInfoCmd(e), newAdminUsageCmd(e), newAdminHealCmd(e),
		newAdminUserCmd(e), newAdminGroupCmd(e), newAdminPolicyCmd(e), newAdminAccessKeyCmd(e),
		newAdminConfigCmd(e), newAdminServiceCmd(e), newAdminClusterCmd(e), newAdminLockCmd(e),
		newAdminKMSCmd(e), newAdminNotifyCmd(e), newAdminProfileCmd(e), newAdminSpeedtestCmd(e),
		newAdminTraceCmd(e), newAdminLogsCmd(e), newAdminAuditCmd(e), newAdminMetricsCmd(e), newAdminPrometheusCmd(e),
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

// formatInfo renders the server information: a summary, the pools with
// their quorums and the state of every erasure set, then one block per
// node (version, uptime, clock offset, background healing) with its
// drives; offline nodes and drives and degraded sets are highlighted.
func (e *env) formatInfo(alias string, info *nexoadmin.ServerInfo) string {
	var b strings.Builder
	p := e.out
	fmt.Fprintf(&b, "%s  Nexo %s (commit %s, built %s)\n", p.Paint(cli.ColorBold, alias), info.Version, orDash(info.Commit), orDash(info.BuildDate))
	fmt.Fprintf(&b, "Mode: %s  Region: %s  Deployment: %s\n", info.Mode, orDash(info.Region), orDash(info.DeploymentID))
	// #nosec G115 -- display of server counters, sizes far below 2^63
	fmt.Fprintf(&b, "Uptime: %s  Buckets: %d  Objects: %d  Usage: %s\n",
		cli.HumanDuration(time.Duration(info.UptimeSeconds)*time.Second), info.Buckets, info.Objects, cli.HumanSize(int64(info.Usage)))
	for _, pool := range info.Pools {
		fmt.Fprintf(&b, "Pool %d: %d set(s) of %d drive(s), parity %d", pool.Index+1, pool.Sets, pool.DrivesPerSet, pool.Parity)
		if pool.ReadQuorum > 0 {
			fmt.Fprintf(&b, ", quorum read %d write %d", pool.ReadQuorum, pool.WriteQuorum)
		}
		b.WriteString("\n")
		for _, set := range pool.ErasureSets {
			fmt.Fprintf(&b, "  set %d  %s  %d of %d drive(s) online", set.Index+1, e.paintSetState(set.State), set.Online, set.Drives)
			if set.Healing > 0 {
				fmt.Fprintf(&b, ", %d healing", set.Healing)
			}
			b.WriteString("\n")
		}
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
		e.formatNode(&b, n)
	}
	return b.String()
}

// paintSetState colors the state of an erasure set: green when ok,
// yellow when degraded, red without the write quorum.
func (e *env) paintSetState(state string) string {
	switch state {
	case nexoadmin.SetStateOK:
		return e.out.Paint(cli.ColorGreen, state)
	case nexoadmin.SetStateDegraded:
		return e.out.Paint(cli.ColorYellow, state)
	default:
		return e.out.Paint(cli.ColorRed, state)
	}
}

// formatNode renders a node of formatInfo and its drives.
func (e *env) formatNode(b *strings.Builder, n nexoadmin.NodeInfo) {
	p := e.out
	state := p.Paint(cli.ColorGreen, n.State)
	if n.State != stateOnline {
		state = p.Paint(cli.ColorRed, n.State)
	}
	uptime := "-"
	if n.UptimeSeconds > 0 {
		uptime = cli.HumanDuration(time.Duration(n.UptimeSeconds) * time.Second)
	}
	fmt.Fprintf(b, "\n  %s  %s  version %s  uptime %s  %d drive(s)", n.Endpoint, state, orDash(n.Version), uptime, len(n.Disks))
	switch {
	case n.Local:
		b.WriteString("  (this node)")
	case n.ClockSkew != nil:
		skew := fmt.Sprintf("clock %+.3fs", float64(n.ClockSkew.OffsetMs)/1000)
		if n.ClockSkew.Exceeded {
			skew = p.Paint(cli.ColorYellow, skew+" (skew above 2s)")
		}
		b.WriteString("  " + skew)
	}
	if q := n.HealQueue; q != nil {
		fmt.Fprintf(b, "\n    background healing: %d pending, %d healed, %d retried, %d dropped", q.Pending, q.Healed, q.Retried, q.Dropped)
	}
	for _, d := range n.Disks {
		dstate := p.Paint(cli.ColorGreen, d.State)
		if d.State != stateOnline {
			dstate = p.Paint(cli.ColorRed, d.State)
		}
		path := d.Path
		if path == "" {
			path = d.Endpoint
		}
		fmt.Fprintf(b, "\n    %s  %s", path, dstate)
		if d.Total > 0 {
			fmt.Fprintf(b, "  used %s of %s (free %s)", cli.HumanSize(int64(d.Used)), cli.HumanSize(int64(d.Total)), cli.HumanSize(int64(d.Free))) // #nosec G115 -- display of drive sizes, far below 2^63
		}
		if d.Healing {
			b.WriteString("  healing")
			if hp := d.HealingProgress; hp != nil {
				fmt.Fprintf(b, " (%d object(s) healed, %d failed, %s", hp.ObjectsHealed, hp.ObjectsFailed, cli.HumanSize(int64(hp.BytesDone))) // #nosec G115 -- display of a byte counter, far below 2^63
				if at := strings.TrimSuffix(hp.Bucket+"/"+hp.Object, "/"); at != "" {
					fmt.Fprintf(b, ", at %s", at)
				}
				b.WriteString(")")
			}
		}
		if d.Error != "" {
			b.WriteString("  error: " + d.Error)
		}
	}
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
				fmt.Fprintf(&b, "\n%-10s %10d %10d  %s", cli.HumanSize(int64(u.Size)), u.ObjectsCount, u.VersionsCount, name) // #nosec G115 -- display of a usage size, far below 2^63
			}
			// #nosec G115 -- display of a usage size, far below 2^63
			fmt.Fprintf(&b, "\n%-10s %10d %10d  %s", cli.HumanSize(int64(du.TotalSize)), du.ObjectsCount, du.VersionsCount,
				fmt.Sprintf("total (%d bucket(s))", du.BucketsCount))
			return e.out.Result(adminUsageRecord{Alias: alias, DataUsage: *du}, b.String())
		},
	}
}

// healPollInterval is the default pause between two queries of a running
// healing job.
const healPollInterval = time.Second

// healMaxItems bounds the items printed by the text output of a job.
const healMaxItems = 20

func newAdminHealCmd(e *env) *cobra.Command {
	var (
		recursive, dryRun, deep, remove, background bool
		interval                                    time.Duration
	)
	cmd := &cobra.Command{
		Use:   "heal [flags] ALIAS[/BUCKET[/PREFIX]]",
		Short: "Heal the drives of the server (erasure-coded deployments)",
		Long: `Start a healing job on the whole server (the format of the drives and every
bucket), a bucket or an object, or with --recursive on every object below the
bucket or prefix, follow its progress and print its outcome: the items examined,
repaired and failed, the drives that missed or held corrupt copies.

With --dry-run the objects are checked and nothing is repaired; --deep verifies
the bitrot checksum of every block (it reads all the data); --remove deletes the
dangling objects found (present on too few drives to ever be read again).
With --background the job is started and its id printed: follow it with
"nexoctl admin heal status ALIAS ID". The exit code is 1 when the job failed or
some item could not be healed.`,
		Example: `  nexoctl admin heal --recursive --dry-run local/photos
  nexoctl admin heal -r --deep local
  nexoctl admin heal --background -r local/photos/2026/
  nexoctl admin heal status local`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, err := e.remoteTarget(ctx, args[0], false)
			if err != nil {
				return err
			}
			if interval <= 0 {
				return cli.UsageError("--interval must be positive")
			}
			client, err := e.admin(ctx, t.Alias)
			if err != nil {
				return err
			}
			job, err := client.Heal(ctx, nexoadmin.HealRequest{
				Bucket: t.Bucket, Prefix: t.Key, Recursive: recursive, DryRun: dryRun, Deep: deep, Remove: remove,
			})
			if err != nil {
				return fmt.Errorf("heal %s: %w", t, err)
			}
			if background {
				text := fmt.Sprintf("Heal %s started on %s: follow it with \"nexoctl admin heal status %s %s\"", job.ID, t, t.Alias, job.ID)
				return e.out.Result(adminHealRecord{Alias: t.Alias, DryRun: dryRun, HealJob: *job}, text)
			}
			job, err = e.waitHeal(ctx, client, job, interval)
			if err != nil {
				return fmt.Errorf("heal %s: %w", t, err)
			}
			return e.healResult(t.Alias, job)
		},
	}
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "heal every object under the bucket or prefix")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "only report what would be healed")
	cmd.Flags().BoolVar(&deep, "deep", false, "verify the bitrot checksum of every block (reads all the data)")
	cmd.Flags().BoolVar(&remove, "remove", false, "delete the dangling objects found")
	cmd.Flags().BoolVar(&background, "background", false, "start the job and print its id without waiting")
	cmd.Flags().DurationVar(&interval, "interval", healPollInterval, "pause between two queries of the job progress")
	cmd.AddCommand(newAdminHealStatusCmd(e))
	return cmd
}

// waitHeal polls a job until it is no longer running, drawing its
// progress on a terminal.
func (e *env) waitHeal(ctx context.Context, client cli.AdminClient, job *nexoadmin.HealJob, interval time.Duration) (*nexoadmin.HealJob, error) {
	bar := e.progress(0)
	defer bar.Finish()
	var bytes uint64
	for !job.Finished && job.State == nexoadmin.HealRunning {
		bar.SetLabel(fmt.Sprintf("scanned %d healed %d failed %d", job.Scanned, job.Healed, job.Failed))
		if job.Bytes > bytes {
			bar.Add(int64(job.Bytes - bytes)) // #nosec G115 -- job.Bytes > bytes is checked just above
			bytes = job.Bytes
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("job %s still runs on the server (nexoctl admin heal status): %w", job.ID, ctx.Err())
		case <-t.C:
		}
		next, err := client.HealStatus(ctx, job.ID)
		if err != nil {
			return nil, err
		}
		job = next
	}
	return job, nil
}

// healResult prints the outcome of a finished job; a failed job, or one
// with items not healed, is an error (exit code 1) after the report.
func (e *env) healResult(alias string, job *nexoadmin.HealJob) error {
	if err := e.out.Result(adminHealRecord{Alias: alias, DryRun: job.Request.DryRun, HealJob: *job}, e.formatHeal(job)); err != nil {
		return err
	}
	switch {
	case job.State == nexoadmin.HealFailed || job.State == nexoadmin.HealCanceled:
		return fmt.Errorf("heal %s %s: %s", job.ID, job.State, orDash(job.Error))
	case job.Failed > 0:
		return fmt.Errorf("heal %s: %d item(s) could not be healed", job.ID, job.Failed)
	}
	return nil
}

// formatHeal renders a job: a summary, the drives that needed healing and
// the items healed or failed.
func (e *env) formatHeal(job *nexoadmin.HealJob) string {
	var b strings.Builder
	p := e.out
	mode := ""
	if job.Request.DryRun {
		mode = " (dry run)"
	}
	fmt.Fprintf(&b, "Heal %s%s: %s, scanned %d, healed %d, failed %d", job.ID, mode, job.State, job.Scanned, job.Healed, job.Failed)
	if job.Dangling > 0 || job.Removed > 0 {
		fmt.Fprintf(&b, ", dangling %d, removed %d", job.Dangling, job.Removed)
	}
	if job.Bytes > 0 {
		fmt.Fprintf(&b, ", %s", cli.HumanSize(int64(job.Bytes))) // #nosec G115 -- display of a byte counter, far below 2^63
	}
	if job.Current != "" {
		fmt.Fprintf(&b, "\n  current: %s", job.Current)
	}
	if job.Error != "" {
		fmt.Fprintf(&b, "\n  error: %s", job.Error)
	}
	for _, d := range job.Drives {
		if d.Missing+d.Corrupt+d.Offline+d.Healed == 0 {
			continue
		}
		state := p.Paint(cli.ColorGreen, d.State)
		if d.State != stateOnline {
			state = p.Paint(cli.ColorRed, d.State)
		}
		fmt.Fprintf(&b, "\n  drive %s (pool %d set %d disk %d) %s: missing %d, corrupt %d, offline %d, healed %d",
			orDash(d.Endpoint), d.Pool+1, d.Set+1, d.Disk+1, state, d.Missing, d.Corrupt, d.Offline, d.Healed)
	}
	for i, it := range job.Items {
		if i == healMaxItems {
			fmt.Fprintf(&b, "\n  ... %d more item(s) (--json lists them)", len(job.Items)-healMaxItems)
			break
		}
		name := strings.TrimSuffix(it.Bucket+"/"+it.Object, "/")
		if name == "" {
			name = it.Kind
		}
		fmt.Fprintf(&b, "\n  %s %s:", it.Kind, name)
		var changes []string
		for _, d := range it.Drives {
			if d.Before != "ok" || d.After != "ok" {
				changes = append(changes, fmt.Sprintf("disk %d %s -> %s", d.Disk+1, d.Before, d.After))
			}
		}
		if len(changes) > 0 {
			b.WriteString(" " + strings.Join(changes, ", "))
		}
		switch {
		case it.Error != "":
			b.WriteString("  " + p.Paint(cli.ColorRed, "error: "+it.Error))
		case it.Removed:
			b.WriteString("  dangling, removed")
		case it.Dangling:
			b.WriteString("  dangling (--remove deletes it)")
		}
	}
	if job.ItemsTruncated {
		b.WriteString("\n  ... the server kept the first items only")
	}
	return b.String()
}

func newAdminHealStatusCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "status ALIAS [ID]",
		Short: "Show a healing job, or list the running and recent ones",
		Long: `Show the state and the outcome of a healing job, or without ID list the running
and recently finished jobs (newest first).`,
		Example: `  nexoctl admin heal status local
  nexoctl admin heal status local 0192f0f0-0000-7000-8000-000000000001`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return cli.UsageError("%s takes 1 or 2 argument(s), got %d", cmd.CommandPath(), len(args))
			}
			return nil
		},
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			if len(args) == 2 {
				job, err := client.HealStatus(ctx, args[1])
				if err != nil {
					return fmt.Errorf("heal %s: %w", args[1], err)
				}
				return e.healResult(alias, job)
			}
			list, err := client.ListHeals(ctx)
			if err != nil {
				return err
			}
			if e.out.JSON() {
				for _, j := range list.Items {
					if err := e.out.Result(adminHealRecord{Alias: alias, DryRun: j.Request.DryRun, HealJob: j}, ""); err != nil {
						return err
					}
				}
				return nil
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%-36s %-9s %-20s %8s %8s %8s  %s", "ID", "STATE", "STARTED", "SCANNED", "HEALED", "FAILED", "TARGET")
			for _, j := range list.Items {
				target := strings.TrimSuffix(alias+"/"+j.Request.Bucket+"/"+j.Request.Prefix, "/")
				target = strings.TrimSuffix(target, "/")
				if j.Request.Recursive {
					target += " (recursive)"
				}
				if j.Request.DryRun {
					target += " (dry run)"
				}
				fmt.Fprintf(&b, "\n%-36s %-9s %-20s %8d %8d %8d  %s", j.ID, j.State, j.StartedAt.UTC().Format(time.DateTime),
					j.Scanned, j.Healed, j.Failed, target)
			}
			return e.out.Text(b.String())
		},
	}
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
	data, err := os.ReadFile(path) // #nosec G304 -- a file the user named on the command line
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
