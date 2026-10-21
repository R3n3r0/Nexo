package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// TestPoolDecommission (S-STO-09, C-ADM-10): on a node with two pools,
// the decommission of the second one moves every version of every object
// to the first; once it is complete, the server restarted without the
// second pool serves every object and version as before. In the modes of
// the matrix (one pool) the API reports the pool and refuses to drain it.
//
// covers: S-STO-09, C-ADM-10
func TestPoolDecommission(t *testing.T) {
	t.Parallel()
	if modeOf(t) != modeSNSD {
		e := newEnv(t)
		admin := adminClient(t, e)
		pools, err := admin.ListPools(e.ctx)
		require.NoError(t, err)
		require.Len(t, pools.Pools, 1)
		var apiErr *nexoadmin.APIError
		require.ErrorAs(t, admin.DecommissionPool(e.ctx, 1), &apiErr)
		assert.Equal(t, 400, apiErr.Status)
		return
	}
	// Single drive: nothing to manage.
	e := newEnv(t)
	_, err := adminClient(t, e).ListPools(e.ctx)
	var apiErr *nexoadmin.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 501, apiErr.Status)

	ctx := context.Background()
	base := t.TempDir()
	pool1 := filepath.Join(base, "p1", "d{1...4}")
	pool2 := filepath.Join(base, "p2", "d{1...4}")
	srv := testutil.StartServer(t, testutil.WithEndpoints(pool1, pool2))
	e = &env{t: t, srv: srv, ctx: ctx}
	e.root = e.client(srv.RootUser, srv.RootPassword, "")
	admin := adminClient(t, e)

	e.mkBucket("plain")
	e.mkBucket("versioned")
	_, err = e.root.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String("versioned"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
	require.NoError(t, err)
	want := map[string][]byte{}
	for i := 0; i < 40; i++ {
		key := fmt.Sprintf("dir%d/obj-%02d", i%4, i)
		data := bytes.Repeat([]byte{byte('a' + i%26)}, 10+i*2111)
		_, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("plain"), Key: aws.String(key), Body: bytes.NewReader(data)})
		require.NoError(t, err)
		want["plain/"+key] = data
	}
	var versions []string
	for i := 0; i < 3; i++ {
		out, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("versioned"), Key: aws.String("doc"),
			Body: bytes.NewReader([]byte(fmt.Sprintf("version %d", i)))})
		require.NoError(t, err)
		versions = append(versions, aws.ToString(out.VersionId))
	}
	_, err = e.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("versioned"), Key: aws.String("doc")})
	require.NoError(t, err)

	pools, err := admin.ListPools(ctx)
	require.NoError(t, err)
	require.Len(t, pools.Pools, 2)
	require.NoError(t, admin.DecommissionPool(ctx, 2))
	require.Eventually(t, func() bool {
		pools, err := admin.ListPools(ctx)
		require.NoError(t, err)
		d := pools.Pools[1].Decommission
		return d != nil && d.State == "complete"
	}, 2*time.Minute, 200*time.Millisecond)
	pools, err = admin.ListPools(ctx)
	require.NoError(t, err)
	assert.True(t, pools.Pools[1].Draining)

	// The pool goes: restart on the first pool alone.
	srv.Stop(t)
	srv = testutil.StartServer(t, testutil.WithEndpoints(pool1))
	e = &env{t: t, srv: srv, ctx: ctx}
	e.root = e.client(srv.RootUser, srv.RootPassword, "")
	for name, data := range want {
		out, err := e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("plain"), Key: aws.String(name[len("plain/"):])})
		require.NoError(t, err, name)
		got, err := io.ReadAll(out.Body)
		_ = out.Body.Close()
		require.NoError(t, err)
		assert.Equal(t, data, got, name)
	}
	for i, v := range versions {
		out, err := e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("versioned"), Key: aws.String("doc"), VersionId: aws.String(v)})
		require.NoError(t, err, v)
		got, _ := io.ReadAll(out.Body)
		_ = out.Body.Close()
		assert.Equal(t, fmt.Sprintf("version %d", i), string(got))
	}
	_, err = e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("versioned"), Key: aws.String("doc")})
	require.Error(t, err, "the delete marker is still the latest version")
	listed, err := e.root.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String("versioned")})
	require.NoError(t, err)
	assert.Len(t, listed.Versions, 3)
	assert.Len(t, listed.DeleteMarkers, 1)
}
