package object_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// memTiers is an object.TierStore in memory.
type memTiers struct {
	mu      sync.Mutex
	objects map[string][]byte
	refs    map[string]object.TierRef
	next    int
	// failPut fails the writes.
	failPut error
}

func newMemTiers() *memTiers {
	return &memTiers{objects: map[string][]byte{}, refs: map[string]object.TierRef{}}
}

func (m *memTiers) PutTiered(_ context.Context, tier string, r io.Reader, size int64, ref object.TierRef) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if int64(len(data)) != size {
		return "", fmt.Errorf("read %d bytes, %d declared", len(data), size)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPut != nil {
		return "", m.failPut
	}
	m.next++
	name := fmt.Sprintf("%s/obj-%d", tier, m.next)
	m.objects[name] = data
	m.refs[name] = ref
	return name, nil
}

func (m *memTiers) OpenTiered(_ context.Context, _, name string, offset, length int64) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrFaultyDisk, name)
	}
	if offset+length > int64(len(data)) {
		return nil, io.ErrUnexpectedEOF
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(data[offset : offset+length]))), nil
}

func (m *memTiers) DeleteTiered(_ context.Context, _, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, name)
	return nil
}

func (m *memTiers) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}

// dataDirs counts the data directories of key on the drive roots.
func dataDirs(t *testing.T, roots []string, key string) int {
	t.Helper()
	n := 0
	for _, root := range roots {
		files, err := filepath.Glob(filepath.Join(root, "bkt", key, "*", "part.*"))
		require.NoError(t, err)
		n += len(files)
	}
	return n
}

func readObj(t *testing.T, svc object.Service, key string, opts object.GetObjectOptions) []byte {
	t.Helper()
	r, err := svc.GetObject(context.Background(), "bkt", key, opts)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return data
}

// TestTransitionObject covers S-STO-12 in the object layer: the data of a
// version moves to its tier and is read back from there (whole, ranges
// across the parts), the local data goes, the version reports the tier as
// its storage class, and a copy brings it back.
//
// covers: S-STO-12
func TestTransitionObject(t *testing.T) {
	ctx := context.Background()
	for _, kind := range implKinds() {
		t.Run(kind, func(t *testing.T) {
			tiers := newMemTiers()
			fx := newFixtureDeps(t, kind, func(d *object.Deps) { d.Tiers = tiers })
			require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			body := payload(5*testBlockSize + 123)
			info, err := fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "big", Reader: bytes.NewReader(body), Size: int64(len(body))})
			require.NoError(t, err)
			require.Positive(t, dataDirs(t, fx.roots, "big"))

			got, err := fx.svc.TransitionObject(ctx, object.TransitionInput{Bucket: "bkt", Key: "big", ModTime: info.ModTime, Tier: "COLD"})
			require.NoError(t, err)
			assert.Equal(t, "COLD", got.StorageClass)
			assert.Equal(t, 1, tiers.count())
			assert.Zero(t, dataDirs(t, fx.roots, "big"), "the local data is released")

			head, err := fx.svc.GetObjectInfo(ctx, "bkt", "big", object.GetObjectOptions{})
			require.NoError(t, err)
			assert.Equal(t, "COLD", head.StorageClass)
			assert.Equal(t, info.ETag, head.ETag)
			assert.Equal(t, int64(len(body)), head.Size)
			assert.False(t, head.Inlined)
			assert.Equal(t, body, readObj(t, fx.svc, "big", object.GetObjectOptions{}))
			rng := &domain.Range{Start: 4000, End: 9000}
			assert.Equal(t, body[4000:9001], readObj(t, fx.svc, "big", object.GetObjectOptions{Range: rng}))

			loc, err := fx.svc.TierLocation(ctx, "bkt", "big", "")
			require.NoError(t, err)
			assert.True(t, loc.Exists)
			assert.Equal(t, "COLD", loc.Tier)
			assert.NotEmpty(t, loc.Object)

			// Again: nothing changes.
			_, err = fx.svc.TransitionObject(ctx, object.TransitionInput{Bucket: "bkt", Key: "big", Tier: "COLD"})
			require.NoError(t, err)
			assert.Equal(t, 1, tiers.count())
			_, err = fx.svc.TransitionObject(ctx, object.TransitionInput{Bucket: "bkt", Key: "big", Tier: "OTHER"})
			require.ErrorIs(t, err, domain.ErrInvalidArgument)

			// A copy onto itself stores the data on the drives again.
			restored, err := fx.svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "bkt", SrcKey: "big", DstBucket: "bkt", DstKey: "big"})
			require.NoError(t, err)
			assert.Equal(t, domain.StorageClassStandard, restored.StorageClass)
			assert.Positive(t, dataDirs(t, fx.roots, "big"))
			assert.Equal(t, body, readObj(t, fx.svc, "big", object.GetObjectOptions{}))
			loc, err = fx.svc.TierLocation(ctx, "bkt", "big", "")
			require.NoError(t, err)
			assert.True(t, loc.Exists)
			assert.Empty(t, loc.Object, "the tier object is an orphan now")
		})
	}
}

// TestTransitionObjectMultipartCompressed: a multipart, compressed version
// is transitioned as stored and read back across its parts.
//
// covers: S-STO-12
func TestTransitionObjectMultipartCompressed(t *testing.T) {
	ctx := context.Background()
	tiers := newMemTiers()
	fx := newFixtureDeps(t, "erasure", func(d *object.Deps) { d.Tiers = tiers }, object.WithCompression(compressTxt))
	require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "doc.txt"})
	require.NoError(t, err)
	var body []byte
	var parts []domain.CompletePart
	for n := 1; n <= 3; n++ {
		chunk := bytes.Repeat([]byte(fmt.Sprintf("part %d of the document; ", n)), 700)
		body = append(body, chunk...)
		p, err := fx.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "doc.txt", UploadID: up.UploadID, PartNumber: n, Reader: bytes.NewReader(chunk), Size: int64(len(chunk))})
		require.NoError(t, err)
		parts = append(parts, domain.CompletePart{Number: n, ETag: p.ETag})
	}
	_, err = fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "doc.txt", UploadID: up.UploadID, Parts: parts})
	require.NoError(t, err)
	_, err = fx.svc.TransitionObject(ctx, object.TransitionInput{Bucket: "bkt", Key: "doc.txt", Tier: "COLD"})
	require.NoError(t, err)
	assert.Zero(t, dataDirs(t, fx.roots, "doc.txt"))
	assert.Equal(t, body, readObj(t, fx.svc, "doc.txt", object.GetObjectOptions{}))
	start := int64(len(body)/3 - 50)
	rng := &domain.Range{Start: start, End: start + 20000}
	assert.Equal(t, body[start:start+20001], readObj(t, fx.svc, "doc.txt", object.GetObjectOptions{Range: rng}))
	assert.Equal(t, body[:len(body)/3], readObj(t, fx.svc, "doc.txt", object.GetObjectOptions{PartNumber: 1}))
}

// TestTransitionObjectRefusals: a version changed since its evaluation, a
// version stored inline, a missing tier store, a failed write to the tier:
// nothing changes.
//
// covers: S-STO-12
func TestTransitionObjectRefusals(t *testing.T) {
	ctx := context.Background()
	tiers := newMemTiers()
	fx := newFixtureDeps(t, "erasure", func(d *object.Deps) { d.Tiers = tiers })
	require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	body := payload(3 * testBlockSize)
	info, err := fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader(body), Size: int64(len(body))})
	require.NoError(t, err)

	_, err = fx.svc.TransitionObject(ctx, object.TransitionInput{Bucket: "bkt", Key: "k", ModTime: info.ModTime.Add(-1), Tier: "COLD"})
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	_, err = fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "small", Reader: bytes.NewReader([]byte("x")), Size: 1})
	require.NoError(t, err)
	_, err = fx.svc.TransitionObject(ctx, object.TransitionInput{Bucket: "bkt", Key: "small", Tier: "COLD"})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	tiers.failPut = errors.New("injected: tier unreachable")
	_, err = fx.svc.TransitionObject(ctx, object.TransitionInput{Bucket: "bkt", Key: "k", Tier: "COLD"})
	require.Error(t, err)
	assert.Zero(t, tiers.count())
	assert.Positive(t, dataDirs(t, fx.roots, "k"))
	assert.Equal(t, body, readObj(t, fx.svc, "k", object.GetObjectOptions{}))

	plain := newFixture(t, "fs")
	_, err = plain.svc.TransitionObject(ctx, object.TransitionInput{Bucket: "bkt", Key: "k", Tier: "COLD"})
	require.ErrorIs(t, err, domain.ErrNotImplemented)
}

// TestTransitionInterrupted: a transition whose release of the local data
// reached only some drives keeps the version readable (from the tier) and
// agreed on, and the healing releases the data left on the other drives.
//
// covers: S-STO-12
func TestTransitionInterrupted(t *testing.T) {
	ctx := context.Background()
	tiers := newMemTiers()
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4, disk: true})
	// The tier store is a dependency of the layer: rebuild it with one.
	svc, err := object.NewServerPools(object.PoolsDeps{
		Pools: []object.PoolSpec{{Sets: [][]storage.StorageAPI{c.drives}}}, DeploymentID: "deployment-test", Codecs: testCodecs("deployment-test"),
		Heal: c.heal, Tiers: tiers,
	}, object.WithInlineLimit(testInlineLimit), object.WithBlockSize(testBlockSize), object.WithMinPartSize(16))
	require.NoError(t, err)
	require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	body := payload(4 * testBlockSize)
	_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader(body), Size: int64(len(body))})
	require.NoError(t, err)

	c.setFaults(func(f *driveFaults) { f.failWriteMetadata = fmt.Errorf("%w: injected", domain.ErrFaultyDisk) }, 0, 1, 2)
	info, err := svc.TransitionObject(ctx, object.TransitionInput{Bucket: "bkt", Key: "k", Tier: "COLD"})
	require.NoError(t, err, "the tier is recorded: the transition stands")
	assert.Equal(t, "COLD", info.StorageClass)
	c.setFaults(func(f *driveFaults) { f.failWriteMetadata = nil }, 0, 1, 2)
	assert.Equal(t, 3, partsOn(t, c.roots, "k"), "three drives kept their shard")

	assert.Equal(t, body, readObj(t, svc, "k", object.GetObjectOptions{}))
	res, err := healerOf(t, svc).HealObject(ctx, "bkt", "k", "", object.HealOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, res.Healed)
	assert.Zero(t, partsOn(t, c.roots, "k"), "the healing released the local data")
	assert.Equal(t, body, readObj(t, svc, "k", object.GetObjectOptions{}))
	head, err := svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{})
	require.NoError(t, err)
	assert.Equal(t, "COLD", head.StorageClass)
}

// partsOn counts the drives holding a shard of key.
func partsOn(t *testing.T, roots []string, key string) int {
	t.Helper()
	n := 0
	for _, root := range roots {
		files, err := filepath.Glob(filepath.Join(root, "bkt", key, "*", "part.1"))
		require.NoError(t, err)
		if len(files) > 0 {
			n++
		}
	}
	return n
}

// TestMoveTransitionedObject: a pool move carries a transitioned version
// as its metadata, the data staying in the tier.
//
// covers: S-STO-12
func TestMoveTransitionedObject(t *testing.T) {
	ctx := context.Background()
	tiers := newMemTiers()
	spec := clusterSpec{pools: 2, sets: 1, drives: 4, tiers: tiers, opts: []object.Option{object.WithPoolSpaceTTL(0)}}
	c := newCluster(t, spec)
	m := mover(t, c)
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	c.setPoolSpace(spec, 0, 10, 0.1)
	c.setPoolSpace(spec, 1, 10, 9)
	body := payload(3*testBlockSize + 9)
	put(t, c.svc, "bkt", "cold.bin", body)
	_, err := c.svc.TransitionObject(ctx, object.TransitionInput{Bucket: "bkt", Key: "cold.bin", Tier: "COLD"})
	require.NoError(t, err)
	before, err := c.svc.TierLocation(ctx, "bkt", "cold.bin", "")
	require.NoError(t, err)

	c.setPoolSpace(spec, 0, 10, 9)
	res, err := m.MoveObject(ctx, object.MoveObjectInput{Bucket: "bkt", Key: "cold.bin", From: 1})
	require.NoError(t, err)
	assert.Equal(t, 0, res.To)
	after, err := c.svc.TierLocation(ctx, "bkt", "cold.bin", "")
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Equal(t, 1, tiers.count())
	assert.Equal(t, body, readObj(t, c.svc, "cold.bin", object.GetObjectOptions{}))
}
