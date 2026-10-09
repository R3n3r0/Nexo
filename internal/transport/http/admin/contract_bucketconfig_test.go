package admin_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
)

// problemBody decodes a problem document with its rule errors.
type problemBody struct {
	Code       string                    `json:"code"`
	RuleErrors []admin.RuleErrorResponse `json:"ruleErrors"`
}

// TestBucketConfigContract exercises the lifecycle, CORS, notification,
// target, Object Lock, attributes and KMS endpoints against a real
// server, every exchange validated against api/openapi.yaml.
func TestBucketConfigContract(t *testing.T) {
	srv := testutil.StartServer(t)
	c := newConsole(t, srv, srv.RootUser, srv.RootPassword)
	c.call(http.MethodPost, "/buckets", map[string]any{"name": "cfg", "versioning": true}, http.StatusCreated)

	// Lifecycle: per-rule errors, then a valid configuration round trip.
	data := c.call(http.MethodPut, "/buckets/cfg/lifecycle", map[string]any{"rules": []map[string]any{
		{"id": "ok", "expiration": map[string]any{"days": 1}},
		{"id": "zero", "expiration": map[string]any{"days": 0}},
		{"id": "ok", "noncurrentExpiration": map[string]any{"noncurrentDays": 1}},
	}}, http.StatusBadRequest)
	p := decodeAs[problemBody](t, data)
	assert.Equal(t, "invalid_configuration", p.Code)
	require.Len(t, p.RuleErrors, 2)
	assert.Equal(t, 2, p.RuleErrors[0].Rule)
	assert.Equal(t, 3, p.RuleErrors[1].Rule)
	assert.Equal(t, []admin.LifecycleRuleResponse{}, decodeAs[admin.LifecycleResponse](t, c.call(http.MethodGet, "/buckets/cfg/lifecycle", nil, http.StatusOK)).Rules,
		"nothing stored")

	set := decodeAs[admin.LifecycleResponse](t, c.call(http.MethodPut, "/buckets/cfg/lifecycle", map[string]any{"rules": []map[string]any{
		{"id": "logs", "filter": map[string]any{"prefix": "logs/", "tags": map[string]string{"k": "v"}}, "expiration": map[string]any{"days": 30}},
		{"enabled": false, "noncurrentExpiration": map[string]any{"noncurrentDays": 7}, "abortIncompleteUpload": map[string]any{"daysAfterInitiation": 2}},
	}}, http.StatusOK))
	require.Len(t, set.Rules, 2)
	assert.NotEmpty(t, set.Rules[1].ID)
	got := decodeAs[admin.LifecycleResponse](t, c.call(http.MethodGet, "/buckets/cfg/lifecycle", nil, http.StatusOK))
	assert.Equal(t, set, got)
	applied := decodeAs[admin.LifecycleApplyResponse](t, c.call(http.MethodPost, "/buckets/cfg/lifecycle/apply", nil, http.StatusOK))
	assert.Equal(t, 1, applied.Buckets)
	c.call(http.MethodDelete, "/buckets/cfg/lifecycle", nil, http.StatusNoContent)
	assert.Empty(t, decodeAs[admin.LifecycleResponse](t, c.call(http.MethodGet, "/buckets/cfg/lifecycle", nil, http.StatusOK)).Rules)

	// CORS.
	data = c.call(http.MethodPut, "/buckets/cfg/cors", map[string]any{"rules": []map[string]any{
		{"allowedOrigins": []string{"*"}, "allowedMethods": []string{"PATCH"}},
	}}, http.StatusBadRequest)
	assert.Equal(t, "invalid_configuration", decodeAs[problemBody](t, data).Code)
	cors := decodeAs[admin.CORSResponse](t, c.call(http.MethodPut, "/buckets/cfg/cors", map[string]any{"rules": []map[string]any{
		{"id": "web", "allowedOrigins": []string{"https://app.example.com"}, "allowedMethods": []string{"GET", "PUT"}, "maxAgeSeconds": 300},
	}}, http.StatusOK))
	require.Len(t, cors.Rules, 1)
	assert.Equal(t, "web", cors.Rules[0].ID)
	c.call(http.MethodGet, "/buckets/cfg/cors", nil, http.StatusOK)
	c.call(http.MethodDelete, "/buckets/cfg/cors", nil, http.StatusNoContent)

	// Notifications: a webhook target, its state, a test event and a
	// subscription naming it.
	var mu sync.Mutex
	var received []string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			EventName string `json:"EventName"`
		}
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		received = append(received, payload.EventName)
		mu.Unlock()
	}))
	defer sink.Close()
	c.call(http.MethodPut, "/config/notify_webhook", map[string]any{"settings": map[string]string{"enable": "on", "endpoint": sink.URL}}, http.StatusOK)
	var target admin.NotifyTargetResponse
	require.Eventually(t, func() bool {
		list := decodeAs[admin.NotifyTargetListResponse](t, c.call(http.MethodGet, "/notify/targets", nil, http.StatusOK))
		for _, tg := range list.Items {
			if tg.Type == "webhook" && tg.Enabled && tg.LastError == "" {
				target = tg
				return true
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond, "the target is loaded")
	assert.Equal(t, "_", target.ID)
	assert.Equal(t, sink.URL, target.Endpoint)
	test := decodeAs[admin.TargetTestResponse](t, c.call(http.MethodPost, "/notify/targets/_:webhook/test", nil, http.StatusOK))
	assert.True(t, test.Delivered, test.Error)
	mu.Lock()
	assert.Contains(t, received, "s3:TestEvent")
	mu.Unlock()
	c.call(http.MethodPost, "/notify/targets/nope:webhook/test", nil, http.StatusNotFound)

	data = c.call(http.MethodPut, "/buckets/cfg/events", map[string]any{"subscriptions": []map[string]any{
		{"arn": "arn:nexo:sqs:" + srv.Config.Region + ":missing:webhook", "events": []string{"s3:ObjectCreated:*"}},
	}}, http.StatusBadRequest)
	assert.Equal(t, 1, decodeAs[problemBody](t, data).RuleErrors[0].Rule)
	subs := decodeAs[admin.NotificationsResponse](t, c.call(http.MethodPut, "/buckets/cfg/events", map[string]any{"subscriptions": []map[string]any{
		{"id": "uploads", "arn": target.ARN, "events": []string{"s3:ObjectCreated:*"}, "prefix": "in/", "suffix": ".txt"},
	}}, http.StatusOK))
	require.Len(t, subs.Subscriptions, 1)
	c.call(http.MethodPost, "/buckets/cfg/objects/upload"+q("in/a.txt"), strings.NewReader("hello"), http.StatusCreated)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, n := range received {
			if n == "s3:ObjectCreated:Put" {
				return true
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond, "the subscription routes the upload")
	assert.Len(t, decodeAs[admin.NotificationsResponse](t, c.call(http.MethodGet, "/buckets/cfg/events", nil, http.StatusOK)).Subscriptions, 1)
	c.call(http.MethodDelete, "/buckets/cfg/events", nil, http.StatusNoContent)

	// Object Lock: retention, legal hold, deletions refused while locked.
	c.call(http.MethodPost, "/buckets", map[string]any{"name": "vault", "objectLock": true}, http.StatusCreated)
	obj := decodeAs[admin.ObjectResponse](t, c.call(http.MethodPost, "/buckets/vault/objects/upload"+q("doc.txt"), strings.NewReader("keep me"), http.StatusCreated))
	ver := "&versionId=" + obj.VersionID
	assert.Equal(t, admin.RetentionResponse{VersionID: obj.VersionID},
		decodeAs[admin.RetentionResponse](t, c.call(http.MethodGet, "/buckets/vault/objects/retention"+q("doc.txt")+ver, nil, http.StatusOK)))
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	ret := decodeAs[admin.RetentionResponse](t, c.call(http.MethodPut, "/buckets/vault/objects/retention"+q("doc.txt")+ver,
		map[string]any{"mode": "GOVERNANCE", "retainUntil": until}, http.StatusOK))
	assert.Equal(t, "GOVERNANCE", ret.Mode)
	require.NotNil(t, ret.RetainUntil)
	assert.True(t, ret.RetainUntil.Equal(until))
	c.call(http.MethodGet, "/buckets/vault/objects/retention"+q("doc.txt")+ver, nil, http.StatusOK)
	hold := decodeAs[admin.LegalHoldResponse](t, c.call(http.MethodPut, "/buckets/vault/objects/legal-hold"+q("doc.txt")+ver, map[string]string{"status": "ON"}, http.StatusOK))
	assert.Equal(t, "ON", hold.Status)
	assert.Equal(t, "ON", decodeAs[admin.LegalHoldResponse](t, c.call(http.MethodGet, "/buckets/vault/objects/legal-hold"+q("doc.txt")+ver, nil, http.StatusOK)).Status)
	del := decodeAs[admin.DeleteObjectsResponse](t, c.call(http.MethodDelete, "/buckets/vault/objects",
		map[string]any{"objects": []map[string]string{{"key": "doc.txt", "versionId": obj.VersionID}}, "bypassGovernance": true}, http.StatusOK))
	require.Len(t, del.Errors, 1, "the legal hold protects the version even with the bypass")
	assert.Equal(t, "object_locked", del.Errors[0].Code)
	c.call(http.MethodPut, "/buckets/vault/objects/legal-hold"+q("doc.txt")+ver, map[string]string{"status": "OFF"}, http.StatusOK)
	data = c.call(http.MethodPut, "/buckets/vault/objects/retention"+q("doc.txt")+ver, map[string]any{}, http.StatusForbidden)
	assert.Contains(t, string(data), "object_locked", "a GOVERNANCE retention is removed only with the bypass")
	c.call(http.MethodPut, "/buckets/vault/objects/retention"+q("doc.txt")+ver, map[string]any{"bypassGovernance": true}, http.StatusOK)
	del = decodeAs[admin.DeleteObjectsResponse](t, c.call(http.MethodDelete, "/buckets/vault/objects",
		map[string]any{"objects": []map[string]string{{"key": "doc.txt", "versionId": obj.VersionID}}}, http.StatusOK))
	assert.Len(t, del.Deleted, 1)
	assert.Empty(t, del.Errors)

	// Attributes of a multipart object.
	up := decodeAs[admin.UploadResponse](t, c.call(http.MethodPost, "/buckets/cfg/uploads"+q("big.bin"), nil, http.StatusCreated))
	var parts []admin.CompletePartRequest
	for n, size := range []int{5 << 20, 10} {
		path := "/buckets/cfg/uploads/" + up.UploadID + "/parts/" + []string{"1", "2"}[n] + q("big.bin")
		part := decodeAs[admin.PartResponse](t, c.call(http.MethodPut, path, bytes.NewReader(make([]byte, size)), http.StatusOK))
		parts = append(parts, admin.CompletePartRequest{PartNumber: part.PartNumber, ETag: part.ETag})
	}
	c.call(http.MethodPost, "/buckets/cfg/uploads/"+up.UploadID+"/complete"+q("big.bin"), admin.CompleteUploadRequest{Parts: parts}, http.StatusOK)
	attrs := decodeAs[admin.ObjectAttributesResponse](t, c.call(http.MethodGet, "/buckets/cfg/objects/attributes"+q("big.bin")+"&maxParts=1", nil, http.StatusOK))
	assert.Equal(t, 2, attrs.PartsCount)
	require.Len(t, attrs.Parts, 1)
	assert.Equal(t, int64(5<<20), attrs.Parts[0].Size)
	assert.True(t, attrs.IsTruncated)
	assert.Equal(t, int64(5<<20+10), attrs.Size)
	small := decodeAs[admin.ObjectAttributesResponse](t, c.call(http.MethodGet, "/buckets/cfg/objects/attributes"+q("in/a.txt"), nil, http.StatusOK))
	assert.Zero(t, small.PartsCount)
	assert.Empty(t, small.Parts)

	// Without KMS the status says so and the keys are not implemented
	// (contract_kms_test.go covers a configured KMS).
	c.call(http.MethodGet, "/kms/status", nil, http.StatusOK)
	c.call(http.MethodGet, "/kms/keys", nil, http.StatusNotImplemented)
	c.call(http.MethodPost, "/kms/keys", map[string]string{"id": "k1"}, http.StatusNotImplemented)
}
