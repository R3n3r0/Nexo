package integration_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/ilm"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// applyLifecycleAt applies the lifecycle of bucket with the server clock
// moved to at, then puts the clock back on the wall clock (the SDK signs
// with it: the server refuses requests too far from its own time).
func (e *env) applyLifecycleAt(clk *clock.Fake, at time.Time, bucket string) ilm.ApplyResult {
	e.t.Helper()
	clk.Set(at)
	res, err := e.srv.ILM.ApplyNow(e.ctx, bucket)
	clk.Set(time.Now())
	require.NoError(e.t, err)
	return res
}

func (e *env) putLifecycle(bucket string, rules ...types.LifecycleRule) error {
	_, err := e.root.PutBucketLifecycleConfiguration(e.ctx, &s3.PutBucketLifecycleConfigurationInput{Bucket: aws.String(bucket),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: rules}})
	return err
}

// T-7.3 / TS-U-11 end to end: the objects expire with a simulated clock
// (unversioned: removed; versioned: a delete marker over the current
// version, the noncurrent versions removed), the incomplete uploads are
// aborted, the responses carry x-amz-expiration and the expirations are
// notified as s3:LifecycleExpiration events.
//
// covers: S-BKT-07
func TestLifecycleExpiration(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now())
	e := newEnv(t, testutil.WithClock(clk))
	ctx := e.ctx
	sink := newEventSink(t)
	e.configureWebhook(sink)
	const bucket, versioned = "expiring", "expiring-ver"
	e.mkBucket(bucket)
	e.mkBucket(versioned)
	e.setVersioning(versioned, types.BucketVersioningStatusEnabled)
	e.notify(bucket, "", "s3:LifecycleExpiration:*")

	// Invalid configurations.
	err := e.putLifecycle(bucket, types.LifecycleRule{ID: aws.String("glacier"), Status: types.ExpirationStatusEnabled,
		Filter:      &types.LifecycleRuleFilter{Prefix: aws.String("")},
		Transitions: []types.Transition{{Days: aws.Int32(30), StorageClass: types.TransitionStorageClassGlacier}}})
	require.Error(t, err)
	assert.Equal(t, http.StatusNotImplemented, httpStatus(err), "no remote tier: %v", err)
	err = e.putLifecycle(bucket, types.LifecycleRule{ID: aws.String("zero"), Status: types.ExpirationStatusEnabled,
		Filter: &types.LifecycleRuleFilter{Prefix: aws.String("")}, Expiration: &types.LifecycleExpiration{Days: aws.Int32(0)}})
	assert.Equal(t, "InvalidArgument", apiCode(err))
	_, err = e.root.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(bucket)})
	assert.Equal(t, "NoSuchLifecycleConfiguration", apiCode(err))

	require.NoError(t, e.putLifecycle(bucket,
		types.LifecycleRule{ID: aws.String("tmp"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{Prefix: aws.String("tmp/")},
			Expiration: &types.LifecycleExpiration{Days: aws.Int32(1)}},
		types.LifecycleRule{ID: aws.String("uploads"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{Prefix: aws.String("")},
			AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(1)}},
	))
	got, err := e.root.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, got.Rules, 2)
	assert.Equal(t, "tmp", aws.ToString(got.Rules[0].ID))
	assert.Equal(t, int32(1), aws.ToInt32(got.Rules[0].Expiration.Days))
	assert.Equal(t, "tmp/", aws.ToString(got.Rules[0].Filter.Prefix))

	put, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("tmp/a.txt"), Body: strings.NewReader("a")})
	require.NoError(t, err)
	assert.Contains(t, aws.ToString(put.Expiration), `rule-id="tmp"`, "PUT reports the expiration")
	e.put(bucket, "keep/b.txt", []byte("b"))
	head, err := e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("tmp/a.txt")})
	require.NoError(t, err)
	assert.Regexp(t, `^expiry-date="[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} 00:00:00 GMT", rule-id="tmp"$`, aws.ToString(head.Expiration))
	out, err := e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("tmp/a.txt")})
	require.NoError(t, err)
	_ = out.Body.Close()
	assert.Equal(t, aws.ToString(head.Expiration), aws.ToString(out.Expiration), "GET reports it too")
	head, err = e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("keep/b.txt")})
	require.NoError(t, err)
	assert.Empty(t, aws.ToString(head.Expiration), "no rule for keep/")
	_, err = e.root.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("big.bin")})
	require.NoError(t, err)

	// Nothing is due yet.
	res := e.applyLifecycleAt(clk, time.Now(), bucket)
	assert.Zero(t, res.Deleted)
	// Three days later.
	res = e.applyLifecycleAt(clk, time.Now().Add(72*time.Hour), bucket)
	assert.Equal(t, 1, res.Deleted)
	assert.Equal(t, 1, res.UploadsAborted)
	_, err = e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("tmp/a.txt")})
	assert.Equal(t, http.StatusNotFound, httpStatus(err), "expired")
	assert.Equal(t, []byte("b"), e.get(bucket, "keep/b.txt"))
	uploads, err := e.root.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	assert.Empty(t, uploads.Uploads, "the incomplete upload is aborted")
	sink.waitFor("s3:LifecycleExpiration:Delete expiring/tmp/a.txt")
	rec, ok := sink.find(event.LifecycleExpirationDelete, "expiring/tmp/a.txt")
	require.True(t, ok)
	assert.Equal(t, event.LifecyclePrincipal, rec.UserIdentity.PrincipalID)

	// Versioned: the current version expires under a delete marker, the
	// noncurrent one is removed.
	v1 := e.putVersion(versioned, "doc", "one")
	v2 := e.putVersion(versioned, "doc", "two")
	require.NoError(t, e.putLifecycle(versioned,
		types.LifecycleRule{ID: aws.String("current"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{Prefix: aws.String("")},
			Expiration: &types.LifecycleExpiration{Days: aws.Int32(2)}},
		types.LifecycleRule{ID: aws.String("noncurrent"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{Prefix: aws.String("")},
			NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{NoncurrentDays: aws.Int32(1)}},
	))
	res = e.applyLifecycleAt(clk, time.Now().Add(96*time.Hour), versioned)
	assert.Equal(t, 1, res.Deleted, "v1")
	assert.Equal(t, 1, res.DeleteMarkers, "over v2")
	versions, err := e.root.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(versioned)})
	require.NoError(t, err)
	require.Len(t, versions.DeleteMarkers, 1)
	assert.True(t, aws.ToBool(versions.DeleteMarkers[0].IsLatest))
	require.Len(t, versions.Versions, 1)
	assert.Equal(t, v2, aws.ToString(versions.Versions[0].VersionId))
	assert.NotEqual(t, v1, v2)

	_, err = e.root.DeleteBucketLifecycle(ctx, &s3.DeleteBucketLifecycleInput{Bucket: aws.String(versioned)})
	require.NoError(t, err)
	_, err = e.root.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(versioned)})
	assert.Equal(t, "NoSuchLifecycleConfiguration", apiCode(err))
}
