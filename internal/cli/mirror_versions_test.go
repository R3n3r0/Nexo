package cli_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/cli/clitest"
)

// history returns a history of PlanMirrorVersions: "" is a delete
// marker, anything else the content of a version (its ETag and size).
func history(rel string, contents ...string) []cli.VersionEntry {
	out := make([]cli.VersionEntry, 0, len(contents))
	for i, c := range contents {
		v := cli.VersionEntry{Entry: cli.Entry{Rel: rel, ModTime: t0.Add(time.Duration(i) * time.Hour)}, VersionID: fmt.Sprintf("%s-v%d", rel, i)}
		if c == "" {
			v.DeleteMarker = true
		} else {
			v.Size, v.ETag = int64(len(c)), `"`+c+`"`
		}
		out = append(out, v)
	}
	return out
}

func TestPlanMirrorVersions(t *testing.T) {
	src := history("a", "one", "two", "", "three")
	tests := []struct {
		name string
		dst  []cli.VersionEntry
		opts cli.MirrorOptions
		want []string
	}{
		{name: "empty target: the whole history", want: []string{"copy a-v0", "copy a-v1", "delete-marker a-v2", "copy a-v3"}},
		{name: "up to date", dst: history("a", "one", "two", "", "three")},
		{name: "interrupted: the newer versions", dst: history("a", "one", "two"), want: []string{"delete-marker a-v2", "copy a-v3"}},
		{name: "replayed over older target versions", dst: history("a", "old", "one", "two", "", "three")},
		{name: "unrelated target: conflict", dst: history("a", "other"), want: []string{"conflict "}},
		{name: "unrelated target, overwrite: the whole history", dst: history("a", "other"), opts: cli.MirrorOptions{Overwrite: true},
			want: []string{"copy a-v0", "copy a-v1", "delete-marker a-v2", "copy a-v3"}},
		{name: "a marker where the source has a version: conflict", dst: history("a", "one", ""), want: []string{"conflict "}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dst := map[string][]cli.VersionEntry{}
			if tc.dst != nil {
				dst["a"] = tc.dst
			}
			var got []string
			for _, a := range cli.PlanMirrorVersions(map[string][]cli.VersionEntry{"a": src}, dst, tc.opts) {
				got = append(got, string(a.Kind)+" "+a.SourceVersion)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestMirrorerPreserve (docs/08 §8.2, TM-04): mirror --preserve replays
// every version and delete marker, oldest first, with the content type,
// the metadata and the tags of each version; a second run replays only the
// newer versions; a target that would not keep the versions is refused.
func TestMirrorerPreserve(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	client, err := f.resolve(ctx, "mem")
	require.NoError(t, err)
	for _, b := range []string{"old", "new"} {
		f.mem.MakeBucket(b)
		require.NoError(t, client.SetVersioning(ctx, b, cli.VersioningEnabled))
	}
	f.mem.PutObject("old", "docs/a.txt", "v1", clitest.ObjectSpec{ContentType: "text/plain", Metadata: map[string]string{"rev": "1"}})
	f.mem.PutObject("old", "docs/a.txt", "v2", clitest.ObjectSpec{ContentType: "text/markdown", Tags: map[string]string{"stage": "final"}})
	f.mem.PutObject("old", "docs/gone.txt", "bye", clitest.ObjectSpec{})
	_, err = client.Remove(ctx, "old", []cli.ObjectVersion{{Key: "docs/gone.txt"}}, cli.DeleteOptions{})
	require.NoError(t, err)

	m, err := cli.NewMirrorer(cli.MirrorDeps{Resolve: f.resolve})
	require.NoError(t, err)
	req := cli.MirrorRequest{Source: f.target(t, "mem/old"), Target: f.target(t, "mem/new"), Preserve: true}
	plan, err := m.Plan(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, int64(len("v1v2bye")), plan.Bytes)
	var ev events
	stats, err := m.Apply(ctx, req, plan, ev.report)
	require.NoError(t, err)
	assert.Equal(t, cli.MirrorStats{Copied: 3, Bytes: int64(len("v1v2bye")), Markers: 1}, stats)
	assert.True(t, ev.has("delete-marker mem/new/docs/gone.txt"))

	versions := func(bucket string) []string {
		var out []string
		for o, err := range client.List(ctx, bucket, cli.ListOptions{Recursive: true, Versions: true}) {
			require.NoError(t, err)
			line := o.Key + " marker"
			if !o.DeleteMarker {
				data, info, err := client.Get(ctx, bucket, o.Key, cli.GetOptions{VersionID: o.VersionID})
				require.NoError(t, err)
				body := make([]byte, info.Size)
				_, _ = data.Read(body)
				_ = data.Close()
				tags, err := client.GetTags(ctx, cli.ObjectRef{Bucket: bucket, Key: o.Key, VersionID: o.VersionID})
				require.NoError(t, err)
				line = fmt.Sprintf("%s %s %s %v %v", o.Key, body, info.ContentType, info.Metadata, tags)
			}
			out = append(out, line)
		}
		return out
	}
	assert.Equal(t, versions("old"), versions("new"), "the same versions in the same order")

	// A second run replays nothing; a new version of the source only.
	plan, err = m.Plan(ctx, req)
	require.NoError(t, err)
	assert.Empty(t, plan.Actions)
	f.mem.PutObject("old", "docs/a.txt", "v3", clitest.ObjectSpec{})
	plan, err = m.Plan(ctx, req)
	require.NoError(t, err)
	require.Len(t, plan.Actions, 1)
	assert.Equal(t, cli.MirrorCopy, plan.Actions[0].Kind)
	_, err = m.Apply(ctx, req, plan, ev.report)
	require.NoError(t, err)
	assert.Equal(t, versions("old"), versions("new"))

	// The versions would be lost in a bucket without versioning.
	f.mem.MakeBucket("plain")
	_, err = m.Plan(ctx, cli.MirrorRequest{Source: f.target(t, "mem/old"), Target: f.target(t, "mem/plain"), Preserve: true})
	assert.ErrorContains(t, err, "needs versioning enabled")

	// Only between buckets, and without --remove.
	_, err = m.Plan(ctx, cli.MirrorRequest{Source: f.target(t, "mem/old"), Target: f.target(t, "out"), Preserve: true})
	assert.ErrorIs(t, err, cli.ErrUsage)
	_, err = m.Plan(ctx, cli.MirrorRequest{Source: f.target(t, "mem/old"), Target: f.target(t, "mem/new"), Preserve: true,
		Options: cli.MirrorOptions{Remove: true}})
	assert.ErrorIs(t, err, cli.ErrUsage)
}
