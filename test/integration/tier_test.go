package integration_test

import (
	"bytes"
	"crypto/rand"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// TestTiering (S-STO-12, S-BKT-07 transitions): a second deployment is the
// remote tier of the first. A lifecycle rule moves the data of the old
// versions there: they read the same (whole and by range), report the
// tier as their storage class, and a copy brings their data back; the
// tier holding data cannot be removed, and its objects no version refers
// to any more are swept.
//
// covers: S-STO-12, S-BKT-07
func TestTiering(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now())
	a := newEnv(t, testutil.WithClock(clk))
	remote := newEnv(t)
	admin := adminClient(t, a)
	ctx := a.ctx
	remote.mkBucket("cold-tier")

	cold := nexoadmin.TierInput{Name: "COLD", Endpoint: remote.srv.S3URL, Bucket: "cold-tier", Prefix: "nexo/",
		AccessKey: remote.srv.RootUser, SecretKey: remote.srv.RootPassword}
	bad := cold
	bad.SecretKey = "not-the-secret"
	_, err := admin.AddTier(ctx, bad)
	require.Error(t, err, "the remote bucket is checked with the credentials")
	_, err = admin.AddTier(ctx, cold)
	require.NoError(t, err)
	tiers, err := admin.ListTiers(ctx)
	require.NoError(t, err)
	require.Len(t, tiers, 1)
	assert.Equal(t, "COLD", tiers[0].Name)

	const bucket = "data"
	a.mkBucket(bucket)
	a.setVersioning(bucket, types.BucketVersioningStatusEnabled)
	require.NoError(t, a.putLifecycle(bucket, types.LifecycleRule{ID: aws.String("cold"), Status: types.ExpirationStatusEnabled,
		Filter:      &types.LifecycleRuleFilter{Prefix: aws.String("archive/")},
		Transitions: []types.Transition{{Days: aws.Int32(2), StorageClass: types.TransitionStorageClass("COLD")}}}))
	big := make([]byte, 3<<20+12345)
	_, _ = rand.Read(big)
	put, err := a.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("archive/big.bin"), Body: bytes.NewReader(big)})
	require.NoError(t, err)
	a.put(bucket, "archive/small.txt", []byte("small"))
	a.put(bucket, "live/hot.bin", big[:1<<20])

	res := a.applyLifecycleAt(clk, time.Now().Add(24*time.Hour), bucket)
	assert.Zero(t, res.Transitioned, "not due yet")
	res = a.applyLifecycleAt(clk, time.Now().Add(4*24*time.Hour), bucket)
	assert.Equal(t, 1, res.Transitioned, "the small object is stored inline and stays")

	head, err := a.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("archive/big.bin")})
	require.NoError(t, err)
	assert.Equal(t, types.StorageClass("COLD"), head.StorageClass)
	assert.Equal(t, aws.ToString(put.ETag), aws.ToString(head.ETag))
	data, _, err := readVersion(ctx, a.root, bucket, "archive/big.bin", "")
	require.NoError(t, err)
	assert.True(t, bytes.Equal(big, data), "the transitioned data reads the same")
	out, err := a.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("archive/big.bin"), Range: aws.String("bytes=1000000-1999999")})
	require.NoError(t, err)
	part := new(bytes.Buffer)
	_, err = part.ReadFrom(out.Body)
	require.NoError(t, out.Body.Close())
	require.NoError(t, err)
	assert.Equal(t, big[1000000:2000000], part.Bytes())
	list, err := a.root.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String("archive/")})
	require.NoError(t, err)
	classes := map[string]types.ObjectStorageClass{}
	for _, o := range list.Contents {
		classes[aws.ToString(o.Key)] = o.StorageClass
	}
	assert.Equal(t, types.ObjectStorageClass("COLD"), classes["archive/big.bin"])
	assert.Equal(t, types.ObjectStorageClassStandard, classes["archive/small.txt"])

	// The data is in the remote bucket, under the prefix of the deployment.
	remoteObjects := func() []string {
		page, err := remote.root.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("cold-tier"), Prefix: aws.String("nexo/")})
		require.NoError(t, err)
		var names []string
		for _, o := range page.Contents {
			names = append(names, aws.ToString(o.Key))
		}
		return names
	}
	require.Len(t, remoteObjects(), 1)

	// A tier holding data cannot be removed.
	err = admin.RemoveTier(ctx, "COLD")
	assert.True(t, nexoadmin.IsConflict(err), "%v", err)

	// A copy onto itself brings the data back; the tier object, now an
	// orphan, is swept once old enough.
	_, err = a.root.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(bucket), Key: aws.String("archive/big.bin"),
		CopySource: aws.String(bucket + "/archive/big.bin"), StorageClass: types.StorageClassStandard})
	require.NoError(t, err)
	head, err = a.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("archive/big.bin")})
	require.NoError(t, err)
	assert.Empty(t, head.StorageClass, "STANDARD again")
	// The old version still refers to the tier object: it stays.
	clk.Set(time.Now().Add(3 * time.Hour))
	sweep, err := a.srv.Tiers.Sweep(ctx)
	clk.Set(time.Now())
	require.NoError(t, err)
	assert.Zero(t, sweep.Removed, "the noncurrent version still refers to it")
	require.Len(t, remoteObjects(), 1)
	_, err = a.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("archive/big.bin"), VersionId: put.VersionId})
	require.NoError(t, err)
	clk.Set(time.Now().Add(3 * time.Hour))
	sweep, err = a.srv.Tiers.Sweep(ctx)
	clk.Set(time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, sweep.Removed)
	assert.Empty(t, remoteObjects())
	data, _, err = readVersion(ctx, a.root, bucket, "archive/big.bin", "")
	require.NoError(t, err)
	assert.True(t, bytes.Equal(big, data))

	require.NoError(t, admin.RemoveTier(ctx, "COLD"))
	tiers, err = admin.ListTiers(ctx)
	require.NoError(t, err)
	assert.Empty(t, tiers)
}
