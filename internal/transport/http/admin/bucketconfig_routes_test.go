package admin_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/ilm"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
)

func intPtr(n int) *int { return &n }

func TestLifecycleRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)

	f.lifecycle.EXPECT().Get(mock.Anything, "photos").Return(&adminsvc.LifecycleConfig{Rules: []adminsvc.LifecycleRule{
		{ID: "r1", Enabled: false, Filter: adminsvc.LifecycleFilter{Prefix: "logs/"}, Expiration: &adminsvc.LifecycleExpiration{Days: intPtr(30)}},
	}}, nil)
	rec := c.do(http.MethodGet, "/buckets/photos/lifecycle", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"rules":[{"id":"r1","enabled":false,"filter":{"prefix":"logs/"},"expiration":{"days":30}}]}`, rec.Body.String())

	// enabled defaults to true; a zero number stays a zero number.
	f.lifecycle.EXPECT().Set(mock.Anything, "photos", adminsvc.LifecycleConfig{Rules: []adminsvc.LifecycleRule{
		{Enabled: true, Expiration: &adminsvc.LifecycleExpiration{Days: intPtr(0)}},
		{ID: "t", Enabled: false, Filter: adminsvc.LifecycleFilter{Tags: map[string]string{"k": "v"}},
			NoncurrentExpiration:  &adminsvc.LifecycleNoncurrentExpiration{NoncurrentDays: intPtr(7)},
			AbortIncompleteUpload: &adminsvc.LifecycleAbortUpload{DaysAfterInitiation: intPtr(2)},
			Transitions:           []adminsvc.LifecycleTransition{{Date: "2030-01-01", StorageClass: "WARM"}}},
	}}).Return(nil, &adminsvc.ConfigError{Kind: "lifecycle", Problems: []adminsvc.RuleProblem{
		{Rule: 1, Code: "InvalidArgument", Message: "'Days' for Expiration action must be a positive integer"},
	}})
	rec = c.do(http.MethodPut, "/buckets/photos/lifecycle", `{"rules":[{"expiration":{"days":0}},
		{"id":"t","enabled":false,"filter":{"tags":{"k":"v"}},"noncurrentExpiration":{"noncurrentDays":7},
		 "abortIncompleteUpload":{"daysAfterInitiation":2},"transitions":[{"date":"2030-01-01","storageClass":"WARM"}]}]}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	p := problemOf(t, rec)
	assert.Equal(t, "invalid_configuration", p.Code)
	assert.Contains(t, rec.Body.String(), `"ruleErrors":[{"rule":1,"code":"InvalidArgument","message":"'Days' for Expiration action must be a positive integer"}]`)

	f.lifecycle.EXPECT().Delete(mock.Anything, "photos").Return(nil)
	rec = c.do(http.MethodDelete, "/buckets/photos/lifecycle", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	f.lifecycle.EXPECT().Apply(mock.Anything, "photos").Return(ilm.ApplyResult{Buckets: 1, Versions: 4, Deleted: 2, DeleteMarkers: 1, Locked: 1}, nil)
	rec = c.do(http.MethodPost, "/buckets/photos/lifecycle/apply", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, admin.LifecycleApplyResponse{Buckets: 1, Versions: 4, Deleted: 2, DeleteMarkers: 1, Locked: 1}, decode[admin.LifecycleApplyResponse](t, rec))

	rec = c.do(http.MethodPut, "/buckets/photos/lifecycle", `{"rules":"nope"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestNotificationRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	sub := adminsvc.Subscription{ID: "s1", ARN: "arn:nexo:sqs:us-east-1:_:webhook", Events: []string{"s3:ObjectCreated:*"}, Prefix: "img/"}
	f.notify.EXPECT().Get(mock.Anything, "photos").Return(&adminsvc.NotificationConfig{Subscriptions: []adminsvc.Subscription{sub}}, nil)
	rec := c.do(http.MethodGet, "/buckets/photos/events", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"subscriptions":[{"id":"s1","arn":"arn:nexo:sqs:us-east-1:_:webhook","events":["s3:ObjectCreated:*"],"prefix":"img/"}]}`, rec.Body.String())

	f.notify.EXPECT().Set(mock.Anything, "photos", adminsvc.NotificationConfig{Subscriptions: []adminsvc.Subscription{sub}}).
		Return(&adminsvc.NotificationConfig{Subscriptions: []adminsvc.Subscription{sub}}, nil)
	rec = c.do(http.MethodPut, "/buckets/photos/events", `{"subscriptions":[{"id":"s1","arn":"arn:nexo:sqs:us-east-1:_:webhook","events":["s3:ObjectCreated:*"],"prefix":"img/"}]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	f.notify.EXPECT().Delete(mock.Anything, "photos").Return(nil)
	rec = c.do(http.MethodDelete, "/buckets/photos/events", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	f.notify.EXPECT().Targets(mock.Anything).Return([]adminsvc.NotifyTarget{
		{ID: "_", Type: "webhook", ARN: "arn:nexo:sqs:us-east-1:_:webhook", Enabled: true, Endpoint: "http://hook", Online: false, Pending: 4, LastError: "503"},
	}, nil)
	rec = c.do(http.MethodGet, "/notify/targets", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"items":[{"id":"_","type":"webhook","arn":"arn:nexo:sqs:us-east-1:_:webhook","enabled":true,
		"endpoint":"http://hook","online":false,"pendingEvents":4,"lastError":"503"}]}`, rec.Body.String())

	// The target is named "<id>:<type>" or by its ARN, escaped or not.
	f.notify.EXPECT().TestTarget(mock.Anything, "_:webhook").Return(&adminsvc.TargetTest{Target: "_:webhook", Delivered: true}, nil)
	rec = c.do(http.MethodPost, "/notify/targets/_:webhook/test", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, admin.TargetTestResponse{Target: "_:webhook", Delivered: true}, decode[admin.TargetTestResponse](t, rec))
	f.notify.EXPECT().TestTarget(mock.Anything, "arn:nexo:sqs::k:kafka").Return(&adminsvc.TargetTest{Target: "k:kafka", Error: "refused"}, nil)
	rec = c.do(http.MethodPost, "/notify/targets/arn%3Anexo%3Asqs%3A%3Ak%3Akafka/test", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, decode[admin.TargetTestResponse](t, rec).Delivered)
	f.notify.EXPECT().TestTarget(mock.Anything, "x:webhook").Return(nil, fmt.Errorf("%w: event target x:webhook", domain.ErrNotFound))
	rec = c.do(http.MethodPost, "/notify/targets/x:webhook/test", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestCORSRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	maxAge := 60
	rule := domain.CORSRule{ID: "web", AllowedOrigins: []string{"https://app"}, AllowedMethods: []string{"GET"}, ExposeHeaders: []string{"ETag"}, MaxAgeSeconds: &maxAge}
	f.cors.EXPECT().Get(mock.Anything, "photos").Return(&adminsvc.CORSConfig{Rules: []domain.CORSRule{}}, nil)
	rec := c.do(http.MethodGet, "/buckets/photos/cors", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"rules":[]}`, rec.Body.String())

	f.cors.EXPECT().Set(mock.Anything, "photos", adminsvc.CORSConfig{Rules: []domain.CORSRule{rule}}).Return(&adminsvc.CORSConfig{Rules: []domain.CORSRule{rule}}, nil)
	rec = c.do(http.MethodPut, "/buckets/photos/cors", `{"rules":[{"id":"web","allowedOrigins":["https://app"],"allowedMethods":["GET"],"exposeHeaders":["ETag"],"maxAgeSeconds":60}]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"rules":[{"id":"web","allowedOrigins":["https://app"],"allowedMethods":["GET"],"exposeHeaders":["ETag"],"maxAgeSeconds":60}]}`, rec.Body.String())

	f.cors.EXPECT().Delete(mock.Anything, "photos").Return(nil)
	rec = c.do(http.MethodDelete, "/buckets/photos/cors", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestObjectLockRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	until := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	ref := adminsvc.VersionRef{Bucket: "photos", Key: "a b.txt", VersionID: "v1"}

	f.browser.EXPECT().Retention(mock.Anything, ref).Return(domain.ObjectRetention{Mode: domain.RetentionCompliance, RetainUntil: until}, nil)
	rec := c.do(http.MethodGet, "/buckets/photos/objects/retention?key=a+b.txt&versionId=v1", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"versionId":"v1","mode":"COMPLIANCE","retainUntil":"2030-01-02T03:04:05Z"}`, rec.Body.String())
	f.browser.EXPECT().Retention(mock.Anything, adminsvc.VersionRef{Bucket: "photos", Key: "plain"}).Return(domain.ObjectRetention{}, nil)
	rec = c.do(http.MethodGet, "/buckets/photos/objects/retention?key=plain", nil)
	assert.JSONEq(t, `{}`, rec.Body.String())

	f.browser.EXPECT().SetRetention(mock.Anything, mock.MatchedBy(func(in adminsvc.SetRetentionInput) bool {
		return in.Requester.Identity.IsRoot && in.Object == ref && in.Retention.Mode == "governance" && in.Retention.RetainUntil.Equal(until) && in.BypassGovernance
	})).Return(domain.ObjectInfo{Key: "a b.txt", VersionID: "v1", Retention: domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: until}}, nil)
	rec = c.do(http.MethodPut, "/buckets/photos/objects/retention?key=a+b.txt&versionId=v1", `{"mode":"governance","retainUntil":"2030-01-02T03:04:05Z","bypassGovernance":true}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"versionId":"v1","mode":"GOVERNANCE","retainUntil":"2030-01-02T03:04:05Z"}`, rec.Body.String())

	f.browser.EXPECT().SetRetention(mock.Anything, mock.MatchedBy(func(in adminsvc.SetRetentionInput) bool { return in.Retention.IsZero() })).
		Return(domain.ObjectInfo{}, domain.NewS3Error(domain.ErrObjectLocked, "AccessDenied", "Access Denied because object protected by object lock."))
	rec = c.do(http.MethodPut, "/buckets/photos/objects/retention?key=locked", `{}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "object_locked", problemOf(t, rec).Code)

	f.browser.EXPECT().LegalHold(mock.Anything, ref).Return(domain.LegalHoldOff, nil)
	rec = c.do(http.MethodGet, "/buckets/photos/objects/legal-hold?key=a+b.txt&versionId=v1", nil)
	assert.JSONEq(t, `{"versionId":"v1","status":"OFF"}`, rec.Body.String())
	f.browser.EXPECT().SetLegalHold(mock.Anything, adminsvc.SetLegalHoldInput{Object: ref, Hold: "ON"}).
		Return(domain.ObjectInfo{VersionID: "v1", LegalHold: domain.LegalHoldOn}, nil)
	rec = c.do(http.MethodPut, "/buckets/photos/objects/legal-hold?key=a+b.txt&versionId=v1", `{"status":"ON"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"versionId":"v1","status":"ON"}`, rec.Body.String())

	mod := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.browser.EXPECT().Attributes(mock.Anything, adminsvc.AttributesInput{Object: adminsvc.VersionRef{Bucket: "photos", Key: "big"}, MaxParts: 2, PartNumberMarker: 1}).
		Return(&object.ObjectAttributes{
			Info: domain.ObjectInfo{Key: "big", ETag: "abc-3", Size: 30, ModTime: mod,
				Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC32C, Value: "AAAA", Type: domain.ChecksumTypeComposite}},
			PartsCount: 3, Parts: []domain.ObjectPart{{Number: 2, Size: 10, Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC32C, Value: "BBBB"}}, {Number: 3, Size: 10}},
			IsTruncated: false,
		}, nil)
	rec = c.do(http.MethodGet, "/buckets/photos/objects/attributes?key=big&maxParts=2&partNumberMarker=1", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"key":"big","etag":"abc-3","size":30,"lastModified":"2026-01-01T00:00:00Z",
		"checksum":{"algorithm":"CRC32C","type":"COMPOSITE","value":"AAAA"},"partsCount":3,
		"parts":[{"partNumber":2,"size":10,"checksum":{"algorithm":"CRC32C","value":"BBBB"}},{"partNumber":3,"size":10}],"isTruncated":false}`, rec.Body.String())
	rec = c.do(http.MethodGet, "/buckets/photos/objects/attributes?key=big&maxParts=-1", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// The console deletion asks the bypass of the governance retention.
	f.browser.EXPECT().Delete(mock.Anything, mock.MatchedBy(func(in adminsvc.DeleteObjectsInput) bool { return in.BypassGovernance })).
		Return(&adminsvc.DeleteObjectsResult{Errors: []adminsvc.ObjectError{{Key: "k", VersionID: "v", Err: domain.NewS3Error(domain.ErrObjectLocked, "AccessDenied", "locked")}}}, nil)
	rec = c.do(http.MethodDelete, "/buckets/photos/objects", `{"objects":[{"key":"k","versionId":"v"}],"bypassGovernance":true}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	res := decode[admin.DeleteObjectsResponse](t, rec)
	require.Len(t, res.Errors, 1)
	assert.Equal(t, "object_locked", res.Errors[0].Code)
}

func TestKMSRoutesAreNotImplementedYet(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodGet, "/kms/status", ""}, {http.MethodGet, "/kms/keys", ""}, {http.MethodPost, "/kms/keys", `{"id":"k1"}`},
	} {
		var body any
		if rt.body != "" {
			body = rt.body
		}
		rec := c.do(rt.method, rt.path, body)
		assert.Equal(t, http.StatusNotImplemented, rec.Code, rt.path)
		assert.Equal(t, "not_implemented", problemOf(t, rec).Code)
	}
}

// TestBucketConfigAuthorizationDenied: every new route asks its S3 or
// admin action first and answers 403 access_denied when it is denied.
func TestBucketConfigAuthorizationDenied(t *testing.T) {
	tests := []struct {
		method, path, body string
		actions            []policy.Action
	}{
		{http.MethodGet, "/buckets/b1/lifecycle", "", []policy.Action{policy.ActionGetLifecycle}},
		{http.MethodPut, "/buckets/b1/lifecycle", `{"rules":[]}`, []policy.Action{policy.ActionPutLifecycle}},
		{http.MethodDelete, "/buckets/b1/lifecycle", "", []policy.Action{policy.ActionPutLifecycle}},
		{http.MethodPost, "/buckets/b1/lifecycle/apply", "", []policy.Action{policy.ActionPutLifecycle, policy.ActionAdminApplyLifecycle}},
		{http.MethodGet, "/buckets/b1/events", "", []policy.Action{policy.ActionGetBucketNotification}},
		{http.MethodPut, "/buckets/b1/events", `{"subscriptions":[]}`, []policy.Action{policy.ActionPutBucketNotification}},
		{http.MethodDelete, "/buckets/b1/events", "", []policy.Action{policy.ActionPutBucketNotification}},
		{http.MethodGet, "/notify/targets", "", []policy.Action{policy.ActionAdminConfigWrite}},
		{http.MethodPost, "/notify/targets/_:webhook/test", "", []policy.Action{policy.ActionAdminConfigWrite}},
		{http.MethodGet, "/buckets/b1/cors", "", []policy.Action{policy.ActionGetBucketCORS}},
		{http.MethodPut, "/buckets/b1/cors", `{"rules":[]}`, []policy.Action{policy.ActionPutBucketCORS}},
		{http.MethodDelete, "/buckets/b1/cors", "", []policy.Action{policy.ActionPutBucketCORS}},
		{http.MethodGet, "/buckets/b1/objects/retention?key=k", "", []policy.Action{policy.ActionGetObjectRetention}},
		{http.MethodGet, "/buckets/b1/objects/legal-hold?key=k", "", []policy.Action{policy.ActionGetObjectLegalHold}},
		{http.MethodPut, "/buckets/b1/objects/legal-hold?key=k", `{"status":"ON"}`, []policy.Action{policy.ActionPutObjectLegalHold}},
		{http.MethodGet, "/buckets/b1/objects/attributes?key=k", "", []policy.Action{policy.ActionGetObjectAttributes}},
		{http.MethodGet, "/buckets/b1/objects/attributes?key=k&versionId=v1", "", []policy.Action{policy.ActionGetObjectVersionAttributes}},
		{http.MethodGet, "/kms/status", "", []policy.Action{policy.ActionAdminKMSKeyStatus}},
		{http.MethodPost, "/kms/keys", `{"id":"k"}`, []policy.Action{policy.ActionAdminKMSCreateKey}},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			f := newFixture(t)
			c := f.session(t, bobKey, bobSecret)
			f.denyAll()
			var body any
			if tt.body != "" {
				body = tt.body
			}
			rec := c.do(tt.method, tt.path, body)
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			p := problemOf(t, rec)
			assert.Equal(t, "access_denied", p.Code)
			assert.Contains(t, p.Detail, string(tt.actions[0]))
			in := f.lastCheck()
			assert.Equal(t, tt.actions, in.Actions)
			wantBucket := ""
			if strings.HasPrefix(tt.path, "/buckets/") {
				wantBucket = "b1"
			}
			assert.Equal(t, wantBucket, in.Bucket, "the scope of the actions")
		})
	}
}

// TestBucketConfigGuardConditions: the object routes carry the condition
// values of the S3 request they stand for.
func TestBucketConfigGuardConditions(t *testing.T) {
	f := newFixture(t)
	c := f.session(t, bobKey, bobSecret)
	f.browser.EXPECT().LegalHold(mock.Anything, mock.Anything).Return(domain.LegalHoldOn, nil)
	f.resetChecks()
	rec := c.do(http.MethodGet, "/buckets/b1/objects/legal-hold?key=a%2Fb&versionId=v9", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	in := f.lastCheck()
	assert.Equal(t, "b1", in.Bucket)
	assert.Equal(t, "a/b", in.Object)
	assert.Equal(t, map[string][]string{"s3:versionid": {"v9"}}, in.Conditions)
	assert.Equal(t, bobKey, in.Requester.Identity.AccessKey)
}

// TestRetentionAuthorizedByService: the governance bypass travels in the
// body, so the service authorizes the retention change and denies it.
func TestRetentionAuthorizedByService(t *testing.T) {
	f := newFixture(t)
	f.browser.EXPECT().SetRetention(mock.Anything, mock.MatchedBy(func(in adminsvc.SetRetentionInput) bool { return in.Requester.Identity.AccessKey == bobKey })).
		Return(domain.ObjectInfo{}, fmt.Errorf("%w: s3:BypassGovernanceRetention: implicit deny", domain.ErrAccessDenied))
	rec := f.session(t, bobKey, bobSecret).do(http.MethodPut, "/buckets/b1/objects/retention?key=k", `{"bypassGovernance":true}`)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "access_denied", problemOf(t, rec).Code)
}

func TestBucketConfigConstructorsRefuseMissingServices(t *testing.T) {
	_, err := admin.NewLifecycleRoutes(admin.LifecycleDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewNotificationRoutes(admin.NotificationDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewCORSRoutes(admin.CORSDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewKMSRoutes(admin.KMSDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}
