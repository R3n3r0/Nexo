package integration_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// replicate makes from replicate bucket to the same bucket of to, with
// its delete markers and deletions.
func replicate(t *testing.T, from, to *env, bucket string) string {
	t.Helper()
	admin := adminClient(t, from)
	target, err := admin.SetReplicationTarget(from.ctx, bucket, nexoadmin.ReplicationTarget{
		Endpoint: to.srv.S3URL, Bucket: bucket, AccessKey: to.srv.RootUser, SecretKey: to.srv.RootPassword, Label: "peer",
	})
	require.NoError(t, err)
	require.NotEmpty(t, target.ARN)
	assert.Empty(t, target.SecretKey, "the secret is never returned")
	_, err = admin.SetBucketReplication(from.ctx, bucket, nexoadmin.ReplicationConfiguration{Rules: []nexoadmin.ReplicationRule{{
		ID: "all", Priority: 1, Enabled: true, Target: target.ARN, DeleteMarkers: true, Deletes: true, Existing: true,
	}}})
	require.NoError(t, err)
	return target.ARN
}

func versioned(t *testing.T, e *env, bucket string) {
	t.Helper()
	e.mkBucket(bucket)
	_, err := e.root.PutBucketVersioning(e.ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
	require.NoError(t, err)
}

// readVersion returns the content of a version, or an error.
func readVersion(ctx context.Context, c *s3.Client, bucket, key, versionID string) ([]byte, *s3.GetObjectOutput, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if versionID != "" {
		in.VersionId = aws.String(versionID)
	}
	out, err := c.GetObject(ctx, in)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	return data, out, err
}

// TestBucketReplication (S-BKT-12): two deployments replicating a bucket
// to each other (active-active). A version written on either side reaches
// the other with its id, time and ETag, stored as a REPLICA and never sent
// back; delete markers follow; the last writer wins; GetBucketReplication
// returns the rules set through the Admin API; a resync replicates the
// versions written before the rule.
//
// covers: S-BKT-12
func TestBucketReplication(t *testing.T) {
	t.Parallel()
	a, b := newEnv(t), newEnv(t)
	const bucket = "replicated"
	versioned(t, a, bucket)
	versioned(t, b, bucket)
	_, err := a.root.PutObject(a.ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("before"), Body: bytes.NewReader([]byte("written before the rule"))})
	require.NoError(t, err)

	// A replication configuration needs versioning and targets of the
	// bucket.
	plain := "plain"
	a.mkBucket(plain)
	_, err = adminClient(t, a).SetBucketReplication(a.ctx, plain, nexoadmin.ReplicationConfiguration{Rules: []nexoadmin.ReplicationRule{{ID: "x", Enabled: true, Target: "arn:nexo:replication::nope:plain"}}})
	require.Error(t, err)

	arnA := replicate(t, a, b, bucket)
	replicate(t, b, a, bucket)

	// The rules are visible through the S3 API too.
	cfg, err := a.root.GetBucketReplication(a.ctx, &s3.GetBucketReplicationInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, cfg.ReplicationConfiguration.Rules, 1)
	assert.Equal(t, arnA, aws.ToString(cfg.ReplicationConfiguration.Rules[0].Destination.Bucket))
	// Versioning cannot be suspended under a replication configuration.
	_, err = a.root.PutBucketVersioning(a.ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusSuspended}})
	require.Error(t, err)

	put := func(e *env, key, data string) *s3.PutObjectOutput {
		out, err := e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader([]byte(data)),
			ContentType: aws.String("text/plain"), Metadata: map[string]string{"site": "x"}})
		require.NoError(t, err)
		return out
	}
	waitVersion := func(e *env, key, versionID, want string) *s3.GetObjectOutput {
		var got *s3.GetObjectOutput
		require.Eventually(t, func() bool {
			data, out, err := readVersion(e.ctx, e.root, bucket, key, versionID)
			if err != nil || string(data) != want {
				return false
			}
			got = out
			return true
		}, 30*time.Second, 50*time.Millisecond, "%s %s", key, versionID)
		return got
	}

	fromA := put(a, "docs/a", "written on A")
	fromB := put(b, "docs/b", "written on B")
	gotB := waitVersion(b, "docs/a", aws.ToString(fromA.VersionId), "written on A")
	assert.Equal(t, "REPLICA", string(gotB.ReplicationStatus))
	assert.Equal(t, aws.ToString(fromA.ETag), aws.ToString(gotB.ETag))
	assert.Equal(t, "x", gotB.Metadata["site"])
	gotA := waitVersion(a, "docs/b", aws.ToString(fromB.VersionId), "written on B")
	assert.Equal(t, "REPLICA", string(gotA.ReplicationStatus))
	require.Eventually(t, func() bool {
		head, err := a.root.HeadObject(a.ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("docs/a")})
		return err == nil && head.ReplicationStatus == types.ReplicationStatusCompleted
	}, 30*time.Second, 50*time.Millisecond)

	// Last writer wins: the newest of two concurrent writes is the
	// latest on both sides.
	put(a, "lww", "first")
	time.Sleep(5 * time.Millisecond)
	last := put(b, "lww", "second")
	for _, e := range []*env{a, b} {
		waitVersion(e, "lww", aws.ToString(last.VersionId), "second")
		require.Eventually(t, func() bool {
			data, _, err := readVersion(e.ctx, e.root, bucket, "lww", "")
			return err == nil && string(data) == "second"
		}, 30*time.Second, 50*time.Millisecond)
	}

	// A delete marker on B hides the object on A.
	del, err := b.root.DeleteObject(b.ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("docs/a")})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, _, err := readVersion(a.ctx, a.root, bucket, "docs/a", "")
		return err != nil
	}, 30*time.Second, 50*time.Millisecond)
	versions, err := a.root.ListObjectVersions(a.ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket), Prefix: aws.String("docs/a")})
	require.NoError(t, err)
	require.Len(t, versions.DeleteMarkers, 1)
	assert.Equal(t, aws.ToString(del.VersionId), aws.ToString(versions.DeleteMarkers[0].VersionId))

	// No ping-pong: the versions settle at one per write.
	time.Sleep(500 * time.Millisecond)
	for e, want := range map[*env]int{a: 5, b: 4} {
		// docs/a, docs/b, the two lww and, on A only, before.
		all, err := e.root.ListObjectVersions(e.ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
		require.NoError(t, err)
		assert.Len(t, all.Versions, want)
	}

	// The resync of A sends what was written before the rule.
	admin := adminClient(t, a)
	res, err := admin.StartReplicationResync(a.ctx, bucket, arnA)
	require.NoError(t, err)
	assert.Equal(t, "running", res.State)
	waitVersion(b, "before", "", "written before the rule")
	require.Eventually(t, func() bool {
		st, err := admin.GetReplicationStatus(a.ctx, bucket)
		require.NoError(t, err)
		return st.Resync != nil && st.Resync.State == "complete"
	}, 30*time.Second, 50*time.Millisecond)
	st, err := admin.GetReplicationStatus(a.ctx, bucket)
	require.NoError(t, err)
	require.Len(t, st.Targets, 1)
	assert.True(t, st.Targets[0].Online)
	if modeOf(t) != modeCluster {
		// The counters are those of the node that answers: in a cluster
		// the writes went to the other nodes too.
		assert.Positive(t, st.Targets[0].Replicated)
	}

	// A target the rules use cannot go; without the rules it can.
	var apiErr *nexoadmin.APIError
	require.True(t, errors.As(admin.RemoveReplicationTarget(a.ctx, bucket, arnA), &apiErr))
	require.NoError(t, admin.DeleteBucketReplication(a.ctx, bucket))
	require.NoError(t, admin.RemoveReplicationTarget(a.ctx, bucket, arnA))
	targets, err := admin.ListReplicationTargets(a.ctx, bucket)
	require.NoError(t, err)
	assert.Empty(t, targets)
	got, err := admin.GetBucketReplication(a.ctx, bucket)
	require.NoError(t, err)
	assert.Empty(t, got.Rules)
}
