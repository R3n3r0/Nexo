package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
)

func newILMCmd(e *env) *cobra.Command {
	cmd := groupCmd("ilm", "Manage the lifecycle rules of buckets (expiration, noncurrent versions, uploads)")
	cmd.Long = `Manage the lifecycle (ILM) rules of buckets: the expiration of the objects, of
their noncurrent versions and of the expired delete markers, and the abort of the
incomplete multipart uploads. The server applies the rules in the background;
"ilm apply" applies them at once (Admin API).

The rules travel as the JSON of the Admin API and of the console (ilm rule
export/import), or as the S3 LifecycleConfiguration XML.`
	rule := groupCmd("rule", "Add, edit, list, remove, export and import lifecycle rules")
	rule.AddCommand(newILMRuleAddCmd(e), newILMRuleEditCmd(e), newILMRuleListCmd(e), newILMRuleRemoveCmd(e),
		newILMRuleExportCmd(e), newILMRuleImportCmd(e))
	cmd.AddCommand(rule, newILMApplyCmd(e))
	return cmd
}

// ilmRuleFlags are the flags of ilm rule add and edit.
type ilmRuleFlags struct {
	id                   string
	prefix               string
	tags                 string
	sizeGT, sizeLT       string
	expireDays           int
	expireDate           string
	expiredDeleteMarker  bool
	noncurrentExpireDays int
	newerNoncurrent      int
	abortMPUDays         int
	enable, disable      bool
}

func (f *ilmRuleFlags) register(fs *pflag.FlagSet, edit bool) {
	idHelp := "rule ID (default: rule-N)"
	if edit {
		idHelp = "ID of the rule to edit (required)"
	}
	fs.StringVar(&f.id, "id", "", idHelp)
	fs.StringVar(&f.prefix, "prefix", "", "apply to the objects under this prefix")
	fs.StringVar(&f.tags, "tags", "", `apply to the objects with these tags: "key1=value1&key2=value2"`)
	fs.StringVar(&f.sizeGT, "size-gt", "", "apply to the objects larger than this size (e.g. 1MiB)")
	fs.StringVar(&f.sizeLT, "size-lt", "", "apply to the objects smaller than this size")
	fs.IntVar(&f.expireDays, "expire-days", 0, "expire the current versions this many days after their creation")
	fs.StringVar(&f.expireDate, "expire-date", "", "expire the current versions on this date (YYYY-MM-DD)")
	fs.BoolVar(&f.expiredDeleteMarker, "expired-delete-marker", false, "remove the delete markers without noncurrent versions")
	fs.IntVar(&f.noncurrentExpireDays, "noncurrent-expire-days", 0, "remove the noncurrent versions this many days after they became noncurrent")
	fs.IntVar(&f.newerNoncurrent, "newer-noncurrent", 0, "keep this many newest noncurrent versions")
	fs.IntVar(&f.abortMPUDays, "abort-mpu-days", 0, "abort the incomplete multipart uploads this many days after their start")
	fs.BoolVar(&f.disable, "disable", false, "disable the rule")
	if edit {
		fs.BoolVar(&f.enable, "enable", false, "enable the rule")
	}
}

// apply sets on r the fields of the flags that were given (all of them
// for a new rule). A zero number or an empty date removes the action when
// editing.
func (f *ilmRuleFlags) apply(fs *pflag.FlagSet, r *cli.LifecycleRule) error {
	changed := fs.Changed
	if f.enable && f.disable {
		return cli.UsageError("--enable excludes --disable")
	}
	for _, n := range []struct {
		flag  string
		value int
	}{{"expire-days", f.expireDays}, {"noncurrent-expire-days", f.noncurrentExpireDays}, {"newer-noncurrent", f.newerNoncurrent}, {"abort-mpu-days", f.abortMPUDays}} {
		if n.value < 0 {
			return cli.UsageError("--%s must not be negative", n.flag)
		}
	}
	if changed("prefix") || changed("tags") || changed("size-gt") || changed("size-lt") {
		filter := cli.LifecycleFilter{}
		if r.Filter != nil {
			filter = *r.Filter
		}
		if changed("prefix") {
			filter.Prefix = f.prefix
		}
		if changed("tags") {
			filter.Tags = nil
			if f.tags != "" {
				tags, err := cli.ParseTags(f.tags)
				if err != nil {
					return err
				}
				filter.Tags = tags
			}
		}
		for _, s := range []struct {
			flag  string
			value string
			dst   **int64
		}{{"size-gt", f.sizeGT, &filter.SizeGreaterThan}, {"size-lt", f.sizeLT, &filter.SizeLessThan}} {
			if !changed(s.flag) {
				continue
			}
			*s.dst = nil
			if s.value == "" {
				continue
			}
			n, err := cli.ParseSize(s.value)
			if err != nil {
				return err
			}
			*s.dst = &n
		}
		r.Filter = &filter
		if filter.Prefix == "" && len(filter.Tags) == 0 && filter.SizeGreaterThan == nil && filter.SizeLessThan == nil {
			r.Filter = nil
		}
	}
	if changed("expire-days") || changed("expire-date") || changed("expired-delete-marker") {
		exp := cli.LifecycleExpiration{}
		if r.Expiration != nil {
			exp = *r.Expiration
		}
		if changed("expire-days") {
			exp.Days = positiveOrNil(f.expireDays)
			if exp.Days != nil {
				exp.Date = ""
			}
		}
		if changed("expire-date") {
			exp.Date = ""
			if f.expireDate != "" {
				d, err := cli.ParseLifecycleDate(f.expireDate)
				if err != nil {
					return err
				}
				exp.Date, exp.Days = d, nil
			}
		}
		if changed("expired-delete-marker") {
			exp.ExpiredObjectDeleteMarker = f.expiredDeleteMarker
		}
		if exp.ExpiredObjectDeleteMarker && (exp.Days != nil || exp.Date != "") {
			return cli.UsageError("--expired-delete-marker excludes --expire-days and --expire-date: a rule has one expiration (add another rule)")
		}
		r.Expiration = &exp
		if exp.Days == nil && exp.Date == "" && !exp.ExpiredObjectDeleteMarker {
			r.Expiration = nil
		}
	}
	if changed("noncurrent-expire-days") || changed("newer-noncurrent") {
		nc := cli.LifecycleNoncurrentExpiration{}
		if r.NoncurrentExpiration != nil {
			nc = *r.NoncurrentExpiration
		}
		if changed("noncurrent-expire-days") {
			nc.NoncurrentDays = positiveOrNil(f.noncurrentExpireDays)
		}
		if changed("newer-noncurrent") {
			nc.NewerNoncurrentVersions = positiveOrNil(f.newerNoncurrent)
		}
		r.NoncurrentExpiration = &nc
		if nc.NoncurrentDays == nil && nc.NewerNoncurrentVersions == nil {
			r.NoncurrentExpiration = nil
		}
	}
	if changed("abort-mpu-days") {
		r.AbortIncompleteUpload = nil
		if f.abortMPUDays > 0 {
			r.AbortIncompleteUpload = &cli.LifecycleAbortUpload{DaysAfterInitiation: positiveOrNil(f.abortMPUDays)}
		}
	}
	switch {
	case f.disable:
		r.Enabled = boolPtr(false)
	case f.enable || r.Enabled == nil:
		r.Enabled = boolPtr(true)
	}
	if r.Expiration == nil && r.NoncurrentExpiration == nil && r.AbortIncompleteUpload == nil &&
		len(r.Transitions) == 0 && len(r.NoncurrentTransitions) == 0 {
		return cli.UsageError("a rule needs an action: --expire-days, --expire-date, --expired-delete-marker, --noncurrent-expire-days, --newer-noncurrent or --abort-mpu-days")
	}
	return nil
}

// positiveOrNil returns &n for a positive n.
func positiveOrNil(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}

// ilmBucket parses ALIAS/BUCKET and returns its S3 client.
func (e *env) ilmBucket(ctx context.Context, arg, what string) (cli.Target, cli.S3Client, error) {
	t, err := e.remoteTarget(ctx, arg, true)
	if err != nil {
		return cli.Target{}, nil, err
	}
	if t.Key != "" {
		return cli.Target{}, nil, cli.UsageError("%s: %s applies to ALIAS/BUCKET", arg, what)
	}
	client, err := e.client(ctx, t.Alias)
	if err != nil {
		return cli.Target{}, nil, err
	}
	return t, client, nil
}

func newILMRuleAddCmd(e *env) *cobra.Command {
	var flags ilmRuleFlags
	cmd := &cobra.Command{
		Use:   "add [flags] ALIAS/BUCKET",
		Short: "Add a lifecycle rule to a bucket",
		Example: `  nexoctl ilm rule add --prefix logs/ --expire-days 30 local/app
  nexoctl ilm rule add --noncurrent-expire-days 7 --newer-noncurrent 3 local/docs
  nexoctl ilm rule add --abort-mpu-days 2 --expired-delete-marker local/docs`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			rule := cli.LifecycleRule{ID: strings.TrimSpace(flags.id)}
			if err := flags.apply(cmd.Flags(), &rule); err != nil {
				return err
			}
			ctx := cmd.Context()
			t, client, err := e.ilmBucket(ctx, args[0], "ilm")
			if err != nil {
				return err
			}
			cfg, err := client.GetLifecycle(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			if rule.ID == "" {
				rule.ID = cli.NextLifecycleRuleID(cfg.Rules)
			}
			for _, r := range cfg.Rules {
				if r.ID == rule.ID {
					return cli.UsageError("%s: rule %q already exists (use \"ilm rule edit\")", t, rule.ID)
				}
			}
			cfg.Rules = append(cfg.Rules, rule)
			if err := client.PutLifecycle(ctx, t.Bucket, *cfg); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(ilmRuleRecord{Target: t.String(), Bucket: t.Bucket, LifecycleRule: rule},
				fmt.Sprintf("Lifecycle rule %q added to `%s`.", rule.ID, t))
		},
	}
	flags.register(cmd.Flags(), false)
	return cmd
}

func newILMRuleEditCmd(e *env) *cobra.Command {
	var flags ilmRuleFlags
	cmd := &cobra.Command{
		Use:   "edit --id ID [flags] ALIAS/BUCKET",
		Short: "Change a lifecycle rule of a bucket",
		Long: `Change the given settings of a lifecycle rule, keeping the others. A zero number
of days or an empty date or filter removes that setting.`,
		Example: `  nexoctl ilm rule edit --id rule-1 --expire-days 90 local/app
  nexoctl ilm rule edit --id rule-1 --disable local/app`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.id == "" {
				return cli.UsageError("--id is required")
			}
			ctx := cmd.Context()
			t, client, err := e.ilmBucket(ctx, args[0], "ilm")
			if err != nil {
				return err
			}
			cfg, err := client.GetLifecycle(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			idx := -1
			for i, r := range cfg.Rules {
				if r.ID == flags.id {
					idx = i
				}
			}
			if idx < 0 {
				return fmt.Errorf("%s: lifecycle rule %q: %w", t, flags.id, domain.ErrNotFound)
			}
			rule := cfg.Rules[idx]
			if err := flags.apply(cmd.Flags(), &rule); err != nil {
				return err
			}
			cfg.Rules[idx] = rule
			if err := client.PutLifecycle(ctx, t.Bucket, *cfg); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(ilmRuleRecord{Target: t.String(), Bucket: t.Bucket, LifecycleRule: rule},
				fmt.Sprintf("Lifecycle rule %q of `%s` updated.", rule.ID, t))
		},
	}
	flags.register(cmd.Flags(), true)
	return cmd
}

func newILMRuleListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS/BUCKET",
		Aliases:           []string{"ls"},
		Short:             "List the lifecycle rules of a bucket",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.ilmBucket(ctx, args[0], "ilm")
			if err != nil {
				return err
			}
			cfg, err := client.GetLifecycle(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			if len(cfg.Rules) == 0 {
				return e.out.Note(fmt.Sprintf("`%s` has no lifecycle rule.", t))
			}
			if err := e.out.Text(fmt.Sprintf("%-16s %-8s %-30s %s", "ID", "STATUS", "FILTER", "ACTIONS")); err != nil {
				return err
			}
			for _, r := range cfg.Rules {
				status := "Enabled"
				if !cli.RuleEnabled(r) {
					status = "Disabled"
				}
				text := fmt.Sprintf("%-16s %-8s %-30s %s", r.ID, status, ilmFilterText(r.Filter), ilmActionsText(r))
				if err := e.out.Result(ilmRuleRecord{Target: t.String(), Bucket: t.Bucket, LifecycleRule: r}, strings.TrimRight(text, " ")); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// ilmFilterText renders the filter of a rule.
func ilmFilterText(f *cli.LifecycleFilter) string {
	if f == nil {
		return "-"
	}
	var parts []string
	if f.Prefix != "" {
		parts = append(parts, "prefix="+f.Prefix)
	}
	if len(f.Tags) > 0 {
		parts = append(parts, "tags="+cli.FormatTags(f.Tags))
	}
	if f.SizeGreaterThan != nil {
		parts = append(parts, "size>"+cli.HumanSize(*f.SizeGreaterThan))
	}
	if f.SizeLessThan != nil {
		parts = append(parts, "size<"+cli.HumanSize(*f.SizeLessThan))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

// ilmActionsText renders the actions of a rule.
func ilmActionsText(r cli.LifecycleRule) string {
	var parts []string
	if e := r.Expiration; e != nil {
		switch {
		case e.Days != nil:
			parts = append(parts, fmt.Sprintf("expire after %dd", *e.Days))
		case e.Date != "":
			parts = append(parts, "expire on "+e.Date)
		}
		if e.ExpiredObjectDeleteMarker {
			parts = append(parts, "remove expired delete markers")
		}
	}
	if n := r.NoncurrentExpiration; n != nil {
		s := "noncurrent versions"
		if n.NoncurrentDays != nil {
			s += fmt.Sprintf(" expire after %dd", *n.NoncurrentDays)
		}
		if n.NewerNoncurrentVersions != nil {
			s += fmt.Sprintf(" (keep %d newest)", *n.NewerNoncurrentVersions)
		}
		parts = append(parts, s)
	}
	if a := r.AbortIncompleteUpload; a != nil && a.DaysAfterInitiation != nil {
		parts = append(parts, fmt.Sprintf("abort uploads after %dd", *a.DaysAfterInitiation))
	}
	for _, tr := range r.Transitions {
		when := tr.Date
		if tr.Days != nil {
			when = fmt.Sprintf("%dd", *tr.Days)
		}
		parts = append(parts, fmt.Sprintf("transition to %s after %s", tr.StorageClass, when))
	}
	for _, tr := range r.NoncurrentTransitions {
		parts = append(parts, "noncurrent transition to "+tr.StorageClass)
	}
	return strings.Join(parts, ", ")
}

func newILMRuleRemoveCmd(e *env) *cobra.Command {
	var (
		ids []string
		all bool
	)
	cmd := &cobra.Command{
		Use:     "remove --id ID... | --all ALIAS/BUCKET",
		Aliases: []string{"rm"},
		Short:   "Remove lifecycle rules of a bucket",
		Example: `  nexoctl ilm rule rm --id rule-1 local/app
  nexoctl ilm rule rm --all local/app`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ids = splitList(ids)
			if all == (len(ids) > 0) {
				return cli.UsageError("give either --id or --all")
			}
			ctx := cmd.Context()
			t, client, err := e.ilmBucket(ctx, args[0], "ilm")
			if err != nil {
				return err
			}
			cfg, err := client.GetLifecycle(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			var kept, removed []cli.LifecycleRule
			drop := map[string]bool{}
			for _, id := range ids {
				drop[id] = true
			}
			for _, r := range cfg.Rules {
				if all || drop[r.ID] {
					removed = append(removed, r)
					delete(drop, r.ID)
					continue
				}
				kept = append(kept, r)
			}
			for _, id := range ids {
				if drop[id] {
					return fmt.Errorf("%s: lifecycle rule %q: %w", t, id, domain.ErrNotFound)
				}
			}
			if err := client.PutLifecycle(ctx, t.Bucket, cli.LifecycleConfiguration{Rules: kept}); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			for _, r := range removed {
				if err := e.out.Info(ilmRuleRecord{Target: t.String(), Bucket: t.Bucket, LifecycleRule: r},
					fmt.Sprintf("Lifecycle rule %q removed from `%s`.", r.ID, t)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&ids, "id", nil, "IDs of the rules to remove (repeatable or comma-separated)")
	cmd.Flags().BoolVar(&all, "all", false, "remove every rule (the lifecycle configuration)")
	return cmd
}

func newILMRuleExportCmd(e *env) *cobra.Command {
	var asXML bool
	cmd := &cobra.Command{
		Use:   "export [--xml] ALIAS/BUCKET",
		Short: "Print the lifecycle configuration of a bucket (JSON or S3 XML)",
		Example: `  nexoctl ilm rule export local/app > lifecycle.json
  nexoctl ilm rule export --xml local/app`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.ilmBucket(ctx, args[0], "ilm")
			if err != nil {
				return err
			}
			cfg, err := client.GetLifecycle(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			if e.out.JSON() {
				return e.out.Result(ilmConfigRecord{Target: t.String(), Bucket: t.Bucket, Rules: cfg.Rules}, "")
			}
			var doc []byte
			if asXML {
				doc, err = cli.EncodeLifecycleXML(*cfg)
			} else {
				doc, err = encodeJSONDocument(cfg)
			}
			if err != nil {
				return err
			}
			_, err = e.out.Stdout().Write(doc)
			return err
		},
	}
	cmd.Flags().BoolVar(&asXML, "xml", false, "print the S3 LifecycleConfiguration XML")
	return cmd
}

func newILMRuleImportCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "import ALIAS/BUCKET [FILE]",
		Short: "Replace the lifecycle configuration of a bucket (JSON or S3 XML)",
		Long: `Replace the lifecycle configuration of a bucket with a document read from FILE
or from the standard input ("-" or no FILE): the JSON of "ilm rule export" and of
the console, or the S3 LifecycleConfiguration XML. A document without rules
removes the configuration.`,
		Example: `  nexoctl ilm rule import local/app lifecycle.json
  nexoctl ilm rule export local/app | nexoctl ilm rule import local/copy`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return cli.UsageError("%s takes ALIAS/BUCKET and an optional FILE", cmd.CommandPath())
			}
			return nil
		},
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := e.readConfigDocument(args[1:])
			if err != nil {
				return err
			}
			cfg, err := cli.ParseLifecycle(data)
			if err != nil {
				return err
			}
			for i := range cfg.Rules {
				if cfg.Rules[i].ID == "" {
					cfg.Rules[i].ID = cli.NextLifecycleRuleID(cfg.Rules)
				}
			}
			ctx := cmd.Context()
			t, client, err := e.ilmBucket(ctx, args[0], "ilm")
			if err != nil {
				return err
			}
			if err := client.PutLifecycle(ctx, t.Bucket, cfg); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(ilmConfigRecord{Target: t.String(), Bucket: t.Bucket, Rules: cfg.Rules},
				fmt.Sprintf("Imported %d lifecycle %s into `%s`.", len(cfg.Rules), plural(len(cfg.Rules), "rule", "rules"), t))
		},
	}
}

func newILMApplyCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "apply ALIAS/BUCKET",
		Short: "Apply the lifecycle rules of a bucket now (Admin API)",
		Long: `Apply the lifecycle rules of a bucket at once instead of waiting for the
background scanner: expired versions and delete markers are removed and stale
uploads aborted. Versions protected by Object Lock are kept. Needs
s3:PutLifecycleConfiguration and admin:ApplyLifecycle.`,
		Example:           "  nexoctl ilm apply local/app",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "ilm apply")
			if err != nil {
				return err
			}
			res, err := client.ApplyBucketLifecycle(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			text := fmt.Sprintf("Lifecycle of `%s` applied: %d %s checked, %d deleted, %d delete %s created, %d %s aborted",
				t, res.Versions, plural(res.Versions, "version", "versions"), res.Deleted, res.DeleteMarkers, plural(res.DeleteMarkers, "marker", "markers"),
				res.UploadsAborted, plural(res.UploadsAborted, "upload", "uploads"))
			if res.Locked > 0 {
				text += fmt.Sprintf(", %d kept by Object Lock", res.Locked)
			}
			return e.out.Info(ilmApplyRecord{Target: t.String(), Bucket: t.Bucket, LifecycleApplyResult: *res}, text+".")
		},
	}
}

// readConfigDocument reads a bucket configuration document from the file
// of args (or the standard input when it is empty or "-"), at most 1 MiB.
func (e *env) readConfigDocument(args []string) ([]byte, error) {
	if len(args) == 0 || args[0] == "-" {
		return cli.ReadConfigDocument(e.deps.Stdin)
	}
	f, err := os.Open(args[0])
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return cli.ReadConfigDocument(f)
}

// encodeJSONDocument renders a document as indented JSON with a final
// newline.
func encodeJSONDocument(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode JSON: %w", err)
	}
	return buf.Bytes(), nil
}
