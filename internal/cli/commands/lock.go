package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
)

// lockFlags select the object versions of the retention and legalhold
// commands.
type lockFlags struct {
	versionID string
	recursive bool
	versions  bool
}

func (f *lockFlags) register(fs *pflag.FlagSet) {
	fs.StringVar(&f.versionID, "version-id", "", "apply to this version of the object")
	fs.BoolVarP(&f.recursive, "recursive", "r", false, "apply to every object under the prefix")
	fs.BoolVar(&f.versions, "versions", false, "apply to every version instead of the latest one")
}

func (f lockFlags) options() cli.LockTargetOptions {
	return cli.LockTargetOptions{Recursive: f.recursive, Versions: f.versions, VersionID: f.versionID}
}

// lockTarget parses the target of the Object Lock commands.
func (e *env) lockTarget(ctx context.Context, arg string) (cli.Target, cli.S3Client, error) {
	t, err := e.remoteTarget(ctx, arg, true)
	if err != nil {
		return cli.Target{}, nil, err
	}
	client, err := e.client(ctx, t.Alias)
	if err != nil {
		return cli.Target{}, nil, err
	}
	return t, client, nil
}

// forEachLockVersion runs fn on every version selected by flags under t and
// reports the failures: a single object returns its error, a recursive
// run warns for each failed version and fails at the end (the first error
// sets the exit code).
func (e *env) forEachLockVersion(ctx context.Context, client cli.S3Client, t cli.Target, flags lockFlags, what string,
	fn func(ref cli.ObjectRef, target cli.Target) error, failed func(ref cli.ObjectRef, target cli.Target, err error) error) error {
	var (
		total, nFailed int
		first          error
	)
	single := !flags.recursive && !flags.versions
	for v, err := range cli.LockTargets(ctx, client, t.Bucket, t.Key, flags.options()) {
		if err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
		total++
		target := t.Child(v.Key)
		ref := cli.ObjectRef{Bucket: t.Bucket, Key: v.Key, VersionID: v.VersionID}
		if err := fn(ref, target); err != nil {
			if single {
				return fmt.Errorf("%s: %w", target, err)
			}
			nFailed++
			if first == nil {
				first = fmt.Errorf("%s: %w", target, err)
			}
			if err := failed(ref, target, err); err != nil {
				return err
			}
		}
	}
	if total == 0 && flags.recursive {
		return e.out.Note(fmt.Sprintf("No object under `%s`.", t))
	}
	return batchError(what, nFailed, total, first)
}

// versionSuffix renders " (version ID)" for an explicit version.
func versionSuffix(versionID string) string {
	if versionID == "" {
		return ""
	}
	return " (version " + versionID + ")"
}

// --- retention ---------------------------------------------------------------------

func newRetentionCmd(e *env) *cobra.Command {
	cmd := groupCmd("retention", "Manage the Object Lock retention of objects and buckets")
	cmd.Long = `Manage the Object Lock retention (S3 WORM) of object versions and the default
retention of buckets. The bucket must have been created with Object Lock
("nexoctl mb --with-lock").

A GOVERNANCE retention can be shortened or removed with --bypass by the users
allowed to (s3:BypassGovernanceRetention); a COMPLIANCE retention can only be
extended, by anyone, root included. The versions under retention cannot be
deleted until it expires.`
	cmd.AddCommand(newRetentionSetCmd(e), newRetentionClearCmd(e), newRetentionInfoCmd(e))
	return cmd
}

func newRetentionSetCmd(e *env) *cobra.Command {
	var (
		flags             lockFlags
		mode, validity    string
		bypass, isDefault bool
	)
	cmd := &cobra.Command{
		Use:   "set --mode governance|compliance --validity 30d|1y [flags] ALIAS/BUCKET[/OBJECT|PREFIX]",
		Short: "Set the retention of objects, or the default retention of a bucket",
		Example: `  nexoctl retention set --mode governance --validity 30d local/vault/report.pdf
  nexoctl retention set --mode compliance --validity 1y -r local/vault/2026/
  nexoctl retention set --default --mode governance --validity 7d local/vault`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if mode == "" || validity == "" {
				return cli.UsageError("--mode and --validity are required")
			}
			m, err := domain.ParseRetentionMode(mode)
			if err != nil {
				return cli.UsageError("invalid --mode %q (governance, compliance)", mode)
			}
			period, err := cli.ParseValidity(validity)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			t, client, err := e.lockTarget(ctx, args[0])
			if err != nil {
				return err
			}
			if isDefault {
				if bypass || flags != (lockFlags{}) {
					return cli.UsageError("--default sets the default retention of a bucket: it excludes --bypass, --version-id, --recursive and --versions")
				}
				return e.setDefaultRetention(ctx, client, t, domain.DefaultRetention{Mode: m, Days: period.Days, Years: period.Years})
			}
			if err := checkObjectTarget(t, flags.recursive, "--default"); err != nil {
				return err
			}
			retention := domain.ObjectRetention{Mode: m, RetainUntil: period.Until(e.deps.Clock.Now())}
			return e.forEachLockVersion(ctx, client, t, flags, "retention changes",
				func(ref cli.ObjectRef, target cli.Target) error {
					if err := client.PutRetention(ctx, ref, retention, bypass); err != nil {
						return err
					}
					rec := retentionRecord{Target: target.String(), Key: ref.Key, VersionID: ref.VersionID, Mode: string(m), RetainUntil: timePtr(retention.RetainUntil)}
					return e.out.Info(rec, fmt.Sprintf("Retention of `%s`%s: %s until %s.", target, versionSuffix(ref.VersionID), m, cli.FormatTime(retention.RetainUntil)))
				}, e.reportLockFailure)
		},
	}
	f := cmd.Flags()
	f.StringVar(&mode, "mode", "", "retention mode: governance or compliance")
	f.StringVar(&validity, "validity", "", "retention period: days (30d) or years (1y)")
	f.BoolVar(&bypass, "bypass", false, "shorten a GOVERNANCE retention (s3:BypassGovernanceRetention)")
	f.BoolVar(&isDefault, "default", false, "set the default retention of the bucket")
	flags.register(f)
	return cmd
}

func newRetentionClearCmd(e *env) *cobra.Command {
	var (
		flags             lockFlags
		bypass, isDefault bool
	)
	cmd := &cobra.Command{
		Use:   "clear [flags] ALIAS/BUCKET[/OBJECT|PREFIX]",
		Short: "Remove the retention of objects, or the default retention of a bucket",
		Long: `Remove the retention of object versions: a GOVERNANCE retention still in force
needs --bypass, a COMPLIANCE one cannot be removed. With --default, remove the
default retention of a bucket (the existing versions keep theirs).`,
		Example: `  nexoctl retention clear --bypass local/vault/report.pdf
  nexoctl retention clear --default local/vault`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.lockTarget(ctx, args[0])
			if err != nil {
				return err
			}
			if isDefault {
				if bypass || flags != (lockFlags{}) {
					return cli.UsageError("--default clears the default retention of a bucket: it excludes --bypass, --version-id, --recursive and --versions")
				}
				return e.setDefaultRetention(ctx, client, t, domain.DefaultRetention{})
			}
			if err := checkObjectTarget(t, flags.recursive, "--default"); err != nil {
				return err
			}
			return e.forEachLockVersion(ctx, client, t, flags, "retention changes",
				func(ref cli.ObjectRef, target cli.Target) error {
					if err := client.PutRetention(ctx, ref, domain.ObjectRetention{}, bypass); err != nil {
						return err
					}
					rec := retentionRecord{Target: target.String(), Key: ref.Key, VersionID: ref.VersionID}
					return e.out.Info(rec, fmt.Sprintf("Retention of `%s`%s removed.", target, versionSuffix(ref.VersionID)))
				}, e.reportLockFailure)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&bypass, "bypass", false, "remove a GOVERNANCE retention still in force (s3:BypassGovernanceRetention)")
	f.BoolVar(&isDefault, "default", false, "remove the default retention of the bucket")
	flags.register(f)
	return cmd
}

func newRetentionInfoCmd(e *env) *cobra.Command {
	var (
		flags     lockFlags
		isDefault bool
	)
	cmd := &cobra.Command{
		Use:   "info [flags] ALIAS/BUCKET[/OBJECT|PREFIX]",
		Short: "Show the retention of objects, or the default retention of a bucket",
		Long: `Show the retention of object versions, or with a bucket (or --default) the
Object Lock configuration of the bucket and its default retention.`,
		Example: `  nexoctl retention info local/vault/report.pdf
  nexoctl retention info -r --versions local/vault/2026/
  nexoctl retention info local/vault`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.lockTarget(ctx, args[0])
			if err != nil {
				return err
			}
			if isDefault || (t.Key == "" && !flags.recursive) {
				if flags != (lockFlags{}) {
					return cli.UsageError("the default retention of a bucket excludes --version-id, --recursive and --versions")
				}
				if t.Key != "" {
					return cli.UsageError("%s: --default applies to ALIAS/BUCKET", t)
				}
				return e.defaultRetentionInfo(ctx, client, t)
			}
			now := e.deps.Clock.Now()
			return e.forEachLockVersion(ctx, client, t, flags, "retention reads",
				func(ref cli.ObjectRef, target cli.Target) error {
					r, err := client.GetRetention(ctx, ref)
					if err != nil {
						return err
					}
					rec := retentionRecord{Target: target.String(), Key: ref.Key, VersionID: ref.VersionID}
					text := fmt.Sprintf("`%s`%s: no retention.", target, versionSuffix(ref.VersionID))
					if !r.IsZero() {
						rec.Mode, rec.RetainUntil = string(r.Mode), timePtr(r.RetainUntil)
						state := "in force"
						if !r.Active(now) {
							state = "expired"
						}
						text = fmt.Sprintf("`%s`%s: %s until %s (%s).", target, versionSuffix(ref.VersionID), r.Mode, cli.FormatTime(r.RetainUntil), state)
					}
					return e.out.Result(rec, text)
				}, e.reportLockFailure)
		},
	}
	cmd.Flags().BoolVar(&isDefault, "default", false, "show the default retention of the bucket")
	flags.register(cmd.Flags())
	return cmd
}

// checkObjectTarget requires an object, or a prefix with --recursive.
func checkObjectTarget(t cli.Target, recursive bool, bucketFlag string) error {
	if t.Key == "" && !recursive {
		return cli.UsageError("%s is a bucket: use %s for its default retention or --recursive for its objects", t, bucketFlag)
	}
	return nil
}

// setDefaultRetention sets (or, with the zero retention, removes) the
// default retention of a bucket.
func (e *env) setDefaultRetention(ctx context.Context, client cli.S3Client, t cli.Target, def domain.DefaultRetention) error {
	if t.Key != "" {
		return cli.UsageError("%s: --default applies to ALIAS/BUCKET", t)
	}
	if err := client.SetObjectLock(ctx, t.Bucket, def); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return fmt.Errorf("%s: %w (Object Lock is enabled when the bucket is created: nexoctl mb --with-lock)", t, err)
		}
		return fmt.Errorf("%s: %w", t, err)
	}
	cfg := &cli.ObjectLockConfig{Enabled: true, Default: def}
	rec := retentionRecord{Target: t.String(), Default: true, ObjectLock: objectLockOf(cfg), Mode: string(def.Mode)}
	if def.IsZero() {
		return e.out.Info(rec, fmt.Sprintf("Removed the default retention of `%s`.", t))
	}
	return e.out.Info(rec, fmt.Sprintf("Default retention of `%s`: %s for %s.", t, def.Mode, cli.Validity{Days: def.Days, Years: def.Years}))
}

// defaultRetentionInfo prints the Object Lock configuration of a bucket.
func (e *env) defaultRetentionInfo(ctx context.Context, client cli.S3Client, t cli.Target) error {
	cfg, err := client.GetObjectLock(ctx, t.Bucket)
	if err != nil {
		return fmt.Errorf("%s: %w", t, err)
	}
	rec := retentionRecord{Target: t.String(), Default: true, ObjectLock: objectLockOf(cfg), Mode: string(cfg.Default.Mode)}
	text := fmt.Sprintf("`%s` has no Object Lock (create the bucket with \"nexoctl mb --with-lock\").", t)
	switch {
	case cfg.Enabled && cfg.Default.IsZero():
		text = fmt.Sprintf("`%s` has Object Lock, without default retention.", t)
	case cfg.Enabled:
		text = fmt.Sprintf("`%s` has Object Lock, default retention %s for %s.", t, cfg.Default.Mode, cli.Validity{Days: cfg.Default.Days, Years: cfg.Default.Years})
	}
	return e.out.Result(rec, text)
}

// reportLockFailure prints a version whose retention or legal hold could
// not be read or changed: a warning, or an "error" JSON line on stderr.
func (e *env) reportLockFailure(ref cli.ObjectRef, target cli.Target, err error) error {
	if e.out.JSON() {
		return e.out.JSONLine(e.out.Stderr(), "error", retentionRecord{Target: target.String(), Key: ref.Key, VersionID: ref.VersionID, Error: err.Error()})
	}
	e.out.Warn(fmt.Sprintf("`%s`%s: %v", target, versionSuffix(ref.VersionID), err))
	return nil
}

// --- legal hold --------------------------------------------------------------------

func newLegalHoldCmd(e *env) *cobra.Command {
	cmd := groupCmd("legalhold", "Manage the Object Lock legal hold of objects")
	cmd.Long = `Manage the legal hold of object versions: a version under legal hold cannot be
deleted, whatever its retention, until the hold is cleared. The bucket must have
been created with Object Lock ("nexoctl mb --with-lock").`
	cmd.AddCommand(
		newLegalHoldSetCmd(e, "set", "Put objects under legal hold", domain.LegalHoldOn),
		newLegalHoldSetCmd(e, "clear", "Release the legal hold of objects", domain.LegalHoldOff),
		newLegalHoldInfoCmd(e),
	)
	return cmd
}

func newLegalHoldSetCmd(e *env, name, short string, hold domain.LegalHoldStatus) *cobra.Command {
	var flags lockFlags
	cmd := &cobra.Command{
		Use:               name + " [flags] ALIAS/BUCKET/OBJECT|PREFIX",
		Short:             short,
		Example:           fmt.Sprintf("  nexoctl legalhold %s local/vault/report.pdf\n  nexoctl legalhold %s -r local/vault/case-42/", name, name),
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.lockTarget(ctx, args[0])
			if err != nil {
				return err
			}
			if t.Key == "" && !flags.recursive {
				return cli.UsageError("%s is a bucket: use --recursive for its objects", t)
			}
			return e.forEachLockVersion(ctx, client, t, flags, "legal hold changes",
				func(ref cli.ObjectRef, target cli.Target) error {
					if err := client.PutLegalHold(ctx, ref, hold); err != nil {
						return err
					}
					rec := legalHoldRecord{Target: target.String(), Key: ref.Key, VersionID: ref.VersionID, LegalHold: string(hold)}
					return e.out.Info(rec, fmt.Sprintf("Legal hold of `%s`%s: %s.", target, versionSuffix(ref.VersionID), hold))
				}, e.reportLegalHoldFailure)
		},
	}
	flags.register(cmd.Flags())
	return cmd
}

func newLegalHoldInfoCmd(e *env) *cobra.Command {
	var flags lockFlags
	cmd := &cobra.Command{
		Use:               "info [flags] ALIAS/BUCKET/OBJECT|PREFIX",
		Short:             "Show the legal hold of objects",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.lockTarget(ctx, args[0])
			if err != nil {
				return err
			}
			if t.Key == "" && !flags.recursive {
				return cli.UsageError("%s is a bucket: use --recursive for its objects", t)
			}
			return e.forEachLockVersion(ctx, client, t, flags, "legal hold reads",
				func(ref cli.ObjectRef, target cli.Target) error {
					hold, err := client.GetLegalHold(ctx, ref)
					if err != nil {
						return err
					}
					rec := legalHoldRecord{Target: target.String(), Key: ref.Key, VersionID: ref.VersionID, LegalHold: string(hold)}
					return e.out.Result(rec, fmt.Sprintf("`%s`%s: legal hold %s.", target, versionSuffix(ref.VersionID), hold))
				}, e.reportLegalHoldFailure)
		},
	}
	flags.register(cmd.Flags())
	return cmd
}

// reportLegalHoldFailure prints a version whose legal hold failed.
func (e *env) reportLegalHoldFailure(ref cli.ObjectRef, target cli.Target, err error) error {
	if e.out.JSON() {
		return e.out.JSONLine(e.out.Stderr(), "error", legalHoldRecord{Target: target.String(), Key: ref.Key, VersionID: ref.VersionID, Error: err.Error()})
	}
	e.out.Warn(fmt.Sprintf("`%s`%s: %v", target, versionSuffix(ref.VersionID), err))
	return nil
}

// lockState renders the Object Lock state of a version for stat and rb:
// "GOVERNANCE until DATE", "legal hold ON", or "".
func lockState(r domain.ObjectRetention, hold domain.LegalHoldStatus, now time.Time) string {
	var parts []string
	if !r.IsZero() {
		state := ""
		if !r.Active(now) {
			state = " (expired)"
		}
		parts = append(parts, fmt.Sprintf("%s until %s%s", r.Mode, cli.FormatTime(r.RetainUntil), state))
	}
	if hold == domain.LegalHoldOn {
		parts = append(parts, "legal hold ON")
	}
	return strings.Join(parts, ", ")
}
