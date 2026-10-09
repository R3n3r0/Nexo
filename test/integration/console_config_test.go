package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// consoleProblem is a problem document of the Admin API.
type consoleProblem struct {
	Code       string `json:"code"`
	RuleErrors []struct {
		Rule int    `json:"rule"`
		ID   string `json:"id"`
		Code string `json:"code"`
	} `json:"ruleErrors"`
}

// decodeJSON decodes a response body.
func decodeJSON[T any](t *testing.T, resp response) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(resp.Body, &v), "%s", resp.Body)
	return v
}

// requireStatus checks the status of a console response.
func requireStatus(t *testing.T, resp response, status int) {
	t.Helper()
	require.Equal(t, status, resp.StatusCode, "%s", resp.Body)
}

// TestConsoleBucketConfigurations: the console edits the lifecycle, CORS
// and notification configurations of a bucket (D-BKT-03, D-BKT-04) in
// structured form, validated rule by rule, stored as the S3 documents the
// S3 API reads; it applies the lifecycle at once, lists the notification
// targets with their state and sends them a test event; each operation is
// authorized with its S3 or admin action.
//
// covers: S-BKT-06, S-BKT-07, S-BKT-10
func TestConsoleBucketConfigurations(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now())
	e := newEnv(t, testutil.WithClock(clk))
	ctx := e.ctx
	sink := newEventSink(t)
	e.configureWebhook(sink)
	const bucket = "consoleconf"
	e.mkBucket(bucket)
	root := e.consoleLogin(e.rootCreds())

	// Lifecycle: every invalid rule is reported, nothing is stored.
	resp := root.do(http.MethodPut, "/buckets/"+bucket+"/lifecycle",
		`{"rules":[{"id":"zero","expiration":{"days":0}},{"id":"ok","expiration":{"days":1}},{"id":"both","expiration":{"days":1,"date":"2030-01-01"}}]}`)
	requireStatus(t, resp, http.StatusBadRequest)
	p := decodeJSON[consoleProblem](t, resp)
	assert.Equal(t, "invalid_configuration", p.Code)
	require.Len(t, p.RuleErrors, 2)
	assert.Equal(t, []int{1, 3}, []int{p.RuleErrors[0].Rule, p.RuleErrors[1].Rule})
	_, err := e.root.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(bucket)})
	assert.Equal(t, "NoSuchLifecycleConfiguration", apiCode(err))

	requireStatus(t, root.do(http.MethodPut, "/buckets/"+bucket+"/lifecycle",
		`{"rules":[{"id":"tmp","filter":{"prefix":"tmp/"},"expiration":{"days":1}}]}`), http.StatusOK)
	lc, err := e.root.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(bucket)})
	require.NoError(t, err, "the S3 API reads what the console wrote")
	require.Len(t, lc.Rules, 1)
	assert.Equal(t, "tmp", aws.ToString(lc.Rules[0].ID))

	// Objects written three days ago expire when the lifecycle is applied
	// at once.
	clk.Set(time.Now().Add(-72 * time.Hour))
	for _, key := range []string{"tmp/old.txt", "keep/old.txt"} {
		_, err := e.srv.App.Objects().PutObject(ctx, object.PutObjectInput{Bucket: bucket, Key: key, Reader: bytes.NewReader([]byte("old")), Size: 3})
		require.NoError(t, err)
	}
	clk.Set(time.Now())
	resp = root.do(http.MethodPost, "/buckets/"+bucket+"/lifecycle/apply", "")
	requireStatus(t, resp, http.StatusOK)
	applied := decodeJSON[struct {
		Buckets int `json:"buckets"`
		Deleted int `json:"deleted"`
	}](t, resp)
	assert.Equal(t, 1, applied.Buckets)
	assert.Equal(t, 1, applied.Deleted)
	_, err = e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("tmp/old.txt")})
	assert.Equal(t, http.StatusNotFound, httpStatus(err), "expired")
	assert.Equal(t, []byte("old"), e.get(bucket, "keep/old.txt"), "outside the rule")
	requireStatus(t, root.do(http.MethodDelete, "/buckets/"+bucket+"/lifecycle", ""), http.StatusNoContent)

	// CORS.
	resp = root.do(http.MethodPut, "/buckets/"+bucket+"/cors", `{"rules":[{"allowedOrigins":["*"],"allowedMethods":["PATCH"]}]}`)
	requireStatus(t, resp, http.StatusBadRequest)
	assert.Equal(t, "invalid_configuration", decodeJSON[consoleProblem](t, resp).Code)
	requireStatus(t, root.do(http.MethodPut, "/buckets/"+bucket+"/cors",
		`{"rules":[{"id":"app","allowedOrigins":["https://app.example.com"],"allowedMethods":["GET"],"maxAgeSeconds":120}]}`), http.StatusOK)
	cors, err := e.root.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, cors.CORSRules, 1)
	assert.Equal(t, []string{"https://app.example.com"}, cors.CORSRules[0].AllowedOrigins)
	pre := e.preflight("/"+bucket+"/keep/old.txt", "https://app.example.com", "GET", "")
	requireStatus(t, pre, http.StatusOK)
	assert.Equal(t, "https://app.example.com", pre.Header.Get("Access-Control-Allow-Origin"))
	requireStatus(t, root.do(http.MethodDelete, "/buckets/"+bucket+"/cors", ""), http.StatusNoContent)

	// Notification targets: state and test event (every node loaded the
	// target: whichever answers delivers).
	resp = root.do(http.MethodGet, "/notify/targets", "")
	requireStatus(t, resp, http.StatusOK)
	targets := decodeJSON[struct {
		Items []struct {
			ID, Type, ARN string
			Enabled       bool
			Online        bool
			PendingEvents int
		} `json:"items"`
	}](t, resp)
	require.Len(t, targets.Items, 1)
	assert.Equal(t, e.webhookARN(), targets.Items[0].ARN)
	assert.True(t, targets.Items[0].Enabled)
	resp = root.do(http.MethodPost, "/notify/targets/_:webhook/test", "")
	requireStatus(t, resp, http.StatusOK)
	assert.Contains(t, string(resp.Body), `"delivered":true`)
	sink.waitFor("s3:TestEvent /")

	// Subscriptions: validated against the configured targets, routed.
	resp = root.do(http.MethodPut, "/buckets/"+bucket+"/events", fmt.Sprintf(
		`{"subscriptions":[{"arn":%q,"events":["s3:ObjectCreated:*"]},{"arn":"arn:nexo:sqs::missing:webhook","events":["s3:ObjectCreated:*"]}]}`, e.webhookARN()))
	requireStatus(t, resp, http.StatusBadRequest)
	p = decodeJSON[consoleProblem](t, resp)
	require.Len(t, p.RuleErrors, 1)
	assert.Equal(t, 2, p.RuleErrors[0].Rule)
	requireStatus(t, root.do(http.MethodPut, "/buckets/"+bucket+"/events", fmt.Sprintf(
		`{"subscriptions":[{"id":"watched","arn":%q,"events":["s3:ObjectCreated:*"],"prefix":"watched/"}]}`, e.webhookARN())), http.StatusOK)
	got, err := e.root.GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, got.QueueConfigurations, 1)
	assert.Equal(t, "watched", aws.ToString(got.QueueConfigurations[0].Id))
	e.put(bucket, "watched/a.txt", []byte("a"))
	sink.waitFor("s3:ObjectCreated:Put " + bucket + "/watched/a.txt")
	requireStatus(t, root.do(http.MethodDelete, "/buckets/"+bucket+"/events", ""), http.StatusNoContent)
	got, err = e.root.GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	assert.Empty(t, got.QueueConfigurations)

	// Authorization: a read-only user changes nothing, a read-write user
	// configures but cannot apply the lifecycle at once (admin action), and
	// the targets need admin:ConfigUpdate.
	reader := e.consoleLogin(e.createUser("cfgreader", "cfgreader-secret", "readonly"))
	writer := e.consoleLogin(e.createUser("cfgwriter", "cfgwriter-secret", "readwrite"))
	for _, c := range []struct {
		who          *console
		method, path string
		body         string
		status       int
	}{
		{reader, http.MethodPut, "/buckets/" + bucket + "/lifecycle", `{"rules":[]}`, http.StatusForbidden},
		{reader, http.MethodPut, "/buckets/" + bucket + "/cors", `{"rules":[]}`, http.StatusForbidden},
		{reader, http.MethodPut, "/buckets/" + bucket + "/events", `{"subscriptions":[]}`, http.StatusForbidden},
		{reader, http.MethodGet, "/notify/targets", "", http.StatusForbidden},
		{writer, http.MethodPut, "/buckets/" + bucket + "/lifecycle", `{"rules":[{"expiration":{"days":30}}]}`, http.StatusOK},
		{writer, http.MethodPost, "/buckets/" + bucket + "/lifecycle/apply", "", http.StatusForbidden},
		{writer, http.MethodPost, "/notify/targets/_:webhook/test", "", http.StatusForbidden},
		{root, http.MethodPost, "/buckets/" + bucket + "/lifecycle/apply", "", http.StatusOK},
	} {
		resp := c.who.do(c.method, c.path, c.body)
		assert.Equal(t, c.status, resp.StatusCode, "%s %s: %s", c.method, c.path, resp.Body)
		if c.status == http.StatusForbidden {
			assert.Contains(t, string(resp.Body), "access_denied")
		}
	}
}

// versionBody reads one version.
func (e *env) versionBody(bucket, key, versionID string) string {
	e.t.Helper()
	body, err := e.getVersionBody(bucket, key, versionID)
	require.NoError(e.t, err)
	return body
}

// TestConsoleObjectLock: the console reads and changes the retention and
// the legal hold of the versions (D-OBJ-06), and its deletions, renames
// and restores honor Object Lock as the S3 API does: a protected version
// is never removed (COMPLIANCE not even by root), the governance bypass
// needs s3:BypassGovernanceRetention.
//
// covers: S-BKT-08, S-OBJ-12, S-OBJ-15, TS-U-12
func TestConsoleObjectLock(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx
	const bucket = "consolevault"
	e.mkLockedBucket(bucket)
	v1 := e.putVersion(bucket, "doc.txt", "one")
	v2 := e.putVersion(bucket, "doc.txt", "two")
	root := e.consoleLogin(e.rootCreds())
	q := func(key, versionID string) string {
		if versionID == "" {
			return "?key=" + key
		}
		return "?key=" + key + "&versionId=" + versionID
	}
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)

	// Retention and legal hold, read back through S3.
	resp := root.do(http.MethodGet, "/buckets/"+bucket+"/objects/retention"+q("doc.txt", v1), "")
	requireStatus(t, resp, http.StatusOK)
	assert.NotContains(t, string(resp.Body), "mode", "no retention yet")
	requireStatus(t, root.do(http.MethodPut, "/buckets/"+bucket+"/objects/retention"+q("doc.txt", v1),
		fmt.Sprintf(`{"mode":"GOVERNANCE","retainUntil":%q}`, until)), http.StatusOK)
	ret, err := e.root.GetObjectRetention(ctx, &s3.GetObjectRetentionInput{Bucket: aws.String(bucket), Key: aws.String("doc.txt"), VersionId: aws.String(v1)})
	require.NoError(t, err)
	assert.Equal(t, types.ObjectLockRetentionModeGovernance, ret.Retention.Mode)
	requireStatus(t, root.do(http.MethodPut, "/buckets/"+bucket+"/objects/legal-hold"+q("doc.txt", v2), `{"status":"ON"}`), http.StatusOK)
	hold, err := e.root.GetObjectLegalHold(ctx, &s3.GetObjectLegalHoldInput{Bucket: aws.String(bucket), Key: aws.String("doc.txt"), VersionId: aws.String(v2)})
	require.NoError(t, err)
	assert.Equal(t, types.ObjectLockLegalHoldStatusOn, hold.LegalHold.Status)
	resp = root.do(http.MethodGet, "/buckets/"+bucket+"/objects/attributes"+q("doc.txt", v2), "")
	requireStatus(t, resp, http.StatusOK)
	assert.Contains(t, string(resp.Body), `"size":3`)

	// Deletions: the locked versions are refused, the latest gets a
	// delete marker.
	deleteErrors := func(c *console, body string) map[string]string {
		t.Helper()
		resp := c.do(http.MethodDelete, "/buckets/"+bucket+"/objects", body)
		requireStatus(t, resp, http.StatusOK)
		res := decodeJSON[struct {
			Deleted []struct {
				Key          string `json:"key"`
				DeleteMarker bool   `json:"deleteMarker"`
			} `json:"deleted"`
			Errors []struct {
				VersionID string `json:"versionId"`
				Code      string `json:"code"`
			} `json:"errors"`
		}](t, resp)
		out := map[string]string{}
		for _, e := range res.Errors {
			out[e.VersionID] = e.Code
		}
		return out
	}
	assert.Equal(t, map[string]string{v1: "object_locked", v2: "object_locked"},
		deleteErrors(root, fmt.Sprintf(`{"objects":[{"key":"doc.txt","versionId":%q},{"key":"doc.txt","versionId":%q}]}`, v1, v2)))
	f1 := e.putVersion(bucket, "folder/a.txt", "a")
	requireStatus(t, root.do(http.MethodPut, "/buckets/"+bucket+"/objects/retention"+q("folder/a.txt", f1),
		fmt.Sprintf(`{"mode":"GOVERNANCE","retainUntil":%q}`, until)), http.StatusOK)
	assert.Equal(t, map[string]string{f1: "object_locked"}, deleteErrors(root, `{"prefixes":["folder"],"allVersions":true}`),
		"a recursive deletion of every version reports the locked ones")
	assert.Empty(t, deleteErrors(root, `{"objects":[{"key":"doc.txt"}]}`), "a delete marker over a locked version is allowed")
	assert.Equal(t, "one", e.versionBody(bucket, "doc.txt", v1), "the data is kept")

	// Restore of a locked version: a new version, the old one kept.
	requireStatus(t, root.do(http.MethodPost, "/buckets/"+bucket+"/objects/restore"+q("doc.txt", v1), ""), http.StatusCreated)
	assert.Equal(t, []byte("one"), e.get(bucket, "doc.txt"))
	ret, err = e.root.GetObjectRetention(ctx, &s3.GetObjectRetentionInput{Bucket: aws.String(bucket), Key: aws.String("doc.txt"), VersionId: aws.String(v1)})
	require.NoError(t, err)
	assert.Equal(t, types.ObjectLockRetentionModeGovernance, ret.Retention.Mode, "the restored version keeps its retention")

	// Rename: the source versions stay, under a delete marker.
	m1 := e.putVersion(bucket, "move.txt", "move")
	requireStatus(t, root.do(http.MethodPut, "/buckets/"+bucket+"/objects/retention"+q("move.txt", m1),
		fmt.Sprintf(`{"mode":"COMPLIANCE","retainUntil":%q}`, until)), http.StatusOK)
	resp = root.do(http.MethodPost, "/buckets/"+bucket+"/objects/rename", `{"source":"move.txt","destination":"moved.txt"}`)
	requireStatus(t, resp, http.StatusOK)
	assert.Equal(t, []byte("move"), e.get(bucket, "moved.txt"))
	assert.Equal(t, "move", e.versionBody(bucket, "move.txt", m1), "the locked source version is kept")
	assert.Equal(t, map[string]string{m1: "object_locked"},
		deleteErrors(root, fmt.Sprintf(`{"objects":[{"key":"move.txt","versionId":%q}],"bypassGovernance":true}`, m1)), "COMPLIANCE ignores the bypass")
	resp = root.do(http.MethodPut, "/buckets/"+bucket+"/objects/retention"+q("move.txt", m1), `{"bypassGovernance":true}`)
	requireStatus(t, resp, http.StatusForbidden)
	assert.Contains(t, string(resp.Body), "object_locked", "COMPLIANCE cannot be removed, not even by root")

	// Governance bypass: refused to a user without
	// s3:BypassGovernanceRetention, allowed to root.
	e.setPolicy("nobypass", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::*"]},
		{"Effect":"Deny","Action":["s3:BypassGovernanceRetention"],"Resource":["arn:aws:s3:::*"]}]}`)
	frank := e.consoleLogin(e.createUser("lockfrank", "lockfrank-secret", "nobypass"))
	assert.Equal(t, map[string]string{v1: "object_locked"},
		deleteErrors(frank, fmt.Sprintf(`{"objects":[{"key":"doc.txt","versionId":%q}],"bypassGovernance":true}`, v1)))
	resp = frank.do(http.MethodPut, "/buckets/"+bucket+"/objects/retention"+q("doc.txt", v1), `{"bypassGovernance":true}`)
	requireStatus(t, resp, http.StatusForbidden)
	assert.Contains(t, string(resp.Body), "access_denied")
	assert.Empty(t, deleteErrors(root, fmt.Sprintf(`{"objects":[{"key":"doc.txt","versionId":%q}],"bypassGovernance":true}`, v1)),
		"root bypasses the governance retention")
	_, err = e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc.txt"), VersionId: aws.String(v1)})
	assert.Error(t, err, "the version is gone")

	// The legal hold protects whatever the bypass; lifted, the version goes.
	assert.Equal(t, map[string]string{v2: "object_locked"},
		deleteErrors(root, fmt.Sprintf(`{"objects":[{"key":"doc.txt","versionId":%q}],"bypassGovernance":true}`, v2)))
	requireStatus(t, root.do(http.MethodPut, "/buckets/"+bucket+"/objects/legal-hold"+q("doc.txt", v2), `{"status":"OFF"}`), http.StatusOK)
	assert.Empty(t, deleteErrors(root, fmt.Sprintf(`{"objects":[{"key":"doc.txt","versionId":%q}]}`, v2)))
}
