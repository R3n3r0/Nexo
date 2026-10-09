package integration_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkLockedBucket creates a bucket with object locking.
func (e *env) mkLockedBucket(name string) {
	e.t.Helper()
	_, err := e.root.CreateBucket(e.ctx, &s3.CreateBucketInput{Bucket: aws.String(name), ObjectLockEnabledForBucket: aws.Bool(true)})
	require.NoError(e.t, err, "create bucket %s", name)
}

// deleteVersion deletes one version as c (bypass: the governance header).
func deleteVersionAs(e *env, c *s3.Client, bucket, key, versionID string, bypass bool) error {
	in := &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID)}
	if bypass {
		in.BypassGovernanceRetention = aws.Bool(true)
	}
	_, err := c.DeleteObject(e.ctx, in)
	return err
}

// TestObjectLockBucket checks the bucket side of Object Lock: enabled at
// creation (or later on a versioned bucket), with versioning that cannot
// be suspended, and a default
// retention applied to the new versions.
//
// covers: S-BKT-08, TS-U-12
func TestObjectLockBucket(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx
	e.mkLockedBucket("vault")
	vs, err := e.root.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String("vault")})
	require.NoError(t, err)
	assert.Equal(t, types.BucketVersioningStatusEnabled, vs.Status, "object locking implies versioning")
	_, err = e.root.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String("vault"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusSuspended}})
	assert.Equal(t, "InvalidBucketState", apiCode(err))
	cfg, err := e.root.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String("vault")})
	require.NoError(t, err)
	assert.Equal(t, types.ObjectLockEnabledEnabled, cfg.ObjectLockConfiguration.ObjectLockEnabled)
	assert.Nil(t, cfg.ObjectLockConfiguration.Rule)

	// An existing bucket gets it only once its versioning is Enabled, as
	// in S3; then its versioning can no longer be suspended.
	e.mkBucket("plain")
	enable := &s3.PutObjectLockConfigurationInput{Bucket: aws.String("plain"),
		ObjectLockConfiguration: &types.ObjectLockConfiguration{ObjectLockEnabled: types.ObjectLockEnabledEnabled}}
	_, err = e.root.PutObjectLockConfiguration(ctx, enable)
	assert.Equal(t, "InvalidBucketState", apiCode(err), "not on an unversioned bucket")
	e.mkBucket("later")
	e.setVersioning("later", types.BucketVersioningStatusSuspended)
	enable.Bucket = aws.String("later")
	_, err = e.root.PutObjectLockConfiguration(ctx, enable)
	assert.Equal(t, "InvalidBucketState", apiCode(err), "not with versioning suspended")
	e.setVersioning("later", types.BucketVersioningStatusEnabled)
	_, err = e.root.PutObjectLockConfiguration(ctx, enable)
	require.NoError(t, err, "a versioned bucket can enable object locking")
	cfg, err = e.root.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String("later")})
	require.NoError(t, err)
	assert.Equal(t, types.ObjectLockEnabledEnabled, cfg.ObjectLockConfiguration.ObjectLockEnabled)
	_, err = e.root.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String("later"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusSuspended}})
	assert.Equal(t, "InvalidBucketState", apiCode(err))
	_, err = e.root.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String("plain")})
	assert.Equal(t, "ObjectLockConfigurationNotFoundError", apiCode(err))
	_, err = e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("plain"), Key: aws.String("k"), Body: strings.NewReader("x"),
		ObjectLockLegalHoldStatus: types.ObjectLockLegalHoldStatusOn, ChecksumAlgorithm: types.ChecksumAlgorithmCrc32})
	assert.Equal(t, "InvalidRequest", apiCode(err), "lock settings need a bucket with object locking")

	// The default retention.
	_, err = e.root.PutObjectLockConfiguration(ctx, &s3.PutObjectLockConfigurationInput{Bucket: aws.String("vault"),
		ObjectLockConfiguration: &types.ObjectLockConfiguration{ObjectLockEnabled: types.ObjectLockEnabledEnabled,
			Rule: &types.ObjectLockRule{DefaultRetention: &types.DefaultRetention{Mode: types.ObjectLockRetentionModeGovernance, Days: aws.Int32(1)}}}})
	require.NoError(t, err)
	cfg, err = e.root.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String("vault")})
	require.NoError(t, err)
	require.NotNil(t, cfg.ObjectLockConfiguration.Rule)
	assert.Equal(t, types.ObjectLockRetentionModeGovernance, cfg.ObjectLockConfiguration.Rule.DefaultRetention.Mode)
	assert.Equal(t, int32(1), aws.ToInt32(cfg.ObjectLockConfiguration.Rule.DefaultRetention.Days))
	_, err = e.root.PutObjectLockConfiguration(ctx, &s3.PutObjectLockConfigurationInput{Bucket: aws.String("vault"),
		ObjectLockConfiguration: &types.ObjectLockConfiguration{ObjectLockEnabled: types.ObjectLockEnabledEnabled,
			Rule: &types.ObjectLockRule{DefaultRetention: &types.DefaultRetention{Mode: types.ObjectLockRetentionModeGovernance, Days: aws.Int32(1), Years: aws.Int32(1)}}}})
	assert.Equal(t, "MalformedXML", apiCode(err), "Days or Years, not both")

	put, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("vault"), Key: aws.String("doc"), Body: strings.NewReader("data")})
	require.NoError(t, err)
	head, err := e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("vault"), Key: aws.String("doc")})
	require.NoError(t, err)
	assert.Equal(t, types.ObjectLockModeGovernance, head.ObjectLockMode)
	require.NotNil(t, head.ObjectLockRetainUntilDate)
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), *head.ObjectLockRetainUntilDate, time.Hour)
	ret, err := e.root.GetObjectRetention(ctx, &s3.GetObjectRetentionInput{Bucket: aws.String("vault"), Key: aws.String("doc")})
	require.NoError(t, err)
	assert.Equal(t, types.ObjectLockRetentionModeGovernance, ret.Retention.Mode)
	assert.Equal(t, "AccessDenied", apiCode(deleteVersionAs(e, e.root, "vault", "doc", aws.ToString(put.VersionId), false)))
	require.NoError(t, deleteVersionAs(e, e.root, "vault", "doc", aws.ToString(put.VersionId), true), "root may bypass the governance mode")
}

// TestObjectLockRetentionAndLegalHold is the TS-U-12 matrix end to end:
// governance and compliance retention, legal hold, bypass with and without
// the permission, root included, through DeleteObject, DeleteObjects,
// PutObjectRetention, the forced bucket deletion and the console.
//
// covers: S-OBJ-12, S-BKT-08, TS-U-12
func TestObjectLockRetentionAndLegalHold(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx
	const bucket = "worm"
	e.mkLockedBucket(bucket)
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	putLocked := func(key string, mode types.ObjectLockMode, hold types.ObjectLockLegalHoldStatus) string {
		// Lock settings need a digest of the body: the SDK computes a CRC32.
		in := &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(key), ObjectLockLegalHoldStatus: hold,
			ChecksumAlgorithm: types.ChecksumAlgorithmCrc32}
		if mode != "" {
			in.ObjectLockMode, in.ObjectLockRetainUntilDate = mode, aws.Time(until)
		}
		out, err := e.root.PutObject(ctx, in)
		require.NoError(t, err, key)
		return aws.ToString(out.VersionId)
	}
	gov := putLocked("gov", types.ObjectLockModeGovernance, "")
	comp := putLocked("comp", types.ObjectLockModeCompliance, "")
	hold := putLocked("hold", "", types.ObjectLockLegalHoldStatusOn)

	head, err := e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("comp")})
	require.NoError(t, err)
	assert.Equal(t, types.ObjectLockModeCompliance, head.ObjectLockMode)
	assert.True(t, until.Equal(aws.ToTime(head.ObjectLockRetainUntilDate)), "%v != %v", until, head.ObjectLockRetainUntilDate)
	head, err = e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("hold")})
	require.NoError(t, err)
	assert.Equal(t, types.ObjectLockLegalHoldStatusOn, head.ObjectLockLegalHoldStatus)

	// Lock settings need a digest of the body, as in S3.
	resp := e.signedDo(http.MethodPut, "/"+bucket+"/raw", e.rootCreds(), []byte("x"), http.Header{
		"X-Amz-Object-Lock-Mode": {"GOVERNANCE"}, "X-Amz-Object-Lock-Retain-Until-Date": {until.Format(time.RFC3339)}})
	requireS3Error(t, resp, http.StatusBadRequest, "InvalidRequest")
	resp = e.signedDo(http.MethodPut, "/"+bucket+"/raw", e.rootCreds(), []byte("x"), http.Header{
		"X-Amz-Object-Lock-Mode": {"GOVERNANCE"}, "X-Amz-Object-Lock-Retain-Until-Date": {until.Format(time.RFC3339)},
		"Content-Md5": {base64MD5([]byte("x"))}})
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	resp = e.signedDo(http.MethodPut, "/"+bucket+"/past", e.rootCreds(), []byte("x"), http.Header{
		"X-Amz-Object-Lock-Mode": {"GOVERNANCE"}, "X-Amz-Object-Lock-Retain-Until-Date": {time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
		"Content-Md5": {base64MD5([]byte("x"))}})
	requireS3Error(t, resp, http.StatusBadRequest, "InvalidArgument")

	// The users: one allowed to bypass the governance mode, one not.
	e.setPolicy("worm-rw", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::worm","arn:aws:s3:::worm/*"]},
		{"Effect":"Deny","Action":["s3:BypassGovernanceRetention"],"Resource":["arn:aws:s3:::worm/*"]}]}`)
	e.setPolicy("worm-admin", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::worm","arn:aws:s3:::worm/*"]}]}`)
	user := e.clientFor(e.createUser("wormuser", "wormuser-secret", "worm-rw"))
	admin := e.clientFor(e.createUser("wormadmin", "wormadmin-secret", "worm-admin"))

	// TS-U-12: who may delete which version.
	tests := []struct {
		name    string
		client  *s3.Client
		key, id string
		bypass  bool
		allowed bool
	}{
		{"governance, user", user, "gov", gov, false, false},
		{"governance, user with header without permission", user, "gov", gov, true, false},
		{"governance, admin without header", admin, "gov", gov, false, false},
		{"compliance, admin with bypass", admin, "comp", comp, true, false},
		{"compliance, root with bypass", e.root, "comp", comp, true, false},
		{"compliance, root", e.root, "comp", comp, false, false},
		{"legal hold, root with bypass", e.root, "hold", hold, true, false},
		{"legal hold, admin with bypass", admin, "hold", hold, true, false},
	}
	for _, tt := range tests {
		err := deleteVersionAs(e, tt.client, bucket, tt.key, tt.id, tt.bypass)
		if tt.allowed {
			assert.NoError(t, err, tt.name)
			continue
		}
		assert.Equal(t, "AccessDenied", apiCode(err), tt.name)
		assert.Equal(t, http.StatusForbidden, httpStatus(err), tt.name)
	}
	// Batch deletions follow the same rules, key by key.
	batch, err := e.root.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(bucket), BypassGovernanceRetention: aws.Bool(true),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{Key: aws.String("comp"), VersionId: aws.String(comp)}, {Key: aws.String("hold"), VersionId: aws.String(hold)},
		}}})
	require.NoError(t, err)
	require.Len(t, batch.Errors, 2)
	for _, e := range batch.Errors {
		assert.Equal(t, "AccessDenied", aws.ToString(e.Code))
	}
	// A delete without version id adds a marker: allowed.
	del, err := e.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("comp")})
	require.NoError(t, err)
	assert.True(t, aws.ToBool(del.DeleteMarker))

	// The forced bucket deletion stops at the locked versions.
	resp = e.signedDo(http.MethodDelete, "/"+bucket, e.rootCreds(), nil, http.Header{"X-Nexo-Force-Delete": {"true"}})
	requireS3Error(t, resp, http.StatusForbidden, "AccessDenied")
	// And so does the console.
	console := e.consoleLogin(e.rootCreds())
	cresp := console.do(http.MethodDelete, "/buckets/"+bucket+"/objects", fmt.Sprintf(`{"objects":[{"key":"comp","versionId":%q}]}`, comp))
	require.Equal(t, http.StatusOK, cresp.StatusCode, "%s", cresp.Body)
	assert.Contains(t, string(cresp.Body), "comp", "the locked version is reported as an error")
	_, err = e.getVersionBody(bucket, "comp", comp)
	require.NoError(t, err, "the compliance version survives every attempt")
	cresp = console.do(http.MethodDelete, "/buckets/"+bucket+"?force=true", "")
	assert.NotEqual(t, http.StatusNoContent, cresp.StatusCode, "%s", cresp.Body)
	_, err = e.root.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err, "the bucket survives the console's forced deletion")

	// Retention changes: extension always, reduction in governance with
	// the permission to bypass only, never in compliance.
	setRetention := func(c *s3.Client, key, id string, mode types.ObjectLockRetentionMode, at time.Time, bypass bool) error {
		in := &s3.PutObjectRetentionInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(id),
			Retention: &types.ObjectLockRetention{Mode: mode, RetainUntilDate: aws.Time(at)}}
		if bypass {
			in.BypassGovernanceRetention = aws.Bool(true)
		}
		_, err := c.PutObjectRetention(ctx, in)
		return err
	}
	require.NoError(t, setRetention(user, "gov", gov, types.ObjectLockRetentionModeGovernance, until.Add(time.Hour), false), "extension")
	assert.Equal(t, "AccessDenied", apiCode(setRetention(user, "gov", gov, types.ObjectLockRetentionModeGovernance, until, true)), "reduction without permission")
	require.NoError(t, setRetention(admin, "gov", gov, types.ObjectLockRetentionModeGovernance, until, true), "reduction with bypass")
	require.NoError(t, setRetention(e.root, "comp", comp, types.ObjectLockRetentionModeCompliance, until.Add(time.Hour), false), "compliance extension")
	assert.Equal(t, "AccessDenied", apiCode(setRetention(e.root, "comp", comp, types.ObjectLockRetentionModeCompliance, until, true)), "compliance reduction, root")
	assert.Equal(t, "AccessDenied", apiCode(setRetention(e.root, "comp", comp, types.ObjectLockRetentionModeGovernance, until.Add(2*time.Hour), true)), "compliance to governance")
	got, err := e.root.GetObjectRetention(ctx, &s3.GetObjectRetentionInput{Bucket: aws.String(bucket), Key: aws.String("comp"), VersionId: aws.String(comp)})
	require.NoError(t, err)
	assert.Equal(t, types.ObjectLockRetentionModeCompliance, got.Retention.Mode)
	assert.True(t, until.Add(time.Hour).Equal(aws.ToTime(got.Retention.RetainUntilDate)))
	_, err = e.root.GetObjectRetention(ctx, &s3.GetObjectRetentionInput{Bucket: aws.String(bucket), Key: aws.String("hold"), VersionId: aws.String(hold)})
	assert.Equal(t, "NoSuchObjectLockConfiguration", apiCode(err))

	// Governance removal with bypass, then the version can go.
	_, err = admin.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{Bucket: aws.String(bucket), Key: aws.String("gov"), VersionId: aws.String(gov),
		Retention: &types.ObjectLockRetention{}, BypassGovernanceRetention: aws.Bool(true)})
	require.NoError(t, err)
	require.NoError(t, deleteVersionAs(e, user, bucket, "gov", gov, false))

	// Legal hold: s3:PutObjectLegalHold, then the version can go.
	lh, err := e.root.GetObjectLegalHold(ctx, &s3.GetObjectLegalHoldInput{Bucket: aws.String(bucket), Key: aws.String("hold"), VersionId: aws.String(hold)})
	require.NoError(t, err)
	assert.Equal(t, types.ObjectLockLegalHoldStatusOn, lh.LegalHold.Status)
	_, err = user.PutObjectLegalHold(ctx, &s3.PutObjectLegalHoldInput{Bucket: aws.String(bucket), Key: aws.String("hold"), VersionId: aws.String(hold),
		LegalHold: &types.ObjectLockLegalHold{Status: types.ObjectLockLegalHoldStatusOff}})
	require.NoError(t, err)
	require.NoError(t, deleteVersionAs(e, user, bucket, "hold", hold, false))
}

// TestObjectLockForceDeleteRace removes a bucket with object locking with
// the forced deletion while a client keeps writing COMPLIANCE versions:
// whatever the outcome of the deletion (refused because of a locked
// version, refused because the bucket is no longer empty, or done before
// the first locked version), no acknowledged locked version is lost and
// the deletion never answers a server error.
//
// covers: S-BKT-08, T-7.4
func TestObjectLockForceDeleteRace(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx
	const bucket = "racy"
	e.mkLockedBucket(bucket)
	// Unlocked objects sorting after the locked ones: the deletion spends
	// its time on them once the locked keys' range has been walked.
	unlocked := 300
	if raceDetector || testing.Short() {
		unlocked = 100
	}
	sem := make(chan struct{}, 16)
	errc := make(chan error, unlocked)
	for i := range unlocked {
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			_, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(fmt.Sprintf("u/%04d", i)), Body: strings.NewReader("x")})
			errc <- err
		}()
	}
	for range unlocked {
		require.NoError(t, <-errc)
	}

	type version struct{ key, id string }
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	deleted := make(chan response, 1)
	go func() {
		deleted <- e.signedDo(http.MethodDelete, "/"+bucket, e.rootCreds(), nil, http.Header{"X-Nexo-Force-Delete": {"true"}})
	}()
	var acked []version
	var resp response
	for i := 0; ; i++ {
		out, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(fmt.Sprintf("a/%d", i)),
			Body: strings.NewReader("locked"), ObjectLockMode: types.ObjectLockModeCompliance, ObjectLockRetainUntilDate: aws.Time(until),
			ChecksumAlgorithm: types.ChecksumAlgorithmCrc32})
		if err == nil {
			acked = append(acked, version{key: fmt.Sprintf("a/%d", i), id: aws.ToString(out.VersionId)})
		}
		select {
		case resp = <-deleted:
		default:
			continue
		}
		break
	}
	t.Logf("deletion: %d, %d locked versions acknowledged", resp.StatusCode, len(acked))
	assert.Contains(t, []int{http.StatusNoContent, http.StatusForbidden, http.StatusConflict}, resp.StatusCode, "%s", resp.Body)
	for _, v := range acked {
		_, err := e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(v.key), VersionId: aws.String(v.id)})
		assert.NoError(t, err, "acknowledged COMPLIANCE version %s of %s", v.id, v.key)
	}
}
