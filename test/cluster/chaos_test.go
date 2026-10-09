//go:build cluster

package cluster_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTC07RealPartition: a 2+2 partition with both halves running (the
// links between them cut, docs/06 TC-07; TestTC07PartitionNoSplitBrain
// freezes one half instead). Neither half has the write quorum (4 drives
// of 8, 2 lock servers of 4): no write is accepted on either side, and
// once the partition heals the writes resume and nothing written during
// it is visible.
//
// covers: TC-07
func TestTC07RealPartition(t *testing.T) {
	t.Parallel() // waits on timeouts: overlaps with the other waiting tests
	c := newCluster(t, 4, withLinks())
	c.start()
	ctx := context.Background()
	mustCreateBucket(t, c.node(1).rootClient(), "tc07r")
	want := dataset(t, c.node(1).rootClient(), "tc07r", "obj/", 10, seedOf(t))

	c.partition([]int{1, 2}, []int{3, 4})
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted []string
	)
	for i := 1; i <= 4; i++ {
		cl := client("http://"+c.node(i).S3, rootUser, rootPassword, 1, 90*time.Second)
		for _, key := range []string{fmt.Sprintf("split/%d", i), "obj/0000"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := put(ctx, cl, "tc07r", key, []byte("split")); err == nil {
					mu.Lock()
					accepted = append(accepted, fmt.Sprintf("node %d: %s", i, key))
					mu.Unlock()
				}
			}()
		}
	}
	timed(t, "refused writes on both halves", wg.Wait)
	assert.Empty(t, accepted, "no write is accepted by either half of a 2+2 partition")
	// Each half still holds the read quorum (4 drives): the reads agree.
	for _, i := range []int{1, 3} {
		assert.Empty(t, checkDataset(ctx, c.node(i).rootClient(), "tc07r", want), "reads on node %d during the partition", i)
	}

	c.heal()
	waitFor(t, 60*time.Second, "writes resume after the partition", func() error {
		return put(ctx, client("http://"+c.node(3).S3, rootUser, rootPassword, 1, 30*time.Second), "tc07r", "after", []byte("healed"))
	})
	for _, nd := range c.nodes {
		assert.Empty(t, checkDataset(ctx, nd.rootClient(), "tc07r", want), "node %d", nd.idx)
		for i := 1; i <= 4; i++ {
			_, found, err := get(ctx, nd.rootClient(), "tc07r", fmt.Sprintf("split/%d", i))
			require.NoError(t, err)
			assert.False(t, found, "no write of the partition is visible on node %d", nd.idx)
		}
	}
}

// TestTC08SlowNode: every link of one node has 200 ms of latency and
// loses 5% of its segments (each loss stalls the stream as a
// retransmission would): the operations of a mixed workload through
// every node complete within their timeout, the reads agree with the
// writes, and no lock stays held afterwards.
//
// covers: TC-08
func TestTC08SlowNode(t *testing.T) {
	t.Parallel()
	c := newCluster(t, 4, withLinks())
	c.start()
	ctx := context.Background()
	mustCreateBucket(t, c.node(1).rootClient(), "tc08")
	c.slowNode(4, 200*time.Millisecond, 0.05, 300*time.Millisecond)

	l := startLoad(c.lbClient(), "tc08", seedOf(t), 4, 8)
	runFor(10 * time.Second)
	timed(t, "last operations on the slow cluster", l.stop)
	ops, errs, violations := l.ledger.report()
	t.Logf("%d operations with a slow node", ops)
	assert.Positive(t, ops)
	assert.Empty(t, errs, "every operation completes within its timeout")
	assert.Empty(t, violations, "no read contradicts a confirmed write")
	assert.Empty(t, l.ledger.verify(ctx, c.lbClient()), "final state matches the ledger")

	// No lock left behind: only the leader lock of the background
	// services remains held.
	waitFor(t, 30*time.Second, "no object lock left", func() error {
		locks, err := c.node(1).admin().ListLocks(ctx)
		if err != nil {
			return err
		}
		var held []string
		for _, lk := range locks {
			if !strings.HasPrefix(lk.Resource, ".nexo.sys/") {
				held = append(held, lk.Resource)
			}
		}
		if len(held) > 0 {
			return fmt.Errorf("held: %v", held)
		}
		return nil
	})
}

// TestTC12RollingUpgrade: the nodes are restarted one after the other
// with the next version of the binary (N → N+1, same major version)
// while a workload runs through the load balancer: no client error, the
// reads agree with the writes, and every node runs the new version.
//
// covers: TC-12
func TestTC12RollingUpgrade(t *testing.T) {
	dir := t.TempDir()
	oldBin, newBin := filepath.Join(dir, "nexo-v1.0.0"), filepath.Join(dir, "nexo-v1.1.0")
	require.NoError(t, buildNexo(oldBin, "v1.0.0"))
	require.NoError(t, buildNexo(newBin, "v1.1.0"))
	c := newCluster(t, 4)
	for _, nd := range c.nodes {
		nd.bin = oldBin
	}
	c.start()
	ctx := context.Background()
	mustCreateBucket(t, c.node(1).rootClient(), "tc12")
	l := startLoad(c.lbClient(), "tc12", seedOf(t), 6, 10)
	// A multipart upload runs through the whole upgrade: each part is
	// uploaded while a node (and its drives) is down, so the parts are
	// acknowledged on different drives.
	mp := newRollingUpload(t, c.lbClient(), "tc12", "multipart/upgrade")
	runFor(2 * time.Second)
	for _, nd := range c.nodes {
		timed(t, fmt.Sprintf("upgrade of node %d", nd.idx), func() {
			nd.stop()
			mp.uploadPart(t)
			nd.bin = newBin
			nd.start()
			c.waitReady(nd)
		})
		runFor(2 * time.Second)
	}
	mp.complete(t)
	l.stop()
	ops, errs, violations := l.ledger.report()
	t.Logf("%d operations during the upgrade", ops)
	assert.Empty(t, errs, "no client error during the rolling upgrade")
	assert.Empty(t, violations)
	assert.Empty(t, l.ledger.verify(ctx, c.lbClient()))

	info, err := c.node(2).admin().ServerInfo(ctx)
	require.NoError(t, err)
	for _, n := range info.Nodes {
		assert.Equal(t, "v1.1.0", n.Version, n.Endpoint)
		assert.Equal(t, "online", n.State, n.Endpoint)
	}
}

// TestTC13ClockSkew: one node runs 5 s ahead. The others report the
// skew (server info, log), and nothing misbehaves: a write or a delete
// through a node behind the skewed one still supersedes what the skewed
// node wrote just before (the modification times of the versions of a
// key only move forward under its write lock).
//
// covers: TC-13
func TestTC13ClockSkew(t *testing.T) {
	t.Parallel()
	c := newCluster(t, 4)
	c.node(4).env = append(c.node(4).env, "NEXO_TEST_CLOCK_OFFSET=5s")
	c.start()
	ctx := context.Background()

	waitFor(t, 30*time.Second, "the skew of node 4 is reported", func() error {
		info, err := c.node(1).admin().ServerInfo(ctx)
		if err != nil {
			return err
		}
		for _, n := range info.Nodes {
			if n.Endpoint != c.node(4).S3 {
				continue
			}
			if n.ClockSkew == nil || !n.ClockSkew.Exceeded {
				return fmt.Errorf("clock skew of node 4: %+v", n.ClockSkew)
			}
			if off := n.ClockSkew.OffsetMs; off < 4000 && off > -4000 {
				return fmt.Errorf("offset %d ms", off)
			}
			return nil
		}
		return errors.New("node 4 missing")
	})
	waitFor(t, 30*time.Second, "the skew is logged", func() error {
		for _, nd := range c.nodes[:3] {
			if len(nd.logLines("clock skew between nodes")) > 0 {
				return nil
			}
		}
		return errors.New("no warning yet")
	})

	ahead, behind := c.node(4).rootClient(), c.node(1).rootClient()
	mustCreateBucket(t, behind, "tc13")
	_, err := behind.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String("tc13"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
	require.NoError(t, err)
	mustCreateBucket(t, behind, "tc13plain")

	for _, bucket := range []string{"tc13", "tc13plain"} {
		for round := range 3 {
			key := fmt.Sprintf("k%d", round)
			require.NoError(t, put(ctx, ahead, bucket, key, []byte("from the node ahead")))
			require.NoError(t, put(ctx, behind, bucket, key, []byte("from the node behind")))
			for _, nd := range c.nodes {
				data, found, err := get(ctx, nd.rootClient(), bucket, key)
				require.NoError(t, err)
				require.True(t, found)
				assert.Equal(t, "from the node behind", string(data), "%s/%s through node %d: the later write wins", bucket, key, nd.idx)
			}
			require.NoError(t, put(ctx, ahead, bucket, key, []byte("ahead again")))
			require.NoError(t, deleteKey(ctx, behind, bucket, key))
			_, found, err := get(ctx, ahead, bucket, key)
			require.NoError(t, err)
			assert.False(t, found, "%s/%s: the later delete wins", bucket, key)
		}
	}
	versions, err := behind.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String("tc13"), Prefix: aws.String("k0")})
	require.NoError(t, err)
	require.Len(t, versions.Versions, 3)
	require.Len(t, versions.DeleteMarkers, 1)
	assert.True(t, aws.ToBool(versions.DeleteMarkers[0].IsLatest), "the delete marker is the latest version")
	for _, v := range versions.Versions {
		// The dates of the listing have a millisecond precision: the
		// versions written within the same millisecond tie.
		assert.False(t, aws.ToTime(v.LastModified).After(aws.ToTime(versions.DeleteMarkers[0].LastModified)),
			"no version is dated after the delete that followed it")
	}
}

// soakDuration is the length of the soak of TC-15: NEXO_CLUSTER_SOAK
// (the nightly job sets minutes), 20s by default.
func soakDuration(t *testing.T) time.Duration {
	t.Helper()
	if v := os.Getenv("NEXO_CLUSTER_SOAK"); v != "" {
		d, err := time.ParseDuration(v)
		require.NoError(t, err, "NEXO_CLUSTER_SOAK")
		return d
	}
	return 20 * time.Second
}

// TestTC15Soak: a mixed workload under random chaos (a node killed and
// restarted, frozen and resumed, or cut from the others and reconnected)
// for NEXO_CLUSTER_SOAK. The checker of the ledger verifies every read
// against the confirmed writes (read-after-write on every key, as a
// linearizability checker of single-writer registers) and the final
// state of every key; the cluster ends with every node online.
//
// covers: TC-15
func TestTC15Soak(t *testing.T) {
	d := soakDuration(t)
	c := newCluster(t, 4, withLinks())
	c.start()
	ctx := context.Background()
	mustCreateBucket(t, c.node(1).rootClient(), "tc15")
	seed := seedOf(t)
	rng := rand.New(rand.NewPCG(seed, 0xc4a05))
	l := startLoad(c.lbClient(), "tc15", seed, 8, 16)
	deadline := time.Now().Add(d)
	events := 0
	for time.Now().Before(deadline) {
		runFor(time.Duration(1000+rng.IntN(2000)) * time.Millisecond)
		victim := c.node(1 + rng.IntN(4))
		switch rng.IntN(3) {
		case 0:
			victim.kill()
			runFor(2 * time.Second)
			victim.start()
			c.waitReady(victim)
		case 1:
			victim.pause()
			runFor(2 * time.Second)
			victim.resume()
		default:
			var others []int
			for i := 1; i <= 4; i++ {
				if i != victim.idx {
					others = append(others, i)
				}
			}
			c.partition([]int{victim.idx}, others)
			runFor(2 * time.Second)
			c.heal()
		}
		events++
	}
	l.stop()
	ops, errs, violations := l.ledger.report()
	t.Logf("soak of %s: %d operations, %d chaos events, %d client errors", d, ops, events, len(errs))
	assert.Empty(t, violations, "no read contradicts a confirmed write")
	assert.Empty(t, l.ledger.verify(ctx, c.lbClient()), "final state matches the ledger")
	// Errors are possible while the node that received a request dies or
	// is cut off; the retries of the client absorb most of them.
	assert.LessOrEqual(t, len(errs), ops/20+1, "client errors: %v", errs)
	waitFor(t, 60*time.Second, "every node online after the soak", func() error {
		info, err := c.node(1).admin().ServerInfo(ctx)
		if err != nil {
			return err
		}
		for _, n := range info.Nodes {
			if n.State != "online" {
				return fmt.Errorf("%s is %s", n.Endpoint, n.State)
			}
		}
		return nil
	})
}

// rollingUpload is a multipart upload whose parts are uploaded one at a
// time while the cluster changes.
type rollingUpload struct {
	client      *s3.Client
	bucket, key string
	id          string
	parts       []types.CompletedPart
	data        []byte
}

func newRollingUpload(t *testing.T, client *s3.Client, bucket, key string) *rollingUpload {
	t.Helper()
	out, err := client.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.NoError(t, err)
	return &rollingUpload{client: client, bucket: bucket, key: key, id: aws.ToString(out.UploadId)}
}

// rollingPartSize is the minimum part size of S3 (every part but the last).
const rollingPartSize = 5 << 20

// uploadPart uploads the next part.
func (u *rollingUpload) uploadPart(t *testing.T) {
	t.Helper()
	n := int32(len(u.parts) + 1)
	body := make([]byte, rollingPartSize)
	rng := rand.New(rand.NewPCG(uint64(n), 12))
	for i := range body {
		body[i] = byte(rng.UintN(256))
	}
	out, err := u.client.UploadPart(context.Background(), &s3.UploadPartInput{
		Bucket: aws.String(u.bucket), Key: aws.String(u.key), UploadId: aws.String(u.id), PartNumber: aws.Int32(n),
		Body: bytes.NewReader(body),
	})
	require.NoError(t, err, "part %d", n)
	u.parts = append(u.parts, types.CompletedPart{PartNumber: aws.Int32(n), ETag: out.ETag})
	u.data = append(u.data, body...)
}

// complete completes the upload and checks the object.
func (u *rollingUpload) complete(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	_, err := u.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(u.bucket), Key: aws.String(u.key), UploadId: aws.String(u.id),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: u.parts},
	})
	require.NoError(t, err, "the parts acknowledged during the rolling restart complete the upload")
	got, found, err := get(ctx, u.client, u.bucket, u.key)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, sum(u.data), sum(got), "the completed object holds every part")
}
