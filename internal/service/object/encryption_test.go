package object_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/crypto"
	"github.com/r3n3r0/nexo/internal/service/kms"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// newTestCrypto returns the crypto service of the fixtures: a builtin KMS
// with a random master key and the key "tenant".
func newTestCrypto(t *testing.T) crypto.Service {
	t.Helper()
	secret := make([]byte, 32)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	master, err := kms.ParseMasterKey("test-master:" + base64.StdEncoding.EncodeToString(secret))
	require.NoError(t, err)
	k, err := kms.NewBuiltin(kms.BuiltinDeps{Logger: slog.New(slog.DiscardHandler)}, kms.WithMasterKey(master))
	require.NoError(t, err)
	require.NoError(t, k.CreateKey(context.Background(), "tenant"))
	return crypto.New(crypto.Deps{KMS: k, Logger: slog.New(slog.DiscardHandler)})
}

func testCustomerKey(t *testing.T) domain.SSECustomerKey {
	t.Helper()
	raw := make([]byte, 32)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	k, err := domain.NewSSECustomerKey(raw, "")
	require.NoError(t, err)
	return k
}

// marked returns n bytes made of a known marker, so that a plaintext
// leak shows up in any file of the drives (erasure data shards are plain
// slices of the stored content).
func marked(n int) []byte {
	return bytes.Repeat([]byte("NEXO-PLAINTEXT-MARKER;"), n/22+1)[:n]
}

// assertNoPlaintextOnDisk scans every file of the drives for the marker.
func assertNoPlaintextOnDisk(t *testing.T, roots []string) {
	t.Helper()
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			assert.False(t, bytes.Contains(data, []byte("NEXO-PLAINTEXT-MARKER")), "plaintext found in %s", path)
			return nil
		})
		require.NoError(t, err)
	}
}

// TestEncryptedObjects (TS-U-13): every kind of server-side encryption
// round-trips on every implementation, sizes and ranges are those of the
// plaintext, the drives never hold it, and SSE-C reads need their key.
func TestEncryptedObjects(t *testing.T) {
	sizes := []int{0, 10, testInlineLimit - 100, 5000, crypto.PackageSize + 3000}
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "encb", object.MakeBucketOptions{}))
		ckey := testCustomerKey(t)
		kinds := []struct {
			name string
			sse  domain.SSERequest
			read domain.SSECustomerKey
		}{
			{name: "SSE-S3", sse: domain.SSERequest{Type: domain.SSES3}},
			{name: "SSE-KMS", sse: domain.SSERequest{Type: domain.SSEKMS, KMSKeyID: "tenant", Context: map[string]string{"app": "test"}}},
			{name: "SSE-C", sse: domain.SSERequest{Type: domain.SSEC, CustomerKey: ckey}, read: ckey},
		}
		for _, kind := range kinds {
			for _, size := range sizes {
				data := marked(size)
				key := fmt.Sprintf("%s/%d", kind.name, size)
				info := put(t, fx.svc, "encb", key, data, func(in *object.PutObjectInput) { in.SSE = kind.sse })
				assert.Equal(t, int64(size), info.Size, "%s %d", kind.name, size)
				assert.Equal(t, kind.sse.Type, info.Encryption.Type)
				if kind.sse.Type == domain.SSES3 {
					assert.Equal(t, md5hex(data), info.ETag, "SSE-S3 ETag is the MD5 of the plaintext")
				} else {
					assert.NotEqual(t, md5hex(data), info.ETag, "%s ETag hides the MD5", kind.name)
				}
				if kind.sse.Type == domain.SSEKMS {
					assert.Equal(t, "tenant", info.Encryption.KMSKeyID)
					assert.Equal(t, map[string]string{"app": "test"}, info.Encryption.Context)
				}

				head, err := fx.svc.GetObjectInfo(ctx, "encb", key, object.GetObjectOptions{SSECustomerKey: kind.read})
				require.NoError(t, err)
				assert.Equal(t, int64(size), head.Size)
				assert.Equal(t, info.ETag, head.ETag)
				got, r := get(t, fx.svc, "encb", key, object.GetObjectOptions{SSECustomerKey: kind.read})
				assert.True(t, bytes.Equal(data, got), "%s %d", kind.name, size)
				assert.Equal(t, kind.sse.Type, r.Info.Encryption.Type)
				if kind.sse.Type == domain.SSEC {
					assert.Equal(t, ckey.KeyMD5, r.Info.Encryption.CustomerKeyMD5)
				}
				if size > 2 {
					for _, rg := range []domain.Range{{Start: 1, End: int64(size) - 2}, {Start: int64(size) - 1, End: int64(size) - 1}, {IsSuffix: true, SuffixLength: 2, End: -1}} {
						part, r := get(t, fx.svc, "encb", key, object.GetObjectOptions{Range: &rg, SSECustomerKey: kind.read})
						off, length, err := rg.Resolve(int64(size))
						require.NoError(t, err)
						assert.Equal(t, data[off:off+length], part)
						assert.Equal(t, off, r.Offset)
					}
				}
			}
		}
		// Ranges around the 64 KiB package boundaries.
		big := marked(3*crypto.PackageSize + 7)
		put(t, fx.svc, "encb", "big", big, func(in *object.PutObjectInput) { in.SSE = domain.SSERequest{Type: domain.SSES3} })
		for _, start := range []int64{0, crypto.PackageSize - 1, crypto.PackageSize, crypto.PackageSize + 1, 2*crypto.PackageSize - 1, 3 * crypto.PackageSize} {
			for _, end := range []int64{start, start + 1, crypto.PackageSize * 2, int64(len(big)) - 1} {
				if end < start {
					continue
				}
				rg := domain.Range{Start: start, End: end}
				got, _ := get(t, fx.svc, "encb", "big", object.GetObjectOptions{Range: &rg})
				require.Equal(t, big[start:end+1], got, "range %d-%d", start, end)
			}
		}

		// SSE-C reads need the key, and only the right one.
		cases := []struct {
			name string
			key  domain.SSECustomerKey
			obj  string
			want error
			code string
		}{
			{name: "no key", obj: "SSE-C/5000", want: domain.ErrInvalidArgument, code: "InvalidRequest"},
			{name: "wrong key", obj: "SSE-C/5000", key: testCustomerKey(t), want: domain.ErrAccessDenied, code: "AccessDenied"},
			{name: "key of an SSE-S3 object", obj: "big", key: ckey, want: domain.ErrInvalidArgument, code: "InvalidRequest"},
		}
		for _, c := range cases {
			_, err := fx.svc.GetObject(ctx, "encb", c.obj, object.GetObjectOptions{SSECustomerKey: c.key})
			require.ErrorIs(t, err, c.want, c.name)
			assert.Equal(t, c.code, s3Code(err), c.name)
			_, err = fx.svc.GetObjectInfo(ctx, "encb", c.obj, object.GetObjectOptions{SSECustomerKey: c.key})
			require.ErrorIs(t, err, c.want, "head: "+c.name)
		}

		// Listings and the data usage report the plaintext sizes.
		list, err := fx.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "encb", Prefix: "big"})
		require.NoError(t, err)
		require.Len(t, list.Objects, 1)
		assert.Equal(t, int64(len(big)), list.Objects[0].Size)
		assert.Equal(t, domain.SSES3, list.Objects[0].Encryption.Type)
		usage, err := fx.svc.DataUsage(ctx, "encb")
		require.NoError(t, err)
		var total uint64
		for _, kind := range kinds {
			_ = kind
			for _, size := range sizes {
				total += uint64(size)
			}
		}
		assert.Equal(t, total+uint64(len(big)), usage[0].Size)

		assertNoPlaintextOnDisk(t, fx.roots)
	})
}

func TestBucketDefaultEncryption(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "defb", object.MakeBucketOptions{}))
		fx.setBucketConfig("defb", object.BucketConfig{Encryption: domain.BucketEncryption{Algorithm: domain.SSEAlgorithmKMS, KMSKeyID: "tenant"}})
		data := marked(4000)
		info := put(t, fx.svc, "defb", "k", data)
		assert.Equal(t, domain.SSEKMS, info.Encryption.Type, "the default encryption of the bucket applies")
		assert.Equal(t, "tenant", info.Encryption.KMSKeyID)
		ckey := testCustomerKey(t)
		info = put(t, fx.svc, "defb", "c", data, func(in *object.PutObjectInput) { in.SSE = domain.SSERequest{Type: domain.SSEC, CustomerKey: ckey} })
		assert.Equal(t, domain.SSEC, info.Encryption.Type, "the request wins over the default")
		got, _ := get(t, fx.svc, "defb", "k", object.GetObjectOptions{})
		assert.Equal(t, data, got)
		assertNoPlaintextOnDisk(t, fx.roots)
	})
}

// TestEncryptedMultipart: every part is encrypted with the key of its
// number in the upload, the completion may select and renumber parts, and
// the reads of the completed object (ranges across parts, one part) see
// the plaintext.
func TestEncryptedMultipart(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "mpb", object.MakeBucketOptions{}))
		ckey := testCustomerKey(t)
		for _, sse := range []domain.SSERequest{{Type: domain.SSES3}, {Type: domain.SSEC, CustomerKey: ckey}} {
			up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "mpb", Key: "obj", SSE: sse})
			require.NoError(t, err)
			assert.Equal(t, sse.Type, up.Encryption.Type)
			partData := map[int][]byte{1: marked(crypto.PackageSize + 100), 2: marked(500), 3: marked(2000)}
			etags := map[int]string{}
			for _, n := range []int{3, 1, 2} {
				p, err := fx.svc.PutObjectPart(ctx, object.PutObjectPartInput{
					Bucket: "mpb", Key: "obj", UploadID: up.UploadID, PartNumber: n,
					Reader: bytes.NewReader(partData[n]), Size: int64(len(partData[n])), SSECustomerKey: sse.CustomerKey,
				})
				require.NoError(t, err)
				assert.Equal(t, int64(len(partData[n])), p.Size, "parts report their plaintext size")
				etags[n] = p.ETag
			}
			if sse.Type == domain.SSEC {
				_, err := fx.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "mpb", Key: "obj", UploadID: up.UploadID, PartNumber: 4, Reader: bytes.NewReader([]byte("x")), Size: 1})
				require.ErrorIs(t, err, domain.ErrInvalidArgument, "the parts of an SSE-C upload need its key")
			}
			parts, err := fx.svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "mpb", Key: "obj", UploadID: up.UploadID})
			require.NoError(t, err)
			require.Len(t, parts.Parts, 3)
			assert.Equal(t, int64(500), parts.Parts[1].Size)

			// Parts 1 and 3 only: part 3 becomes part 2 of the object.
			info, err := fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{
				Bucket: "mpb", Key: "obj", UploadID: up.UploadID,
				Parts: []domain.CompletePart{{Number: 1, ETag: etags[1]}, {Number: 3, ETag: etags[3]}},
			})
			require.NoError(t, err)
			want := append(append([]byte(nil), partData[1]...), partData[3]...)
			assert.Equal(t, int64(len(want)), info.Size)
			assert.Equal(t, sse.Type, info.Encryption.Type)
			assert.Equal(t, 2, info.Parts)

			got, _ := get(t, fx.svc, "mpb", "obj", object.GetObjectOptions{SSECustomerKey: sse.CustomerKey})
			require.True(t, bytes.Equal(want, got))
			rg := domain.Range{Start: int64(len(partData[1])) - 10, End: int64(len(partData[1])) + 10}
			got, _ = get(t, fx.svc, "mpb", "obj", object.GetObjectOptions{Range: &rg, SSECustomerKey: sse.CustomerKey})
			assert.Equal(t, want[rg.Start:rg.End+1], got, "a range across the parts")
			got, r := get(t, fx.svc, "mpb", "obj", object.GetObjectOptions{PartNumber: 2, SSECustomerKey: sse.CustomerKey})
			assert.Equal(t, partData[3], got, "part 2 of the object is part 3 of the upload")
			assert.Equal(t, int64(len(partData[1])), r.Offset)
			attrs, err := fx.svc.GetObjectAttributes(ctx, "mpb", "obj", object.AttributesOptions{})
			require.NoError(t, err)
			require.Len(t, attrs.Parts, 2)
			assert.Equal(t, int64(len(partData[3])), attrs.Parts[1].Size)
		}
		assertNoPlaintextOnDisk(t, fx.roots)
	})
}

// TestEncryptedCopy: a copy decrypts its source with the source key and
// encrypts the copy with its own encryption: SSE-C to SSE-S3, and the
// rotation of an SSE-C key in place.
func TestEncryptedCopy(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "cpb", object.MakeBucketOptions{}))
		oldKey, newKey := testCustomerKey(t), testCustomerKey(t)
		data := marked(crypto.PackageSize + 77)
		put(t, fx.svc, "cpb", "src", data, func(in *object.PutObjectInput) { in.SSE = domain.SSERequest{Type: domain.SSEC, CustomerKey: oldKey} })

		_, err := fx.svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "cpb", SrcKey: "src", DstBucket: "cpb", DstKey: "s3"})
		require.ErrorIs(t, err, domain.ErrInvalidArgument, "the source needs its key")
		info, err := fx.svc.CopyObject(ctx, object.CopyObjectInput{
			SrcBucket: "cpb", SrcKey: "src", SrcSSECustomerKey: oldKey, DstBucket: "cpb", DstKey: "s3",
			SSE: domain.SSERequest{Type: domain.SSES3},
		})
		require.NoError(t, err)
		assert.Equal(t, domain.SSES3, info.Encryption.Type)
		assert.Equal(t, md5hex(data), info.ETag)
		got, _ := get(t, fx.svc, "cpb", "s3", object.GetObjectOptions{})
		assert.Equal(t, data, got)

		// Key rotation: a copy onto itself with a new key is legal.
		_, err = fx.svc.CopyObject(ctx, object.CopyObjectInput{
			SrcBucket: "cpb", SrcKey: "src", SrcSSECustomerKey: oldKey, DstBucket: "cpb", DstKey: "src",
			SSE: domain.SSERequest{Type: domain.SSEC, CustomerKey: newKey},
		})
		require.NoError(t, err)
		got, _ = get(t, fx.svc, "cpb", "src", object.GetObjectOptions{SSECustomerKey: newKey})
		assert.Equal(t, data, got)
		_, err = fx.svc.GetObject(ctx, "cpb", "src", object.GetObjectOptions{SSECustomerKey: oldKey})
		require.ErrorIs(t, err, domain.ErrAccessDenied, "the old key is gone")

		// UploadPartCopy from an SSE-C source into an SSE-S3 upload.
		up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "cpb", Key: "mpb", SSE: domain.SSERequest{Type: domain.SSES3}})
		require.NoError(t, err)
		rg := domain.Range{Start: 10, End: 5009}
		p, err := fx.svc.CopyObjectPart(ctx, object.CopyObjectPartInput{
			SrcBucket: "cpb", SrcKey: "src", SrcSSECustomerKey: newKey, SrcRange: &rg,
			DstBucket: "cpb", DstKey: "mpb", UploadID: up.UploadID, PartNumber: 1,
		})
		require.NoError(t, err)
		_, err = fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "cpb", Key: "mpb", UploadID: up.UploadID, Parts: []domain.CompletePart{{Number: 1, ETag: p.ETag}}})
		require.NoError(t, err)
		got, _ = get(t, fx.svc, "cpb", "mpb", object.GetObjectOptions{})
		assert.Equal(t, data[10:5010], got)
		assertNoPlaintextOnDisk(t, fx.roots)
	})
}

// TestEncryptionWithoutKMS: without KMS, SSE-S3 and SSE-KMS are refused
// before anything is written, SSE-C works; without the crypto service at
// all, every encryption request is refused.
func TestEncryptionWithoutKMS(t *testing.T) {
	ctx := context.Background()
	drives, _ := newDrives(t, 1)
	svc, err := object.NewFS(object.Deps{Store: drives[0], Crypto: crypto.New(crypto.Deps{Logger: slog.New(slog.DiscardHandler)}), Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	for _, sse := range []domain.SSERequest{{Type: domain.SSES3}, {Type: domain.SSEKMS}} {
		_, err := svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader([]byte("x")), Size: 1, SSE: sse})
		require.ErrorIs(t, err, domain.ErrKMSNotConfigured, string(sse.Type))
		_, err = svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "k", SSE: sse})
		require.ErrorIs(t, err, domain.ErrKMSNotConfigured, string(sse.Type))
	}
	_, err = svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{})
	require.ErrorIs(t, err, domain.ErrObjectNotFound, "nothing was written")
	ckey := testCustomerKey(t)
	put(t, svc, "bkt", "c", []byte("customer data"), func(in *object.PutObjectInput) { in.SSE = domain.SSERequest{Type: domain.SSEC, CustomerKey: ckey} })
	got, _ := get(t, svc, "bkt", "c", object.GetObjectOptions{SSECustomerKey: ckey})
	assert.Equal(t, []byte("customer data"), got)

	plain, err := object.NewFS(object.Deps{Store: drives[0], Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	_, err = plain.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader([]byte("x")), Size: 1, SSE: domain.SSERequest{Type: domain.SSEC, CustomerKey: ckey}})
	require.ErrorIs(t, err, domain.ErrNotImplemented)
	_, err = plain.GetObject(ctx, "bkt", "c", object.GetObjectOptions{SSECustomerKey: ckey})
	require.ErrorIs(t, err, domain.ErrNotImplemented)
}

// TestEncryptedDataTampering: a modified byte of the stored ciphertext
// fails the read instead of returning altered data.
func TestEncryptedDataTampering(t *testing.T) {
	fx := newFixture(t, "fs")
	ctx := context.Background()
	require.NoError(t, fx.svc.MakeBucket(ctx, "tmp", object.MakeBucketOptions{}))
	data := marked(crypto.PackageSize + 10)
	put(t, fx.svc, "tmp", "k", data, func(in *object.PutObjectInput) { in.SSE = domain.SSERequest{Type: domain.SSES3} })
	var parts []string
	require.NoError(t, filepath.WalkDir(filepath.Join(fx.roots[0], "tmp"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(path) == "part.1" {
			parts = append(parts, path)
		}
		return err
	}))
	require.Len(t, parts, 1)
	raw, err := os.ReadFile(parts[0])
	require.NoError(t, err)
	raw[len(raw)/2] ^= 0x40
	require.NoError(t, os.WriteFile(parts[0], raw, 0o600))
	r, err := fx.svc.GetObject(ctx, "tmp", "k", object.GetObjectOptions{})
	require.NoError(t, err)
	_, err = io.ReadAll(r)
	require.NoError(t, r.Close())
	require.ErrorIs(t, err, domain.ErrDecryption)
}
