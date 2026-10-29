package admin

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// zipErrorsEntry is the name of the entry listing the skipped objects.
const zipErrorsEntry = "nexo-zip-errors.txt"

// Zip implements ObjectBrowser.
func (b *objectBrowser) Zip(ctx context.Context, in ZipInput, open ZipOpener) (*ZipResult, error) {
	if len(in.Keys) == 0 && len(in.Prefixes) == 0 {
		return nil, fmt.Errorf("%w: no object or prefix to archive", domain.ErrInvalidArgument)
	}
	if len(in.Keys) > maxExplicitObjects {
		return nil, fmt.Errorf("%w: at most %d objects per archive request", domain.ErrInvalidArgument, maxExplicitObjects)
	}
	in.Base, in.Prefixes = folderPrefix(in.Base), folderPrefixes(in.Prefixes)
	if err := domain.ValidateObjectPrefix(in.Base); err != nil {
		return nil, err
	}
	for _, p := range in.Prefixes {
		if err := domain.ValidateObjectPrefix(p); err != nil {
			return nil, err
		}
	}
	// Listing the prefixes needs s3:ListBucket: the errors entry names the
	// objects of a prefix the requester may not read.
	if err := b.authorizeWalks(ctx, in.Requester, in.Bucket, in.Prefixes, false); err != nil {
		return nil, err
	}
	// The explicit objects are checked before anything is written: a
	// missing or forbidden one fails the whole request.
	explicit := make([]domain.ObjectInfo, 0, len(in.Keys))
	for _, key := range in.Keys {
		if err := domain.ValidateObjectKey(key); err != nil {
			return nil, err
		}
		if err := authorizeObject(ctx, b.deps.Authz, in.Requester, in.Bucket, key, ObjectRequest{}, policy.ActionGetObject); err != nil {
			return nil, err
		}
		if err := b.authorizeProtected(ctx, in.Requester, in.Bucket, key, ""); err != nil {
			return nil, err
		}
		info, err := b.deps.Objects.GetObjectInfo(ctx, in.Bucket, key, object.GetObjectOptions{})
		if err != nil {
			return nil, err
		}
		explicit = append(explicit, info)
	}
	for _, p := range in.Prefixes {
		// A prefix of a bucket that does not exist fails before writing.
		if _, err := b.deps.Objects.ListObjects(ctx, domain.ListObjectsInput{Bucket: in.Bucket, Prefix: p, MaxKeys: 1}); err != nil {
			return nil, err
		}
	}
	w, err := open(zipName(in))
	if err != nil {
		return nil, err
	}
	zw := &zipWriter{b: b, ctx: ctx, in: in, zw: zip.NewWriter(w), seen: map[string]bool{}, out: &ZipResult{Skipped: []ObjectError{}}}
	for _, info := range explicit {
		if err := zw.add(info); err != nil {
			return zw.out, err
		}
	}
	for _, p := range in.Prefixes {
		err := b.walkObjects(ctx, in.Bucket, p, func(info domain.ObjectInfo) error {
			if !b.allowed(ctx, in.Requester, in.Bucket, info.Key, ObjectConditions(ObjectRequest{}), policy.ActionGetObject) {
				zw.skip(info.Key, fmt.Errorf("%w: s3:GetObject", domain.ErrAccessDenied))
				return nil
			}
			if err := b.authorizeProtected(ctx, in.Requester, in.Bucket, info.Key, ""); err != nil {
				zw.skip(info.Key, err)
				return nil
			}
			return zw.add(info)
		})
		if err != nil {
			return zw.out, err
		}
	}
	if err := zw.finish(); err != nil {
		return zw.out, err
	}
	b.deps.Logger.InfoContext(ctx, "admin: zip archive", "bucket", in.Bucket, "files", zw.out.Files, "bytes", zw.out.Bytes, "skipped", len(zw.out.Skipped))
	return zw.out, nil
}

// zipWriter streams the entries of an archive.
type zipWriter struct {
	b         *objectBrowser
	ctx       context.Context
	in        ZipInput
	zw        *zip.Writer
	seen      map[string]bool
	out       *ZipResult
	truncated bool
}

func (z *zipWriter) skip(key string, err error) {
	z.out.Skipped = append(z.out.Skipped, ObjectError{Key: key, Err: err})
}

// add writes one object; a vanished object is skipped. Once the entry
// limit is reached the remaining objects are ignored and finish reports
// the truncation.
func (z *zipWriter) add(info domain.ObjectInfo) error {
	if z.truncated {
		return nil
	}
	name := zipEntryName(z.in.Base, info.Key)
	if name == "" || z.seen[name] {
		return nil
	}
	if z.out.Files >= z.b.zipMaxObjects {
		z.truncated = true
		return nil
	}
	z.seen[name] = true
	if strings.HasSuffix(name, "/") {
		// A folder placeholder becomes a directory entry.
		if _, err := z.zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, Modified: info.ModTime}); err != nil {
			return fmt.Errorf("admin: zip: %w", err)
		}
		return nil
	}
	rd, err := z.b.deps.Objects.GetObject(z.ctx, z.in.Bucket, info.Key, object.GetObjectOptions{})
	if err != nil {
		if errors.Is(err, domain.ErrObjectNotFound) || errors.Is(err, domain.ErrNoSuchVersion) {
			z.skip(info.Key, err)
			return nil
		}
		return err
	}
	defer func() { _ = rd.Close() }()
	method := zip.Deflate
	if compressed(rd.Info.ContentType) {
		method = zip.Store
	}
	w, err := z.zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: rd.Info.ModTime})
	if err != nil {
		return fmt.Errorf("admin: zip: %w", err)
	}
	n, err := io.Copy(w, rd)
	z.out.Bytes += n
	if err != nil {
		return fmt.Errorf("admin: zip %s: %w", info.Key, err)
	}
	z.out.Files++
	return nil
}

// finish writes the errors entry, when needed, and the central directory.
func (z *zipWriter) finish() error {
	if len(z.out.Skipped) > 0 || z.truncated {
		w, err := z.zw.Create(zipErrorsEntry)
		if err != nil {
			return fmt.Errorf("admin: zip: %w", err)
		}
		var sb strings.Builder
		for _, s := range z.out.Skipped {
			fmt.Fprintf(&sb, "%s: %v\n", s.Key, s.Err)
		}
		if z.truncated {
			fmt.Fprintf(&sb, "archive truncated: at most %d objects per archive\n", z.b.zipMaxObjects)
		}
		if _, err := io.WriteString(w, sb.String()); err != nil {
			return fmt.Errorf("admin: zip: %w", err)
		}
	}
	if err := z.zw.Close(); err != nil {
		return fmt.Errorf("admin: zip: %w", err)
	}
	return nil
}

// zipName is the file name of the archive: the only folder requested,
// else the folder being browsed (Base), else the bucket.
func zipName(in ZipInput) string {
	base := in.Base
	if len(in.Prefixes) == 1 && len(in.Keys) == 0 {
		// One folder: the archive is named after it.
		base = in.Prefixes[0]
	}
	name := in.Bucket
	if seg := path.Base(strings.TrimSuffix(base, "/")); base != "" && seg != "." && seg != "/" {
		name = seg
	}
	return sanitizeSegment(name) + ".zip"
}

// zipEntryName returns the archive name of key, relative to base when the
// key lies under it, as a safe relative path: forward slashes only, no
// leading slash, no empty, "." or ".." segment. A trailing slash marks a
// directory. Keys that reduce to nothing are skipped ("").
func zipEntryName(base, key string) string {
	rel := key
	if base != "" && strings.HasPrefix(key, base) && key != base {
		rel = key[len(base):]
	}
	dir := strings.HasSuffix(rel, "/")
	rel = strings.ReplaceAll(rel, "\\", "/")
	parts := strings.Split(rel, "/")
	clean := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			continue
		}
		clean = append(clean, sanitizeSegment(p))
	}
	if len(clean) == 0 {
		return ""
	}
	name := strings.Join(clean, "/")
	if dir {
		name += "/"
	}
	return name
}

// sanitizeSegment replaces the characters that are invalid in file names
// on common file systems (control characters, ':' and the Windows
// reserved punctuation).
func sanitizeSegment(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7f:
			return '_'
		case strings.ContainsRune(`:*?"<>|`, r):
			return '_'
		default:
			return r
		}
	}, s)
}

// compressed reports content types not worth deflating again.
func compressed(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(mediaType, "image/") && mediaType != "image/svg+xml" && mediaType != "image/bmp":
		return true
	case strings.HasPrefix(mediaType, "video/"), strings.HasPrefix(mediaType, "audio/"):
		return true
	}
	switch mediaType {
	case "application/zip", "application/gzip", "application/x-gzip", "application/x-7z-compressed",
		"application/x-bzip2", "application/x-xz", "application/zstd", "application/x-rar-compressed":
		return true
	}
	return false
}
