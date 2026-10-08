package commands

import (
	"context"
	"fmt"
	"path"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/r3n3r0/nexo/internal/cli"
)

// transferFlags are the flags shared by cp, mv, mirror and pipe.
type transferFlags struct {
	attr          string
	tags          string
	contentType   string
	parallel      int
	partSize      string
	limitUpload   string
	limitDownload string
}

func (f *transferFlags) register(fs *pflag.FlagSet) {
	fs.StringVar(&f.attr, "attr", "", `user metadata of the uploaded objects: "key1=value1;key2=value2"`)
	fs.StringVar(&f.tags, "tags", "", `tags of the uploaded objects: "key1=value1&key2=value2"`)
	fs.StringVar(&f.contentType, "content-type", "", "content type of the uploaded objects (default: from the file extension)")
	fs.IntVar(&f.parallel, "parallel", cli.DefaultParallel, "parts of a multipart upload sent at once")
	fs.StringVar(&f.partSize, "part-size", "", "part size of the multipart uploads, 5MiB-5GiB (default: computed from the size)")
	fs.StringVar(&f.limitUpload, "limit-upload", "", "cap the upload bandwidth (e.g. 10MiB, per second)")
	fs.StringVar(&f.limitDownload, "limit-download", "", "cap the download bandwidth (e.g. 10MiB, per second)")
}

// options validates the flags and returns the transfer options; it also
// sets the bandwidth limits of the clients.
func (f *transferFlags) options(e *env) (cli.TransferOptions, error) {
	var opts cli.TransferOptions
	if f.parallel < 1 || f.parallel > 64 {
		return opts, cli.UsageError("--parallel must be between 1 and 64")
	}
	opts.Parallel = f.parallel
	opts.ContentType = f.contentType
	if f.partSize != "" {
		size, err := cli.ParseSize(f.partSize)
		if err != nil {
			return opts, err
		}
		if err := cli.CheckPartSize(size); err != nil {
			return opts, err
		}
		opts.PartSize = size
	}
	if f.attr != "" {
		meta, err := cli.ParseMetadata(f.attr)
		if err != nil {
			return opts, err
		}
		opts.Metadata = meta
	}
	if f.tags != "" {
		tags, err := cli.ParseTags(f.tags)
		if err != nil {
			return opts, err
		}
		opts.Tags = tags
	}
	for _, l := range []struct {
		flag  string
		value string
		dst   *int64
	}{{"--limit-upload", f.limitUpload, &e.limitUpload}, {"--limit-download", f.limitDownload, &e.limitDownload}} {
		if l.value == "" {
			continue
		}
		rate, err := cli.ParseSize(l.value)
		if err != nil {
			return opts, err
		}
		if rate < 1024 {
			return opts, cli.UsageError("%s must be at least 1KiB per second", l.flag)
		}
		*l.dst = rate
	}
	return opts, nil
}

func newCpCmd(e *env) *cobra.Command {
	return newCopyCmd(e, false)
}

func newMvCmd(e *env) *cobra.Command {
	return newCopyCmd(e, true)
}

// newCopyCmd builds cp, or mv when move is set.
func newCopyCmd(e *env, move bool) *cobra.Command {
	var (
		flags     transferFlags
		recursive bool
		versionID string
	)
	cmd := &cobra.Command{
		Use:   "cp [flags] SOURCE... TARGET",
		Short: "Copy files and objects",
		Long: `Copy local files to a bucket, objects to local files, or objects between
buckets (server-side within one alias). Large files are sent as multipart
uploads whose parts are uploaded in parallel; Ctrl-C aborts the upload on the
server.

A source ending with "/" copies the content of the directory or prefix,
otherwise the directory itself; directories and prefixes need --recursive.`,
		Example: `  nexoctl cp report.pdf local/docs/
  nexoctl cp --recursive photos/ local/photos/2026/
  nexoctl cp local/docs/report.pdf ./
  nexoctl cp --attr "owner=ops;env=prod" --tags "team=ops" app.log local/logs/`,
		Args:              minArgs(2),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.copy(cmd.Context(), args, flags, recursive, versionID, move)
		},
	}
	if move {
		cmd.Use = "mv [flags] SOURCE... TARGET"
		cmd.Short = "Move files and objects"
		cmd.Long = `Move local files and objects: copy (like cp) then remove each source once its
copy succeeded.`
		cmd.Example = `  nexoctl mv local/inbox/report.pdf local/archive/
  nexoctl mv --recursive local/inbox/ ./downloads/`
	}
	f := cmd.Flags()
	f.BoolVarP(&recursive, "recursive", "r", false, "copy directories and prefixes")
	if !move {
		f.StringVar(&versionID, "version-id", "", "copy this version of the source object")
	}
	flags.register(f)
	return cmd
}

func (e *env) copy(ctx context.Context, args []string, flags transferFlags, recursive bool, versionID string, move bool) error {
	opts, err := flags.options(e)
	if err != nil {
		return err
	}
	targets, err := e.targets(ctx, args, true)
	if err != nil {
		return err
	}
	sources, dest := targets[:len(targets)-1], targets[len(targets)-1]
	items, err := cli.PlanCopy(ctx, e.resolver(), sources, dest, cli.PlanOptions{Recursive: recursive, VersionID: versionID})
	if err != nil {
		return err
	}
	var total int64
	for _, it := range items {
		if it.Err == nil {
			total += it.Size
		}
	}
	bar := e.progress(total)
	opts.Progress = bar
	tr := e.transfer()
	action, verb := "copy", "Copied"
	if move {
		action, verb = "move", "Moved"
	}
	var (
		bytes          int64
		copied, failed int
		first          error
	)
	for _, it := range items {
		if it.Err != nil {
			// Never written: reported as a failure of this object only.
			failed++
			if first == nil {
				first = fmt.Errorf("%s: %w", it.Source, it.Err)
			}
			if err := e.reportSkipped(action, it); err != nil {
				bar.Finish()
				return err
			}
			continue
		}
		bar.SetLabel(displayName(it.Source))
		res, err := tr.Copy(ctx, it, opts)
		if err != nil {
			bar.Finish()
			return err
		}
		if move {
			if err := tr.Delete(ctx, it.Source, it.SourceVersion); err != nil {
				bar.Finish()
				return fmt.Errorf("remove %s after the copy: %w", it.Source, err)
			}
		}
		bytes += res.Size
		copied++
		rec := transferRecord{Action: action, Source: it.Source.String(), Target: it.Dest.String(), Size: res.Size, ETag: res.ETag, VersionID: res.VersionID, Parts: res.Parts}
		if bar.Enabled() && !e.out.JSON() {
			continue
		}
		if err := e.out.Info(rec, fmt.Sprintf("`%s` -> `%s` (%s)", it.Source, it.Dest, cli.HumanSize(res.Size))); err != nil {
			return err
		}
	}
	bar.Finish()
	if err := e.out.Note(fmt.Sprintf("%s %d %s (%s).", verb, copied, plural(copied, "object", "objects"), cli.HumanSize(bytes))); err != nil {
		return err
	}
	return batchError(action+" operations", failed, len(items), first)
}

// reportSkipped prints a transfer that was not attempted (CopyItem.Err):
// a warning, or an "error" JSON line on stderr.
func (e *env) reportSkipped(action string, it cli.CopyItem) error {
	if e.out.JSON() {
		rec := transferRecord{Action: action, Source: it.Source.String(), Size: it.Size, Error: it.Err.Error()}
		return e.out.JSONLine(e.out.Stderr(), "error", rec)
	}
	e.out.Warn(fmt.Sprintf("skipping `%s`: %s", it.Source, cli.ErrorMessage(it.Err)))
	return nil
}

func newPipeCmd(e *env) *cobra.Command {
	var flags transferFlags
	cmd := &cobra.Command{
		Use:   "pipe [flags] ALIAS/BUCKET/OBJECT",
		Short: "Stream the standard input to an object",
		Long: `Upload the standard input as an object. The stream is sent as a multipart
upload with parts of growing size, so its length need not be known; Ctrl-C
aborts the upload on the server.`,
		Example:           "  tar czf - photos/ | nexoctl pipe local/backups/photos.tgz",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			opts, err := flags.options(e)
			if err != nil {
				return err
			}
			t, err := e.remoteTarget(ctx, args[0], true)
			if err != nil {
				return err
			}
			if t.Key == "" || t.IsDirLike() {
				return cli.UsageError("%s: pipe needs an object name", args[0])
			}
			bar := e.progress(0)
			bar.SetLabel(path.Base(t.Key))
			opts.Progress = bar
			res, err := e.transfer().Upload(ctx, e.deps.Stdin, -1, t, opts)
			bar.Finish()
			if err != nil {
				return err
			}
			rec := transferRecord{Action: "upload", Source: "-", Target: t.String(), Size: res.Size, ETag: res.ETag, VersionID: res.VersionID, Parts: res.Parts}
			return e.out.Info(rec, fmt.Sprintf("`stdin` -> `%s` (%s)", t, cli.HumanSize(res.Size)))
		},
	}
	flags.register(cmd.Flags())
	return cmd
}

// displayName is the short name of a target shown by the progress bar.
func displayName(t cli.Target) string {
	if t.IsRemote() {
		return path.Base(t.Key)
	}
	return filepath.Base(t.Path)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
