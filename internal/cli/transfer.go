package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// CopyItem is one transfer of cp, mv or mirror: a local file or a remote
// object copied to a local path or a remote key.
type CopyItem struct {
	Source Target
	// SourceVersion selects a version of a remote source.
	SourceVersion string
	Dest          Target
	// Size, ModTime and ETag describe the source (from the listing).
	Size    int64
	ModTime time.Time
	ETag    string
	// Err, when set, is why the item must not be transferred (an object
	// name that would escape the local destination, ErrUnsafeName): Dest
	// is unset and the caller reports the item as failed without writing
	// anything.
	Err error
}

// PlanOptions tune PlanCopy.
type PlanOptions struct {
	// Recursive copies directories and prefixes (-r).
	Recursive bool
	// VersionID selects the version of a single remote source.
	VersionID string
	// Encryption carries the SSE-C keys needed to read the remote sources
	// (--enc-c).
	Encryption EncryptionRules
}

// plannedSource is a source expanded by PlanCopy: rel is the path of the
// item under the destination directory.
type plannedSource struct {
	item   CopyItem
	rel    string
	single bool
}

// PlanCopy expands the sources of cp and mv into transfers (docs/02
// C-OBJ-03). A source ending with a separator copies the content of the
// directory or prefix, otherwise the directory itself (like rsync and mc);
// directories and prefixes need opts.Recursive. The destination is a
// directory when it ends with a separator, is a bucket, is an existing
// local directory, or receives several items. An item whose name cannot
// be written safely under a local destination directory is returned with
// Err set (see CopyItem.Err).
func PlanCopy(ctx context.Context, resolve ClientResolver, sources []Target, dest Target, opts PlanOptions) ([]CopyItem, error) {
	if len(sources) == 0 {
		return nil, UsageError("no source")
	}
	if dest.IsRemote() && dest.Bucket == "" {
		return nil, UsageError("%s: the destination needs a bucket", dest)
	}
	if opts.VersionID != "" && (len(sources) != 1 || opts.Recursive || !sources[0].IsRemote()) {
		return nil, UsageError("--version-id needs exactly one remote object and no --recursive")
	}
	var planned []plannedSource
	for _, src := range sources {
		var (
			items []plannedSource
			err   error
		)
		if src.IsRemote() {
			items, err = planRemote(ctx, resolve, src, opts)
		} else {
			items, err = planLocal(src, opts)
		}
		if err != nil {
			return nil, err
		}
		planned = append(planned, items...)
	}
	destDir := dest.IsDirLike() || len(sources) > 1 || len(planned) > 1
	if !dest.IsRemote() && isLocalDir(dest.Path) {
		destDir = true
	}
	for _, p := range planned {
		if !p.single {
			destDir = true
		}
	}
	out := make([]CopyItem, 0, len(planned))
	for _, p := range planned {
		item := p.item
		if destDir {
			joined, err := JoinTarget(dest, p.rel)
			if err != nil {
				item.Err = err
				out = append(out, item)
				continue
			}
			item.Dest = joined
		} else {
			item.Dest = dest
		}
		if item.Dest.IsRemote() && (item.Dest.Key == "" || strings.HasSuffix(item.Dest.Key, "/")) {
			return nil, UsageError("%s: invalid destination object name", item.Dest)
		}
		out = append(out, item)
	}
	return out, nil
}

// planLocal expands a local source.
func planLocal(src Target, opts PlanOptions) ([]plannedSource, error) {
	st, err := os.Stat(src.Path)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: %s is not a regular file", domain.ErrInvalidArgument, src.Path)
		}
		return []plannedSource{{
			item:   CopyItem{Source: src, Size: st.Size(), ModTime: st.ModTime()},
			rel:    filepath.Base(src.Path),
			single: true,
		}}, nil
	}
	if !opts.Recursive {
		return nil, UsageError("%s is a directory: use --recursive", src.Path)
	}
	base := ""
	if !contentsOf(src.Raw) {
		base = filepath.Base(filepath.Clean(src.Path)) + "/"
	}
	var out []plannedSource
	for e, err := range walkLocal(src.Path) {
		if err != nil {
			return nil, err
		}
		file := filepath.Join(src.Path, filepath.FromSlash(e.Rel))
		out = append(out, plannedSource{
			item: CopyItem{Source: Target{Raw: file, Path: file}, Size: e.Size, ModTime: e.ModTime},
			rel:  base + e.Rel,
		})
	}
	return out, nil
}

// contentsOf reports whether a source argument designates the content of a
// directory rather than the directory itself.
func contentsOf(raw string) bool {
	if strings.HasSuffix(raw, "/") || strings.HasSuffix(raw, string(filepath.Separator)) {
		return true
	}
	switch filepath.Base(raw) {
	case ".", "..", string(filepath.Separator):
		return true
	}
	return raw == "~"
}

// planRemote expands a remote source.
func planRemote(ctx context.Context, resolve ClientResolver, src Target, opts PlanOptions) ([]plannedSource, error) {
	if src.Bucket == "" {
		return nil, UsageError("%s: the source needs a bucket", src)
	}
	client, err := resolve(ctx, src.Alias)
	if err != nil {
		return nil, err
	}
	isPrefix := src.Key == "" || strings.HasSuffix(src.Key, "/")
	if !opts.Recursive {
		if isPrefix {
			return nil, UsageError("%s is a bucket or a prefix: use --recursive", src)
		}
		info, err := StatObject(ctx, client, src.Bucket, src.Key, StatOptions{VersionID: opts.VersionID, SSE: opts.Encryption.CustomerKeyFor(src)})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src, err)
		}
		return []plannedSource{{
			item:   CopyItem{Source: src, SourceVersion: opts.VersionID, Size: info.Size, ModTime: info.LastModified, ETag: info.ETag},
			rel:    path.Base(src.Key),
			single: true,
		}}, nil
	}
	base := ""
	if !strings.HasSuffix(src.Raw, "/") {
		if src.Key == "" {
			base = src.Bucket + "/"
		} else {
			base = path.Base(src.Key) + "/"
		}
	}
	prefix := DirPrefix(src.Key)
	var out []plannedSource
	for e, err := range Walk(ctx, resolve, src) {
		if err != nil {
			return nil, err
		}
		out = append(out, plannedSource{
			item: CopyItem{Source: src.Child(prefix + e.Rel), Size: e.Size, ModTime: e.ModTime, ETag: e.ETag},
			rel:  base + e.Rel,
		})
	}
	if !isPrefix {
		// "alias/bucket/name" also designates the object "name" itself.
		info, err := client.Stat(ctx, src.Bucket, src.Key, StatOptions{SSE: opts.Encryption.CustomerKeyFor(src)})
		switch {
		case err == nil:
			out = append([]plannedSource{{
				item:   CopyItem{Source: src, Size: info.Size, ModTime: info.LastModified, ETag: info.ETag},
				rel:    path.Base(src.Key),
				single: true,
			}}, out...)
		case !errors.Is(err, domain.ErrObjectNotFound):
			return nil, fmt.Errorf("%s: %w", src, err)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: %w", src, domain.ErrObjectNotFound)
	}
	return out, nil
}

// TransferOptions tune Transfer.Copy and Transfer.Upload.
type TransferOptions struct {
	// ContentType of the uploaded objects ("" guesses it from the
	// extension of local files and keeps the one of remote sources).
	ContentType string
	// Metadata, when not nil, sets (replaces) the user metadata.
	Metadata map[string]string
	// Tags, when not nil, sets the tags.
	Tags map[string]string
	// PartSize and Parallel tune the multipart uploads.
	PartSize int64
	Parallel int
	// Progress receives the transferred bytes (nil: none).
	Progress *Progress
	// Encryption selects the server-side encryption of the uploaded
	// objects and the SSE-C keys of the remote sources (--enc-s3,
	// --enc-kms, --enc-c).
	Encryption EncryptionRules
	// Checksum is the additional checksum of the uploaded objects
	// (--checksum).
	Checksum domain.ChecksumAlgorithm
}

// Transfer runs the transfers of cp, mv, mirror and pipe between local
// files and remote objects, with the S3 clients of Resolve.
type Transfer struct {
	Resolve ClientResolver
}

// Copy transfers one item: an upload, a download, a server-side copy
// (same alias), a stream between two aliases or a local copy. Downloads
// are written to a temporary file renamed into place, with the
// modification time of the object.
func (t Transfer) Copy(ctx context.Context, item CopyItem, opts TransferOptions) (*PutResult, error) {
	src, dst := item.Source, item.Dest
	switch {
	case src.IsRemote() && dst.IsRemote():
		return t.copyRemote(ctx, item, opts)
	case src.IsRemote():
		return t.download(ctx, item, opts)
	case dst.IsRemote():
		f, err := os.Open(src.Path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		st, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if opts.ContentType == "" {
			opts.ContentType = GuessContentType(src.Path)
		}
		return t.Upload(ctx, f, st.Size(), dst, opts)
	default:
		return t.copyLocal(item, opts)
	}
}

// Upload stores body (size bytes, -1 when unknown) as a remote object.
func (t Transfer) Upload(ctx context.Context, body io.Reader, size int64, dst Target, opts TransferOptions) (*PutResult, error) {
	client, err := t.Resolve(ctx, dst.Alias)
	if err != nil {
		return nil, err
	}
	res, err := client.Put(ctx, dst.Bucket, dst.Key, body, PutOptions{
		Size: size, ContentType: opts.ContentType, Metadata: opts.Metadata, Tags: opts.Tags,
		PartSize: opts.PartSize, Parallel: opts.Parallel, Progress: progressFunc(opts.Progress),
		SSE: opts.Encryption.For(dst), Checksum: opts.Checksum,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dst, err)
	}
	return res, nil
}

// copyRemote copies between two remote targets: server-side within one
// alias, streamed otherwise.
func (t Transfer) copyRemote(ctx context.Context, item CopyItem, opts TransferOptions) (*PutResult, error) {
	src, dst := item.Source, item.Dest
	srcClient, err := t.Resolve(ctx, src.Alias)
	if err != nil {
		return nil, err
	}
	if src.Alias == dst.Alias {
		res, err := srcClient.Copy(ctx,
			ObjectRef{Bucket: src.Bucket, Key: src.Key, VersionID: item.SourceVersion},
			ObjectRef{Bucket: dst.Bucket, Key: dst.Key},
			CopyOptions{
				Metadata: opts.Metadata, ContentType: opts.ContentType, Tags: opts.Tags,
				PartSize: opts.PartSize, Parallel: opts.Parallel, Progress: progressFunc(opts.Progress),
				SourceSSE: opts.Encryption.CustomerKeyFor(src), SSE: opts.Encryption.For(dst), Checksum: opts.Checksum,
			})
		if err != nil {
			return nil, fmt.Errorf("%s -> %s: %w", src, dst, err)
		}
		return res, nil
	}
	body, info, err := srcClient.Get(ctx, src.Bucket, src.Key, GetOptions{VersionID: item.SourceVersion, SSE: opts.Encryption.CustomerKeyFor(src)})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	defer func() { _ = body.Close() }()
	if opts.ContentType == "" {
		opts.ContentType = info.ContentType
	}
	if opts.Metadata == nil {
		opts.Metadata = info.Metadata
	}
	return t.Upload(ctx, body, info.Size, dst, opts)
}

// download writes a remote object to a local file.
func (t Transfer) download(ctx context.Context, item CopyItem, opts TransferOptions) (*PutResult, error) {
	src, dst := item.Source, item.Dest
	client, err := t.Resolve(ctx, src.Alias)
	if err != nil {
		return nil, err
	}
	body, info, err := client.Get(ctx, src.Bucket, src.Key, GetOptions{VersionID: item.SourceVersion, SSE: opts.Encryption.CustomerKeyFor(src)})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	defer func() { _ = body.Close() }()
	mod := item.ModTime
	if mod.IsZero() {
		mod = info.LastModified
	}
	n, err := writeFileAtomic(dst.Path, body, mod, opts.Progress)
	if err != nil {
		return nil, err
	}
	return &PutResult{ETag: info.ETag, VersionID: info.VersionID, Size: n}, nil
}

// copyLocal copies a local file.
func (t Transfer) copyLocal(item CopyItem, opts TransferOptions) (*PutResult, error) {
	f, err := os.Open(item.Source.Path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if same, err := sameFile(st, item.Dest.Path); err == nil && same {
		return nil, fmt.Errorf("%w: %s and %s are the same file", domain.ErrInvalidArgument, item.Source.Path, item.Dest.Path)
	}
	n, err := writeFileAtomic(item.Dest.Path, f, st.ModTime(), opts.Progress)
	if err != nil {
		return nil, err
	}
	return &PutResult{Size: n}, nil
}

func sameFile(src fs.FileInfo, dst string) (bool, error) {
	st, err := os.Stat(dst)
	if err != nil {
		return false, err
	}
	return os.SameFile(src, st), nil
}

// Delete removes a local file or a remote object (version).
func (t Transfer) Delete(ctx context.Context, target Target, versionID string) error {
	if !target.IsRemote() {
		if err := os.Remove(target.Path); err != nil {
			return err
		}
		return nil
	}
	client, err := t.Resolve(ctx, target.Alias)
	if err != nil {
		return err
	}
	res, err := client.Remove(ctx, target.Bucket, []ObjectVersion{{Key: target.Key, VersionID: versionID}}, DeleteOptions{})
	if err != nil {
		return fmt.Errorf("%s: %w", target, err)
	}
	for _, r := range res {
		if r.Err != nil {
			return fmt.Errorf("%s: %w", target, r.Err)
		}
	}
	return nil
}

// writeFileAtomic writes r to path through a temporary file in the same
// directory, sets its modification time and renames it into place.
func writeFileAtomic(dst string, r io.Reader, mod time.Time, progress *Progress) (int64, error) {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- directories of the user's downloads, subject to the umask like cp
		return 0, fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".nexoctl-*.part")
	if err != nil {
		return 0, fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	name := tmp.Name()
	fail := func(err error) (int64, error) {
		_ = tmp.Close()
		_ = os.Remove(name)
		return 0, err
	}
	var w io.Writer = tmp
	if progress != nil {
		w = progressWriter{w: tmp, p: progress}
	}
	n, err := io.Copy(w, r)
	if err != nil {
		return fail(fmt.Errorf("write %s: %w", dst, err))
	}
	if err := tmp.Close(); err != nil {
		return fail(fmt.Errorf("write %s: %w", dst, err))
	}
	if !mod.IsZero() {
		if err := os.Chtimes(name, mod, mod); err != nil {
			return fail(fmt.Errorf("set times of %s: %w", dst, err))
		}
	}
	if err := os.Rename(name, dst); err != nil {
		return fail(fmt.Errorf("rename into %s: %w", dst, err))
	}
	return n, nil
}

// progressWriter reports the bytes written to a Progress.
type progressWriter struct {
	w io.Writer
	p *Progress
}

func (pw progressWriter) Write(b []byte) (int, error) {
	n, err := pw.w.Write(b)
	pw.p.Add(int64(n))
	return n, err
}

// progressFunc adapts a Progress to the callbacks of S3Client.
func progressFunc(p *Progress) func(int64) {
	if p == nil {
		return nil
	}
	return p.Add
}

// GuessContentType returns the content type of a file name from its
// extension ("" when unknown).
func GuessContentType(name string) string {
	return mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
}
