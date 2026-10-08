package object_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// TestUUIDSegmentIsAPrefix is a regression test: a key segment shaped like
// a data directory ("x/<uuid>/y") is a prefix. Writing "x" over it must be
// refused (it would hide the child from listings) and no operation on "x"
// may destroy the child.
func TestUUIDSegmentIsAPrefix(t *testing.T) {
	const prefix = "x/0f8fad5b-d9cb-469f-a165-70867728950e/"
	children := map[string][]byte{
		"inline": []byte("child"),
		"parts":  payload(2*testBlockSize + 3),
	}
	for name, body := range children {
		t.Run(name, func(t *testing.T) {
			forEachImpl(t, func(t *testing.T, fx *fixture) {
				ctx := context.Background()
				svc := fx.svc
				require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
				child := prefix + "y"
				put(t, svc, "bkt", child, body)

				assertExistsAsDirectory := func(err error) {
					t.Helper()
					require.ErrorIs(t, err, domain.ErrInvalidObjectName)
					var s3err *domain.S3Error
					require.ErrorAs(t, err, &s3err)
					assert.Equal(t, "XNexoObjectExistsAsDirectory", s3err.Code)
				}
				_, err := svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "x", Reader: bytes.NewReader([]byte("parent")), Size: 6})
				assertExistsAsDirectory(err)
				big := payload(2*testBlockSize + 1)
				_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "x", Reader: bytes.NewReader(big), Size: int64(len(big))})
				assertExistsAsDirectory(err)
				_, err = svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "bkt", SrcKey: child, DstBucket: "bkt", DstKey: "x"})
				assertExistsAsDirectory(err)

				// A refused completion keeps the upload whole: it can be
				// completed later under the same key once the prefix is gone.
				up, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "x"})
				require.NoError(t, err)
				part := payload(32)
				p, err := svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "x", UploadID: up.UploadID, PartNumber: 1, Reader: bytes.NewReader(part), Size: int64(len(part))})
				require.NoError(t, err)
				completeIn := object.CompleteMultipartInput{Bucket: "bkt", Key: "x", UploadID: up.UploadID, Parts: []domain.CompletePart{{Number: 1, ETag: p.ETag}}}
				_, err = svc.CompleteMultipartUpload(ctx, completeIn)
				assertExistsAsDirectory(err)
				assertNoTmpLeft(t, fx)

				// Deleting "x" (not an object) leaves the child alone.
				_, err = svc.DeleteObject(ctx, "bkt", "x", object.DeleteObjectOptions{})
				require.NoError(t, err)
				data, _ := get(t, svc, "bkt", child, object.GetObjectOptions{})
				assert.Equal(t, body, data)
				res, err := svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt"})
				require.NoError(t, err)
				require.Len(t, res.Objects, 1)
				assert.Equal(t, child, res.Objects[0].Key)

				_, err = svc.DeleteObject(ctx, "bkt", child, object.DeleteObjectOptions{})
				require.NoError(t, err)
				_, err = svc.CompleteMultipartUpload(ctx, completeIn)
				require.NoError(t, err, "the parts of the refused completion are intact")
				data, _ = get(t, svc, "bkt", "x", object.GetObjectOptions{})
				assert.Equal(t, part, data)
				assertNoTmpLeft(t, fx)
			})
		})
	}
}
