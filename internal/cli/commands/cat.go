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
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
)

func newCatCmd(e *env) *cobra.Command {
	var (
		versionID string
		enc       encryptionFlags
	)
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
			keys, err := enc.rules(ctx, e)
			if err != nil {
				return err
			}
			for _, t := range targets {
				r, err := e.open(ctx, t, versionID, keys)
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
	enc.registerSSEC(cmd.Flags())
	return cmd
}

func newHeadCmd(e *env) *cobra.Command {
	var (
		versionID string
		lines     int
		enc       encryptionFlags
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
			keys, err := enc.rules(ctx, e)
			if err != nil {
				return err
			}
			for _, t := range targets {
				r, err := e.open(ctx, t, versionID, keys)
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
	enc.registerSSEC(cmd.Flags())
	return cmd
}

// open opens a remote object (with its SSE-C key, if any) or a local file.
func (e *env) open(ctx context.Context, t cli.Target, versionID string, keys cli.EncryptionRules) (io.ReadCloser, error) {
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
	sse := keys.CustomerKeyFor(t)
	body, _, err := client.Get(ctx, t.Bucket, t.Key, cli.GetOptions{VersionID: versionID, SSE: sse})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t, sseHint(err, sse))
	}
	return body, nil
}

// sseHint adds a hint to the refusal of a read without SSE-C key: the
// server answers 400 for an object encrypted with SSE-C.
func sseHint(err error, sse domain.SSERequest) error {
	if sse.Type == domain.SSEC || !errors.Is(err, domain.ErrInvalidArgument) {
		return err
	}
	return fmt.Errorf("%w (an object encrypted with SSE-C is read with its key: --enc-c ALIAS/BUCKET[/PREFIX]=KEY)", err)
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

// statOptions are the flags of stat.
type statOptions struct {
	versionID string
	versions  bool
	checksum  bool
	keys      cli.EncryptionRules
}

func newStatCmd(e *env) *cobra.Command {
	var (
		opts statOptions
		enc  encryptionFlags
	)
	cmd := &cobra.Command{
		Use:   "stat [flags] TARGET...",
		Short: "Show the metadata of objects, buckets and files",
		Long: `Show the metadata of objects (size, ETag, content type, user metadata, tags,
version, Object Lock retention and legal hold, server-side encryption, and with
--checksum the additional checksum), of buckets (versioning, Object Lock,
anonymous access, tags) and of local files. --versions shows every version of
an object.`,
		Example: `  nexoctl stat local/docs/report.pdf
  nexoctl stat --checksum --versions local/docs/report.pdf
  nexoctl stat --enc-c "local/private=$(cat key.b64)" local/private/secret.txt
  nexoctl stat --json local/docs`,
		Args:              minArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if opts.versionID != "" && len(args) != 1 {
				return cli.UsageError("--version-id needs exactly one object")
			}
			if opts.versionID != "" && opts.versions {
				return cli.UsageError("--version-id excludes --versions")
			}
			targets, err := e.targets(ctx, args, true)
			if err != nil {
				return err
			}
			if opts.keys, err = enc.rules(ctx, e); err != nil {
				return err
			}
			for i, t := range targets {
				if i > 0 && !e.out.JSON() {
					if err := e.out.Result(nil, ""); err != nil {
						return err
					}
				}
				if err := e.stat(ctx, t, opts); err != nil {
					return err
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.versionID, "version-id", "", "show this version of the object")
	f.BoolVar(&opts.versions, "versions", false, "show every version of the object")
	f.BoolVar(&opts.checksum, "checksum", false, "show the additional checksum of the object")
	enc.registerSSEC(f)
	return cmd
}

func (e *env) stat(ctx context.Context, t cli.Target, opts statOptions) error {
	if !t.IsRemote() {
		st, err := os.Stat(t.Path)
		if err != nil {
			return err
		}
		rec := entryRecord{Type: typeFile, Target: t.Path, Name: filepath.Base(t.Path), Size: st.Size(), LastModified: timePtr(st.ModTime())}
		if st.IsDir() {
			rec.Type = typeFolder
		}
		return e.out.Result(rec, statText(rec, e.deps.Clock.Now()))
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
	if opts.versions {
		return e.statVersions(ctx, client, t, opts)
	}
	return e.statObject(ctx, client, t, opts.versionID, opts, true)
}

// statObject prints one version of an object (the latest for an empty
// versionID); a missing object that is a prefix is printed as a folder
// when folders is set.
func (e *env) statObject(ctx context.Context, client cli.S3Client, t cli.Target, versionID string, opts statOptions, folders bool) error {
	sse := opts.keys.CustomerKeyFor(t)
	info, err := client.Stat(ctx, t.Bucket, t.Key, cli.StatOptions{VersionID: versionID, Checksum: opts.checksum, SSE: sse})
	if err != nil {
		if folders && errors.Is(err, domain.ErrObjectNotFound) && versionID == "" {
			// A prefix ("folder") has no metadata of its own.
			if found, _ := hasObjectsUnder(ctx, client, t.Bucket, cli.DirPrefix(t.Key)); found {
				rec := entryRecord{Type: typeFolder, Target: t.String(), Key: cli.DirPrefix(t.Key), Name: path.Base(t.Key) + "/"}
				return e.out.Result(rec, statText(rec, e.deps.Clock.Now()))
			}
		}
		return fmt.Errorf("%s: %w", t, sseHint(err, sse))
	}
	tags, err := client.GetTags(ctx, cli.ObjectRef{Bucket: t.Bucket, Key: t.Key, VersionID: versionID})
	// The tags are informative: a server that refuses to read them
	// without the SSE-C key of the object does not fail stat.
	sseCTags := info.Encryption.Type == domain.SSEC && errors.Is(err, domain.ErrInvalidArgument)
	if err != nil && !errors.Is(err, domain.ErrNotImplemented) && !sseCTags {
		return fmt.Errorf("%s: tags: %w", t, err)
	}
	rec := objectEntry(t.Child(""), *info, path.Base(t.Key), false)
	rec.Target = t.String()
	rec.Key = t.Key
	if versionID != "" {
		rec.VersionID = versionID
	}
	if len(tags) > 0 {
		rec.Tags = tags
	}
	rec.Retention, rec.LegalHold = retentionOf(info.Retention), string(info.LegalHold)
	rec.Checksum, rec.Encryption = checksumOf(info.Checksum), encryptionOf(info.Encryption)
	return e.out.Result(rec, statText(rec, e.deps.Clock.Now()))
}

// statVersions prints every version of an object, the latest first; the
// delete markers are printed as such.
func (e *env) statVersions(ctx context.Context, client cli.S3Client, t cli.Target, opts statOptions) error {
	found := 0
	for o, err := range client.List(ctx, t.Bucket, cli.ListOptions{Prefix: t.Key, Recursive: true, Versions: true}) {
		if err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
		if o.Key != t.Key {
			continue
		}
		if found > 0 && !e.out.JSON() {
			if err := e.out.Result(nil, ""); err != nil {
				return err
			}
		}
		found++
		if o.DeleteMarker {
			rec := objectEntry(t.Child(""), o, path.Base(t.Key), true)
			rec.Target, rec.Key = t.String(), t.Key
			if err := e.out.Result(rec, statText(rec, e.deps.Clock.Now())); err != nil {
				return err
			}
			continue
		}
		if err := e.statObject(ctx, client, t, o.VersionID, opts, false); err != nil {
			return err
		}
	}
	if found == 0 {
		return fmt.Errorf("%s: %w", t, domain.ErrObjectNotFound)
	}
	return nil
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
	// Object Lock is reported when enabled; an endpoint without it (or
	// that refuses to tell) is not an error of stat.
	if lock, err := client.GetObjectLock(ctx, t.Bucket); err == nil && lock.Enabled {
		rec.ObjectLock = objectLockOf(lock)
	}
	return e.out.Result(rec, statText(rec, e.deps.Clock.Now()))
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
func statText(r entryRecord, now time.Time) string {
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
	if r.Type == typeFile && !r.DeleteMarker {
		row("Size", fmt.Sprintf("%s (%d bytes)", cli.HumanSize(r.Size), r.Size))
	}
	row("Type", r.Type)
	row("ETag", r.ETag)
	row("Content-Type", r.ContentType)
	row("Class", r.StorageClass)
	row("VersionID", r.VersionID)
	if r.DeleteMarker {
		row("Deleted", "yes (delete marker)")
	}
	row("Versioning", r.Versioning)
	if l := r.ObjectLock; l != nil {
		lock := "Enabled"
		if l.Mode != "" {
			lock += fmt.Sprintf(", default %s for %s", l.Mode, l.Validity)
		}
		row("Object Lock", lock)
	}
	row("Anonymous", r.Anonymous)
	if r.Retention != nil {
		ret := domain.ObjectRetention{Mode: domain.RetentionMode(r.Retention.Mode)}
		if r.Retention.RetainUntil != nil {
			ret.RetainUntil = *r.Retention.RetainUntil
		}
		row("Retention", lockState(ret, "", now))
	}
	if r.LegalHold != "" {
		row("Legal hold", r.LegalHold)
	}
	if c := r.Checksum; c != nil {
		sum := c.Algorithm + " " + c.Value
		if c.Type != "" {
			sum += " (" + c.Type + ")"
		}
		row("Checksum", sum)
	}
	if enc := r.Encryption; enc != nil {
		row("Encryption", cli.EncryptionName(domain.EncryptionInfo{Type: domain.SSEType(enc.Type), KMSKeyID: enc.KMSKeyID}))
	}
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
