//go:build cluster

package cluster_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// TestTC01RandomStartOrder: the nodes start in a random order with
// random delays; nobody serves before a quorum of nodes is up, then every
// node joins the same deployment.
//
// covers: TC-01
func TestTC01RandomStartOrder(t *testing.T) {
	c := newCluster(t, 4)
	seed := seedOf(t)
	rng := rand.New(rand.NewPCG(seed, 1))
	order := rng.Perm(4)

	// Two nodes of four hold 4 drives of 8: below the write quorum of the
	// format (5), the bootstrap waits.
	for _, i := range order[:2] {
		c.nodes[i].start()
		time.Sleep(time.Duration(rng.IntN(1500)) * time.Millisecond)
	}
	time.Sleep(time.Second)
	for _, i := range order[:2] {
		nd := c.nodes[i]
		assert.Equal(t, http.StatusOK, nd.probe(domain.HealthLivePath), "node %d is alive", nd.idx)
		assert.Equal(t, http.StatusServiceUnavailable, nd.probe(domain.HealthReadyPath), "node %d cannot be ready without a quorum", nd.idx)
		assert.False(t, nd.exited(), "node %d waits for its peers", nd.idx)
	}
	for _, i := range order[2:] {
		time.Sleep(time.Duration(rng.IntN(2500)) * time.Millisecond)
		c.nodes[i].start()
	}
	c.waitReady(c.nodes...)

	// One deployment: the format.json of every node and the server info
	// of every node name the same id.
	ids := map[string]bool{}
	for i := 1; i <= 4; i++ {
		ids[c.deploymentID(i)] = true
		info, err := c.node(i).admin().ServerInfo(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "distributed", info.Mode)
		ids[info.DeploymentID] = true
	}
	assert.Len(t, ids, 1, "every node joined the same deployment")

	ctx := context.Background()
	first := c.nodes[order[0]].rootClient()
	mustCreateBucket(t, first, "tc01")
	require.NoError(t, put(ctx, first, "tc01", "hello", []byte("world")))
	for _, nd := range c.nodes {
		data, found, err := get(ctx, nd.rootClient(), "tc01", "hello")
		require.NoError(t, err)
		require.True(t, found, "node %d", nd.idx)
		assert.Equal(t, "world", string(data))
	}
}

// TestTC02TopologyMismatch: a node started with another topology is
// refused by its peers and exits with a clear error, without touching the
// deployment.
//
// covers: TC-02
func TestTC02TopologyMismatch(t *testing.T) {
	c := newCluster(t, 4)
	for _, nd := range c.nodes[:3] {
		nd.start()
	}
	odd := c.node(4)
	// Same nodes, three drives per node instead of two.
	other := *c
	other.pools = []int{3}
	odd.args = other.endpoints()
	odd.start()
	c.waitReady(c.nodes[:3]...)
	require.True(t, odd.wait(readyTimeout), "the node with another topology exits")
	require.Error(t, odd.exitErr())
	assert.NotEmpty(t, odd.logLines("topology"), "the error names the topology")

	// Started with the right topology, it joins.
	odd.args = nil
	odd.start()
	c.waitReady(odd)
	assert.Equal(t, c.deploymentID(1), c.deploymentID(4))
}

// TestTC03CrossNodeConsistency: what a node writes is visible at once
// through every other node (a round-robin load balancer in front of the
// cluster), for single and multipart objects, overwrites, listings and
// deletions.
//
// covers: TC-03
func TestTC03CrossNodeConsistency(t *testing.T) {
	c := newCluster(t, 4)
	c.start()
	ctx := context.Background()
	n1, n2, n3, n4 := c.node(1).rootClient(), c.node(2).rootClient(), c.node(3).rootClient(), c.node(4).rootClient()
	mustCreateBucket(t, n1, "tc03")

	seed := seedOf(t)
	rng := rand.New(rand.NewPCG(seed, 3))
	want := map[string]string{}
	for i := range 30 {
		key := fmt.Sprintf("obj/%03d", i)
		data := content(rng)
		require.NoError(t, put(ctx, n1, "tc03", key, data))
		// Read on node 3 right after the write on node 1.
		got, found, err := get(ctx, n3, "tc03", key)
		require.NoError(t, err)
		require.True(t, found, "%s written on node 1 is visible on node 3", key)
		require.Equal(t, sum(data), sum(got), key)
		want[key] = sum(data)
	}
	// Listing on node 4.
	var listed []string
	p := s3.NewListObjectsV2Paginator(n4, &s3.ListObjectsV2Input{Bucket: aws.String("tc03"), MaxKeys: aws.Int32(7)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		require.NoError(t, err)
		for _, o := range page.Contents {
			listed = append(listed, aws.ToString(o.Key))
		}
	}
	assert.Len(t, listed, len(want))

	// Overwrite on node 2, read on node 1; delete on node 3, absent on 4.
	require.NoError(t, put(ctx, n2, "tc03", "obj/000", []byte("v2")))
	got, _, err := get(ctx, n1, "tc03", "obj/000")
	require.NoError(t, err)
	assert.Equal(t, "v2", string(got))
	_, err = n3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("tc03"), Key: aws.String("obj/001")})
	require.NoError(t, err)
	_, found, err := get(ctx, n4, "tc03", "obj/001")
	require.NoError(t, err)
	assert.False(t, found, "deleted on node 3, absent on node 4")

	// Multipart: created on node 1, parts on nodes 2 and 3, completed on
	// node 4, read on node 1.
	up, err := n1.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("tc03"), Key: aws.String("big")})
	require.NoError(t, err)
	part1 := bytes.Repeat([]byte("a"), 5<<20)
	part2 := []byte("tail")
	var parts []types.CompletedPart
	for i, pc := range []struct {
		c    *s3.Client
		data []byte
	}{{n2, part1}, {n3, part2}} {
		out, err := pc.c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("tc03"), Key: aws.String("big"), UploadId: up.UploadId,
			PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(pc.data)})
		require.NoError(t, err)
		parts = append(parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(int32(i + 1))})
	}
	_, err = n4.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("tc03"), Key: aws.String("big"), UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	require.NoError(t, err)
	got, found, err = get(ctx, n1, "tc03", "big")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, sum(append(slices.Clone(part1), part2...)), sum(got))

	// Through the load balancer every request lands on another node.
	lb := c.lbClient()
	delete(want, "obj/000")
	delete(want, "obj/001")
	for range 2 {
		assert.Empty(t, checkDataset(ctx, lb, "tc03", want))
	}
}

// TestTC04IdentityPropagation: users, policies and bucket policies
// created through the Admin API of one node are effective at once on
// every node; so is the disabling of a user.
//
// covers: TC-04
func TestTC04IdentityPropagation(t *testing.T) {
	c := newCluster(t, 4)
	c.start()
	ctx := context.Background()
	mustCreateBucket(t, c.node(1).rootClient(), "tc04")
	require.NoError(t, put(ctx, c.node(1).rootClient(), "tc04", "doc", []byte("data")))

	// A read-only user created on node 2 reads at once on every node and
	// cannot write anywhere.
	_, err := c.node(2).admin().CreateUser(ctx, nexoadmin.CreateUserRequest{AccessKey: "reader", SecretKey: "reader-secret", PolicyNames: []string{"readonly"}})
	require.NoError(t, err)
	for _, nd := range c.nodes {
		rc := client("http://"+nd.S3, "reader", "reader-secret", 1, 10*time.Second)
		data, found, err := get(ctx, rc, "tc04", "doc")
		require.NoError(t, err, "node %d", nd.idx)
		require.True(t, found)
		assert.Equal(t, "data", string(data))
		assert.Error(t, put(ctx, rc, "tc04", "denied", []byte("x")), "node %d: read-only", nd.idx)
	}

	// A policy created on node 3 and attached on node 3 lets the user
	// write through node 1.
	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:PutObject"],"Resource":["arn:aws:s3:::tc04/up/*"]}]}`
	_, err = c.node(3).admin().CreatePolicy(ctx, "tc04-upload", []byte(doc))
	require.NoError(t, err)
	require.NoError(t, c.node(3).admin().AttachPolicy(ctx, nexoadmin.AttachRequest{PolicyNames: []string{"tc04-upload"}, User: "reader"}))
	rc1 := client("http://"+c.node(1).S3, "reader", "reader-secret", 1, 10*time.Second)
	require.NoError(t, put(ctx, rc1, "tc04", "up/file", []byte("x")), "the policy attached on node 3 is effective on node 1")

	// Disabled on node 4: refused at once on nodes 1, 2 and 3.
	disabled := "disabled"
	_, err = c.node(4).admin().UpdateUser(ctx, "reader", nexoadmin.UpdateUserRequest{Status: &disabled})
	require.NoError(t, err)
	for _, nd := range c.nodes[:3] {
		_, _, err := get(ctx, client("http://"+nd.S3, "reader", "reader-secret", 1, 10*time.Second), "tc04", "doc")
		assert.Error(t, err, "node %d refuses the disabled user", nd.idx)
	}

	// A bucket policy set on node 1 opens the bucket to anonymous reads
	// on node 4.
	public := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::tc04/*"]}]}`
	require.NoError(t, c.node(1).admin().SetBucketPolicy(ctx, "tc04", []byte(public)))
	resp, err := http.Get("http://" + c.node(4).S3 + "/tc04/doc")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "data", string(body))
}

// TestTC05NodeFailureUnderLoad: one node of four crashes (SIGKILL) or
// freezes (SIGSTOP) during a mixed workload through a load balancer: the
// clients see no error (the SDK retries), no read contradicts a confirmed
// write, and the node is healed when it comes back.
//
// covers: TC-05
func TestTC05NodeFailureUnderLoad(t *testing.T) {
	for _, mode := range []string{"kill", "pause"} {
		t.Run(mode, func(t *testing.T) {
			c := newCluster(t, 4)
			c.start()
			ctx := context.Background()
			mustCreateBucket(t, c.node(1).rootClient(), "tc05")
			l := startLoad(c.lbClient(), "tc05", seedOf(t), 8, 12)

			victim := c.node(3)
			runFor(2 * time.Second)
			if mode == "kill" {
				victim.kill()
			} else {
				victim.pause()
			}
			runFor(5 * time.Second)
			if mode == "kill" {
				victim.start()
				c.waitReady(victim)
			} else {
				victim.resume()
			}
			runFor(2 * time.Second)
			l.stop()

			ops, errs, violations := l.ledger.report()
			t.Logf("%d operations", ops)
			assert.Empty(t, errs, "no client error")
			assert.Empty(t, violations, "no read contradicts a confirmed write")
			assert.Empty(t, l.ledger.verify(ctx, c.lbClient()), "final state matches the ledger")

			// Healed (MRF): every object has its latest metadata on the
			// drives of the node that was down.
			present := l.ledger.present()
			waitFor(t, 90*time.Second, "the objects written while the node was down are healed on it", func() error {
				for _, d := range c.driveDirs(3, 0) {
					if stale := l.ledger.stale(d); len(stale) > 0 {
						return fmt.Errorf("%d stale objects on %s: %v", len(stale), d, stale)
					}
				}
				return nil
			})
			// And the healed shards are right: with two other nodes down,
			// the reads need the drives of node 3.
			c.node(1).kill()
			c.node(4).kill()
			assert.Empty(t, checkDataset(ctx, c.node(2).rootClient(), "tc05", present), "the reads through the healed node match")
		})
	}
}

// TestTC06TwoNodesDown: with two nodes of four down the cluster keeps
// serving reads but refuses writes (no quorum) without corrupting
// anything; when the nodes return, everything is as before.
//
// covers: TC-06
func TestTC06TwoNodesDown(t *testing.T) {
	t.Parallel() // waits on timeouts: overlaps with the other waiting tests
	c := newCluster(t, 4)
	c.start()
	ctx := context.Background()
	mustCreateBucket(t, c.node(1).rootClient(), "tc06")
	want := dataset(t, c.node(1).rootClient(), "tc06", "obj/", 20, seedOf(t))

	c.node(3).kill()
	c.node(4).kill()
	for _, nd := range c.nodes[:2] {
		cl := client("http://"+nd.S3, rootUser, rootPassword, 1, 45*time.Second)
		assert.Empty(t, checkDataset(ctx, cl, "tc06", want), "node %d serves the reads", nd.idx)
		out, err := cl.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("tc06")})
		require.NoError(t, err)
		assert.Len(t, out.Contents, len(want))
	}
	// The writes are refused (503) once the lock acquisition gives up:
	// they run concurrently.
	cl := client("http://"+c.node(1).S3, rootUser, rootPassword, 1, 45*time.Second)
	refused := []struct {
		name string
		op   func() error
	}{
		{"new object", func() error { return put(ctx, cl, "tc06", "new", []byte("refused")) }},
		{"overwrite", func() error { return put(ctx, cl, "tc06", "obj/0000", []byte("refused")) }},
		{"deletion", func() error {
			_, err := cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("tc06"), Key: aws.String("obj/0001")})
			return err
		}},
	}
	timed(t, "refused writes", func() {
		var wg sync.WaitGroup
		for _, w := range refused {
			wg.Add(1)
			go func() {
				defer wg.Done()
				assert.Error(t, w.op(), "no write quorum for a %s", w.name)
			}()
		}
		wg.Wait()
	})
	_, found, err := get(ctx, cl, "tc06", "new")
	require.NoError(t, err)
	assert.False(t, found, "nothing of the refused write is visible")

	c.node(3).start()
	c.node(4).start()
	c.waitReady(c.node(3), c.node(4))
	for _, nd := range c.nodes {
		assert.Empty(t, checkDataset(ctx, nd.rootClient(), "tc06", want), "node %d: every object as before", nd.idx)
		_, found, err := get(ctx, nd.rootClient(), "tc06", "new")
		require.NoError(t, err)
		assert.False(t, found)
	}
}

// TestTC07PartitionNoSplitBrain: a 2+2 partition, simulated by freezing
// one half (SIGSTOP) and then the other: neither half accepts a write, and
// the deployment is intact once whole again.
//
// covers: TC-07
func TestTC07PartitionNoSplitBrain(t *testing.T) {
	t.Parallel() // waits on timeouts: overlaps with the other waiting tests
	c := newCluster(t, 4)
	c.start()
	ctx := context.Background()
	mustCreateBucket(t, c.node(1).rootClient(), "tc07")
	want := dataset(t, c.node(1).rootClient(), "tc07", "obj/", 10, seedOf(t))

	halves := [][2]int{{1, 2}, {3, 4}}
	for h, alive := range halves {
		frozen := halves[1-h]
		c.node(frozen[0]).pause()
		c.node(frozen[1]).pause()
		var wg sync.WaitGroup
		for _, i := range alive {
			cl := client("http://"+c.node(i).S3, rootUser, rootPassword, 1, 90*time.Second)
			for _, key := range []string{fmt.Sprintf("split/%d", i), "obj/0000"} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					assert.Error(t, put(ctx, cl, "tc07", key, []byte("split")), "node %d accepts no write of %s in a 2+2 partition", i, key)
				}()
			}
		}
		timed(t, fmt.Sprintf("refused writes on nodes %v", alive), wg.Wait)
		c.node(frozen[0]).resume()
		c.node(frozen[1]).resume()
	}
	for _, nd := range c.nodes {
		assert.Empty(t, checkDataset(ctx, nd.rootClient(), "tc07", want), "node %d", nd.idx)
		for i := 1; i <= 4; i++ {
			_, found, err := get(ctx, nd.rootClient(), "tc07", fmt.Sprintf("split/%d", i))
			require.NoError(t, err)
			assert.False(t, found, "no write of the partition is visible")
		}
	}
}

// TestTC09LockHolderCrash: a node that crashes while it holds the write
// lock of an object does not block the object for ever: the lock expires
// (NEXO_LOCK_STALE_AFTER) and the writes resume. The lock holder is a
// helper process joining the lock servers with the internode credentials
// (the test binary itself), killed with SIGKILL while it holds the lock.
//
// covers: TC-09
func TestTC09LockHolderCrash(t *testing.T) {
	t.Parallel() // waits on timeouts: overlaps with the other waiting tests
	c := newCluster(t, 4)
	c.start()
	ctx := context.Background()
	mustCreateBucket(t, c.node(1).rootClient(), "tc09")

	var nodes []string
	for _, nd := range c.nodes {
		nodes = append(nodes, nd.S3)
	}
	holder := exec.Command(os.Args[0], "-test.run=^$")
	holder.Env = append(nodeEnv(), envLockHolder+"=1", envLockNodes+"="+strings.Join(nodes, ","),
		envLockDeployment+"="+c.deploymentID(1), envLockResource+"=tc09/locked")
	holder.Stderr = os.Stderr
	stdout, err := holder.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, holder.Start())
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, lockHolderReady, strings.TrimSpace(line))

	// The lock is held: a write of the key waits for it.
	short := client("http://"+c.node(2).S3, rootUser, rootPassword, 1, 2*time.Second)
	assert.Error(t, put(ctx, short, "tc09", "locked", []byte("blocked")), "the key is locked by the holder")
	// Other keys are free.
	require.NoError(t, put(ctx, short, "tc09", "free", []byte("ok")))

	// The holder crashes: its lock expires on the lock servers and the
	// write goes through.
	require.NoError(t, holder.Process.Kill())
	_ = holder.Wait()
	start := time.Now()
	cl := client("http://"+c.node(3).S3, rootUser, rootPassword, 1, time.Minute)
	require.NoError(t, put(ctx, cl, "tc09", "locked", []byte("resumed")))
	elapsed := time.Since(start)
	t.Logf("write resumed %s after the crash of the holder", elapsed.Round(time.Millisecond))
	assert.Less(t, elapsed, staleAfter+10*time.Second, "the orphan lock expires within NEXO_LOCK_STALE_AFTER")
	data, _, err := get(ctx, c.node(4).rootClient(), "tc09", "locked")
	require.NoError(t, err)
	assert.Equal(t, "resumed", string(data))
}

// TestTC10DriveReplacement: the drives of a node are wiped (replaced by
// empty ones); when the node comes back they are healed, and the objects
// read through them have the original checksums.
//
// covers: TC-10
func TestTC10DriveReplacement(t *testing.T) {
	c := newCluster(t, 4)
	c.start()
	ctx := context.Background()
	mustCreateBucket(t, c.node(1).rootClient(), "tc10")
	want := dataset(t, c.node(1).rootClient(), "tc10", "obj/", 30, seedOf(t))

	victim := c.node(2)
	victim.stop()
	for _, d := range c.driveDirs(2, 0) {
		require.NoError(t, os.RemoveAll(d))
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	victim.start()
	c.waitReady(victim)
	eventually(t, 90*time.Second, "the replaced drives are healed", func() bool {
		for _, d := range c.driveDirs(2, 0) {
			if _, err := os.Stat(filepath.Join(d, domain.SystemBucket, "healing.bin")); err == nil {
				return false
			}
			for key := range want {
				if !hasMeta(d, "tc10", key) {
					return false
				}
			}
		}
		return true
	})

	// With nodes 3 and 4 down the reads need the healed drives.
	c.node(3).kill()
	c.node(4).kill()
	assert.Empty(t, checkDataset(ctx, c.node(1).rootClient(), "tc10", want), "the checksums of every object match the originals")
}

// TestTC11PoolExpansion: the deployment is restarted with a second pool;
// the old objects stay readable (and their overwrites stay in the first
// pool), the new objects are spread over both pools weighted by free
// space (the second pool has twice the drives).
//
// covers: TC-11
func TestTC11PoolExpansion(t *testing.T) {
	if !multiPool() {
		t.Skip("several pools on one machine need the per-process /proc/self/cwd of Linux")
	}
	c := newCluster(t, 4, withPools(2))
	c.start()
	ctx := context.Background()
	mustCreateBucket(t, c.node(1).rootClient(), "tc11")
	seed := seedOf(t)
	old := dataset(t, c.node(1).rootClient(), "tc11", "old/", 20, seed)

	c.stop()
	c.pools = []int{2, 4}
	c.start()
	lb := c.lbClient()
	assert.Empty(t, checkDataset(ctx, lb, "tc11", old), "the objects of the first pool are readable")

	inPool := func(key string, k int) bool {
		for i := 1; i <= 4; i++ {
			for _, d := range c.driveDirs(i, k) {
				if hasMeta(d, "tc11", key) {
					return true
				}
			}
		}
		return false
	}
	// Overwrites stay in the pool of the object.
	rng := rand.New(rand.NewPCG(seed, 11))
	for i := range 5 {
		key := fmt.Sprintf("old/%04d", i)
		data := content(rng)
		require.NoError(t, put(ctx, lb, "tc11", key, data))
		old[key] = sum(data)
		assert.True(t, inPool(key, 0) && !inPool(key, 1), "%s stays in the first pool", key)
	}
	fresh := dataset(t, lb, "tc11", "new/", 60, seed+1)
	inSecond := 0
	for key := range fresh {
		first, second := inPool(key, 0), inPool(key, 1)
		assert.True(t, first != second, "%s is in exactly one pool", key)
		if second {
			inSecond++
		}
	}
	t.Logf("%d of %d new objects in the second pool", inSecond, len(fresh))
	assert.GreaterOrEqual(t, inSecond, len(fresh)/5, "the new pool (most of the free space) receives its share of the new objects")
	assert.Empty(t, checkDataset(ctx, lb, "tc11", fresh))
	assert.Empty(t, checkDataset(ctx, lb, "tc11", old))

	// The listing merges the pools.
	out, err := lb.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("tc11")})
	require.NoError(t, err)
	assert.Len(t, out.Contents, len(old)+len(fresh))
}

// leaders returns the running nodes whose last leadership event in the
// log of their current run made them the leader.
func leaders(c *cluster) []*node {
	var out []*node
	for _, nd := range c.nodes {
		if nd.exited() {
			continue
		}
		var last string
		for _, line := range nd.logLines("cluster: ") {
			switch {
			case strings.Contains(line, "this node is the leader"):
				last = "elected"
			case strings.Contains(line, "leadership released"), strings.Contains(line, "leadership lost"):
				last = "revoked"
			}
		}
		if last == "elected" {
			out = append(out, nd)
		}
	}
	return out
}

// TestTC14SingletonLeader: exactly one node runs the singleton jobs
// (scanner, MRF, ILM); when it dies another node takes over, and the old
// leader does not take the leadership back when it returns.
//
// covers: TC-14
func TestTC14SingletonLeader(t *testing.T) {
	c := newCluster(t, 4)
	c.start()
	var leader *node
	eventually(t, 30*time.Second, "one leader", func() bool {
		l := leaders(c)
		if len(l) == 1 {
			leader = l[0]
		}
		return len(l) == 1
	})
	t.Logf("leader: node %d", leader.idx)
	time.Sleep(3 * time.Second)
	require.Len(t, leaders(c), 1, "still exactly one leader")

	start := time.Now()
	leader.kill()
	var next *node
	eventually(t, 45*time.Second, "a new leader after the crash of the leader", func() bool {
		l := leaders(c)
		if len(l) == 1 {
			next = l[0]
		}
		return len(l) == 1
	})
	t.Logf("node %d took over after %s", next.idx, time.Since(start).Round(time.Millisecond))
	assert.NotEqual(t, leader.idx, next.idx)

	leader.start()
	c.waitReady(leader)
	time.Sleep(staleAfter)
	l := leaders(c)
	require.Len(t, l, 1, "exactly one leader with every node up")
	assert.Equal(t, next.idx, l[0].idx, "the leader keeps its role")
}

// TestServerInfoNodes: the server info of any node describes every
// node of the deployment and its drives; a node that is down is reported
// offline, with its drives (the cluster page of the console, nexoctl admin
// info).
func TestServerInfoNodes(t *testing.T) {
	c := newCluster(t, 4)
	c.start()
	ctx := context.Background()
	info, err := c.node(3).admin().ServerInfo(ctx)
	require.NoError(t, err)
	assert.Equal(t, "distributed", info.Mode)
	assert.Equal(t, c.deploymentID(3), info.DeploymentID)
	require.Len(t, info.Pools, 1)
	assert.Equal(t, 8, info.Pools[0].DrivesPerSet)
	require.Len(t, info.Nodes, 4)
	assert.Equal(t, c.node(3).S3, info.Nodes[0].Endpoint, "this node first")
	assert.True(t, info.Nodes[0].Local)
	for i, n := range info.Nodes {
		assert.Equal(t, "online", n.State, n.Endpoint)
		assert.Len(t, n.Disks, 2, n.Endpoint)
		assert.NotNil(t, n.HealQueue, "background healing of %s", n.Endpoint)
		if i > 0 {
			// The peers run on the same machine: their clocks agree.
			require.NotNil(t, n.ClockSkew, n.Endpoint)
			assert.False(t, n.ClockSkew.Exceeded, n.Endpoint)
		}
	}
	// One set of 8 drives with EC:4: 4 to read, 5 to write.
	assert.Equal(t, 4, info.Pools[0].ReadQuorum)
	assert.Equal(t, 5, info.Pools[0].WriteQuorum)
	assert.Equal(t, []nexoadmin.ErasureSetInfo{{Index: 0, Drives: 8, Online: 8, State: nexoadmin.SetStateOK}}, info.Pools[0].ErasureSets)

	c.node(4).kill()
	info, err = c.node(1).admin().ServerInfo(ctx)
	require.NoError(t, err)
	require.Len(t, info.Nodes, 4)
	states := map[string]string{}
	for _, n := range info.Nodes {
		states[n.Endpoint] = n.State
		if n.Endpoint == c.node(4).S3 {
			require.Len(t, n.Disks, 2)
			assert.Equal(t, "offline", n.Disks[0].State)
		}
	}
	assert.Equal(t, map[string]string{c.node(1).S3: "online", c.node(2).S3: "online", c.node(3).S3: "online", c.node(4).S3: "offline"}, states)
	assert.Equal(t, []nexoadmin.ErasureSetInfo{{Index: 0, Drives: 8, Online: 6, Offline: 2, State: nexoadmin.SetStateDegraded}}, info.Pools[0].ErasureSets,
		"the set keeps its write quorum without a node")
}
