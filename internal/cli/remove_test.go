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
	"github.com/r3n3r0/nexo/internal/domain"
)

// collect gathers the reported removals as "key@version" strings.
func collect(out *[]string) func(cli.RemoveResult) error {
	return func(r cli.RemoveResult) error {
		s := r.Key
		if r.VersionID != "" {
			s += "@" + r.VersionID
		}
		if r.DeleteMarker && r.DeleteMarkerVersionID != "" {
			s += " marker " + r.DeleteMarkerVersionID
		}
		if r.Err != nil {
			s += " error"
		}
		*out = append(*out, s)
		return nil
	}
}

// covers: C-OBJ-04, TCL-E-05
func TestRemoveObjects(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	client, _ := f.resolve(ctx, "mem")
	for _, k := range []string{"logs/a.log", "logs/b.log", "logs/sub/c.log", "logsX", "logs", "keep.txt"} {
		f.mem.PutObject("bkt", k, k, clitest.ObjectSpec{})
	}

	var got []string
	sum, err := cli.RemoveObjects(ctx, client, "bkt", "logs/a.log", cli.RemoveOptions{}, collect(&got))
	require.NoError(t, err)
	assert.Equal(t, cli.RemoveSummary{Removed: 1}, sum)
	assert.Equal(t, []string{"logs/a.log"}, got)

	_, err = cli.RemoveObjects(ctx, client, "bkt", "logs/missing", cli.RemoveOptions{}, collect(&got))
	assert.ErrorIs(t, err, domain.ErrObjectNotFound, "removing a missing object is an error")
	_, err = cli.RemoveObjects(ctx, client, "bkt", "logs/", cli.RemoveOptions{}, collect(&got))
	assert.ErrorIs(t, err, cli.ErrUsage, "a prefix needs --recursive")

	got = nil
	sum, err = cli.RemoveObjects(ctx, client, "bkt", "logs", cli.RemoveOptions{Recursive: true, DryRun: true}, collect(&got))
	require.NoError(t, err)
	assert.Equal(t, []string{"logs", "logs/b.log", "logs/sub/c.log"}, got, "the object and the prefix, not logsX")
	assert.Equal(t, 3, sum.Removed)
	assert.Len(t, f.mem.Keys("bkt"), 5, "dry run removes nothing")

	got = nil
	_, err = cli.RemoveObjects(ctx, client, "bkt", "logs", cli.RemoveOptions{Recursive: true}, collect(&got))
	require.NoError(t, err)
	assert.Equal(t, []string{"keep.txt", "logsX"}, f.mem.Keys("bkt"))

	got = nil
	sum, err = cli.RemoveObjects(ctx, client, "bkt", "", cli.RemoveOptions{Recursive: true}, collect(&got))
	require.NoError(t, err)
	assert.Equal(t, 2, sum.Removed)
	assert.Empty(t, f.mem.Keys("bkt"))

	_, err = cli.RemoveObjects(ctx, client, "bkt", "x", cli.RemoveOptions{VersionID: "v", Recursive: true}, collect(&got))
	assert.ErrorIs(t, err, cli.ErrUsage)
}

// covers: C-OBJ-04, TCL-E-05
func TestRemoveVersionsAndAge(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	client, _ := f.resolve(ctx, "mem")
	f.mem.MakeBucket("ver")
	require.NoError(t, client.SetVersioning(ctx, "ver", cli.VersioningEnabled))
	v1 := f.mem.PutObject("ver", "doc", "1", clitest.ObjectSpec{})
	v2 := f.mem.PutObject("ver", "doc", "2", clitest.ObjectSpec{})
	f.mem.PutObject("ver", "other", "o", clitest.ObjectSpec{})

	var got []string
	_, err := cli.RemoveObjects(ctx, client, "ver", "doc", cli.RemoveOptions{}, collect(&got))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "doc marker v0004", "a versioned removal adds a delete marker")

	got = nil
	_, err = cli.RemoveObjects(ctx, client, "ver", "doc", cli.RemoveOptions{VersionID: v1}, collect(&got))
	require.NoError(t, err)
	assert.Equal(t, []string{"doc@" + v1}, got)

	got = nil
	sum, err := cli.RemoveObjects(ctx, client, "ver", "doc", cli.RemoveOptions{Versions: true}, collect(&got))
	require.NoError(t, err)
	assert.Equal(t, []string{"doc@v0004", "doc@" + v2}, got, "every version and the marker, not other")
	assert.Equal(t, 2, sum.Removed)
	assert.Equal(t, []string{"other"}, f.mem.Keys("ver"))

	got = nil
	sum, err = cli.RemoveObjects(ctx, client, "ver", "doc", cli.RemoveOptions{VersionID: "nope"}, collect(&got))
	require.NoError(t, err)
	assert.Equal(t, cli.RemoveSummary{Failed: 1}, sum, "per-object failures are reported, not returned")
	assert.Equal(t, []string{"doc@nope error"}, got)

	// Age filters.
	f.mem.MakeBucket("age")
	now := t0.Add(10 * 24 * time.Hour)
	for i, age := range []time.Duration{time.Hour, 2 * 24 * time.Hour, 5 * 24 * time.Hour} {
		f.mem.PutObject("age", fmt.Sprintf("f%d", i), "x", clitest.ObjectSpec{ModTime: now.Add(-age)})
	}
	got = nil
	_, err = cli.RemoveObjects(ctx, client, "age", "", cli.RemoveOptions{Recursive: true, OlderThan: 24 * time.Hour, NewerThan: 3 * 24 * time.Hour, Now: now}, collect(&got))
	require.NoError(t, err)
	assert.Equal(t, []string{"f1"}, got)

	// Failure of the request.
	f.mem.FailOn("Remove", domain.ErrAccessDenied)
	_, err = cli.RemoveObjects(ctx, client, "age", "f0", cli.RemoveOptions{}, collect(&got))
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	f.mem.FailOn("Remove", nil)
	f.mem.FailOn("List", domain.ErrAccessDenied)
	_, err = cli.RemoveObjects(ctx, client, "age", "", cli.RemoveOptions{Recursive: true}, collect(&got))
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
}

// covers: C-OBJ-02
func TestRemoveUploadsAndEmptyBucket(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	client, _ := f.resolve(ctx, "mem")
	f.mem.AddUpload("bkt", "big/a", "u1")
	f.mem.AddUpload("bkt", "big/b", "u2")
	f.mem.AddUpload("bkt", "other", "u3")
	f.mem.PutObject("bkt", "x", "x", clitest.ObjectSpec{})

	var ids []string
	report := func(u cli.UploadInfo, err error) error {
		require.NoError(t, err)
		ids = append(ids, u.UploadID)
		return nil
	}
	sum, err := cli.RemoveUploads(ctx, client, "bkt", "big/a", false, true, report)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Removed)
	assert.Equal(t, []string{"u1"}, ids)
	ids = nil
	_, err = cli.RemoveUploads(ctx, client, "bkt", "big/", true, false, report)
	require.NoError(t, err)
	assert.Equal(t, []string{"u1", "u2"}, ids)

	assert.ErrorIs(t, client.RemoveBucket(ctx, "bkt"), domain.ErrBucketNotEmpty)
	var removed []string
	sum, err = cli.EmptyBucket(ctx, client, "bkt", collect(&removed))
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Removed)
	require.NoError(t, client.RemoveBucket(ctx, "bkt"))
	assert.Empty(t, f.mem.BucketNames())
}
