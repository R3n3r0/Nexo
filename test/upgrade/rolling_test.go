//go:build upgrade

package upgrade_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bucketLoad is the bucket of the workload of the rolling upgrade.
const bucketLoad = "load"

// TestRollingUpgradeCluster: a cluster of 4 nodes x 2 drives written by
// the release before is upgraded one node at a time while a workload runs
// through the load balancer (puts, gets checked against the sha256 of the
// last acknowledged write, deletes) and a multipart upload gets a part
// while each node is down: no client error beyond the retries of the SDK,
// every read agrees with the writes, the data set of the old release reads
// back, every node runs the new release. Then a node of the release before
// rejoins the upgraded cluster (rollback of one node) and leaves it again:
// it serves the data the new release wrote and corrupts nothing.
//
// covers: T-9.8, TC-12
func TestRollingUpgradeCluster(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t, 4, 2)
	d.start(oldBin)
	n1 := d.node(1)
	before := serverVersion(t, n1)
	ds := writeDataset(t, d.lbClient(), n1.admin(), nil)
	old := driveFormats(t, d.driveDirs(1)[0], bucketData, "keep")

	_, err := d.lbClient().CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucketLoad)})
	require.NoError(t, err)
	l := startLoad(d.lbClient(), 8, 12, seed(t))
	mp := newRollingUpload(t, d.lbClient(), bucketLoad, "multipart/upgrade")
	time.Sleep(2 * time.Second)
	for _, nd := range d.nodes {
		start := time.Now()
		nd.stop()
		mp.uploadPart(t)
		nd.start(newBin)
		nd.waitReady()
		waitFor(t, 2*time.Minute, fmt.Sprintf("write quorum after the upgrade of node %d", nd.idx), func() error {
			if nd.clusterHealthy() {
				return nil
			}
			return fmt.Errorf("node %d does not report the cluster healthy", nd.idx)
		})
		t.Logf("node %d upgraded in %s", nd.idx, time.Since(start).Round(time.Millisecond))
		time.Sleep(2 * time.Second)
	}
	mp.complete(t)
	l.stop()
	ops, errs, mismatches := l.report()
	t.Logf("%d operations of the workload during the rolling upgrade", ops)
	assert.Greater(t, ops, 100, "the workload ran")
	assert.Empty(t, errs, "no client error during the rolling upgrade")
	assert.Empty(t, mismatches, "every read agrees with the acknowledged writes")
	assert.Empty(t, l.verify(ctx, d.lbClient()), "the final state of the workload")
	mp.verify(t, d.lbClient())

	info, err := n1.admin().ServerInfo(ctx)
	require.NoError(t, err)
	require.Len(t, info.Nodes, 4)
	after := info.Nodes[0].Version
	t.Logf("rolling upgrade %s -> %s", before, after)
	for _, n := range info.Nodes {
		assert.Equal(t, "online", n.State, n.Endpoint)
		assert.Equal(t, after, n.Version, "every node runs the release under test: %s", n.Endpoint)
	}
	if builtVersions {
		assert.Equal(t, oldVersion, before)
		assert.Equal(t, newVersion, after)
	}
	ds.verifyObjects(t, d.lbClient(), "rolling upgraded")
	ds.verifyState(t, n1, "rolling upgraded")
	ds.change(t, d.lbClient(), "new")
	written := driveFormats(t, d.driveDirs(1)[0], bucketData, "sse-kms-new")
	compatible := old.formatVersion == written.formatVersion && old.xlMetaMajor == written.xlMetaMajor

	// Rollback of one node: node 4 runs the release before again.
	n4 := d.node(4)
	n4.stop()
	n4.start(oldBin)
	err = n4.awaitReady(readyTimeout)
	if !compatible {
		require.Error(t, err, "newer formats: the release before refuses the drives")
		require.True(t, n4.exited())
		t.Logf("rollback of a node refused, as expected with newer formats: %v", err)
	} else {
		require.NoError(t, err, "same formats: a node of the release before rejoins")
		waitFor(t, 2*time.Minute, "write quorum with the rolled back node", func() error {
			if n4.clusterHealthy() {
				return nil
			}
			return fmt.Errorf("node 4 does not report the cluster healthy")
		})
		// The old node serves what the new release wrote, and writes
		// what the new nodes read.
		ds.verifyObjects(t, n4.rootClient(), "through the rolled back node")
		assert.Empty(t, l.verify(ctx, n4.rootClient()), "the workload through the rolled back node")
		ds.change(t, n4.rootClient(), "rollback")
		ds.verifyObjects(t, n1.rootClient(), "written by the rolled back node, read by an upgraded one")
		n4.stop()
	}
	n4.start(newBin)
	n4.waitReady()
	ds.verifyObjects(t, d.lbClient(), "upgraded again")
	ds.verifyState(t, n4, "upgraded again")
	assert.Empty(t, l.verify(ctx, d.lbClient()), "the workload after the rollback")
	d.stop()
}

// seed returns the seed of the workload (logged, to replay a failure).
func seed(t *testing.T) uint64 {
	s := rand.Uint64() //nolint:gosec // a workload seed
	t.Logf("workload seed %d", s)
	return s
}

// load is a workload of puts, gets and deletes: each worker owns its keys,
// so that the expected state of a key is the last write it acknowledged.
type load struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu         sync.Mutex
	ops        int
	errs       []string
	mismatches []string
	// state is the sha256 of every key ("" when deleted).
	state map[string]string
}

// startLoad starts workers on c, each with keys keys.
func startLoad(c *s3.Client, workers, keys int, seed uint64) *load {
	ctx, cancel := context.WithCancel(context.Background())
	l := &load{cancel: cancel, state: map[string]string{}}
	for w := range workers {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			rng := rand.New(rand.NewPCG(seed, uint64(w))) //nolint:gosec // a workload
			mine := map[string]string{}
			for ctx.Err() == nil {
				key := fmt.Sprintf("w%02d/k%03d", w, rng.IntN(keys))
				// Each operation runs to its end (with the retries of
				// the SDK): stopping the load never cuts one.
				opCtx := context.WithoutCancel(ctx)
				var err error
				switch p := rng.IntN(100); {
				case p < 45:
					data := make([]byte, 1+rng.IntN(512<<10))
					for i := range data {
						data[i] = byte(rng.Uint32())
					}
					_, err = c.PutObject(opCtx, &s3.PutObjectInput{Bucket: aws.String(bucketLoad), Key: aws.String(key), Body: bytes.NewReader(data)})
					if err == nil {
						mine[key] = digest(data)
					}
				case p < 90:
					var got string
					got, err = readKey(opCtx, c, key)
					if err == nil && got != mine[key] {
						l.mismatch(fmt.Sprintf("%s: read %.12q, last acknowledged write %.12q", key, got, mine[key]))
					}
				default:
					_, err = c.DeleteObject(opCtx, &s3.DeleteObjectInput{Bucket: aws.String(bucketLoad), Key: aws.String(key)})
					if err == nil {
						mine[key] = ""
					}
				}
				l.done(key, err)
			}
			l.mu.Lock()
			for k, v := range mine {
				l.state[k] = v
			}
			l.mu.Unlock()
		}()
	}
	return l
}

// readKey returns the sha256 of key ("" when it does not exist).
func readKey(ctx context.Context, c *s3.Client, key string) (string, error) {
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucketLoad), Key: aws.String(key)})
	if apiCode(err) == "NoSuchKey" {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return "", err
	}
	return digest(data), nil
}

func (l *load) done(key string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops++
	if err != nil {
		l.errs = append(l.errs, fmt.Sprintf("%s: %v", key, err))
	}
}

func (l *load) mismatch(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.mismatches = append(l.mismatches, s)
}

// stop stops the workers and waits for their last operations.
func (l *load) stop() {
	l.cancel()
	l.wg.Wait()
}

// report returns the operations, the client errors and the reads that
// disagreed with the writes.
func (l *load) report() (int, []string, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ops, l.errs, l.mismatches
}

// verify reads every key of the workload through c and returns the keys
// whose content differs from the last acknowledged write.
func (l *load) verify(ctx context.Context, c *s3.Client) []string {
	l.mu.Lock()
	state := make(map[string]string, len(l.state))
	for k, v := range l.state {
		state[k] = v
	}
	l.mu.Unlock()
	var bad []string
	for key, want := range state {
		got, err := readKey(ctx, c, key)
		if err != nil || got != want {
			bad = append(bad, fmt.Sprintf("%s: %.12q (%v), want %.12q", key, got, err, want))
		}
	}
	return bad
}

// rollingUpload is a multipart upload whose parts are uploaded one at a
// time while the nodes restart.
type rollingUpload struct {
	client      *s3.Client
	bucket, key string
	id          string
	etags       []string
	data        bytes.Buffer
}

func newRollingUpload(t *testing.T, c *s3.Client, bucket, key string) *rollingUpload {
	t.Helper()
	out, err := c.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.NoError(t, err)
	return &rollingUpload{client: c, bucket: bucket, key: key, id: aws.ToString(out.UploadId)}
}

// uploadPart uploads the next part (partSize bytes).
func (u *rollingUpload) uploadPart(t *testing.T) {
	t.Helper()
	body := payload(fmt.Sprintf("%s-%d", u.key, len(u.etags)+1), partSize)
	out, err := u.client.UploadPart(context.Background(), &s3.UploadPartInput{Bucket: aws.String(u.bucket), Key: aws.String(u.key),
		UploadId: aws.String(u.id), PartNumber: aws.Int32(int32(len(u.etags) + 1)), Body: bytes.NewReader(body)}) //nolint:gosec // a few parts
	require.NoError(t, err, "part %d while a node restarts", len(u.etags)+1)
	u.etags = append(u.etags, aws.ToString(out.ETag))
	u.data.Write(body)
}

// complete completes the upload.
func (u *rollingUpload) complete(t *testing.T) {
	t.Helper()
	parts := make([]types.CompletedPart, 0, len(u.etags))
	for i, etag := range u.etags {
		parts = append(parts, types.CompletedPart{ETag: aws.String(etag), PartNumber: aws.Int32(int32(i + 1))}) //nolint:gosec // a few parts
	}
	_, err := u.client.CompleteMultipartUpload(context.Background(), &s3.CompleteMultipartUploadInput{Bucket: aws.String(u.bucket), Key: aws.String(u.key),
		UploadId: aws.String(u.id), MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	require.NoError(t, err, "completion of the upload that ran through the upgrade")
}

// verify reads the completed object.
func (u *rollingUpload) verify(t *testing.T, c *s3.Client) {
	t.Helper()
	out, err := c.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(u.bucket), Key: aws.String(u.key)})
	require.NoError(t, err)
	data, err := io.ReadAll(out.Body)
	_ = out.Body.Close()
	require.NoError(t, err)
	assert.Equal(t, digest(u.data.Bytes()), digest(data), "the multipart object uploaded through the upgrade")
}
