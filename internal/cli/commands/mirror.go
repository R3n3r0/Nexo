package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
)

// mirrorFlags are the flags of mirror.
type mirrorFlags struct {
	transferFlags
	remove    bool
	overwrite bool
	watch     bool
	dryRun    bool
	preserve  bool
}

func newMirrorCmd(e *env) *cobra.Command {
	var flags mirrorFlags
	cmd := &cobra.Command{
		Use:   "mirror [flags] SOURCE TARGET",
		Short: "Synchronize a directory or prefix to another one",
		Long: `Copy to TARGET the files and objects of SOURCE that are missing or different
there (same size and ETag, or a target not older than the source, means up to
date): a second run transfers nothing. Different targets are replaced only with
--overwrite; --remove deletes what is not in SOURCE. --watch keeps a local
SOURCE synchronized until Ctrl-C.

--preserve copies every version and delete marker between two buckets, the
oldest first, with their content type, metadata and tags: with "admin cluster
export/import" (bucket settings first, so that the target keeps the versions),
it migrates a deployment to another mode. A second run replays only the newer
versions of the source.`,
		Example: `  nexoctl mirror ./site/ local/website
  nexoctl mirror --remove --overwrite local/photos ./backup/photos
  nexoctl mirror --watch --remove --overwrite ./docs local/docs
  nexoctl mirror --preserve old/photos new/photos`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.mirror(cmd.Context(), args, flags)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&flags.remove, "remove", false, "remove from TARGET what is not in SOURCE")
	f.BoolVar(&flags.overwrite, "overwrite", false, "replace the different files and objects of TARGET")
	f.BoolVarP(&flags.watch, "watch", "w", false, "keep watching a local SOURCE and synchronize its changes")
	f.BoolVar(&flags.dryRun, "dry-run", false, "print what would be done without doing it")
	f.BoolVar(&flags.preserve, "preserve", false, "copy every version and delete marker between two buckets, with metadata and tags")
	flags.register(f)
	return cmd
}

func (e *env) mirror(ctx context.Context, args []string, flags mirrorFlags) error {
	opts, err := flags.options(e)
	if err != nil {
		return err
	}
	targets, err := e.targets(ctx, args, false)
	if err != nil {
		return err
	}
	src, dst := targets[0], targets[1]
	for _, t := range targets {
		if t.IsRemote() && t.Bucket == "" {
			return cli.UsageError("%s: mirror needs a bucket", t)
		}
	}
	if !src.IsRemote() {
		st, err := os.Stat(src.Path)
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return cli.UsageError("%s: the source of mirror is a directory", src.Path)
		}
	}
	if flags.watch && (src.IsRemote() || flags.dryRun) {
		return cli.UsageError("--watch needs a local source and excludes --dry-run")
	}
	if flags.preserve && (!src.IsRemote() || !dst.IsRemote() || flags.remove) {
		return cli.UsageError("--preserve copies the versions between two buckets and excludes --remove")
	}
	m, err := cli.NewMirrorer(cli.MirrorDeps{Resolve: e.resolver()}, cli.WithWatchDebounce(e.deps.WatchDebounce))
	if err != nil {
		return err
	}
	req := cli.MirrorRequest{
		Source: src, Target: dst, DryRun: flags.dryRun, Preserve: flags.preserve, Transfer: opts,
		Options: cli.MirrorOptions{Overwrite: flags.overwrite, Remove: flags.remove},
	}
	if !flags.watch {
		return e.mirrorPass(ctx, m, req)
	}
	// The watches are in place before the first pass: no change made
	// during the synchronization is missed.
	ready := func() error {
		if err := e.mirrorPass(ctx, m, req); err != nil {
			return err
		}
		return e.out.Note(fmt.Sprintf("Watching `%s` for changes (Ctrl-C to stop).", src))
	}
	onErr := func(err error) {
		if ctx.Err() == nil {
			e.out.Warn(err.Error())
		}
	}
	return m.Watch(ctx, req, ready, func(ev cli.MirrorEvent) error { return e.reportMirror(nil, ev, false) }, onErr)
}

// mirrorPass runs one comparison of the two sides and prints the events
// and the summary.
func (e *env) mirrorPass(ctx context.Context, m cli.Mirrorer, req cli.MirrorRequest) error {
	plan, err := m.Plan(ctx, req)
	if err != nil {
		return err
	}
	bar := e.progress(plan.Bytes)
	if req.DryRun {
		bar = cli.NewProgress(e.deps.Stderr, 0, false, e.deps.Clock)
	}
	req.Transfer.Progress = bar
	stats, err := m.Apply(ctx, req, plan, func(ev cli.MirrorEvent) error { return e.reportMirror(bar, ev, req.DryRun) })
	bar.Finish()
	if err != nil {
		return err
	}
	verb := "Mirrored"
	if req.DryRun {
		verb = "Would mirror"
	}
	markers := ""
	if stats.Markers > 0 {
		markers = fmt.Sprintf(", %d delete %s", stats.Markers, plural(stats.Markers, "marker", "markers"))
	}
	if err := e.out.Note(fmt.Sprintf("%s %d %s (%s)%s, removed %d, skipped %d.", verb, stats.Copied,
		plural(stats.Copied, "object", "objects"), cli.HumanSize(stats.Bytes), markers, stats.Removed, stats.Conflicts)); err != nil {
		return err
	}
	return batchError("mirror operations", stats.Failed, stats.Copied+stats.Markers+stats.Removed+stats.Conflicts+stats.Failed, stats.First)
}

// reportMirror prints one mirror event: an action not attempted (or a
// removal that failed during a watch), a conflict kept, or an action done
// (would be done in a dry run). Done actions are not listed while the
// progress bar is drawn.
func (e *env) reportMirror(bar *cli.Progress, ev cli.MirrorEvent, dryRun bool) error {
	if ev.Err != nil {
		if ev.Kind == cli.MirrorRemove && ev.Source.Raw == "" {
			e.out.Warn(fmt.Sprintf("failed to remove `%s`: %v", ev.Dest, ev.Err))
			return nil
		}
		return e.reportSkipped(string(ev.Kind), cli.CopyItem{Source: ev.Source, Dest: ev.Dest, Size: ev.Size, Err: ev.Err})
	}
	rec := transferRecord{Action: string(ev.Kind), Source: ev.Source.String(), Target: ev.Dest.String(), Size: ev.Size, DryRun: dryRun,
		VersionID: ev.SourceVersion}
	if ev.Kind == cli.MirrorConflict {
		if e.out.JSON() {
			return e.out.Result(rec, "")
		}
		e.out.Warn(fmt.Sprintf("skipping `%s`: the target differs (use --overwrite)", ev.Dest))
		return nil
	}
	if ev.Kind == cli.MirrorRemove {
		rec.Source = ""
	}
	if ev.Kind == cli.MirrorDeleteMarker {
		rec.Size = 0
	}
	if bar.Enabled() && !e.out.JSON() {
		return nil
	}
	source := fmt.Sprintf("`%s`", ev.Source)
	if ev.SourceVersion != "" {
		source = fmt.Sprintf("`%s` (version %s)", ev.Source, ev.SourceVersion)
	}
	var text string
	switch ev.Kind {
	case cli.MirrorDeleteMarker:
		text = fmt.Sprintf("Added a delete marker to `%s`.", ev.Dest)
		if dryRun {
			text = fmt.Sprintf("Would add a delete marker to `%s`.", ev.Dest)
		}
	case cli.MirrorRemove:
		text = fmt.Sprintf("Removed `%s`.", ev.Dest)
		if dryRun {
			text = fmt.Sprintf("Would remove `%s`.", ev.Dest)
		}
	case cli.MirrorOverwrite:
		text = fmt.Sprintf("%s -> `%s` (%s, overwritten)", source, ev.Dest, cli.HumanSize(ev.Size))
	default:
		text = fmt.Sprintf("%s -> `%s` (%s)", source, ev.Dest, cli.HumanSize(ev.Size))
	}
	if dryRun && ev.Kind != cli.MirrorRemove && ev.Kind != cli.MirrorDeleteMarker {
		text = "Would copy " + text
	}
	return e.out.Info(rec, text)
}
