package object_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/crypto"
	"github.com/r3n3r0/nexo/internal/service/kms"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// drivesContain reports whether a file of the drives holds needle.
func drivesContain(t *testing.T, roots []string, needle string) string {
	t.Helper()
	found := ""
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || found != "" {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(data, []byte(needle)) {
				found = path
			}
			return nil
		})
		require.NoError(t, err)
	}
	return found
}

func xorBytes(a, b []byte) []byte {
	out := make([]byte, min(len(a), len(b)))
	for i := range out {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// TestEncryptedPartReupload (T-7.5): a part number uploaded again in an
// encrypted upload is encrypted under a fresh key: the two ciphertexts on
// the drive never reveal the XOR of the two plaintexts (no keystream
// reuse), and the completed object reads back the last upload of the part.
func TestEncryptedPartReupload(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "ebkt", object.MakeBucketOptions{}))
		ckey := testCustomerKey(t)
		for _, sse := range []domain.SSERequest{{Type: domain.SSES3}, {Type: domain.SSEC, CustomerKey: ckey}} {
			up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "ebkt", Key: "obj", SSE: sse})
			require.NoError(t, err)
			p1 := bytes.Repeat([]byte("A"), 200<<10)
			p2 := bytes.Repeat([]byte("secret-plaintext-"), (200<<10)/17+1)[:200<<10]
			putPart := func(data []byte) string {
				p, err := fx.svc.PutObjectPart(ctx, object.PutObjectPartInput{
					Bucket: "ebkt", Key: "obj", UploadID: up.UploadID, PartNumber: 1,
					Reader: bytes.NewReader(data), Size: int64(len(data)), SSECustomerKey: sse.CustomerKey,
				})
				require.NoError(t, err)
				return p.ETag
			}
			partFile := func() []byte {
				files, err := filepath.Glob(filepath.Join(fx.roots[0], domain.SystemBucket, "multipart", "ebkt", up.UploadID, "part.1.*"))
				require.NoError(t, err)
				for _, f := range files {
					if filepath.Ext(f) != ".meta" {
						data, err := os.ReadFile(f)
						require.NoError(t, err)
						return data
					}
				}
				t.Fatal("no part file")
				return nil
			}
			putPart(p1)
			var c1 []byte
			if len(fx.roots) == 1 {
				c1 = partFile()
			}
			etag := putPart(p2)
			if len(fx.roots) == 1 {
				c2 := partFile()
				const n = 4096
				assert.False(t, bytes.Equal(xorBytes(p1, p2)[:n], xorBytes(c1, c2)[:n]), "%s: the keystream of part 1 is never reused", sse.Type)
				assert.False(t, bytes.Contains(xorBytes(xorBytes(c1, c2), p1), []byte("secret-plaintext-")), "%s: nothing to recover", sse.Type)
			}
			info, err := fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{
				Bucket: "ebkt", Key: "obj", UploadID: up.UploadID, Parts: []domain.CompletePart{{Number: 1, ETag: etag}},
			})
			require.NoError(t, err)
			assert.Equal(t, int64(len(p2)), info.Size)
			got, _ := get(t, fx.svc, "ebkt", "obj", object.GetObjectOptions{SSECustomerKey: sse.CustomerKey})
			assert.True(t, bytes.Equal(p2, got), "%s: the last upload of the part is read back", sse.Type)
		}
	})
}

// TestEncryptedChecksumsSealed (T-7.5): the checksums of the plaintext of
// encrypted versions and of their parts never reach the drives in clear;
// they are reported to the requests that can unseal the object key: the
// write responses, GET, HEAD with the checksum mode, GetObjectAttributes,
// ListParts and the completion (which needs the SSE-C key of an SSE-C
// upload to compute the checksum of the object, as in S3).
func TestEncryptedChecksumsSealed(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "cbkt", object.MakeBucketOptions{}))
		ckey := testCustomerKey(t)
		var secrets []string

		// SSE-C of a tiny object: its SHA-256 would identify it.
		sum := sha256.Sum256([]byte("1234"))
		pinSum := base64.StdEncoding.EncodeToString(sum[:])
		info := put(t, fx.svc, "cbkt", "pin", []byte("1234"), func(in *object.PutObjectInput) {
			in.SSE = domain.SSERequest{Type: domain.SSEC, CustomerKey: ckey}
			in.Checksum = domain.Checksum{Algorithm: domain.ChecksumSHA256, Value: pinSum}
		})
		assert.Equal(t, pinSum, info.Checksum.Value, "the response reports the checksum")
		secrets = append(secrets, pinSum)

		// SSE-S3 with a checksum computed by the server.
		info = put(t, fx.svc, "cbkt", "s3obj", marked(3000), func(in *object.PutObjectInput) {
			in.SSE = domain.SSERequest{Type: domain.SSES3}
			in.Checksum = domain.Checksum{Algorithm: domain.ChecksumCRC32}
		})
		require.NotEmpty(t, info.Checksum.Value)
		s3Sum := info.Checksum.Value
		secrets = append(secrets, s3Sum)

		// Multipart uploads with part checksums: SSE-KMS and SSE-C.
		type upload struct {
			key  string
			sse  domain.SSERequest
			algo domain.ChecksumAlgorithm
		}
		for _, u := range []upload{
			{key: "kmsmp", sse: domain.SSERequest{Type: domain.SSEKMS, KMSKeyID: "tenant"}, algo: domain.ChecksumCRC32},
			{key: "cmp", sse: domain.SSERequest{Type: domain.SSEC, CustomerKey: ckey}, algo: domain.ChecksumCRC32C},
		} {
			up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "cbkt", Key: u.key, SSE: u.sse, Checksum: u.algo})
			require.NoError(t, err)
			var parts []domain.CompletePart
			partSums := map[int]string{}
			for n, data := range [][]byte{marked(1000), marked(700)} {
				p, err := fx.svc.PutObjectPart(ctx, object.PutObjectPartInput{
					Bucket: "cbkt", Key: u.key, UploadID: up.UploadID, PartNumber: n + 1,
					Reader: bytes.NewReader(data), Size: int64(len(data)), SSECustomerKey: u.sse.CustomerKey,
					Checksum: domain.Checksum{Algorithm: u.algo},
				})
				require.NoError(t, err)
				require.NotEmpty(t, p.Checksum.Value, "the part response reports its checksum")
				partSums[n+1] = p.Checksum.Value
				secrets = append(secrets, p.Checksum.Value)
				parts = append(parts, domain.CompletePart{Number: n + 1, ETag: p.ETag, Checksum: p.Checksum})
			}
			for _, s := range partSums {
				assert.Empty(t, drivesContain(t, fx.roots, s), "%s: part checksum in clear on the drives", u.key)
			}
			listed, err := fx.svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "cbkt", Key: u.key, UploadID: up.UploadID, SSECustomerKey: u.sse.CustomerKey})
			require.NoError(t, err)
			require.Len(t, listed.Parts, 2)
			assert.Equal(t, partSums[2], listed.Parts[1].Checksum.Value, "%s: ListParts opens the checksums", u.key)
			complete := object.CompleteMultipartInput{Bucket: "cbkt", Key: u.key, UploadID: up.UploadID, Parts: parts}
			if u.sse.Type == domain.SSEC {
				listed, err = fx.svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "cbkt", Key: u.key, UploadID: up.UploadID})
				require.NoError(t, err)
				assert.Equal(t, u.algo, listed.Parts[1].Checksum.Algorithm)
				assert.Empty(t, listed.Parts[1].Checksum.Value, "without the SSE-C key, the algorithm only")
				_, err = fx.svc.CompleteMultipartUpload(ctx, complete)
				require.ErrorIs(t, err, domain.ErrInvalidArgument, "the checksums of an SSE-C upload need its key")
				complete.SSECustomerKey = ckey
			}
			info, err := fx.svc.CompleteMultipartUpload(ctx, complete)
			require.NoError(t, err)
			require.NotEmpty(t, info.Checksum.Value, "the completion reports the checksum of the object")
			secrets = append(secrets, info.Checksum.Value)

			attrs, err := fx.svc.GetObjectAttributes(ctx, "cbkt", u.key, object.AttributesOptions{SSECustomerKey: u.sse.CustomerKey})
			require.NoError(t, err)
			assert.Equal(t, info.Checksum.Value, attrs.Info.Checksum.Value)
			require.Len(t, attrs.Parts, 2)
			assert.Equal(t, partSums[1], attrs.Parts[0].Checksum.Value)
			assert.Equal(t, partSums[2], attrs.Parts[1].Checksum.Value)
			r, err := fx.svc.GetObject(ctx, "cbkt", u.key, object.GetObjectOptions{PartNumber: 2, SSECustomerKey: u.sse.CustomerKey})
			require.NoError(t, err)
			require.NoError(t, r.Close())
			assert.Equal(t, partSums[2], r.Info.Checksum.Value, "a part read reports the checksum of the part")
		}

		// Reads.
		_, r := get(t, fx.svc, "cbkt", "pin", object.GetObjectOptions{SSECustomerKey: ckey})
		assert.Equal(t, pinSum, r.Info.Checksum.Value)
		head, err := fx.svc.GetObjectInfo(ctx, "cbkt", "pin", object.GetObjectOptions{SSECustomerKey: ckey})
		require.NoError(t, err)
		assert.Equal(t, pinSum, head.Checksum.Value, "an SSE-C HEAD unseals the key anyway")
		head, err = fx.svc.GetObjectInfo(ctx, "cbkt", "s3obj", object.GetObjectOptions{})
		require.NoError(t, err)
		assert.Equal(t, domain.ChecksumCRC32, head.Checksum.Algorithm)
		assert.Empty(t, head.Checksum.Value, "SSE-S3 HEAD without the checksum mode: no KMS call, the algorithm only")
		head, err = fx.svc.GetObjectInfo(ctx, "cbkt", "s3obj", object.GetObjectOptions{ChecksumMode: true})
		require.NoError(t, err)
		assert.Equal(t, s3Sum, head.Checksum.Value)
		assert.Equal(t, domain.ChecksumTypeFullObject, head.Checksum.Type)
		list, err := fx.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "cbkt"})
		require.NoError(t, err)
		for _, o := range list.Objects {
			assert.NotEmpty(t, o.Checksum.Algorithm, "listings report the algorithm of %s", o.Key)
			assert.Empty(t, o.Checksum.Value, "and never a sealed value")
		}

		for _, s := range secrets {
			assert.Empty(t, drivesContain(t, fx.roots, s), "checksum %s in clear on the drives", s)
		}
	})
}

// TestEncryptedDigestsOnDisk (docs/07 §7.2): the drives never hold the
// MD5 of the plaintext of an SSE-C or SSE-KMS object, of a single PUT or
// of the parts of a multipart upload (their ETags are opaque), even for a
// tiny object whose MD5 would give its content away. The ETag of SSE-S3 is
// the MD5 of the plaintext, as in S3 (documented, not hidden).
func TestEncryptedDigestsOnDisk(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "md5bkt", object.MakeBucketOptions{}))
		ckey := testCustomerKey(t)
		for _, sse := range []domain.SSERequest{
			{Type: domain.SSEKMS, KMSKeyID: "tenant"},
			{Type: domain.SSEC, CustomerKey: ckey},
		} {
			var digests []string
			for _, data := range [][]byte{[]byte("1234"), marked(300 << 10)} {
				key := fmt.Sprintf("%s/%d", sse.Type, len(data))
				info := put(t, fx.svc, "md5bkt", key, data, func(in *object.PutObjectInput) { in.SSE = sse })
				assert.NotEqual(t, md5hex(data), info.ETag, "%s: opaque ETag", key)
				digests = append(digests, md5hex(data))
			}
			key := fmt.Sprintf("%s/multipart", sse.Type)
			up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "md5bkt", Key: key, SSE: sse})
			require.NoError(t, err)
			var parts []domain.CompletePart
			for n, data := range [][]byte{marked(1000), []byte("5678")} {
				p, err := fx.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "md5bkt", Key: key, UploadID: up.UploadID, PartNumber: n + 1,
					Reader: bytes.NewReader(data), Size: int64(len(data)), SSECustomerKey: sse.CustomerKey})
				require.NoError(t, err)
				assert.NotEqual(t, md5hex(data), p.ETag, "%s part %d: opaque ETag", key, n+1)
				digests = append(digests, md5hex(data))
				parts = append(parts, domain.CompletePart{Number: n + 1, ETag: p.ETag})
			}
			for _, d := range digests[2:] {
				assert.Empty(t, drivesContain(t, fx.roots, d), "%s: the MD5 of a pending part is on the drives", key)
			}
			_, err = fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "md5bkt", Key: key, UploadID: up.UploadID, Parts: parts, SSECustomerKey: sse.CustomerKey})
			require.NoError(t, err)
			for _, d := range digests {
				assert.Empty(t, drivesContain(t, fx.roots, d), "%s: the MD5 %s of a plaintext is on the drives", sse.Type, d)
			}
		}
		info := put(t, fx.svc, "md5bkt", "s3", []byte("1234"), func(in *object.PutObjectInput) { in.SSE = domain.SSERequest{Type: domain.SSES3} })
		assert.Equal(t, md5hex([]byte("1234")), info.ETag, "SSE-S3: the ETag is the MD5 of the plaintext, as in S3")
	})
}

// cryptoWith returns a crypto service on a builtin KMS with the master key
// and the previous ones, and the key "tenant".
func cryptoWith(t *testing.T, master kms.MasterKey, previous ...kms.MasterKey) crypto.Service {
	t.Helper()
	opts := []kms.Option{kms.WithMasterKey(master)}
	if len(previous) > 0 {
		opts = append(opts, kms.WithPreviousKeys(previous...))
	}
	k, err := kms.NewBuiltin(kms.BuiltinDeps{Logger: slog.New(slog.DiscardHandler)}, opts...)
	require.NoError(t, err)
	require.NoError(t, k.CreateKey(context.Background(), "tenant"))
	return crypto.New(crypto.Deps{KMS: k, Logger: slog.New(slog.DiscardHandler)})
}

func newMasterKey(t *testing.T) kms.MasterKey {
	t.Helper()
	secret := make([]byte, 32)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	k, err := kms.ParseMasterKey("nexo:" + base64.StdEncoding.EncodeToString(secret))
	require.NoError(t, err)
	return k
}

// reopenWithCrypto returns an object layer over the drives of fx with the crypto
// service c (a server restarted with another KMS configuration).
func reopenWithCrypto(t *testing.T, fx *fixture, c crypto.Service) object.Service {
	t.Helper()
	deps := object.Deps{Buckets: fx.buckets, Events: fx.events, Crypto: c, Clock: fx.clk, Logger: slog.New(slog.DiscardHandler)}
	opts := []object.Option{object.WithInlineLimit(testInlineLimit), object.WithBlockSize(testBlockSize), object.WithMinPartSize(16)}
	var (
		svc object.Service
		err error
	)
	if len(fx.drives) == 1 {
		deps.Store = fx.drives[0]
		svc, err = object.NewFS(deps, opts...)
	} else {
		deps.Sets = [][]storage.StorageAPI{fx.drives}
		deps.Parity = 2
		deps.DeploymentID = "deployment-test"
		deps.Codecs = testCodecs("deployment-test")
		svc, err = object.NewErasure(deps, opts...)
	}
	require.NoError(t, err)
	return svc
}

// TestResealKeysAfterMasterKeyRotation (TS-U-13, docs/07 §7.2): after a
// rotation of the master key, ResealKeys moves the data keys of the SSE-S3
// and SSE-KMS versions (multipart, versioned, sealed checksums included)
// to the new master key: the objects then read back without the previous
// master key; SSE-C versions are left alone.
func TestResealKeysAfterMasterKeyRotation(t *testing.T) {
	for _, kind := range []string{"fs", "erasure"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			fx := newFixture(t, kind)
			oldKey, newKey := newMasterKey(t), newMasterKey(t)
			before := reopenWithCrypto(t, fx, cryptoWith(t, oldKey))
			require.NoError(t, before.MakeBucket(ctx, "rot", object.MakeBucketOptions{}))
			fx.setBucketConfig("rot", object.BucketConfig{Versioning: domain.VersioningEnabled})
			ckey := testCustomerKey(t)
			bodies := map[string][]byte{}
			var versions []string
			for _, o := range []struct {
				key string
				sse domain.SSERequest
			}{
				{"s3", domain.SSERequest{Type: domain.SSES3}},
				{"s3", domain.SSERequest{Type: domain.SSES3}},
				{"kms", domain.SSERequest{Type: domain.SSEKMS, KMSKeyID: "tenant"}},
				{"dir/c", domain.SSERequest{Type: domain.SSEC, CustomerKey: ckey}},
				{"plain", domain.SSERequest{}},
			} {
				data := marked(3000 + len(versions))
				info := put(t, before, "rot", o.key, data, func(in *object.PutObjectInput) {
					in.SSE = o.sse
					in.Checksum = domain.Checksum{Algorithm: domain.ChecksumCRC32}
				})
				bodies[o.key+"@"+info.VersionID] = data
				versions = append(versions, o.key+"@"+info.VersionID)
			}
			// Pending uploads created before the rotation: SSE-S3 and
			// SSE-KMS hold a KMS-sealed key in their description.
			uploads := map[string]domain.MultipartUpload{}
			firstParts := map[string]domain.Part{}
			for key, sse := range map[string]domain.SSERequest{
				"mp-s3":  {Type: domain.SSES3},
				"mp-kms": {Type: domain.SSEKMS, KMSKeyID: "tenant"},
				"mp-c":   {Type: domain.SSEC, CustomerKey: ckey},
			} {
				up, err := before.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "rot", Key: key, SSE: sse})
				require.NoError(t, err)
				uploads[key] = up
				p, err := before.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "rot", Key: key, UploadID: up.UploadID, PartNumber: 1,
					Reader: bytes.NewReader(marked(100)), Size: 100, SSECustomerKey: sse.CustomerKey})
				require.NoError(t, err)
				firstParts[key] = p
			}

			rotated := reopenWithCrypto(t, fx, cryptoWith(t, newKey, oldKey))
			res, err := rotated.ResealKeys(ctx, "rot")
			require.NoError(t, err)
			assert.Equal(t, object.ResealResult{Versions: 3, Uploads: 2, Resealed: 5}, res,
				"SSE-S3 (2 versions), SSE-KMS and the SSE-S3 and SSE-KMS uploads; SSE-C and plaintext skipped")

			after := reopenWithCrypto(t, fx, cryptoWith(t, newKey))
			for _, v := range versions {
				key, id, _ := strings.Cut(v, "@")
				opts := object.GetObjectOptions{VersionID: id}
				if key == "dir/c" {
					opts.SSECustomerKey = ckey
				}
				got, r := get(t, after, "rot", key, opts)
				assert.True(t, bytes.Equal(bodies[v], got), "%s reads back without the previous master key", v)
				assert.NotEmpty(t, r.Info.Checksum.Value, "%s: the sealed checksum opens", v)
			}
			// The pending uploads take parts and complete without the
			// previous master key.
			for _, key := range []string{"mp-s3", "mp-kms"} {
				up := uploads[key]
				p2, err := after.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "rot", Key: key, UploadID: up.UploadID, PartNumber: 2,
					Reader: bytes.NewReader([]byte("tail")), Size: 4})
				require.NoError(t, err, "%s: a part after the rotation", key)
				_, err = after.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "rot", Key: key, UploadID: up.UploadID,
					Parts: []domain.CompletePart{{Number: 1, ETag: firstParts[key].ETag}, {Number: 2, ETag: p2.ETag}}})
				require.NoError(t, err, "%s: the completion after the rotation", key)
				got, _ := get(t, after, "rot", key, object.GetObjectOptions{})
				assert.Equal(t, append(marked(100), []byte("tail")...), got)
			}
			_, err = rotated.ResealKeys(ctx, "missing-bucket")
			assert.ErrorIs(t, err, domain.ErrBucketNotFound)
		})
	}
}
