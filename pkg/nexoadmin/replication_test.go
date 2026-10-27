package nexoadmin_test

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// TestReplicationAgainstServer drives the bucket replication methods
// against a server replicating a bucket to another of its own buckets,
// every exchange validated against api/openapi.yaml.
//
// covers: S-BKT-12
func TestReplicationAgainstServer(t *testing.T) {
	h := newHarness(t)
	ctx, c := h.ctx, h.root
	for _, name := range []string{"src", "dst"} {
		_, err := h.s3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)})
		require.NoError(t, err)
		_, err = h.s3.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(name),
			VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
		require.NoError(t, err)
	}

	cfg, err := c.GetBucketReplication(ctx, "src")
	require.NoError(t, err)
	assert.Empty(t, cfg.Rules)
	_, err = c.SetReplicationTarget(ctx, "src", nexoadmin.ReplicationTarget{Endpoint: h.srv.S3URL, Bucket: "dst", AccessKey: h.srv.RootUser, SecretKey: "wrong"})
	assert.Equal(t, http.StatusBadRequest, apiError(t, err).Status, "the remote is checked")
	target, err := c.SetReplicationTarget(ctx, "src", nexoadmin.ReplicationTarget{Endpoint: h.srv.S3URL, Bucket: "dst", AccessKey: h.srv.RootUser, SecretKey: h.srv.RootPassword})
	require.NoError(t, err)
	targets, err := c.ListReplicationTargets(ctx, "src")
	require.NoError(t, err)
	require.Len(t, targets, 1)
	assert.Empty(t, targets[0].SecretKey)

	set, err := c.SetBucketReplication(ctx, "src", nexoadmin.ReplicationConfiguration{Rules: []nexoadmin.ReplicationRule{{
		ID: "logs", Priority: 1, Enabled: true, Prefix: "logs/", Tags: map[string]string{"keep": "yes"}, Target: target.ARN, DeleteMarkers: true,
	}}})
	require.NoError(t, err)
	require.Len(t, set.Rules, 1)
	assert.Equal(t, map[string]string{"keep": "yes"}, set.Rules[0].Tags)

	_, err = h.s3.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("src"), Key: aws.String("logs/a"), Body: bytes.NewReader([]byte("a")), Tagging: aws.String("keep=yes")})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := h.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("dst"), Key: aws.String("logs/a")})
		return err == nil
	}, 20*time.Second, 50*time.Millisecond)
	st, err := c.GetReplicationStatus(ctx, "src")
	require.NoError(t, err)
	require.Len(t, st.Targets, 1)
	res, err := c.StartReplicationResync(ctx, "src", target.ARN)
	require.NoError(t, err)
	assert.Equal(t, target.ARN, res.Target)

	require.Error(t, c.RemoveReplicationTarget(ctx, "src", target.ARN))
	require.NoError(t, c.DeleteBucketReplication(ctx, "src"))
	require.NoError(t, c.RemoveReplicationTarget(ctx, "src", target.ARN))
}
