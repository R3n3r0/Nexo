package object_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

func mover(t *testing.T, c *cluster) object.PoolMover {
	t.Helper()
	m, ok := c.svc.(object.PoolMover)
	require.True(t, ok, "the pools layer implements PoolMover")
	return m
}

// allKeys pages through PoolKeys.
func allKeys(t *testing.T, m object.PoolMover, pool int, bucket string) []string {
	t.Helper()
	var out []string
	after := ""
	for {
		keys, err := m.PoolKeys(context.Background(), pool, bucket, after, 3)
		require.NoError(t, err)
		if len(keys) == 0 {
			return out
		}
		out = append(out, keys...)
		after = keys[len(keys)-1]
	}
}

// TestMoveObject (S-STO-09): every kind of version moves to another pool
// with its ids, times, metadata and content, and reads the same afterwards;
// the source keeps nothing.
//
// covers: S-STO-09
func TestMoveObject(t *testing.T) {
	ctx := context.Background()
	spec := clusterSpec{pools: 2, sets: 2, drives: 4, buckets: versionedBuckets(t), crypto: newTestCrypto(t),
		opts: []object.Option{object.WithPoolSpaceTTL(0), object.WithCompression(compressTxt)}}
	c := newCluster(t, spec)
	m := mover(t, c)
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	// Everything goes to pool 1 first.
	c.setPoolSpace(spec, 0, 10, 0.1)
	c.setPoolSpace(spec, 1, 10, 9)

	type item struct {
		key  string
		data []byte
		put  func(*object.PutObjectInput)
	}
	items := []item{
		{key: "inline", data: []byte("small")},
		{key: "big/erasure.bin", data: payload(3*testBlockSize + 17)},
		{key: "logs/app.txt", data: compressible(2*testBlockSize, 3)},
		{key: "secret.bin", data: payload(2*testBlockSize + 5), put: func(in *object.PutObjectInput) { in.SSE = domain.SSERequest{Type: domain.SSES3} }},
		{key: "empty", data: nil},
	}
	versions := map[string][]domain.ObjectInfo{}
	for _, it := range items {
		var mut []func(*object.PutObjectInput)
		if it.put != nil {
			mut = append(mut, it.put)
		}
		versions[it.key] = append(versions[it.key], put(t, c.svc, "bkt", it.key, it.data, mut...))
		versions[it.key] = append(versions[it.key], put(t, c.svc, "bkt", it.key, append([]byte("v2-"), it.data...), mut...))
	}
	// A multipart version and a delete marker.
	up, err := c.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "mp"})
	require.NoError(t, err)
	var parts []domain.CompletePart
	var whole []byte
	for i := 1; i <= 3; i++ {
		data := payload(40 + i)
		whole = append(whole, data...)
		p, err := c.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "mp", UploadID: up.UploadID, PartNumber: i, Reader: bytes.NewReader(data), Size: int64(len(data))})
		require.NoError(t, err)
		parts = append(parts, domain.CompletePart{Number: i, ETag: p.ETag})
	}
	mp, err := c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "mp", UploadID: up.UploadID, Parts: parts})
	require.NoError(t, err)
	_, err = c.svc.DeleteObject(ctx, "bkt", "inline", object.DeleteObjectOptions{})
	require.NoError(t, err)

	before, err := c.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "bkt"})
	require.NoError(t, err)
	keys := allKeys(t, m, 1, "bkt")
	assert.Equal(t, []string{"big/erasure.bin", "empty", "inline", "logs/app.txt", "mp", "secret.bin"}, keys)
	assert.Empty(t, allKeys(t, m, 0, "bkt"))

	c.setPoolSpace(spec, 0, 10, 9)
	for _, k := range keys {
		res, err := m.MoveObject(ctx, object.MoveObjectInput{Bucket: "bkt", Key: k, From: 1})
		require.NoError(t, err, k)
		assert.Equal(t, 0, res.To, k)
		assert.Equal(t, []int{0}, c.poolOf(t, spec, "bkt", k), k)
	}
	assert.Empty(t, allKeys(t, m, 1, "bkt"))
	after, err := c.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "bkt"})
	require.NoError(t, err)
	assert.Equal(t, versionTrace(before), versionTrace(after), "the versions are the same, in the same order")

	for _, it := range items {
		for i, v := range versions[it.key] {
			want := it.data
			if i == 1 {
				want = append([]byte("v2-"), it.data...)
			}
			got, _ := get(t, c.svc, "bkt", it.key, object.GetObjectOptions{VersionID: v.VersionID})
			assert.Equal(t, string(want), string(got), "%s version %d", it.key, i)
		}
	}
	got, _ := get(t, c.svc, "bkt", "mp", object.GetObjectOptions{})
	assert.Equal(t, whole, got)
	info, err := c.svc.GetObjectInfo(ctx, "bkt", "mp", object.GetObjectOptions{})
	require.NoError(t, err)
	assert.Equal(t, mp.ETag, info.ETag)
	_, err = c.svc.GetObjectInfo(ctx, "bkt", "inline", object.GetObjectOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound, "the delete marker moved and is still the latest")

	// Moving again, or a key the pool does not hold, does nothing.
	res, err := m.MoveObject(ctx, object.MoveObjectInput{Bucket: "bkt", Key: "mp", From: 1})
	require.NoError(t, err)
	assert.Equal(t, object.MoveObjectResult{To: -1}, res)
	_, err = m.MoveObject(ctx, object.MoveObjectInput{Bucket: "bkt", Key: "mp", From: 7})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// versionTrace prints the versions of a listing.
func versionTrace(res domain.ListObjectsResult) []string {
	out := make([]string, 0, len(res.Objects))
	for _, o := range res.Objects {
		out = append(out, fmt.Sprintf("%s %s %v %d %s latest=%v marker=%v", o.Key, o.VersionID, o.ModTime.UnixNano(), o.Size, o.ETag, o.IsLatest, o.IsDeleteMarker))
	}
	return out
}

// TestDrainingPool: a draining pool receives no new key, version or
// upload; a key it holds gets its new versions elsewhere and the move
// joins the older ones to them; the null version written after a copy
// wins over the copy.
//
// covers: S-STO-09
func TestDrainingPool(t *testing.T) {
	ctx := context.Background()
	spec := clusterSpec{pools: 2, sets: 1, drives: 4, buckets: versionedBuckets(t), opts: []object.Option{object.WithPoolSpaceTTL(0)}}
	c := newCluster(t, spec)
	m := mover(t, c)
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	c.setPoolSpace(spec, 0, 10, 9)
	c.setPoolSpace(spec, 1, 10, 0.1)
	old := put(t, c.svc, "bkt", "k", payload(2*testBlockSize))
	require.Equal(t, []int{0}, c.poolOf(t, spec, "bkt", "k"))

	// Pool 1 has room now, pool 0 drains: new versions, keys and uploads
	// go to pool 1, even when pool 0 has more room.
	c.setPoolSpace(spec, 1, 10, 5)
	m.SetPoolDraining(0, true)
	usage := m.PoolUsage(ctx)
	require.Len(t, usage, 2)
	assert.True(t, usage[0].Draining)
	assert.False(t, usage[1].Draining)
	newer := put(t, c.svc, "bkt", "k", []byte("newer"))
	assert.Equal(t, []int{0, 1}, c.poolOf(t, spec, "bkt", "k"))
	put(t, c.svc, "bkt", "fresh", []byte("fresh"))
	assert.Equal(t, []int{1}, c.poolOf(t, spec, "bkt", "fresh"))
	up, err := c.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "k"})
	require.NoError(t, err)
	n, err := m.PoolUploads(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	n, err = m.PoolUploads(ctx, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	require.NoError(t, c.svc.AbortMultipartUpload(ctx, "bkt", "k", up.UploadID))

	got, _ := get(t, c.svc, "bkt", "k", object.GetObjectOptions{})
	assert.Equal(t, "newer", string(got))
	res, err := m.MoveObject(ctx, object.MoveObjectInput{Bucket: "bkt", Key: "k", From: 0})
	require.NoError(t, err)
	assert.Equal(t, 1, res.To, "joins the versions already in pool 1")
	assert.Equal(t, 1, res.Versions)
	assert.Equal(t, []int{1}, c.poolOf(t, spec, "bkt", "k"))
	got, _ = get(t, c.svc, "bkt", "k", object.GetObjectOptions{})
	assert.Equal(t, "newer", string(got))
	got, _ = get(t, c.svc, "bkt", "k", object.GetObjectOptions{VersionID: old.VersionID})
	assert.Equal(t, payload(2*testBlockSize), got)
	_ = newer

	// No pool to go to: the move fails and keeps the source.
	m.SetPoolDraining(1, true)
	put2 := func() {
		_, err := c.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "other", Reader: bytes.NewReader([]byte("x")), Size: 1})
		require.ErrorIs(t, err, domain.ErrStorageFull)
	}
	put2()
	_, err = m.MoveObject(ctx, object.MoveObjectInput{Bucket: "bkt", Key: "fresh", From: 1})
	require.ErrorIs(t, err, domain.ErrStorageFull)
	assert.Equal(t, []int{1}, c.poolOf(t, spec, "bkt", "fresh"))
	got, _ = get(t, c.svc, "bkt", "fresh", object.GetObjectOptions{})
	assert.Equal(t, "fresh", string(got))
}

// TestMoveObjectFailureKeepsSource: a target that cannot take the copy
// (its drives fail) leaves the source untouched and readable.
//
// covers: S-STO-09
func TestMoveObjectFailureKeepsSource(t *testing.T) {
	ctx := context.Background()
	spec := clusterSpec{pools: 2, sets: 1, drives: 4, opts: []object.Option{object.WithPoolSpaceTTL(0)}}
	c := newCluster(t, spec)
	m := mover(t, c)
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	c.setPoolSpace(spec, 0, 10, 9)
	c.setPoolSpace(spec, 1, 10, 0.1)
	data := payload(3 * testBlockSize)
	put(t, c.svc, "bkt", "k", data)
	c.setPoolSpace(spec, 1, 10, 9)
	// Three of the four drives of pool 1 are offline: no write quorum.
	c.setFaults(func(f *driveFaults) { f.offline = true }, 4, 5, 6)
	_, err := m.MoveObject(ctx, object.MoveObjectInput{Bucket: "bkt", Key: "k", From: 0, Targets: []int{1}})
	require.Error(t, err)
	assert.Equal(t, []int{0}, c.poolOf(t, spec, "bkt", "k"))
	got, _ := get(t, c.svc, "bkt", "k", object.GetObjectOptions{})
	assert.Equal(t, data, got)

	c.setFaults(func(f *driveFaults) { f.offline = false }, 4, 5, 6)
	res, err := m.MoveObject(ctx, object.MoveObjectInput{Bucket: "bkt", Key: "k", From: 0, Targets: []int{1}})
	require.NoError(t, err)
	assert.Equal(t, 1, res.To)
	got, _ = get(t, c.svc, "bkt", "k", object.GetObjectOptions{})
	assert.Equal(t, data, got)
}

// TestMoveObjectConcurrentWrites: writers keep adding versions and readers
// keep reading while every key is moved back and forth between the pools;
// no acknowledged version is lost and reads never fail.
//
// covers: S-STO-09, S-STO-10
func TestMoveObjectConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	spec := clusterSpec{pools: 2, sets: 2, drives: 4, buckets: versionedBuckets(t), opts: []object.Option{object.WithPoolSpaceTTL(0)}}
	c := newCluster(t, spec)
	m := mover(t, c)
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	c.setPoolSpace(spec, 0, 10, 9)
	c.setPoolSpace(spec, 1, 10, 9)
	keys := []string{"a", "b/c", "d", "e/f/g"}
	for _, k := range keys {
		put(t, c.svc, "bkt", k, []byte(k+"-0"))
	}

	var mu sync.Mutex
	written := map[string][]string{} // key → version ids acknowledged
	stop := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for w, k := range keys {
		wg.Add(2)
		go func() { // writer
			defer wg.Done()
			for i := 1; i <= 30; i++ {
				select {
				case <-stop:
					return
				default:
				}
				data := []byte(fmt.Sprintf("%s-%d", k, i))
				if i%3 == 0 {
					data = payload(testBlockSize + w + i)
				}
				info, err := c.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: k, Reader: bytes.NewReader(data), Size: int64(len(data))})
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				written[k] = append(written[k], info.VersionID)
				mu.Unlock()
			}
		}()
		go func() { // reader
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				r, err := c.svc.GetObject(ctx, "bkt", k, object.GetObjectOptions{})
				if err != nil {
					errs <- fmt.Errorf("read %s: %w", k, err)
					return
				}
				_, err = io.Copy(io.Discard, r)
				_ = r.Close()
				if err != nil {
					errs <- fmt.Errorf("read %s: %w", k, err)
					return
				}
			}
		}()
	}
	for round := 0; round < 6; round++ {
		from := round % 2
		for _, k := range keys {
			if _, err := m.MoveObject(ctx, object.MoveObjectInput{Bucket: "bkt", Key: k, From: from}); err != nil {
				t.Fatalf("move %s from pool %d: %v", k, from, err)
			}
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for _, k := range keys {
		for _, v := range written[k] {
			_, err := c.svc.GetObjectInfo(ctx, "bkt", k, object.GetObjectOptions{VersionID: v})
			assert.NoError(t, err, "%s version %s", k, v)
		}
		res, err := c.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "bkt", Prefix: k})
		require.NoError(t, err)
		n := 0
		for _, o := range res.Objects {
			if o.Key == k {
				n++
			}
		}
		assert.Equal(t, len(written[k])+1, n, "%s: every version listed once", k)
	}
}
