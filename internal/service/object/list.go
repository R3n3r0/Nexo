package object

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// errListDone stops a walk once enough entries were collected.
var errListDone = errors.New("object: listing complete")

// listEntry is one item of a listing: an object (with the versions to
// report, latest first) or a common prefix.
type listEntry struct {
	name     string
	isPrefix bool
	infos    []storage.FileInfo
}

// listParams are the common parameters of the listing operations.
type listParams struct {
	bucket    string
	prefix    string
	delimiter string
	// keyMarker is the key after which the listing resumes.
	keyMarker string
	// versionMarker, with keyMarker, resumes within the versions of a key.
	versionMarker string
	limit         int
	versions      bool
}

// items counts what an entry contributes to the listing limit.
func (e listEntry) items() int {
	if e.isPrefix {
		return 1
	}
	return len(e.infos)
}

// clampMaxKeys returns the effective limit of a listing: the default
// (1000) for zero and for larger values, none for domain.ListLimitNone
// (any negative value).
func clampMaxKeys(n int) int {
	switch {
	case n < 0:
		return 0
	case n == 0 || n > maxListKeys:
		return maxListKeys
	default:
		return n
	}
}

// splitPrefix returns the directory to walk and the key filter of a prefix.
func splitPrefix(prefix string) (baseDir, filter string) {
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		return prefix[:i], prefix
	}
	return "", prefix
}

// listEntries walks every store (merged in S3 order) and collects the
// entries of the listing. The returned slice holds at most limit items
// plus one extra entry when the listing is truncated.
func (l *layer) listEntries(ctx context.Context, p listParams) ([]listEntry, bool, error) {
	if err := domain.ValidateObjectPrefix(p.prefix); err != nil {
		return nil, false, err
	}
	if _, err := l.sets[0].StatVol(ctx, p.bucket); err != nil {
		return nil, false, fmt.Errorf("%s: list %s: %w", l.name, p.bucket, err)
	}
	if p.limit == 0 {
		// max-keys=0: an empty listing, not truncated (as S3 answers).
		return nil, false, nil
	}
	all, err := l.collect(ctx, p)
	if err != nil {
		return nil, false, fmt.Errorf("%s: list %s: %w", l.name, p.bucket, err)
	}

	// The prefix itself may be a directory object stored in the parent,
	// which the walk of the prefix does not reach. All its versions are
	// read (entryFor resumes after the version marker when the listing
	// resumes within them).
	if strings.HasSuffix(p.prefix, "/") && p.prefix >= p.keyMarker {
		infos, err := l.dirObjectVersions(ctx, p.bucket, p.prefix)
		if err != nil {
			return nil, false, fmt.Errorf("%s: list %s: %w", l.name, p.bucket, err)
		}
		if e, ok := l.entryFor(p, p.prefix, p.bucket, infos); ok {
			all = append([]listEntry{e}, all...)
		}
	}

	out := make([]listEntry, 0, len(all))
	count := 0
	for _, e := range all {
		if count >= p.limit {
			return out, true, nil
		}
		if e.isPrefix {
			out = append(out, e)
			count++
			continue
		}
		if count+len(e.infos) > p.limit {
			e.infos = e.infos[:p.limit-count]
			out = append(out, e)
			return out, true, nil
		}
		out = append(out, e)
		count += len(e.infos)
	}
	return out, false, nil
}

// dirObjectVersions returns all the versions (latest first) of the
// directory object key ("dir/", stored in the parent directory), none when
// it does not exist.
func (l *layer) dirObjectVersions(ctx context.Context, bucket, key string) ([]storage.FileInfo, error) {
	parent, _ := splitPrefix(strings.TrimSuffix(key, "/"))
	var infos []storage.FileInfo
	err := l.walkAll(ctx, storage.WalkDirOptions{Bucket: bucket, BaseDir: parent, FilterPrefix: key, ForwardTo: key}, func(e walkEntry) error {
		if e.IsDir || e.Name != key {
			return nil
		}
		infos = e.Versions
		return errListDone
	})
	if err != nil && !errors.Is(err, errListDone) {
		return nil, err
	}
	return infos, nil
}

// skipPrefix reports whether the common prefix cp is before the marker: a
// prefix equal to the marker was the last entry of the previous page (its
// NextMarker), and a prefix before the marker holds only earlier keys,
// unless the marker lies inside it ("a/" with start-after "a/b"), in which
// case the keys after the marker still roll up into it.
func skipPrefix(cp, keyMarker string) bool {
	return cp == keyMarker || (cp < keyMarker && !strings.HasPrefix(keyMarker, cp))
}

// prefixHasKeysAfter reports whether the common prefix cp, which contains
// the marker, holds an object after it: only then is cp part of the
// listing (S3 lists the keys after the marker, then rolls them up).
func (l *layer) prefixHasKeysAfter(ctx context.Context, bucket, cp, keyMarker string) (bool, error) {
	found := false
	opts := storage.WalkDirOptions{Bucket: bucket, BaseDir: strings.TrimSuffix(cp, "/"), FilterPrefix: cp, Recursive: true, ForwardTo: keyMarker}
	err := l.walkAll(ctx, opts, func(e walkEntry) error {
		if e.IsDir || e.Name <= keyMarker {
			return nil
		}
		found = true
		return errListDone
	})
	if err != nil && !errors.Is(err, errListDone) {
		return false, err
	}
	return found, nil
}

// collect walks the stores, merged, and collects the entries of the
// listing, up to limit+1 items.
func (l *layer) collect(ctx context.Context, p listParams) ([]listEntry, error) {
	baseDir, filter := splitPrefix(p.prefix)
	opts := storage.WalkDirOptions{
		Bucket:       p.bucket,
		BaseDir:      baseDir,
		FilterPrefix: filter,
		Recursive:    p.delimiter != "/",
		ForwardTo:    p.keyMarker,
	}
	var entries []listEntry
	count := 0
	err := l.walkAll(ctx, opts, func(e walkEntry) error {
		if e.IsDir {
			if p.delimiter != "/" {
				return nil // flat listings descend into prefixes
			}
			if skipPrefix(e.Name, p.keyMarker) {
				return nil
			}
			if e.Name < p.keyMarker {
				// The marker lies inside the prefix.
				ok, err := l.prefixHasKeysAfter(ctx, p.bucket, e.Name, p.keyMarker)
				if err != nil || !ok {
					return err
				}
			}
			var added bool
			if entries, added = appendEntry(entries, listEntry{name: e.Name, isPrefix: true}); added {
				count++
			}
		} else {
			entry, ok := l.entryFor(p, e.Name, p.bucket, e.Versions)
			if !ok {
				return nil
			}
			var added bool
			if entries, added = appendEntry(entries, entry); added {
				count += entry.items()
			}
		}
		if count > p.limit {
			return errListDone
		}
		return nil
	})
	if err != nil && !errors.Is(err, errListDone) {
		return nil, err
	}
	return entries, nil
}

// entryFor turns the versions of key into a listing entry, applying the
// markers, the delimiter roll-up and the version filter.
func (l *layer) entryFor(p listParams, key, bucket string, infos []storage.FileInfo) (listEntry, bool) {
	if key < p.keyMarker || (key == p.keyMarker && (!p.versions || p.versionMarker == "")) {
		return listEntry{}, false
	}
	if p.delimiter != "" {
		rest := key[len(p.prefix):]
		if i := strings.Index(rest, p.delimiter); i >= 0 {
			cp := key[:len(p.prefix)+i+len(p.delimiter)]
			// key is after the marker: cp holds it even when cp itself
			// sorts before a marker inside it.
			if skipPrefix(cp, p.keyMarker) {
				return listEntry{}, false
			}
			return listEntry{name: cp, isPrefix: true}, true
		}
	}
	if !p.versions {
		if len(infos) == 0 || infos[0].Deleted {
			return listEntry{}, false
		}
		return listEntry{name: key, infos: infos[:1]}, true
	}
	if key == p.keyMarker {
		// Resume after the version marker ("null" is the empty id).
		marker := p.versionMarker
		if marker == "null" {
			marker = ""
		}
		idx := -1
		for i, fi := range infos {
			if fi.VersionID == marker {
				idx = i
				break
			}
		}
		if idx < 0 || idx+1 >= len(infos) {
			return listEntry{}, false
		}
		infos = infos[idx+1:]
	}
	return listEntry{name: key, infos: infos}, true
}

// appendEntry adds an entry, collapsing a prefix repeated by consecutive
// keys; added reports whether the entry was appended.
func appendEntry(entries []listEntry, e listEntry) (out []listEntry, added bool) {
	if e.isPrefix && len(entries) > 0 {
		last := entries[len(entries)-1]
		if last.isPrefix && last.name == e.name {
			return entries, false
		}
	}
	return append(entries, e), true
}

// ListObjects implements Service (v1 and v2).
func (l *layer) ListObjects(ctx context.Context, in domain.ListObjectsInput) (domain.ListObjectsResult, error) {
	if err := validBucket(in.Bucket); err != nil {
		return domain.ListObjectsResult{}, err
	}
	marker := in.Marker
	if in.ContinuationToken != "" {
		decoded, err := decodeToken(in.ContinuationToken)
		if err != nil {
			return domain.ListObjectsResult{}, err
		}
		marker = decoded
	} else if in.StartAfter != "" && marker == "" {
		marker = in.StartAfter
	}
	limit := clampMaxKeys(in.MaxKeys)
	entries, truncated, err := l.listEntries(ctx, listParams{
		bucket: in.Bucket, prefix: in.Prefix, delimiter: in.Delimiter, keyMarker: marker, limit: limit,
	})
	if err != nil {
		return domain.ListObjectsResult{}, err
	}
	res := domain.ListObjectsResult{IsTruncated: truncated}
	for _, e := range entries {
		if e.isPrefix {
			res.CommonPrefixes = append(res.CommonPrefixes, e.name)
			continue
		}
		res.Objects = append(res.Objects, objectInfo(in.Bucket, e.name, e.infos[0]))
	}
	if truncated && len(entries) > 0 {
		last := entries[len(entries)-1].name
		res.NextMarker = last
		res.NextContinuationToken = encodeToken(last)
	}
	return res, nil
}

// ListObjectVersions implements Service.
func (l *layer) ListObjectVersions(ctx context.Context, in domain.ListObjectsInput) (domain.ListObjectsResult, error) {
	if err := validBucket(in.Bucket); err != nil {
		return domain.ListObjectsResult{}, err
	}
	limit := clampMaxKeys(in.MaxKeys)
	entries, truncated, err := l.listEntries(ctx, listParams{
		bucket: in.Bucket, prefix: in.Prefix, delimiter: in.Delimiter, keyMarker: in.Marker,
		versionMarker: in.VersionIDMarker, limit: limit, versions: true,
	})
	if err != nil {
		return domain.ListObjectsResult{}, err
	}
	res := domain.ListObjectsResult{IsTruncated: truncated}
	for _, e := range entries {
		if e.isPrefix {
			res.CommonPrefixes = append(res.CommonPrefixes, e.name)
			continue
		}
		for _, fi := range e.infos {
			res.Objects = append(res.Objects, objectInfo(in.Bucket, e.name, fi))
		}
	}
	if truncated && len(entries) > 0 {
		last := entries[len(entries)-1]
		res.NextMarker = last.name
		if !last.isPrefix && len(last.infos) > 0 {
			res.NextVersionIDMarker = last.infos[len(last.infos)-1].VersionID
			if res.NextVersionIDMarker == "" {
				res.NextVersionIDMarker = "null"
			}
		}
	}
	return res, nil
}

func encodeToken(key string) string { return base64.StdEncoding.EncodeToString([]byte(key)) }

func decodeToken(token string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return "", domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "The continuation token provided is incorrect")
	}
	return string(b), nil
}
