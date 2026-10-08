package commands

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
)

func newCatCmd(e *env) *cobra.Command {
	var versionID string
	cmd := &cobra.Command{
		Use:               "cat [flags] TARGET...",
		Short:             "Print the content of objects and files",
		Example:           "  nexoctl cat local/logs/app.log",
		Args:              minArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if versionID != "" && len(args) != 1 {
				return cli.UsageError("--version-id needs exactly one object")
			}
			targets, err := e.targets(ctx, args, true)
			if err != nil {
				return err
			}
			for _, t := range targets {
				r, err := e.open(ctx, t, versionID)
				if err != nil {
					return err
				}
				err = e.writeContent(t, r, -1)
				_ = r.Close()
				if err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&versionID, "version-id", "", "print this version of the object")
	return cmd
}

func newHeadCmd(e *env) *cobra.Command {
	var (
		versionID string
		lines     int
	)
	cmd := &cobra.Command{
		Use:               "head [flags] TARGET...",
		Short:             "Print the first lines of objects and files",
		Example:           "  nexoctl head -n 5 local/logs/app.log",
		Args:              minArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if lines < 0 {
				return cli.UsageError("-n must not be negative")
			}
			if versionID != "" && len(args) != 1 {
				return cli.UsageError("--version-id needs exactly one object")
			}
			targets, err := e.targets(ctx, args, true)
			if err != nil {
				return err
			}
			for _, t := range targets {
				r, err := e.open(ctx, t, versionID)
				if err != nil {
					return err
				}
				err = e.writeContent(t, r, lines)
				_ = r.Close()
				if err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&lines, "lines", "n", 10, "number of lines")
	cmd.Flags().StringVar(&versionID, "version-id", "", "print this version of the object")
	return cmd
}

// open opens a remote object or a local file.
func (e *env) open(ctx context.Context, t cli.Target, versionID string) (io.ReadCloser, error) {
	if !t.IsRemote() {
		if versionID != "" {
			return nil, cli.UsageError("--version-id applies to remote objects")
		}
		return os.Open(t.Path)
	}
	if t.Bucket == "" || t.Key == "" || strings.HasSuffix(t.Key, "/") {
		return nil, cli.UsageError("%s is not an object", t)
	}
	client, err := e.client(ctx, t.Alias)
	if err != nil {
		return nil, err
	}
	body, _, err := client.Get(ctx, t.Bucket, t.Key, cli.GetOptions{VersionID: versionID})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t, err)
	}
	return body, nil
}

// writeContent copies r to stdout: all of it (lines < 0) or the first
// lines. With --json the content is one record (base64 when it is not
// UTF-8).
func (e *env) writeContent(t cli.Target, r io.Reader, lines int) error {
	if lines >= 0 {
		r = firstLines(r, lines)
	}
	if !e.out.JSON() {
		_, err := io.Copy(e.out.Stdout(), r)
		return err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	rec := contentRecord{Target: t.String(), Size: int64(len(data))}
	if utf8.Valid(data) {
		rec.Content = string(data)
	} else {
		rec.ContentBase64 = base64.StdEncoding.EncodeToString(data)
	}
	return e.out.Result(rec, "")
}

// firstLines returns a reader over the first n lines of r.
func firstLines(r io.Reader, n int) io.Reader {
	return &lineLimitReader{br: bufio.NewReader(r), left: n}
}

// lineLimitReader reads up to left lines.
type lineLimitReader struct {
	br   *bufio.Reader
	left int
	buf  []byte
}

func (l *lineLimitReader) Read(p []byte) (int, error) {
	for len(l.buf) == 0 {
		if l.left <= 0 {
			return 0, io.EOF
		}
		line, err := l.br.ReadBytes('\n')
		l.left--
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return 0, err
			}
			l.left = 0
			if len(line) == 0 {
				return 0, io.EOF
			}
		}
		l.buf = line
	}
	n := copy(p, l.buf)
	l.buf = l.buf[n:]
	return n, nil
}

func newStatCmd(e *env) *cobra.Command {
	var versionID string
	cmd := &cobra.Command{
		Use:   "stat [flags] TARGET...",
		Short: "Show the metadata of objects, buckets and files",
		Example: `  nexoctl stat local/docs/report.pdf
  nexoctl stat --json local/docs`,
		Args:              minArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if versionID != "" && len(args) != 1 {
				return cli.UsageError("--version-id needs exactly one object")
			}
			targets, err := e.targets(ctx, args, true)
			if err != nil {
				return err
			}
			for i, t := range targets {
				if i > 0 && !e.out.JSON() {
					if err := e.out.Result(nil, ""); err != nil {
						return err
					}
				}
				if err := e.stat(ctx, t, versionID); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&versionID, "version-id", "", "show this version of the object")
	return cmd
}

func (e *env) stat(ctx context.Context, t cli.Target, versionID string) error {
	if !t.IsRemote() {
		st, err := os.Stat(t.Path)
		if err != nil {
			return err
		}
		rec := entryRecord{Type: typeFile, Target: t.Path, Name: filepath.Base(t.Path), Size: st.Size(), LastModified: timePtr(st.ModTime())}
		if st.IsDir() {
			rec.Type = typeFolder
		}
		return e.out.Result(rec, statText(rec))
	}
	if t.Bucket == "" {
		return cli.UsageError("%s: stat needs a bucket or an object", t)
	}
	client, err := e.client(ctx, t.Alias)
	if err != nil {
		return err
	}
	if t.Key == "" {
		return e.statBucket(ctx, client, t)
	}
	info, err := client.Stat(ctx, t.Bucket, t.Key, cli.StatOptions{VersionID: versionID})
	if err != nil {
		if errors.Is(err, domain.ErrObjectNotFound) && versionID == "" {
			// A prefix ("folder") has no metadata of its own.
			if found, _ := hasObjectsUnder(ctx, client, t.Bucket, cli.DirPrefix(t.Key)); found {
				rec := entryRecord{Type: typeFolder, Target: t.String(), Key: cli.DirPrefix(t.Key), Name: path.Base(t.Key) + "/"}
				return e.out.Result(rec, statText(rec))
			}
		}
		return fmt.Errorf("%s: %w", t, err)
	}
	tags, err := client.GetTags(ctx, cli.ObjectRef{Bucket: t.Bucket, Key: t.Key, VersionID: versionID})
	if err != nil && !errors.Is(err, domain.ErrNotImplemented) {
		return fmt.Errorf("%s: tags: %w", t, err)
	}
	rec := objectEntry(t.Child(""), *info, path.Base(t.Key), false)
	rec.Target = t.String()
	rec.Key = t.Key
	if len(tags) > 0 {
		rec.Tags = tags
	}
	return e.out.Result(rec, statText(rec))
}

func (e *env) statBucket(ctx context.Context, client cli.S3Client, t cli.Target) error {
	exists, err := client.BucketExists(ctx, t.Bucket)
	if err != nil {
		return fmt.Errorf("%s: %w", t, err)
	}
	if !exists {
		return fmt.Errorf("%s: %w", t, domain.ErrBucketNotFound)
	}
	rec := entryRecord{Type: typeBucket, Target: t.String(), Name: t.Bucket}
	for _, b := range e.bucketsOf(ctx, client) {
		if b.Name == t.Bucket {
			rec.LastModified = timePtr(b.Created)
		}
	}
	v, err := client.GetVersioning(ctx, t.Bucket)
	if err != nil {
		return fmt.Errorf("%s: versioning: %w", t, err)
	}
	rec.Versioning = string(v)
	if rec.Versioning == "" {
		rec.Versioning = "Unversioned"
	}
	access, err := e.anonymousAccess(ctx, client, t.Bucket, "")
	if err != nil {
		return fmt.Errorf("%s: %w", t, err)
	}
	rec.Anonymous = string(access)
	tags, err := client.GetTags(ctx, cli.ObjectRef{Bucket: t.Bucket})
	if err != nil && !errors.Is(err, domain.ErrNotImplemented) {
		return fmt.Errorf("%s: tags: %w", t, err)
	}
	if len(tags) > 0 {
		rec.Tags = tags
	}
	return e.out.Result(rec, statText(rec))
}

// hasObjectsUnder reports whether a prefix holds at least one object.
func hasObjectsUnder(ctx context.Context, client cli.S3Client, bucket, prefix string) (bool, error) {
	for _, err := range client.List(ctx, bucket, cli.ListOptions{Prefix: prefix}) {
		return err == nil, err
	}
	return false, nil
}

// bucketsOf lists the buckets, ignoring failures (the creation date is
// informative).
func (e *env) bucketsOf(ctx context.Context, client cli.S3Client) []cli.BucketInfo {
	buckets, err := client.ListBuckets(ctx)
	if err != nil {
		return nil
	}
	return buckets
}

// statText renders the human-readable form of stat.
func statText(r entryRecord) string {
	var b bytes.Buffer
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%-12s: %s\n", k, v)
		}
	}
	row("Name", r.Name)
	if r.LastModified != nil {
		row("Date", cli.FormatTime(*r.LastModified))
	}
	if r.Type == typeFile {
		row("Size", fmt.Sprintf("%s (%d bytes)", cli.HumanSize(r.Size), r.Size))
	}
	row("Type", r.Type)
	row("ETag", r.ETag)
	row("Content-Type", r.ContentType)
	row("Class", r.StorageClass)
	row("VersionID", r.VersionID)
	row("Versioning", r.Versioning)
	row("Anonymous", r.Anonymous)
	if len(r.Metadata) > 0 {
		fmt.Fprintf(&b, "%-12s:\n", "Metadata")
		for _, k := range cli.SortedKeys(r.Metadata) {
			fmt.Fprintf(&b, "  %s: %s\n", k, r.Metadata[k])
		}
	}
	if len(r.Tags) > 0 {
		row("Tags", cli.FormatTags(r.Tags))
	}
	return strings.TrimSuffix(b.String(), "\n")
}
