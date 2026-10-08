package testutil_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// nodeClient returns a path-style S3 client for base.
func nodeClient(srv *testutil.Server, base, accessKey, secretKey string) *s3.Client {
	return s3.New(s3.Options{
		Region:       srv.Config.Region,
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		BaseEndpoint: aws.String(base),
		UsePathStyle: true,
		HTTPClient:   &http.Client{},
	})
}

// TestSNMDDrives: Drives(4) runs a single node with four drives.
func TestSNMDDrives(t *testing.T) {
	srv := testutil.StartServer(t, testutil.Drives(4))
	assert.Equal(t, domain.ModeSNMD, srv.App.Mode())
	require.Len(t, srv.Nodes, 1)
	ctx := context.Background()
	c := nodeClient(srv, srv.S3URL, srv.RootUser, srv.RootPassword)
	_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("snmd")})
	require.NoError(t, err)
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("snmd"), Key: aws.String("k"), Body: bytes.NewReader([]byte("v"))})
	require.NoError(t, err)
	srv.Restart(t)
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("snmd"), Key: aws.String("k")})
	require.NoError(t, err)
	data, _ := io.ReadAll(out.Body)
	_ = out.Body.Close()
	assert.Equal(t, "v", string(data))
}

// TestClusterInProcess: StartCluster runs 4 nodes x 2 drives in the test
// process; an object written through one node is read through every
// other one (TC-03), a user created on one node is valid at once on every
// node (TC-04), and the cluster survives a restart.
func TestClusterInProcess(t *testing.T) {
	srv := testutil.StartCluster(t, 4, 2)
	require.Len(t, srv.Nodes, 4)
	assert.Equal(t, domain.ModeDistributed, srv.App.Mode())
	ctx := context.Background()
	ids := map[string]bool{}
	leaders := 0
	for _, n := range srv.Nodes {
		ids[n.App.DeploymentID()] = true
		if n.App.IsLeader() {
			leaders++
		}
	}
	assert.Len(t, ids, 1, "every node joined the same deployment")
	assert.LessOrEqual(t, leaders, 1, "at most one leader")

	first := nodeClient(srv, srv.Nodes[0].S3URL, srv.RootUser, srv.RootPassword)
	_, err := first.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("shared")})
	require.NoError(t, err)
	_, err = first.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("shared"), Key: aws.String("k"), Body: bytes.NewReader([]byte("hello"))})
	require.NoError(t, err)

	_, err = srv.Nodes[1].App.IAM().CreateUser(ctx, iam.CreateUserInput{AccessKey: "reader", SecretKey: "reader-secret", PolicyNames: []string{"readonly"}})
	require.NoError(t, err)
	for i, n := range append([]*testutil.Node{{S3URL: srv.S3URL}}, srv.Nodes...) {
		for _, creds := range [][2]string{{srv.RootUser, srv.RootPassword}, {"reader", "reader-secret"}} {
			c := nodeClient(srv, n.S3URL, creds[0], creds[1])
			out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("shared"), Key: aws.String("k")})
			require.NoError(t, err, "node %d as %s", i, creds[0])
			data, _ := io.ReadAll(out.Body)
			_ = out.Body.Close()
			assert.Equal(t, "hello", string(data))
		}
	}
	// The read-only user cannot write, on any node.
	_, err = nodeClient(srv, srv.Nodes[3].S3URL, "reader", "reader-secret").PutObject(ctx,
		&s3.PutObjectInput{Bucket: aws.String("shared"), Key: aws.String("x"), Body: bytes.NewReader(nil)})
	require.Error(t, err)

	srv.Restart(t)
	out, err := nodeClient(srv, srv.S3URL, "reader", "reader-secret").GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("shared"), Key: aws.String("k")})
	require.NoError(t, err)
	_ = out.Body.Close()
}
