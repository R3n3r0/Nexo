//go:build upgrade

package upgrade_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// nextVersion is the version of the binary the orchestrated update
// installs: the release under test, built again.
const nextVersion = "v0.9.1"

// TestOrchestratedUpdate (S-OPS-09, C-ADM-09): a cluster of 4 nodes, each
// running its own copy of the release under test, is updated by one Admin
// API request (nexoctl admin update) to a binary served over HTTP while a
// workload runs through the load balancer: every node downloads and
// verifies it, the nodes restart with it one at a time, the node answering
// last, and the job completes once it runs again. No client error, every
// read agrees with the writes, the data set reads back, every node runs
// the new version and keeps the previous binary as nexo.old. A rolling
// restart of the cluster follows, then a coordinated stop: every process
// exits.
//
// covers: S-OPS-09, C-ADM-09
func TestOrchestratedUpdate(t *testing.T) {
	ctx := context.Background()
	next := filepath.Join(t.TempDir(), "nexo-next")
	require.NoError(t, build(filepath.Join("..", ".."), next, nextVersion))
	nextData, err := os.ReadFile(next)
	require.NoError(t, err)
	sum := sha256.Sum256(nextData)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, next)
	}))
	t.Cleanup(srv.Close)
	current, err := os.ReadFile(newBin)
	require.NoError(t, err)

	d := newDeployment(t, 4, 2)
	// Every node runs its own copy of the binary, in a directory of its
	// own: the update replaces it.
	for _, nd := range d.nodes {
		bin := filepath.Join(nd.Dir, "bin", "nexo")
		require.NoError(t, os.MkdirAll(filepath.Dir(bin), 0o755))
		require.NoError(t, os.WriteFile(bin, current, 0o755)) //nolint:gosec // an executable
		nd.start(bin)
	}
	for _, nd := range d.nodes {
		nd.waitReady()
	}
	n1 := d.node(1)
	before := serverVersion(t, n1)
	ds := writeDataset(t, d.lbClient(), n1.admin(), nil)
	_, err = d.lbClient().CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucketLoad)})
	require.NoError(t, err)
	l := startLoad(d.lbClient(), 8, 12, seed(t))
	time.Sleep(2 * time.Second)

	admin := n1.admin()
	start := time.Now()
	job, err := admin.StartUpdate(ctx, nexoadmin.UpdateRequest{URL: srv.URL + "/nexo", SHA256: hex.EncodeToString(sum[:])})
	require.NoError(t, err)
	assert.Equal(t, "running", job.State)
	job = waitJob(t, admin, 10*time.Minute)
	t.Logf("orchestrated update %s -> %s in %s", before, job.Version, time.Since(start).Round(time.Millisecond))
	require.Equal(t, "completed", job.State, "%+v", job)
	assert.Equal(t, nextVersion, job.Version)
	for _, n := range job.Nodes {
		assert.Equal(t, "done", n.Phase, "%s: %s", n.Node, n.Error)
	}
	for _, nd := range d.nodes {
		assert.False(t, nd.exited(), "node %d keeps its process (replaced in place)", nd.idx)
		got, err := os.ReadFile(filepath.Join(nd.Dir, "bin", "nexo"))
		require.NoError(t, err)
		assert.Equal(t, sum, sha256.Sum256(got), "node %d runs the new binary", nd.idx)
		old, err := os.ReadFile(filepath.Join(nd.Dir, "bin", "nexo.old"))
		require.NoError(t, err)
		assert.Equal(t, sha256.Sum256(current), sha256.Sum256(old), "node %d keeps the previous binary", nd.idx)
	}
	waitFor(t, 2*time.Minute, "every node runs the new version", func() error {
		info, err := admin.ServerInfo(ctx)
		if err != nil {
			return err
		}
		for _, n := range info.Nodes {
			if n.State != "online" || n.Version != nextVersion {
				return fmt.Errorf("%s: %s %s", n.Endpoint, n.State, n.Version)
			}
		}
		return nil
	})

	// A rolling restart of every node, the workload still running.
	job, err = admin.RestartCluster(ctx, "rolling")
	require.NoError(t, err)
	assert.Equal(t, "restart", job.Kind)
	job = waitJob(t, admin, 10*time.Minute)
	require.Equal(t, "completed", job.State, "%+v", job)

	l.stop()
	ops, errs, mismatches := l.report()
	t.Logf("%d operations of the workload during the update and the restart", ops)
	assert.Greater(t, ops, 100, "the workload ran")
	assert.Empty(t, errs, "no client error during the update")
	assert.Empty(t, mismatches, "every read agrees with the acknowledged writes")
	assert.Empty(t, l.verify(ctx, d.lbClient()), "the final state of the workload")
	ds.verifyObjects(t, d.lbClient(), "updated")
	ds.verifyState(t, n1, "updated")

	// A coordinated stop: every process exits.
	require.NoError(t, admin.StopCluster(ctx))
	for _, nd := range d.nodes {
		assert.True(t, nd.wait(stopTimeout), "node %d stops", nd.idx)
	}
}

// waitJob polls the last job until it ends: the node answering restarts
// meanwhile, so the errors are retried.
func waitJob(t *testing.T, admin nexoadmin.AdminClient, timeout time.Duration) *nexoadmin.UpdateJob {
	t.Helper()
	var job *nexoadmin.UpdateJob
	waitFor(t, timeout, "the end of the job", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		j, err := admin.UpdateStatus(ctx)
		if err != nil {
			return err
		}
		if !j.Done() {
			return fmt.Errorf("job %s %s: %+v", j.ID, j.State, j.Nodes)
		}
		job = j
		return nil
	})
	return job
}
