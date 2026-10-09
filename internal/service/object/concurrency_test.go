package object_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// TS-I-13: 100 concurrent PUTs on the same key while readers GET it: every
// read returns one complete version, consistent with its own ETag, and the
// final object is one of the written versions.
func TestConcurrentPutsOnOneKey(t *testing.T) {
	t.Parallel()
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		const writers = 100
		bodies := make(map[string][]byte, writers)
		for i := 0; i < writers; i++ {
			// Alternate inline and data-dir objects.
			size := 200 + i
			if i%2 == 1 {
				size = 2*testBlockSize + i
			}
			b := bytes.Repeat([]byte{byte(i)}, size)
			bodies[md5hex(b)] = b
		}
		put(t, fx.svc, "bkt", "hot", bytes.Repeat([]byte{0xff}, 10))
		bodies[md5hex(bytes.Repeat([]byte{0xff}, 10))] = bytes.Repeat([]byte{0xff}, 10)

		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			failures []string
		)
		fail := func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			failures = append(failures, fmt.Sprintf(format, args...))
		}
		// Readers pause between reads: the local locker has no writer
		// preference, and continuous readers would starve the writers.
		for r := 0; r < 4; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := 0; n < 40; n++ {
					time.Sleep(200 * time.Microsecond)
					rd, err := fx.svc.GetObject(ctx, "bkt", "hot", object.GetObjectOptions{})
					if err != nil {
						fail("get: %v", err)
						return
					}
					got, err := io.ReadAll(rd)
					_ = rd.Close()
					if err != nil {
						fail("read: %v", err)
						return
					}
					want, ok := bodies[rd.Info.ETag]
					if !ok || !bytes.Equal(want, got) || md5hex(got) != rd.Info.ETag {
						fail("inconsistent read: etag %s, %d bytes", rd.Info.ETag, len(got))
						return
					}
				}
			}()
		}
		var writersWG sync.WaitGroup
		for _, body := range bodies {
			writersWG.Add(1)
			go func(body []byte) {
				defer writersWG.Done()
				if _, err := fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "hot", Reader: bytes.NewReader(body), Size: int64(len(body))}); err != nil {
					fail("put: %v", err)
				}
			}(body)
		}
		writersWG.Wait()
		wg.Wait()
		require.Empty(t, failures)

		final, _ := get(t, fx.svc, "bkt", "hot", object.GetObjectOptions{})
		assert.Contains(t, bodies, md5hex(final))
		versions, err := fx.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "bkt"})
		require.NoError(t, err)
		assert.Len(t, versions.Objects, 1, "unversioned overwrites keep a single version")
		assertNoTmpLeft(t, fx)
	})
}

// TS-I-14: a write is visible to the very next read and listing.
func TestReadAfterWrite(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		for i := 0; i < 25; i++ {
			key := fmt.Sprintf("dir%d/obj-%02d", i%3, i)
			body := payload(i * 97)
			info := put(t, fx.svc, "bkt", key, body)
			got, r := get(t, fx.svc, "bkt", key, object.GetObjectOptions{})
			assert.Equal(t, body, got)
			assert.Equal(t, info.ETag, r.Info.ETag)
			res, err := fx.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", Prefix: key})
			require.NoError(t, err)
			require.Len(t, res.Objects, 1, key)
			assert.Equal(t, info.ETag, res.Objects[0].ETag)

			_, err = fx.svc.DeleteObject(ctx, "bkt", key, object.DeleteObjectOptions{})
			require.NoError(t, err)
			res, err = fx.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", Prefix: key})
			require.NoError(t, err)
			assert.Empty(t, res.Objects, "a delete is visible to the next listing")
			put(t, fx.svc, "bkt", key, body)
		}
		res, err := fx.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt"})
		require.NoError(t, err)
		assert.Len(t, res.Objects, 25)
	})
}

// Crossed copies (a→b while b→a) take their locks in a fixed order and
// never deadlock.
func TestCrossedCopiesDoNotDeadlock(t *testing.T) {
	fx := newFixture(t, "fs")
	ctx := context.Background()
	require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	put(t, fx.svc, "bkt", "a", payload(5000))
	put(t, fx.svc, "bkt", "b", payload(6000))
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := fx.svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "bkt", SrcKey: "a", DstBucket: "bkt", DstKey: "b"})
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := fx.svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "bkt", SrcKey: "b", DstBucket: "bkt", DstKey: "a"})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	a, _ := get(t, fx.svc, "bkt", "a", object.GetObjectOptions{})
	b, _ := get(t, fx.svc, "bkt", "b", object.GetObjectOptions{})
	assert.Equal(t, a, b, "after crossed copies both keys hold the same content")
}

// Concurrent uploads of the same part number never mix the data of one
// with the ETag of another.
func TestConcurrentUploadsOfOnePart(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "k"})
		require.NoError(t, err)
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				body := bytes.Repeat([]byte{byte('a' + i)}, testBlockSize+i)
				_, err := fx.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "k", UploadID: up.UploadID, PartNumber: 1, Reader: bytes.NewReader(body), Size: int64(len(body))})
				assert.NoError(t, err)
			}(i)
		}
		wg.Wait()
		parts, err := fx.svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "bkt", Key: "k", UploadID: up.UploadID})
		require.NoError(t, err)
		require.Len(t, parts.Parts, 1)
		info, err := fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "k", UploadID: up.UploadID,
			Parts: []domain.CompletePart{{Number: 1, ETag: parts.Parts[0].ETag}}})
		require.NoError(t, err)
		got, _ := get(t, fx.svc, "bkt", "k", object.GetObjectOptions{})
		assert.Equal(t, parts.Parts[0].ETag, md5hex(got), "the data matches the recorded ETag")
		assert.Equal(t, int64(len(got)), info.Size)
		assertNoTmpLeft(t, fx)
	})
}

// Regression of the placement race: "k" and "k/x" written concurrently
// took different locks, both committed, and "k/x" stayed readable while
// invisible to ListObjects (on an erasure set each could even commit on
// part of the drives and stay readable after failing its write quorum).
// One write of each pair wins, and what is readable is what is listed.
func TestConcurrentParentChildPuts(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		const pairs = 100
		keyOf := func(i int) string {
			if i%2 == 0 {
				return fmt.Sprintf("k%03d", i/2)
			}
			return fmt.Sprintf("k%03d/x", i/2)
		}
		var (
			wg  sync.WaitGroup
			mu  sync.Mutex
			won = map[string]bool{}
		)
		for i := 0; i < 2*pairs; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				body := payload(100 + i)
				if (i/2)%2 == 1 {
					body = payload(2*testBlockSize + i)
				}
				_, err := fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: keyOf(i), Reader: bytes.NewReader(body), Size: int64(len(body))})
				if err != nil {
					assert.ErrorIs(t, err, domain.ErrInvalidObjectName, keyOf(i))
					return
				}
				mu.Lock()
				won[keyOf(i)] = true
				mu.Unlock()
			}(i)
		}
		wg.Wait()
		for p := 0; p < pairs; p++ {
			assert.False(t, won[keyOf(2*p)] && won[keyOf(2*p+1)], "both %s and %s were written", keyOf(2*p), keyOf(2*p+1))
		}
		var readable []string
		for i := 0; i < 2*pairs; i++ {
			if _, err := fx.svc.GetObjectInfo(ctx, "bkt", keyOf(i), object.GetObjectOptions{}); err == nil {
				readable = append(readable, keyOf(i))
			}
		}
		res, err := fx.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", MaxKeys: 1000})
		require.NoError(t, err)
		listed := make([]string, 0, len(res.Objects))
		for _, o := range res.Objects {
			listed = append(listed, o.Key)
		}
		assert.Equal(t, readable, listed, "every readable object is listed")
		assert.Len(t, won, pairs, "the namespace locks serialize the pair: one write wins on every drive")
	})
}
