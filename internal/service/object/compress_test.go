package object_test

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// compressTxt compresses the keys ending in .txt.
func compressTxt(_ context.Context, _, key, _ string) bool { return strings.HasSuffix(key, ".txt") }

// compressible returns n bytes of text: lines of numbered random words,
// different in every block of the stream, compressing about 3 to 1.
func compressible(n int, seed uint64) []byte {
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliett"}
	r := rand.New(rand.NewPCG(seed, 7)) // #nosec G404 -- test data
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "%08d %s %s %s\n", i, words[r.IntN(len(words))], words[r.IntN(len(words))], words[r.IntN(len(words))])
	}
	return b.Bytes()[:n]
}

// diskBytes sums the sizes of the files of the drives.
func diskBytes(t *testing.T, roots []string) int64 {
	t.Helper()
	var total int64
	for _, root := range roots {
		require.NoError(t, filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			if err == nil {
				total += info.Size()
			}
			return err
		}))
	}
	return total
}

// TestCompressedObjects (S-OBJ-14): the versions the policy selects are
// stored compressed and read back whole or by range, with the sizes, ETag
// and listings of their content; the others, the small and the encrypted
// ones are stored as they are.
func TestCompressedObjects(t *testing.T) {
	forEachImplWith(t, []object.Option{object.WithCompression(compressTxt)}, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "cmp", object.MakeBucketOptions{}))
		big := compressible(3<<20+777, 1)
		before := diskBytes(t, fx.roots)
		info := put(t, fx.svc, "cmp", "big.txt", big)
		stored := diskBytes(t, fx.roots) - before
		assert.Equal(t, int64(len(big)), info.Size)
		assert.Equal(t, md5hex(big), info.ETag, "the ETag is the MD5 of the content")
		// Uncompressed, the drives would hold the content once (a single
		// drive) or twice (2 data and 2 parity shards).
		assert.Less(t, stored, int64(len(big)), "the data is stored compressed (%d bytes on the drives)", stored)

		head, err := fx.svc.GetObjectInfo(ctx, "cmp", "big.txt", object.GetObjectOptions{})
		require.NoError(t, err)
		assert.Equal(t, int64(len(big)), head.Size)
		got, _ := get(t, fx.svc, "cmp", "big.txt", object.GetObjectOptions{})
		require.True(t, bytes.Equal(big, got), "the content is read back")

		// Ranges in the first block, across the blocks (1 MiB) and at the
		// end.
		size := int64(len(big))
		for _, rg := range []domain.Range{
			{Start: 0, End: 0}, {Start: 5, End: 1000}, {Start: 1<<20 - 3, End: 1<<20 + 3}, {Start: 1 << 20, End: 2<<20 + 17},
			{Start: 2<<20 + 1, End: size - 1}, {Start: size - 1, End: size - 1}, {IsSuffix: true, SuffixLength: 4096, End: -1},
		} {
			part, r := get(t, fx.svc, "cmp", "big.txt", object.GetObjectOptions{Range: &rg})
			off, length, err := rg.Resolve(size)
			require.NoError(t, err)
			require.True(t, bytes.Equal(big[off:off+length], part), "range %+v", rg)
			assert.Equal(t, off, r.Offset)
		}

		// Small bodies (inlined), other keys and encrypted versions are
		// not compressed, and read back alike.
		for key, data := range map[string][]byte{"small.txt": compressible(500, 2), "mid.txt": compressible(5000, 3), "other.bin": compressible(70000, 4)} {
			info := put(t, fx.svc, "cmp", key, data)
			assert.Equal(t, int64(len(data)), info.Size, key)
			got, _ := get(t, fx.svc, "cmp", key, object.GetObjectOptions{})
			assert.True(t, bytes.Equal(data, got), key)
		}
		enc := compressible(200000, 5)
		info = put(t, fx.svc, "cmp", "enc.txt", enc, func(in *object.PutObjectInput) { in.SSE = domain.SSERequest{Type: domain.SSES3} })
		assert.Equal(t, domain.SSES3, info.Encryption.Type)
		got, _ = get(t, fx.svc, "cmp", "enc.txt", object.GetObjectOptions{})
		assert.True(t, bytes.Equal(enc, got))

		// A body of unknown size.
		unknown := compressible(300000, 6)
		info, err = fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "cmp", Key: "unknown.txt", Reader: bytes.NewReader(unknown), Size: -1})
		require.NoError(t, err)
		assert.Equal(t, int64(len(unknown)), info.Size)
		got, _ = get(t, fx.svc, "cmp", "unknown.txt", object.GetObjectOptions{})
		assert.True(t, bytes.Equal(unknown, got))

		// A body shorter than its declared size is refused.
		_, err = fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "cmp", Key: "short.txt", Reader: bytes.NewReader(big[:200000]), Size: 300000})
		require.ErrorIs(t, err, domain.ErrInvalidArgument)
		assertNoTmpLeft(t, fx)

		// Listings and the data usage report the sizes of the content.
		list, err := fx.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "cmp", Prefix: "big"})
		require.NoError(t, err)
		require.Len(t, list.Objects, 1)
		assert.Equal(t, int64(len(big)), list.Objects[0].Size)
		usage, err := fx.svc.DataUsage(ctx, "cmp")
		require.NoError(t, err)
		assert.Equal(t, uint64(len(big)+500+5000+70000+len(enc)+len(unknown)), usage[0].Size)

		// A copy to a key the policy does not select is stored as it is.
		copied, err := fx.svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "cmp", SrcKey: "big.txt", DstBucket: "cmp", DstKey: "copy.bin"})
		require.NoError(t, err)
		assert.Equal(t, info.Size, int64(len(unknown)))
		assert.Equal(t, int64(len(big)), copied.Size)
		got, _ = get(t, fx.svc, "cmp", "copy.bin", object.GetObjectOptions{})
		assert.True(t, bytes.Equal(big, got))
	})
}

// TestCompressedMultipart: every part of an upload the policy selects is
// compressed on its own; the object reads back whole, across the parts
// and by part number, with the part sizes of the content.
func TestCompressedMultipart(t *testing.T) {
	forEachImplWith(t, []object.Option{object.WithCompression(compressTxt)}, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "mpc", object.MakeBucketOptions{}))
		up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "mpc", Key: "obj.txt"})
		require.NoError(t, err)
		partData := map[int][]byte{1: compressible(1<<20+5000, 11), 2: compressible(700, 12), 3: compressible(200000, 13), 4: nil}
		etags := map[int]string{}
		for _, n := range []int{3, 1, 2, 4} {
			p, err := fx.svc.PutObjectPart(ctx, object.PutObjectPartInput{
				Bucket: "mpc", Key: "obj.txt", UploadID: up.UploadID, PartNumber: n,
				Reader: bytes.NewReader(partData[n]), Size: int64(len(partData[n])),
			})
			require.NoError(t, err)
			assert.Equal(t, int64(len(partData[n])), p.Size, "parts report the size of their content")
			etags[n] = p.ETag
		}
		parts, err := fx.svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "mpc", Key: "obj.txt", UploadID: up.UploadID})
		require.NoError(t, err)
		require.Len(t, parts.Parts, 4)
		assert.Equal(t, int64(700), parts.Parts[1].Size)

		// Parts 1, 3 and the empty part 4.
		info, err := fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{
			Bucket: "mpc", Key: "obj.txt", UploadID: up.UploadID,
			Parts: []domain.CompletePart{{Number: 1, ETag: etags[1]}, {Number: 3, ETag: etags[3]}, {Number: 4, ETag: etags[4]}},
		})
		require.NoError(t, err)
		want := append(append([]byte(nil), partData[1]...), partData[3]...)
		assert.Equal(t, int64(len(want)), info.Size)

		got, _ := get(t, fx.svc, "mpc", "obj.txt", object.GetObjectOptions{})
		require.True(t, bytes.Equal(want, got))
		for _, rg := range []domain.Range{
			{Start: int64(len(partData[1])) - 10, End: int64(len(partData[1])) + 10},
			{Start: 1<<20 + 1, End: int64(len(want)) - 1},
			{Start: 3, End: 1 << 20},
		} {
			got, _ = get(t, fx.svc, "mpc", "obj.txt", object.GetObjectOptions{Range: &rg})
			require.True(t, bytes.Equal(want[rg.Start:rg.End+1], got), "range %+v", rg)
		}
		got, r := get(t, fx.svc, "mpc", "obj.txt", object.GetObjectOptions{PartNumber: 2})
		assert.True(t, bytes.Equal(partData[3], got), "part 2 of the object is part 3 of the upload")
		assert.Equal(t, int64(len(partData[1])), r.Offset)
		got, _ = get(t, fx.svc, "mpc", "obj.txt", object.GetObjectOptions{PartNumber: 3})
		assert.Empty(t, got)
		attrs, err := fx.svc.GetObjectAttributes(ctx, "mpc", "obj.txt", object.AttributesOptions{})
		require.NoError(t, err)
		require.Len(t, attrs.Parts, 3)
		assert.Equal(t, int64(len(partData[3])), attrs.Parts[1].Size)
		assert.Equal(t, int64(len(want)), attrs.Info.Size)
	})
}
