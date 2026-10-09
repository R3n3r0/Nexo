package cli

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// MirrorDeleteMarker replays a delete marker of the source on the target
// (mirror --preserve).
const MirrorDeleteMarker MirrorActionKind = "delete-marker"

// VersionEntry is one version (or delete marker) of a remote listing of
// every version.
type VersionEntry struct {
	Entry
	VersionID    string
	DeleteMarker bool
}

// CollectVersions lists every version and delete marker under a remote
// prefix (DirPrefix of the key), grouped by relative path, the oldest
// first. Directory placeholders (keys ending with "/") are skipped.
func CollectVersions(ctx context.Context, resolve ClientResolver, t Target) (map[string][]VersionEntry, error) {
	if !t.IsRemote() || t.Bucket == "" {
		return nil, UsageError("%s: the versions of a bucket or prefix are needed", t)
	}
	client, err := resolve(ctx, t.Alias)
	if err != nil {
		return nil, err
	}
	prefix := DirPrefix(t.Key)
	out := map[string][]VersionEntry{}
	for o, err := range client.List(ctx, t.Bucket, ListOptions{Prefix: prefix, Recursive: true, Versions: true}) {
		if err != nil {
			return nil, err
		}
		if o.Prefix || strings.HasSuffix(o.Key, "/") {
			continue
		}
		rel := strings.TrimPrefix(o.Key, prefix)
		out[rel] = append(out[rel], VersionEntry{
			Entry:     Entry{Rel: rel, Size: o.Size, ModTime: o.LastModified, ETag: o.ETag},
			VersionID: o.VersionID, DeleteMarker: o.DeleteMarker,
		})
	}
	// The listings return the latest version of each key first.
	for _, vs := range out {
		slices.Reverse(vs)
	}
	return out, nil
}

// PlanMirrorVersions compares the histories of the keys of the source and
// of the target of mirror --preserve and returns the versions to replay on
// the target, sorted by path and, for each path, from the oldest. The
// target is up to date when its history ends with the whole history of the
// source; when it ends with the oldest versions of the source only, the
// newer ones are replayed (an interrupted or earlier mirror). A target
// path whose history has none of the source is a conflict, replayed whole
// with Overwrite (its own versions stay below the replayed ones).
func PlanMirrorVersions(src, dst map[string][]VersionEntry, opts MirrorOptions) []MirrorAction {
	var out []MirrorAction
	for _, rel := range SortedKeys(src) {
		s, d := src[rel], dst[rel]
		from := replayedVersions(s, d)
		if from < 0 {
			if !opts.Overwrite {
				latest := s[len(s)-1]
				out = append(out, MirrorAction{Kind: MirrorConflict, Rel: rel, Source: latest.Entry, Target: d[len(d)-1].Entry})
				continue
			}
			from = 0
		}
		for _, v := range s[from:] {
			kind := MirrorCopy
			if v.DeleteMarker {
				kind = MirrorDeleteMarker
			}
			out = append(out, MirrorAction{Kind: kind, Rel: rel, Source: v.Entry, SourceVersion: v.VersionID})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}

// replayedVersions returns how many of the oldest versions of the source
// history s end the target history d (those are already replayed), or -1
// when d is not empty and ends with none of them.
func replayedVersions(s, d []VersionEntry) int {
	if len(d) == 0 {
		return 0
	}
	for n := min(len(s), len(d)); n > 0; n-- {
		if sameHistory(s[:n], d[len(d)-n:]) {
			return n
		}
	}
	return -1
}

// sameHistory reports whether two histories of the same length hold the
// same versions in the same order: delete markers in the same places and
// objects that SameEntry reports up to date.
func sameHistory(s, d []VersionEntry) bool {
	for i := range s {
		if s[i].DeleteMarker != d[i].DeleteMarker {
			return false
		}
		if !s[i].DeleteMarker && !SameEntry(s[i].Entry, d[i].Entry) {
			return false
		}
	}
	return true
}

// needsVersioning reports whether replaying the actions needs a target
// bucket with versioning enabled: more than one version of a path, or a
// delete marker.
func needsVersioning(actions []MirrorAction) bool {
	seen := map[string]bool{}
	for _, a := range actions {
		if a.Kind == MirrorDeleteMarker || (a.Kind == MirrorCopy && seen[a.Rel]) {
			return true
		}
		seen[a.Rel] = true
	}
	return false
}

// planVersions is Plan for mirror --preserve: both sides are remote and
// the target bucket must keep the versions it receives.
func (m *mirrorer) planVersions(ctx context.Context, req MirrorRequest) (*MirrorPlan, error) {
	if !req.Source.IsRemote() || !req.Target.IsRemote() {
		return nil, UsageError("--preserve copies the versions between two buckets: %s and %s must be remote", req.Source, req.Target)
	}
	if req.Options.Remove {
		return nil, UsageError("--preserve excludes --remove")
	}
	src, err := CollectVersions(ctx, m.deps.Resolve, req.Source)
	if err != nil {
		return nil, err
	}
	dst, err := CollectVersions(ctx, m.deps.Resolve, req.Target)
	if err != nil {
		return nil, err
	}
	plan := &MirrorPlan{Actions: PlanMirrorVersions(src, dst, req.Options)}
	for _, a := range plan.Actions {
		if a.Kind == MirrorCopy {
			plan.Bytes += a.Source.Size
		}
	}
	if !needsVersioning(plan.Actions) {
		return plan, nil
	}
	client, err := m.deps.Resolve(ctx, req.Target.Alias)
	if err != nil {
		return nil, err
	}
	v, err := client.GetVersioning(ctx, req.Target.Bucket)
	if err != nil {
		return nil, err
	}
	if v != VersioningEnabled {
		return nil, fmt.Errorf("%s: --preserve needs versioning enabled on the target bucket to keep the versions of the source "+
			"(nexoctl version enable, or admin cluster import first)", req.Target)
	}
	return plan, nil
}

// replayVersion copies one version of the source (with its content type,
// metadata and tags) or replays one delete marker on the target.
func (m *mirrorer) replayVersion(ctx context.Context, item CopyItem, kind MirrorActionKind, opts TransferOptions) (int64, error) {
	if kind == MirrorDeleteMarker {
		if err := m.transfer.Delete(ctx, item.Dest, ""); err != nil {
			return 0, fmt.Errorf("delete marker %s: %w", item.Dest, err)
		}
		return 0, nil
	}
	client, err := m.deps.Resolve(ctx, item.Source.Alias)
	if err != nil {
		return 0, err
	}
	tags, err := client.GetTags(ctx, ObjectRef{Bucket: item.Source.Bucket, Key: item.Source.Key, VersionID: item.SourceVersion})
	if err != nil {
		return 0, fmt.Errorf("%s: %w", item.Source, err)
	}
	if len(tags) > 0 {
		opts.Tags = tags
	}
	res, err := m.transfer.Copy(ctx, item, opts)
	if err != nil {
		return 0, err
	}
	return res.Size, nil
}
