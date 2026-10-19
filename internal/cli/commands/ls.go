package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
)

// lsOptions are the flags of ls.
type lsOptions struct {
	recursive  bool
	versions   bool
	incomplete bool
	// media lists the media sidecars (domain.MediaSidecarPrefix).
	media bool
}

func newLsCmd(e *env) *cobra.Command {
	var opts lsOptions
	cmd := &cobra.Command{
		Use:   "ls [flags] TARGET...",
		Short: "List buckets, objects and object versions",
		Long: `List the buckets of an alias, the objects of a bucket or prefix (one level,
or every level with --recursive), the versions of the objects (--versions), the
incomplete multipart uploads (--incomplete) or a local directory.

The files the media worker derives from the videos (renditions, posters,
thumbnails) live under the prefix ` + domain.MediaSidecarPrefix + ` of a bucket: the listings of a
bucket hide it unless --media is given or the target is under it.`,
		Example: `  nexoctl ls local
  nexoctl ls local/photos/2026/
  nexoctl ls --recursive --versions local/photos
  nexoctl ls --json local/photos`,
		Args:              minArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			targets, err := e.targets(ctx, args, true)
			if err != nil {
				return err
			}
			for _, t := range targets {
				if err := e.ls(ctx, t, opts); err != nil {
					return err
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&opts.recursive, "recursive", "r", false, "list every level under the prefix")
	f.BoolVar(&opts.versions, "versions", false, "list every version and delete marker")
	f.BoolVarP(&opts.incomplete, "incomplete", "I", false, "list the incomplete multipart uploads")
	f.BoolVar(&opts.media, "media", false, "also list the media sidecars ("+domain.MediaSidecarPrefix+")")
	return cmd
}

func (e *env) ls(ctx context.Context, t cli.Target, opts lsOptions) error {
	if !t.IsRemote() {
		return e.lsLocal(t, opts)
	}
	client, err := e.client(ctx, t.Alias)
	if err != nil {
		return err
	}
	if t.Bucket == "" {
		buckets, err := client.ListBuckets(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
		for _, b := range buckets {
			rec := entryRecord{Type: typeBucket, Target: cli.JoinRemote(t.Alias, b.Name, ""), Name: b.Name + "/", LastModified: timePtr(b.Created)}
			if err := e.out.Result(rec, e.lsLine(b.Created, 0, "", e.out.Paint(cli.ColorBlue, b.Name+"/"))); err != nil {
				return err
			}
		}
		return nil
	}
	if opts.incomplete {
		return e.lsUploads(ctx, client, t, opts)
	}
	prefix := t.Key
	exact := prefix != "" && !strings.HasSuffix(prefix, "/")
	if exact {
		// "alias/bucket/name": the object itself, or the prefix "name/".
		found, err := e.lsExact(ctx, client, t, opts)
		if err != nil || found {
			return err
		}
		prefix += "/"
	}
	listed := 0
	for o, err := range client.List(ctx, t.Bucket, cli.ListOptions{Prefix: prefix, Recursive: opts.recursive, Versions: opts.versions}) {
		if err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
		listed++
		if hiddenMediaSidecar(prefix, o.Key, opts.media) {
			continue
		}
		if err := e.lsObject(t, o, strings.TrimPrefix(o.Key, prefix), opts.versions); err != nil {
			return err
		}
	}
	if exact && listed == 0 {
		// Neither an object nor a prefix.
		return fmt.Errorf("%s: %w", t, domain.ErrObjectNotFound)
	}
	return nil
}

// hiddenMediaSidecar reports whether a key listed under prefix is a media
// sidecar to hide: the keys under domain.MediaSidecarPrefix are hidden
// unless show is set or the listing itself is under it.
func hiddenMediaSidecar(prefix, key string, show bool) bool {
	return !show && domain.IsMediaSidecar(key) && !domain.IsMediaSidecar(strings.TrimSuffix(prefix, "/")+"/")
}

// lsExact lists the object named by the key of t (all its versions with
// --versions); found is false when there is none.
func (e *env) lsExact(ctx context.Context, client cli.S3Client, t cli.Target, opts lsOptions) (bool, error) {
	if !opts.versions {
		info, err := client.Stat(ctx, t.Bucket, t.Key, cli.StatOptions{})
		if errors.Is(err, domain.ErrObjectNotFound) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("%s: %w", t, err)
		}
		return true, e.lsObject(t, *info, path.Base(t.Key), false)
	}
	found := false
	for o, err := range client.List(ctx, t.Bucket, cli.ListOptions{Prefix: t.Key, Versions: true}) {
		if err != nil {
			return false, fmt.Errorf("%s: %w", t, err)
		}
		if o.Key != t.Key {
			continue
		}
		found = true
		if err := e.lsObject(t, o, path.Base(o.Key), true); err != nil {
			return true, err
		}
	}
	return found, nil
}

func (e *env) lsObject(t cli.Target, o cli.ObjectInfo, name string, versions bool) error {
	rec := objectEntry(t, o, name, versions)
	if o.Prefix {
		return e.out.Result(rec, e.lsPrefixLine(name))
	}
	display := name
	if versions {
		kind := "PUT"
		if o.DeleteMarker {
			kind = "DEL"
		}
		latest := ""
		if o.IsLatest {
			latest = " (latest)"
		}
		version := o.VersionID
		if version == "" {
			version = "null"
		}
		return e.out.Result(rec, e.lsLine(o.LastModified, o.Size, "", fmt.Sprintf("%s %s %s%s", version, kind, display, latest)))
	}
	return e.out.Result(rec, e.lsLine(o.LastModified, o.Size, o.StorageClass, display))
}

func (e *env) lsUploads(ctx context.Context, client cli.S3Client, t cli.Target, _ lsOptions) error {
	for u, err := range client.ListUploads(ctx, t.Bucket, t.Key) {
		if err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
		rec := entryRecord{Type: typeUpload, Target: t.Child(u.Key).String(), Key: u.Key, Name: strings.TrimPrefix(u.Key, t.Key), LastModified: timePtr(u.Initiated), UploadID: u.UploadID}
		if err := e.out.Result(rec, fmt.Sprintf("[%s] %s %s", cli.FormatTime(u.Initiated), u.UploadID, u.Key)); err != nil {
			return err
		}
	}
	return nil
}

// lsLocal lists a local file or directory.
func (e *env) lsLocal(t cli.Target, opts lsOptions) error {
	st, err := os.Stat(t.Path)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		rec := entryRecord{Type: typeFile, Target: t.Path, Name: filepath.Base(t.Path), Size: st.Size(), LastModified: timePtr(st.ModTime())}
		return e.out.Result(rec, e.lsLine(st.ModTime(), st.Size(), "", filepath.Base(t.Path)))
	}
	if opts.recursive {
		for en, err := range cli.Walk(context.Background(), nil, t) {
			if err != nil {
				return err
			}
			p := filepath.Join(t.Path, filepath.FromSlash(en.Rel))
			rec := entryRecord{Type: typeFile, Target: p, Name: en.Rel, Size: en.Size, LastModified: timePtr(en.ModTime)}
			if err := e.out.Result(rec, e.lsLine(en.ModTime, en.Size, "", en.Rel)); err != nil {
				return err
			}
		}
		return nil
	}
	entries, err := os.ReadDir(t.Path)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, d := range entries {
		info, err := d.Info()
		if err != nil {
			return err
		}
		p := filepath.Join(t.Path, d.Name())
		if d.IsDir() {
			rec := entryRecord{Type: typeFolder, Target: p, Name: d.Name() + "/"}
			if err := e.out.Result(rec, e.lsPrefixLine(d.Name()+"/")); err != nil {
				return err
			}
			continue
		}
		rec := entryRecord{Type: typeFile, Target: p, Name: d.Name(), Size: info.Size(), LastModified: timePtr(info.ModTime())}
		if err := e.out.Result(rec, e.lsLine(info.ModTime(), info.Size(), "", d.Name())); err != nil {
			return err
		}
	}
	return nil
}

// lsLine renders "[date]   size CLASS name".
func (e *env) lsLine(t time.Time, size int64, class, name string) string {
	s := fmt.Sprintf("[%s] %7s ", cli.FormatTime(t), cli.HumanSize(size))
	if class != "" {
		s += class + " "
	}
	return s + name
}

// lsPrefixLine renders a folder, aligned with lsLine ("PRE" in place of the
// size).
func (e *env) lsPrefixLine(name string) string {
	return fmt.Sprintf("%25s %7s %s", "", "PRE", e.out.Paint(cli.ColorBlue, name))
}
