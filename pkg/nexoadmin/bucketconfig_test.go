package nexoadmin_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

func intPtr(n int) *int { return &n }

// TestBucketConfigurationsAgainstServer drives the lifecycle, CORS,
// notification, target and Object Lock methods against a real server,
// every exchange validated against api/openapi.yaml.
func TestBucketConfigurationsAgainstServer(t *testing.T) {
	h := newHarness(t)
	ctx, c := h.ctx, h.root
	_, err := c.CreateBucket(ctx, nexoadmin.CreateBucketRequest{Name: "conf", Versioning: true})
	require.NoError(t, err)

	t.Run("lifecycle", func(t *testing.T) {
		_, err := c.SetBucketLifecycle(ctx, "conf", nexoadmin.LifecycleConfiguration{Rules: []nexoadmin.LifecycleRule{
			{ID: "zero", Expiration: &nexoadmin.LifecycleExpiration{Days: intPtr(0)}},
		}})
		e := apiError(t, err)
		assert.Equal(t, nexoadmin.CodeInvalidConfiguration, e.Code)
		require.Len(t, e.RuleErrors, 1)
		assert.Equal(t, nexoadmin.RuleError{Rule: 1, ID: "zero", Code: "InvalidArgument", Message: "'Days' for Expiration action must be a positive integer"}, e.RuleErrors[0])
		assert.Contains(t, e.Error(), "rule 1:")

		disabled := false
		set, err := c.SetBucketLifecycle(ctx, "conf", nexoadmin.LifecycleConfiguration{Rules: []nexoadmin.LifecycleRule{
			{ID: "tmp", Filter: &nexoadmin.LifecycleFilter{Prefix: "tmp/"}, Expiration: &nexoadmin.LifecycleExpiration{Days: intPtr(1)}},
			{Enabled: &disabled, NoncurrentExpiration: &nexoadmin.LifecycleNoncurrentExpiration{NoncurrentDays: intPtr(3)}},
		}})
		require.NoError(t, err)
		require.Len(t, set.Rules, 2)
		require.NotNil(t, set.Rules[0].Enabled)
		assert.True(t, *set.Rules[0].Enabled)
		assert.False(t, *set.Rules[1].Enabled)
		got, err := c.GetBucketLifecycle(ctx, "conf")
		require.NoError(t, err)
		assert.Equal(t, set, got)

		// The S3 API reads what the Admin API wrote.
		out, err := h.s3.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String("conf")})
		require.NoError(t, err)
		require.Len(t, out.Rules, 2)
		assert.Equal(t, "tmp", aws.ToString(out.Rules[0].ID))

		res, err := c.ApplyBucketLifecycle(ctx, "conf")
		require.NoError(t, err)
		assert.Equal(t, 1, res.Buckets)
		require.NoError(t, c.DeleteBucketLifecycle(ctx, "conf"))
		got, err = c.GetBucketLifecycle(ctx, "conf")
		require.NoError(t, err)
		assert.Empty(t, got.Rules)
	})

	t.Run("cors", func(t *testing.T) {
		set, err := c.SetBucketCORS(ctx, "conf", nexoadmin.CORSConfiguration{Rules: []nexoadmin.CORSRule{
			{ID: "web", AllowedOrigins: []string{"https://app.example.com"}, AllowedMethods: []string{"GET"}, MaxAgeSeconds: intPtr(60)},
		}})
		require.NoError(t, err)
		require.Len(t, set.Rules, 1)
		got, err := c.GetBucketCORS(ctx, "conf")
		require.NoError(t, err)
		assert.Equal(t, set, got)
		_, err = c.SetBucketCORS(ctx, "conf", nexoadmin.CORSConfiguration{Rules: []nexoadmin.CORSRule{{AllowedOrigins: []string{"*"}}}})
		assert.Equal(t, nexoadmin.CodeInvalidConfiguration, apiError(t, err).Code)
		require.NoError(t, c.DeleteBucketCORS(ctx, "conf"))
	})

	t.Run("notifications", func(t *testing.T) {
		var delivered atomic.Int32
		sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			delivered.Add(1)
		}))
		defer sink.Close()
		_, err := c.SetConfig(ctx, "notify_webhook:hook", map[string]string{"enable": "on", "endpoint": sink.URL})
		require.NoError(t, err)
		var target nexoadmin.NotifyTarget
		require.Eventually(t, func() bool {
			targets, err := c.ListNotifyTargets(ctx)
			require.NoError(t, err)
			for _, tg := range targets {
				if tg.ID == "hook" && tg.LastError == "" {
					target = tg
					return true
				}
			}
			return false
		}, 10*time.Second, 20*time.Millisecond)
		assert.Equal(t, "webhook", target.Type)
		assert.True(t, target.Online)
		res, err := c.TestNotifyTarget(ctx, "hook:webhook")
		require.NoError(t, err)
		assert.True(t, res.Delivered, res.Error)
		res, err = c.TestNotifyTarget(ctx, target.ARN)
		require.NoError(t, err)
		assert.True(t, res.Delivered, res.Error)
		assert.Equal(t, int32(2), delivered.Load())
		_, err = c.TestNotifyTarget(ctx, "none:webhook")
		assert.True(t, nexoadmin.IsNotFound(err), "%v", err)

		set, err := c.SetBucketEvents(ctx, "conf", nexoadmin.BucketNotifications{Subscriptions: []nexoadmin.EventSubscription{
			{ARN: target.ARN, Events: []string{"s3:ObjectRemoved:*"}, Suffix: ".log"},
		}})
		require.NoError(t, err)
		require.Len(t, set.Subscriptions, 1)
		assert.NotEmpty(t, set.Subscriptions[0].ID)
		out, err := h.s3.GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{Bucket: aws.String("conf")})
		require.NoError(t, err)
		require.Len(t, out.QueueConfigurations, 1, "the S3 API reads what the Admin API wrote")
		require.NoError(t, c.DeleteBucketEvents(ctx, "conf"))
		got, err := c.GetBucketEvents(ctx, "conf")
		require.NoError(t, err)
		assert.Empty(t, got.Subscriptions)
	})

	t.Run("object lock", func(t *testing.T) {
		_, err := c.CreateBucket(ctx, nexoadmin.CreateBucketRequest{Name: "locked", ObjectLock: true})
		require.NoError(t, err)
		put, err := h.s3.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("locked"), Key: aws.String("a.txt"), Body: strings.NewReader("a")})
		require.NoError(t, err)
		v := aws.ToString(put.VersionId)
		until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		ret, err := c.SetObjectRetention(ctx, "locked", "a.txt", v, nexoadmin.SetObjectRetentionRequest{Mode: "COMPLIANCE", RetainUntil: &until})
		require.NoError(t, err)
		assert.Equal(t, "COMPLIANCE", ret.Mode)
		got, err := c.GetObjectRetention(ctx, "locked", "a.txt", v)
		require.NoError(t, err)
		require.NotNil(t, got.RetainUntil)
		assert.True(t, got.RetainUntil.Equal(until))
		_, err = c.SetObjectRetention(ctx, "locked", "a.txt", v, nexoadmin.SetObjectRetentionRequest{BypassGovernance: true})
		assert.Equal(t, nexoadmin.CodeObjectLocked, apiError(t, err).Code, "COMPLIANCE cannot be removed, not even by root")

		hold, err := c.SetObjectLegalHold(ctx, "locked", "a.txt", "", "ON")
		require.NoError(t, err)
		assert.Equal(t, "ON", hold.Status)
		hold, err = c.GetObjectLegalHold(ctx, "locked", "a.txt", v)
		require.NoError(t, err)
		assert.Equal(t, "ON", hold.Status)

		res, err := c.DeleteObjects(ctx, "locked", nexoadmin.DeleteObjectsRequest{Objects: []nexoadmin.ObjectRef{{Key: "a.txt", VersionID: v}}, BypassGovernance: true})
		require.NoError(t, err)
		require.Len(t, res.Errors, 1)
		assert.Equal(t, nexoadmin.CodeObjectLocked, res.Errors[0].Code)

		attrs, err := c.GetObjectAttributes(ctx, "locked", "a.txt", nexoadmin.ObjectAttributesOptions{VersionID: v, MaxParts: 10})
		require.NoError(t, err)
		assert.Equal(t, int64(1), attrs.Size)
		assert.Equal(t, v, attrs.VersionID)
		assert.Zero(t, attrs.PartsCount)
	})

	t.Logf("operations exercised: %v", h.contract.operations())
}

func TestRuleErrorsDecoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"title":"Bad Request","status":400,"code":"invalid_configuration","ruleErrors":[{"rule":2,"id":"r","code":"InvalidRequest","message":"bad"}]}`)
	}))
	defer srv.Close()
	c, err := nexoadmin.New(nexoadmin.Config{Endpoint: srv.URL, AccessKey: "ak", SecretKey: "secret-key"})
	require.NoError(t, err)
	_, err = c.SetBucketCORS(context.Background(), "b", nexoadmin.CORSConfiguration{})
	e := apiError(t, err)
	assert.Equal(t, []nexoadmin.RuleError{{Rule: 2, ID: "r", Code: "InvalidRequest", Message: "bad"}}, e.RuleErrors)
	assert.Contains(t, e.Error(), "rule 2: bad")
}
