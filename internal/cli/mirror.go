package cli

import (
	"sort"
	"strings"
	"time"
)

// MirrorActionKind is what mirror does with one path.
type MirrorActionKind string

// Mirror actions.
const (
	// MirrorCopy copies a path missing from the target.
	MirrorCopy MirrorActionKind = "copy"
	// MirrorOverwrite replaces a different target path (--overwrite).
	MirrorOverwrite MirrorActionKind = "overwrite"
	// MirrorRemove removes a target path missing from the source
	// (--remove).
	MirrorRemove MirrorActionKind = "remove"
	// MirrorConflict reports a different target path kept because
	// --overwrite was not given.
	MirrorConflict MirrorActionKind = "conflict"
)

// MirrorOptions tune PlanMirror.
type MirrorOptions struct {
	Overwrite bool
	Remove    bool
}

// MirrorAction is one step of a mirror.
type MirrorAction struct {
	Kind MirrorActionKind
	Rel  string
	// Source is the source entry (copy, overwrite, conflict) and Target
	// the existing target entry (overwrite, remove, conflict).
	Source Entry
	Target Entry
	// SourceVersion is the version of the source replayed by mirror
	// --preserve (PlanMirrorVersions).
	SourceVersion string
}

// PlanMirror compares the listings of the source and of the target of a
// mirror and returns the actions sorted by path (docs/02 C-OBJ-06). Paths
// that SameEntry reports as identical are left alone, so that a second
// run transfers nothing.
func PlanMirror(src, dst map[string]Entry, opts MirrorOptions) []MirrorAction {
	var out []MirrorAction
	for rel, s := range src {
		d, ok := dst[rel]
		switch {
		case !ok:
			out = append(out, MirrorAction{Kind: MirrorCopy, Rel: rel, Source: s})
		case SameEntry(s, d):
		case opts.Overwrite:
			out = append(out, MirrorAction{Kind: MirrorOverwrite, Rel: rel, Source: s, Target: d})
		default:
			out = append(out, MirrorAction{Kind: MirrorConflict, Rel: rel, Source: s, Target: d})
		}
	}
	if opts.Remove {
		for rel, d := range dst {
			if _, ok := src[rel]; !ok {
				out = append(out, MirrorAction{Kind: MirrorRemove, Rel: rel, Target: d})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}

// SameEntry reports whether a target entry is up to date with a source
// entry: same size and either the same ETag, or (when the ETags cannot be
// compared: local files, multipart uploads) a target that is not older
// than the source, at the precision of the second of S3 dates.
func SameEntry(src, dst Entry) bool {
	if src.Size != dst.Size {
		return false
	}
	if src.ETag != "" && dst.ETag != "" {
		if src.ETag == dst.ETag {
			return true
		}
		if !isMultipartETag(src.ETag) && !isMultipartETag(dst.ETag) {
			return false
		}
	}
	return !src.ModTime.Truncate(time.Second).After(dst.ModTime.Truncate(time.Second))
}

// isMultipartETag recognizes the ETag of a multipart upload ("md5-N"),
// which is not the MD5 of the content.
func isMultipartETag(etag string) bool { return strings.Contains(etag, "-") }
