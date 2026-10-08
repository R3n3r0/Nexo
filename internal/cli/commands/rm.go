package commands

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
)

// rmFlags are the flags of rm.
type rmFlags struct {
	recursive  bool
	force      bool
	versions   bool
	versionID  string
	olderThan  string
	newerThan  string
	dryRun     bool
	incomplete bool
}

func newRmCmd(e *env) *cobra.Command {
	var flags rmFlags
	cmd := &cobra.Command{
		Use:   "rm [flags] ALIAS/BUCKET/OBJECT...",
		Short: "Remove objects, object versions and incomplete uploads",
		Long: `Remove objects. In a versioned bucket removing an object adds a delete
marker; --versions removes every version and delete marker, --version-id one
version. Removing a prefix needs --recursive --force. --dry-run only prints what
would be removed.`,
		Example: `  nexoctl rm local/logs/app.log
  nexoctl rm --recursive --force --older-than 30d local/logs/
  nexoctl rm --versions local/docs/report.pdf
  nexoctl rm --incomplete --recursive --force local/uploads/`,
		Args:              minArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.rm(cmd.Context(), args, flags)
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&flags.recursive, "recursive", "r", false, "remove everything under the prefix (needs --force)")
	f.BoolVar(&flags.force, "force", false, "confirm a recursive removal")
	f.BoolVar(&flags.versions, "versions", false, "remove every version and delete marker")
	f.StringVar(&flags.versionID, "version-id", "", "remove this version")
	f.StringVar(&flags.olderThan, "older-than", "", "remove only the objects older than the duration (e.g. 7d, 12h)")
	f.StringVar(&flags.newerThan, "newer-than", "", "remove only the objects newer than the duration")
	f.BoolVar(&flags.dryRun, "dry-run", false, "print what would be removed without removing it")
	f.BoolVarP(&flags.incomplete, "incomplete", "I", false, "abort the incomplete multipart uploads instead")
	return cmd
}

func (e *env) rm(ctx context.Context, args []string, flags rmFlags) error {
	if flags.recursive && !flags.force {
		return cli.UsageError("removing recursively needs --force")
	}
	opts := cli.RemoveOptions{
		Recursive: flags.recursive, Versions: flags.versions, VersionID: flags.versionID,
		DryRun: flags.dryRun, Now: e.deps.Clock.Now(),
	}
	var err error
	if flags.olderThan != "" {
		if opts.OlderThan, err = cli.ParseDuration(flags.olderThan); err != nil {
			return err
		}
	}
	if flags.newerThan != "" {
		if opts.NewerThan, err = cli.ParseDuration(flags.newerThan); err != nil {
			return err
		}
	}
	var (
		failed, total int
		first         error
	)
	for _, arg := range args {
		t, err := e.remoteTarget(ctx, arg, true)
		if err != nil {
			return err
		}
		client, err := e.client(ctx, t.Alias)
		if err != nil {
			return err
		}
		if flags.incomplete {
			sum, err := cli.RemoveUploads(ctx, client, t.Bucket, t.Key, flags.recursive, flags.dryRun, func(u cli.UploadInfo, rerr error) error {
				if rerr != nil && first == nil {
					first = fmt.Errorf("%s: %w", t.Child(u.Key), rerr)
				}
				return e.reportRemoval(t, cli.RemoveResult{Key: u.Key, Err: rerr}, u.UploadID, flags.dryRun)
			})
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			failed, total = failed+sum.Failed, total+sum.Removed+sum.Failed
			continue
		}
		sum, err := cli.RemoveObjects(ctx, client, t.Bucket, t.Key, opts, func(r cli.RemoveResult) error {
			if r.Err != nil && first == nil {
				first = fmt.Errorf("%s: %w", t.Child(r.Key), r.Err)
			}
			return e.reportRemoval(t, r, "", flags.dryRun)
		})
		failed, total = failed+sum.Failed, total+sum.Removed+sum.Failed
		if err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
	}
	return batchError("removals", failed, total, first)
}

// reportRemoval prints one removed (or failed) object, version or upload.
func (e *env) reportRemoval(t cli.Target, r cli.RemoveResult, uploadID string, dryRun bool) error {
	target := t.Child(r.Key)
	rec := removeRecord{
		Target: target.String(), Key: r.Key, VersionID: r.VersionID, DeleteMarker: r.DeleteMarker,
		DeleteMarkerVersionID: r.DeleteMarkerVersionID, UploadID: uploadID, DryRun: dryRun,
	}
	if r.Err != nil {
		rec.Error = r.Err.Error()
		if e.out.JSON() {
			return e.out.JSONLine(e.out.Stderr(), "error", rec)
		}
		e.out.Warn(fmt.Sprintf("failed to remove `%s`: %v", target, r.Err))
		return nil
	}
	detail := ""
	switch {
	case uploadID != "":
		detail = fmt.Sprintf(" (upload %s)", uploadID)
	case r.VersionID != "":
		detail = fmt.Sprintf(" (version %s)", r.VersionID)
	case r.DeleteMarker && r.DeleteMarkerVersionID != "":
		detail = fmt.Sprintf(" (delete marker %s)", r.DeleteMarkerVersionID)
	}
	verb := "Removed"
	if dryRun {
		verb = "Would remove"
	}
	return e.out.Info(rec, fmt.Sprintf("%s `%s`%s.", verb, target, detail))
}
