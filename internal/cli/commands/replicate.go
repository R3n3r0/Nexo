package commands

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// replicationRuleRecord is the JSON record of a replication rule.
type replicationRuleRecord struct {
	Target string `json:"target"`
	Bucket string `json:"bucket"`
	nexoadmin.ReplicationRule
	// Endpoint and RemoteBucket describe the replication target of the
	// rule (empty when the target is gone).
	Endpoint     string `json:"endpoint,omitempty"`
	RemoteBucket string `json:"remoteBucket,omitempty"`
}

// replicationStatusRecord is the JSON record of replicate status.
type replicationStatusRecord struct {
	Target string `json:"target"`
	Bucket string `json:"bucket"`
	nexoadmin.ReplicationStatus
}

// newReplicateCmd returns replicate (S-BKT-12).
func newReplicateCmd(e *env) *cobra.Command {
	cmd := groupCmd("replicate", "Manage the replication of buckets to other servers (Admin API)")
	cmd.Long = `Manage the replication of buckets to buckets of other S3 servers (Nexo or not).

Every new version of an object a rule selects, its tags and, if the rule says
so, its delete markers and deletions are copied to the remote bucket in the
background, with the version id, the time and the ETag of the source; a
version that cannot be sent is retried with an increasing delay. Both buckets
need versioning, and versioning cannot be suspended while rules exist.

Two buckets replicating to each other (active-active) converge: the replicas
are never sent back, and the most recent version of a key wins on both sides.
Run "replicate add" on each side. "replicate resync" sends the versions that
existed before the rule (for the rules with --existing).

The remote bucket is given as REMOTE_ALIAS/BUCKET: the URL and the credentials
of that alias are stored, sealed, by the source server, which uses them to
write (s3:PutObject, s3:DeleteObject, s3:PutObjectTagging and, on a Nexo
server, s3:ReplicateObject, s3:ReplicateDelete and s3:ReplicateTags).`
	cmd.AddCommand(newReplicateAddCmd(e), newReplicateLsCmd(e), newReplicateRmCmd(e), newReplicateStatusCmd(e), newReplicateResyncCmd(e))
	return cmd
}

// replicaRemote resolves REMOTE_ALIAS/BUCKET into the replication target it
// names (endpoint, bucket and credentials of the alias).
func (e *env) replicaRemote(ctx context.Context, arg string) (nexoadmin.ReplicationTarget, error) {
	t, err := e.remoteTarget(ctx, arg, true)
	if err != nil {
		return nexoadmin.ReplicationTarget{}, err
	}
	if t.Key != "" {
		return nexoadmin.ReplicationTarget{}, cli.UsageError("%s: the remote is ALIAS/BUCKET", arg)
	}
	aliases, err := e.loadAliases(ctx)
	if err != nil {
		return nexoadmin.ReplicationTarget{}, err
	}
	a, ok := aliases[t.Alias]
	if !ok {
		return nexoadmin.ReplicationTarget{}, &cli.AliasNotFoundError{Name: t.Alias}
	}
	return nexoadmin.ReplicationTarget{Endpoint: strings.TrimRight(a.URL, "/"), Bucket: t.Bucket, Region: a.Region,
		AccessKey: a.AccessKey, SecretKey: a.SecretKey, Label: t.Alias}, nil
}

// sameEndpoint reports whether two endpoint URLs name the same server.
func sameEndpoint(a, b string) bool {
	norm := func(s string) string {
		u, err := url.Parse(strings.TrimRight(s, "/"))
		if err != nil {
			return s
		}
		return strings.ToLower(u.Scheme + "://" + u.Host)
	}
	return norm(a) == norm(b)
}

// findTarget returns the target of targets replicating to remote.
func findTarget(targets []nexoadmin.ReplicationTarget, remote nexoadmin.ReplicationTarget) (nexoadmin.ReplicationTarget, bool) {
	for _, t := range targets {
		if t.Bucket == remote.Bucket && sameEndpoint(t.Endpoint, remote.Endpoint) {
			return t, true
		}
	}
	return nexoadmin.ReplicationTarget{}, false
}

// replicateRuleFlags are the flags of replicate add.
type replicateRuleFlags struct {
	id              string
	prefix          string
	tags            []string
	priority        int
	noDeleteMarkers bool
	deletes         bool
	existing        bool
	disable         bool
}

// parseRuleTags parses the key=value pairs of --tags.
func parseRuleTags(list []string) (map[string]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, raw := range splitList(list) {
		for _, kv := range strings.Split(raw, "&") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return nil, cli.UsageError("invalid tag %q: use KEY=VALUE", kv)
			}
			if _, dup := out[k]; dup {
				return nil, cli.UsageError("tag %q given twice", k)
			}
			out[k] = v
		}
	}
	return out, nil
}

// defaultRuleID is the rule ID of replicate add without --id: the alias and
// the bucket of the remote.
func defaultRuleID(remote nexoadmin.ReplicationTarget) string {
	id := remote.Label + "-" + remote.Bucket
	if len(id) > 255 {
		id = id[:255]
	}
	return id
}

func newReplicateAddCmd(e *env) *cobra.Command {
	var f replicateRuleFlags
	cmd := &cobra.Command{
		Use:   "add ALIAS/BUCKET REMOTE_ALIAS/BUCKET [flags]",
		Short: "Replicate a bucket to a remote bucket, or change the rule with the same ID",
		Long: `Replicate a bucket to a bucket of another server: register the remote as a
replication target of the bucket (with the URL and the credentials of
REMOTE_ALIAS, updated if it is already one) and add a rule that sends it the
keys under --prefix with the --tags (every key by default), or replace the rule
with the same --id (default REMOTE_ALIAS-BUCKET). When several rules select a
key for the same remote, the one with the highest --priority applies.

Delete markers are replicated unless --no-delete-markers; --deletes also
replicates the deletions of versions (permanent: off by default); --existing
lets "replicate resync" send the versions written before the rule.`,
		Example: `  nexoctl replicate add local/photos backup/photos
  nexoctl replicate add local/photos backup/photos --id docs --prefix docs/ --tags class=public --existing
  # active-active: one rule on each side
  nexoctl replicate add site-a/shared site-b/shared --deletes
  nexoctl replicate add site-b/shared site-a/shared --deletes`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			tags, err := parseRuleTags(f.tags)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "replicate")
			if err != nil {
				return err
			}
			remote, err := e.replicaRemote(ctx, args[1])
			if err != nil {
				return err
			}
			if sameEndpoint(remote.Endpoint, aliasURL(ctx, e, t.Alias)) && remote.Bucket == t.Bucket {
				return cli.UsageError("a bucket cannot replicate to itself")
			}
			targets, err := client.ListReplicationTargets(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			if old, ok := findTarget(targets, remote); ok {
				remote.ARN = old.ARN
			}
			stored, err := client.SetReplicationTarget(ctx, t.Bucket, remote)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			cfg, err := client.GetBucketReplication(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			rule := nexoadmin.ReplicationRule{ID: strings.TrimSpace(f.id), Priority: f.priority, Enabled: !f.disable,
				Prefix: f.prefix, Tags: tags, Target: stored.ARN, DeleteMarkers: !f.noDeleteMarkers, Deletes: f.deletes, Existing: f.existing}
			if rule.ID == "" {
				rule.ID = defaultRuleID(remote)
			}
			rules := slices.Clone(cfg.Rules)
			replaced := false
			for i, r := range rules {
				if r.ID == rule.ID {
					if !cmd.Flags().Changed("priority") {
						rule.Priority = r.Priority
					}
					rules[i], replaced = rule, true
				}
			}
			if !replaced {
				if !cmd.Flags().Changed("priority") {
					for _, r := range rules {
						rule.Priority = max(rule.Priority, r.Priority+1)
					}
				}
				rules = append(rules, rule)
			}
			if _, err := client.SetBucketReplication(ctx, t.Bucket, nexoadmin.ReplicationConfiguration{Rules: rules}); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			text := fmt.Sprintf("`%s` replicates to %s/%s (rule %q).", t, remote.Endpoint, remote.Bucket, rule.ID)
			if replaced {
				text = fmt.Sprintf("Replication rule %q of `%s` replaced: to %s/%s.", rule.ID, t, remote.Endpoint, remote.Bucket)
			}
			return e.out.Info(replicationRuleRecord{Target: t.String(), Bucket: t.Bucket, ReplicationRule: rule,
				Endpoint: stored.Endpoint, RemoteBucket: stored.Bucket}, text)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.id, "id", "", "ID of the rule (default REMOTE_ALIAS-BUCKET)")
	fs.StringVar(&f.prefix, "prefix", "", "replicate the keys under this prefix (default: the whole bucket)")
	fs.StringSliceVar(&f.tags, "tags", nil, "replicate the objects with these tags (KEY=VALUE, repeatable or joined with '&')")
	fs.IntVar(&f.priority, "priority", 1, "priority of the rule among those to the same remote (default: above the others)")
	fs.BoolVar(&f.noDeleteMarkers, "no-delete-markers", false, "do not replicate the delete markers")
	fs.BoolVar(&f.deletes, "deletes", false, "replicate the deletions of versions")
	fs.BoolVar(&f.existing, "existing", false, `let "replicate resync" send the versions written before the rule`)
	fs.BoolVar(&f.disable, "disable", false, "add the rule disabled")
	return cmd
}

// aliasURL returns the URL of an alias ("" if unknown).
func aliasURL(ctx context.Context, e *env, alias string) string {
	aliases, err := e.loadAliases(ctx)
	if err != nil {
		return ""
	}
	return aliases[alias].URL
}

// ruleSelection describes what a rule selects.
func ruleSelection(r nexoadmin.ReplicationRule) string {
	var parts []string
	if r.Prefix != "" {
		parts = append(parts, "prefix "+r.Prefix)
	}
	keys := make([]string, 0, len(r.Tags))
	for k := range r.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, "tag "+k+"="+r.Tags[k])
	}
	if len(parts) == 0 {
		return "every key"
	}
	return strings.Join(parts, ", ")
}

// ruleOptions describes the toggles of a rule.
func ruleOptions(r nexoadmin.ReplicationRule) string {
	var parts []string
	if !r.Enabled {
		parts = append(parts, "disabled")
	}
	if r.DeleteMarkers {
		parts = append(parts, "delete markers")
	}
	if r.Deletes {
		parts = append(parts, "deletes")
	}
	if r.Existing {
		parts = append(parts, "existing")
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

func newReplicateLsCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "ls ALIAS/BUCKET",
		Aliases:           []string{"list"},
		Short:             "List the replication rules of a bucket and their remotes",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "replicate")
			if err != nil {
				return err
			}
			cfg, err := client.GetBucketReplication(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			targets, err := client.ListReplicationTargets(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			if len(cfg.Rules) == 0 {
				return e.out.Note(fmt.Sprintf("`%s` is not replicated.", t))
			}
			if err := e.out.Text(fmt.Sprintf("%-20s %-8s %-40s %-30s %s", "ID", "PRIORITY", "REMOTE", "SELECTS", "REPLICATES")); err != nil {
				return err
			}
			for _, r := range cfg.Rules {
				rec := replicationRuleRecord{Target: t.String(), Bucket: t.Bucket, ReplicationRule: r}
				remote := r.Target
				if i := slices.IndexFunc(targets, func(x nexoadmin.ReplicationTarget) bool { return x.ARN == r.Target }); i >= 0 {
					rec.Endpoint, rec.RemoteBucket = targets[i].Endpoint, targets[i].Bucket
					remote = rec.Endpoint + "/" + rec.RemoteBucket
				}
				text := fmt.Sprintf("%-20s %-8d %-40s %-30s %s", r.ID, r.Priority, remote, ruleSelection(r), ruleOptions(r))
				if err := e.out.Result(rec, text); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newReplicateRmCmd(e *env) *cobra.Command {
	var id string
	var all bool
	cmd := &cobra.Command{
		Use:     "rm ALIAS/BUCKET (--id ID | --all)",
		Aliases: []string{"remove"},
		Short:   "Remove replication rules of a bucket",
		Long: `Remove a replication rule of a bucket, or all of them with --all. The targets
no rule uses any more are removed with their stored credentials; what was
already replicated stays on the remotes.`,
		Example: `  nexoctl replicate rm local/photos --id backup-photos
  nexoctl replicate rm local/photos --all`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (id == "") == !all {
				return cli.UsageError("give --id ID or --all")
			}
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "replicate")
			if err != nil {
				return err
			}
			cfg, err := client.GetBucketReplication(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			var keep []nexoadmin.ReplicationRule
			if !all {
				keep = slices.DeleteFunc(slices.Clone(cfg.Rules), func(r nexoadmin.ReplicationRule) bool { return r.ID == id })
				if len(keep) == len(cfg.Rules) {
					return fmt.Errorf("%s: no replication rule %q", t, id)
				}
			}
			if len(keep) == 0 {
				err = client.DeleteBucketReplication(ctx, t.Bucket)
			} else {
				_, err = client.SetBucketReplication(ctx, t.Bucket, nexoadmin.ReplicationConfiguration{Rules: keep})
			}
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			targets, err := client.ListReplicationTargets(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			for _, target := range targets {
				if slices.ContainsFunc(keep, func(r nexoadmin.ReplicationRule) bool { return r.Target == target.ARN }) {
					continue
				}
				if err := client.RemoveReplicationTarget(ctx, t.Bucket, target.ARN); err != nil {
					return fmt.Errorf("%s: %w", t, err)
				}
			}
			removed := len(cfg.Rules) - len(keep)
			text := fmt.Sprintf("Removed replication rule %q of `%s`.", id, t)
			if all {
				text = fmt.Sprintf("Removed the replication of `%s` (%d %s).", t, removed, plural(removed, "rule", "rules"))
			}
			return e.out.Info(map[string]any{"target": t.String(), "bucket": t.Bucket, "removed": removed}, text)
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "the ID of the rule to remove")
	cmd.Flags().BoolVar(&all, "all", false, "remove every rule")
	return cmd
}

func newReplicateStatusCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "status ALIAS/BUCKET",
		Short: "Show the replication of a bucket: queue, counters and resync",
		Long: `Show the replication of a bucket to each remote: whether the remote answers,
the versions waiting, those replicated and failed since the server started,
with the last error, and the progress of the last resync. In a cluster the
counters are those of the node that answers.`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "replicate")
			if err != nil {
				return err
			}
			st, err := client.GetReplicationStatus(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			if e.out.JSON() {
				return e.out.Result(replicationStatusRecord{Target: t.String(), Bucket: t.Bucket, ReplicationStatus: *st}, "")
			}
			if len(st.Targets) == 0 {
				return e.out.Result(nil, fmt.Sprintf("`%s` has no replication target.", t))
			}
			lines := []string{fmt.Sprintf("%-50s %-8s %-7s %-11s %-9s %-7s %s", "TARGET", "STATE", "QUEUED", "REPLICATED", "SIZE", "FAILED", "LAST ERROR")}
			for _, s := range st.Targets {
				state := "online"
				if !s.Online {
					state = "offline"
				}
				last := "-"
				if s.LastError != "" {
					last = s.LastError
				}
				lines = append(lines, fmt.Sprintf("%-50s %-8s %-7d %-11d %-9s %-7d %s", s.ARN, state, s.Queued, s.Replicated, cli.HumanSize(s.Bytes), s.Failed, last))
			}
			if r := st.Resync; r != nil {
				line := fmt.Sprintf("Resync to %s: %s, %d versions scanned, %d queued", r.Target, r.State, r.Scanned, r.Queued)
				if r.Error != "" {
					line += ", error: " + r.Error
				}
				lines = append(lines, line)
			}
			return e.out.Result(nil, strings.Join(lines, "\n"))
		},
	}
}

func newReplicateResyncCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "resync ALIAS/BUCKET REMOTE_ALIAS/BUCKET",
		Short: "Send to a remote the versions written before its rules",
		Long: `Walk every version of the bucket and send to the remote those its rules with
--existing select and the remote does not have yet. It runs in the background
on the server: follow it with "replicate status".`,
		Example:           "  nexoctl replicate resync local/photos backup/photos",
		Args:              exactArgs(2),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "replicate")
			if err != nil {
				return err
			}
			remote, err := e.replicaRemote(ctx, args[1])
			if err != nil {
				return err
			}
			targets, err := client.ListReplicationTargets(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			target, ok := findTarget(targets, remote)
			if !ok {
				return fmt.Errorf("%s: %s is not a replication target of the bucket", t, args[1])
			}
			res, err := client.StartReplicationResync(ctx, t.Bucket, target.ARN)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(res, fmt.Sprintf("Resync of `%s` to %s started: follow it with \"nexoctl replicate status %s\".", t, args[1], t))
		},
	}
}
