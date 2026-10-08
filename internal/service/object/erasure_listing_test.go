package object_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// TestListingQuorum: an entry is listed when a read quorum of the drives
// of its set hold it; a version or an object present on fewer drives (the
// leftover of a failed write) is hidden, and so is a prefix holding only
// such leftovers. An object missing from one drive is still listed.
func TestListingQuorum(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	put(t, c.svc, "bkt", "dir/real", []byte("real"))
	put(t, c.svc, "bkt", "top", []byte("top"))

	ghost := storage.FileInfo{VersionID: "", ModTime: time.Now().UTC(), Size: 5, Data: []byte("ghost"),
		Metadata: map[string]string{"etag": "x"}}
	require.NoError(t, c.drives[0].WriteMetadata(ctx, "bkt", "ghost/x", ghost))
	require.NoError(t, c.drives[1].WriteMetadata(ctx, "bkt", "dir/half", ghost))
	// An object one drive missed.
	require.NoError(t, c.drives[3].DeleteVersion(ctx, "bkt", "top", storage.FileInfo{}, storage.DeleteOptions{}))

	flat, err := c.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt"})
	require.NoError(t, err)
	assert.Equal(t, []string{"dir/real", "top"}, keysOf(flat))
	delimited, err := c.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", Delimiter: "/"})
	require.NoError(t, err)
	assert.Equal(t, []string{"dir/"}, delimited.CommonPrefixes, "ghost/ exists on one drive only")
	assert.Equal(t, []string{"top"}, keysOf(delimited))
	versions, err := c.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "bkt"})
	require.NoError(t, err)
	assert.Len(t, versions.Objects, 2)

	// One drive offline: the listing is the same.
	c.setFaults(func(f *driveFaults) { f.offline = true }, 2)
	flat, err = c.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt"})
	require.NoError(t, err)
	assert.Equal(t, []string{"dir/real", "top"}, keysOf(flat))
}

// TestConcurrentWritersOneKey: 100 goroutines write the same key while
// others read it: every read returns one complete version, the last
// committed version wins, and nothing is left behind (one data directory,
// no temporary data). With versioning every write is a version.
func TestConcurrentWritersOneKey(t *testing.T) {
	const writers = 100
	for _, versioned := range []bool{false, true} {
		t.Run(fmt.Sprintf("versioned=%t", versioned), func(t *testing.T) {
			ctx := context.Background()
			spec := clusterSpec{pools: 1, sets: 1, drives: 4}
			if versioned {
				spec.buckets = versionedBuckets(t)
			}
			c := newCluster(t, spec)
			require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			bodies := make(map[string]bool, writers)
			bodyOf := func(i int) []byte {
				b := bytes.Repeat([]byte{byte('A' + i%26)}, 2*testBlockSize+i)
				return append(b, []byte(fmt.Sprintf("#%03d", i))...)
			}
			for i := 0; i < writers; i++ {
				bodies[string(bodyOf(i))] = true
			}
			put(t, c.svc, "bkt", "key", bodyOf(0))
			var wg sync.WaitGroup
			stop := make(chan struct{})
			readErrs := make(chan error, 8)
			for r := 0; r < 4; r++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						rd, err := c.svc.GetObject(ctx, "bkt", "key", object.GetObjectOptions{})
						if err != nil {
							readErrs <- err
							return
						}
						data, err := io.ReadAll(rd)
						_ = rd.Close()
						if err != nil || !bodies[string(data)] {
							readErrs <- fmt.Errorf("torn read (%d bytes): %v", len(data), err)
							return
						}
					}
				}()
			}
			var wwg sync.WaitGroup
			for i := 0; i < writers; i++ {
				wwg.Add(1)
				go func() {
					defer wwg.Done()
					body := bodyOf(i)
					_, err := c.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "key", Reader: bytes.NewReader(body), Size: int64(len(body))})
					assert.NoError(t, err)
				}()
			}
			wwg.Wait()
			close(stop)
			wg.Wait()
			close(readErrs)
			for err := range readErrs {
				t.Error(err)
			}

			info, err := c.svc.GetObjectInfo(ctx, "bkt", "key", object.GetObjectOptions{})
			require.NoError(t, err)
			got, _ := get(t, c.svc, "bkt", "key", object.GetObjectOptions{})
			assert.True(t, bodies[string(got)])
			assert.Equal(t, int64(len(got)), info.Size)
			res, err := c.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "bkt"})
			require.NoError(t, err)
			if versioned {
				assert.Len(t, res.Objects, writers+1)
				assert.Equal(t, info.VersionID, res.Objects[0].VersionID, "the latest listed is the latest read")
			} else {
				require.Len(t, res.Objects, 1)
				for i, root := range c.roots {
					dirs, _ := filepath.Glob(filepath.Join(root, "bkt", "key", "*-*"))
					assert.Len(t, dirs, 1, "drive %d keeps the data of the current version only", i)
				}
			}
			assertNoTmp(t, c.roots)
		})
	}
}

// TestListingTenThousandObjectsOnErasure: 10k objects on 4 drives (tmpfs)
// are listed, page by page, in less than 2 seconds.
func TestListingTenThousandObjectsOnErasure(t *testing.T) {
	if testing.Short() {
		t.Skip("10k objects: not in -short")
	}
	if raceEnabled {
		t.Skip("timing test: not with the race detector")
	}
	base, err := os.MkdirTemp("/dev/shm", "nexo-perf-")
	if err != nil {
		t.Skip("no tmpfs (/dev/shm)")
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	drives := make([]storage.StorageAPI, 4)
	for i := range drives {
		drives[i] = testutil.NewLocalDriveIndex(t, filepath.Join(base, fmt.Sprintf("d%d", i)), i)
	}
	svc, err := object.NewServerPools(object.PoolsDeps{
		Pools: []object.PoolSpec{{Sets: [][]storage.StorageAPI{drives}}}, DeploymentID: "perf", Logger: slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	const n = 10000
	keys := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range keys {
				_, err := svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: fmt.Sprintf("d%d/obj-%05d", i%10, i), Reader: bytes.NewReader(nil)})
				if err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		keys <- i
	}
	close(keys)
	wg.Wait()

	start := time.Now()
	listed := 0
	in := domain.ListObjectsInput{Bucket: "bkt", MaxKeys: 1000}
	prev := ""
	for {
		res, err := svc.ListObjects(ctx, in)
		require.NoError(t, err)
		for _, o := range res.Objects {
			require.Greater(t, o.Key, prev)
			prev = o.Key
		}
		listed += len(res.Objects)
		if !res.IsTruncated {
			break
		}
		in.ContinuationToken = res.NextContinuationToken
	}
	elapsed := time.Since(start)
	assert.Equal(t, n, listed)
	t.Logf("listed %d objects on 4 drives in %s", listed, elapsed)
	assert.Less(t, elapsed, 2*time.Second)
}
