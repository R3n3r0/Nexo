package commands

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
)

// mirrorFlags are the flags of mirror.
type mirrorFlags struct {
	transferFlags
	remove    bool
	overwrite bool
	watch     bool
	dryRun    bool
}

// mirrorStats counts the actions of a mirror.
type mirrorStats struct {
	copied    int
	bytes     int64
	removed   int
	conflicts int
	// failed counts the actions not attempted (unsafe local names), the
	// first of which is first.
	failed int
	first  error
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
SOURCE synchronized until Ctrl-C.`,
		Example: `  nexoctl mirror ./site/ local/website
  nexoctl mirror --remove --overwrite local/photos ./backup/photos
  nexoctl mirror --watch --remove --overwrite ./docs local/docs`,
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
	if flags.watch {
		// The watches are in place before the first pass: no change made
		// during the synchronization is missed.
		return e.mirrorWatch(ctx, src, dst, flags, opts, func() error {
			if err := e.mirrorPass(ctx, src, dst, flags, opts); err != nil {
				return err
			}
			return e.out.Note(fmt.Sprintf("Watching `%s` for changes (Ctrl-C to stop).", src))
		})
	}
	return e.mirrorPass(ctx, src, dst, flags, opts)
}

// mirrorPass runs one comparison of the two sides and prints the summary.
func (e *env) mirrorPass(ctx context.Context, src, dst cli.Target, flags mirrorFlags, opts cli.TransferOptions) error {
	stats, err := e.mirrorOnce(ctx, src, dst, flags, opts)
	if err != nil {
		return err
	}
	verb := "Mirrored"
	if flags.dryRun {
		verb = "Would mirror"
	}
	if err := e.out.Note(fmt.Sprintf("%s %d %s (%s), removed %d, skipped %d.", verb, stats.copied,
		plural(stats.copied, "object", "objects"), cli.HumanSize(stats.bytes), stats.removed, stats.conflicts)); err != nil {
		return err
	}
	return batchError("mirror operations", stats.failed, stats.copied+stats.removed+stats.conflicts+stats.failed, stats.first)
}

// mirrorItem returns the transfer of a mirror action. The source of a
// removal is not needed: its name may not even be valid on the source
// side. The error is ErrUnsafeName for a name that cannot be written under
// a local directory; the item then carries the source to report.
func mirrorItem(src, dst cli.Target, a cli.MirrorAction) (cli.CopyItem, error) {
	item := cli.CopyItem{Size: a.Source.Size, ModTime: a.Source.ModTime, ETag: a.Source.ETag}
	var err error
	if a.Kind != cli.MirrorRemove {
		if item.Source, err = cli.JoinTarget(src, a.Rel); err != nil {
			return item, err
		}
	}
	if item.Dest, err = cli.JoinTarget(dst, a.Rel); err != nil {
		return item, err
	}
	return item, nil
}

// mirrorOnce compares the two sides and runs the actions.
func (e *env) mirrorOnce(ctx context.Context, src, dst cli.Target, flags mirrorFlags, opts cli.TransferOptions) (mirrorStats, error) {
	var stats mirrorStats
	srcEntries, err := cli.CollectEntries(cli.Walk(ctx, e.resolver(), src))
	if err != nil {
		return stats, err
	}
	dstEntries, err := cli.CollectEntries(cli.Walk(ctx, e.resolver(), dst))
	if err != nil {
		return stats, err
	}
	actions := cli.PlanMirror(srcEntries, dstEntries, cli.MirrorOptions{Overwrite: flags.overwrite, Remove: flags.remove})
	var total int64
	for _, a := range actions {
		if a.Kind == cli.MirrorCopy || a.Kind == cli.MirrorOverwrite {
			total += a.Source.Size
		}
	}
	bar := e.progress(total)
	if flags.dryRun {
		bar = cli.NewProgress(e.deps.Stderr, 0, false, e.deps.Clock)
	}
	opts.Progress = bar
	defer bar.Finish()
	tr := e.transfer()
	for _, a := range actions {
		item, err := mirrorItem(src, dst, a)
		if err != nil {
			// Never written: reported as a failure of this path only.
			if item.Source.Raw == "" {
				item.Source = cli.Target{Raw: a.Rel, Path: a.Rel}
			}
			item.Err = err
			stats.failed++
			if stats.first == nil {
				stats.first = fmt.Errorf("%s: %w", item.Source, err)
			}
			if err := e.reportSkipped(string(a.Kind), item); err != nil {
				return stats, err
			}
			continue
		}
		switch a.Kind {
		case cli.MirrorConflict:
			stats.conflicts++
			if e.out.JSON() {
				rec := transferRecord{Action: string(a.Kind), Source: item.Source.String(), Target: item.Dest.String(), Size: a.Source.Size, DryRun: flags.dryRun}
				if err := e.out.Result(rec, ""); err != nil {
					return stats, err
				}
				continue
			}
			e.out.Warn(fmt.Sprintf("skipping `%s`: the target differs (use --overwrite)", item.Dest))
		case cli.MirrorRemove:
			if !flags.dryRun {
				if err := tr.Delete(ctx, item.Dest, ""); err != nil {
					return stats, fmt.Errorf("remove %s: %w", item.Dest, err)
				}
			}
			stats.removed++
			if err := e.reportMirror(bar, a.Kind, item, 0, flags.dryRun); err != nil {
				return stats, err
			}
		default:
			size := a.Source.Size
			if !flags.dryRun {
				bar.SetLabel(displayName(item.Source))
				res, err := tr.Copy(ctx, item, opts)
				if err != nil {
					return stats, err
				}
				size = res.Size
			}
			stats.copied++
			stats.bytes += size
			if err := e.reportMirror(bar, a.Kind, item, size, flags.dryRun); err != nil {
				return stats, err
			}
		}
	}
	return stats, nil
}

// reportMirror prints one mirror action.
func (e *env) reportMirror(bar *cli.Progress, kind cli.MirrorActionKind, item cli.CopyItem, size int64, dryRun bool) error {
	rec := transferRecord{Action: string(kind), Source: item.Source.String(), Target: item.Dest.String(), Size: size, DryRun: dryRun}
	if kind == cli.MirrorRemove {
		rec.Source = ""
	}
	if bar.Enabled() && !e.out.JSON() {
		return nil
	}
	var text string
	switch kind {
	case cli.MirrorRemove:
		text = fmt.Sprintf("Removed `%s`.", item.Dest)
		if dryRun {
			text = fmt.Sprintf("Would remove `%s`.", item.Dest)
		}
	case cli.MirrorOverwrite:
		text = fmt.Sprintf("`%s` -> `%s` (%s, overwritten)", item.Source, item.Dest, cli.HumanSize(size))
	default:
		text = fmt.Sprintf("`%s` -> `%s` (%s)", item.Source, item.Dest, cli.HumanSize(size))
	}
	if dryRun && kind != cli.MirrorRemove {
		text = "Would copy " + text
	}
	return e.out.Info(rec, text)
}

// mirrorWatch synchronizes the changes of a local source until ctx is
// done; ready runs the first pass once the watches are in place.
func (e *env) mirrorWatch(ctx context.Context, src, dst cli.Target, flags mirrorFlags, opts cli.TransferOptions, ready func() error) error {
	tr := e.transfer()
	opts.Progress = nil
	handle := func(ctx context.Context, ev cli.WatchEvent) error {
		from, err := cli.JoinTarget(src, ev.Rel)
		if err != nil {
			return err
		}
		to, err := cli.JoinTarget(dst, ev.Rel)
		if err != nil {
			return err
		}
		item := cli.CopyItem{Source: from, Dest: to}
		if ev.Removed {
			if !flags.remove {
				return nil
			}
			return e.watchRemove(ctx, item.Dest)
		}
		st, err := os.Stat(item.Source.Path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		source := cli.Entry{Rel: ev.Rel, Size: st.Size(), ModTime: st.ModTime()}
		item.Size, item.ModTime = source.Size, source.ModTime
		kind := cli.MirrorCopy
		if existing, ok, err := e.entryOf(ctx, item.Dest); err != nil {
			return err
		} else if ok {
			if cli.SameEntry(source, existing) {
				return nil
			}
			if !flags.overwrite {
				e.out.Warn(fmt.Sprintf("skipping `%s`: the target differs (use --overwrite)", item.Dest))
				return nil
			}
			kind = cli.MirrorOverwrite
		}
		res, err := tr.Copy(ctx, item, opts)
		if err != nil {
			return err
		}
		return e.reportMirror(nil, kind, item, res.Size, false)
	}
	onErr := func(err error) {
		if ctx.Err() == nil {
			e.out.Warn(err.Error())
		}
	}
	return cli.WatchDir(ctx, src.Path, e.deps.WatchDebounce, ready, handle, onErr)
}

// watchRemove removes a target path and everything under it (a removed
// file or directory of the source).
func (e *env) watchRemove(ctx context.Context, dst cli.Target) error {
	if !dst.IsRemote() {
		if _, err := os.Lstat(dst.Path); err != nil {
			return nil
		}
		if err := os.RemoveAll(dst.Path); err != nil {
			return err
		}
		return e.reportMirror(nil, cli.MirrorRemove, cli.CopyItem{Dest: dst}, 0, false)
	}
	client, err := e.client(ctx, dst.Alias)
	if err != nil {
		return err
	}
	_, err = cli.RemoveObjects(ctx, client, dst.Bucket, dst.Key, cli.RemoveOptions{Recursive: true}, func(r cli.RemoveResult) error {
		if r.Err != nil {
			e.out.Warn(fmt.Sprintf("failed to remove `%s`: %v", dst.Child(r.Key), r.Err))
			return nil
		}
		return e.reportMirror(nil, cli.MirrorRemove, cli.CopyItem{Dest: dst.Child(r.Key)}, 0, false)
	})
	return err
}

// entryOf returns the entry of a single local file or remote object; ok
// is false when it does not exist.
func (e *env) entryOf(ctx context.Context, t cli.Target) (cli.Entry, bool, error) {
	if !t.IsRemote() {
		st, err := os.Stat(t.Path)
		if errors.Is(err, os.ErrNotExist) {
			return cli.Entry{}, false, nil
		}
		if err != nil {
			return cli.Entry{}, false, err
		}
		return cli.Entry{Size: st.Size(), ModTime: st.ModTime()}, true, nil
	}
	client, err := e.client(ctx, t.Alias)
	if err != nil {
		return cli.Entry{}, false, err
	}
	info, err := client.Stat(ctx, t.Bucket, t.Key, cli.StatOptions{})
	if errors.Is(err, domain.ErrObjectNotFound) {
		return cli.Entry{}, false, nil
	}
	if err != nil {
		return cli.Entry{}, false, err
	}
	return cli.Entry{Size: info.Size, ModTime: info.LastModified, ETag: info.ETag}, true, nil
}
