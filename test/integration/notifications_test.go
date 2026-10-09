package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/event"
)

// sinkToken is the auth token of the test webhook.
const sinkToken = "sink-token"

// eventSink is a webhook endpoint receiving the bucket notifications; it
// can be taken offline (503).
type eventSink struct {
	t       *testing.T
	srv     *httptest.Server
	offline atomic.Bool
	mu      sync.Mutex
	got     []event.Payload
	// wire holds the eventName of the records as they were sent, by
	// "<payload event name> <bucket>/<key>".
	wire map[string]string
}

func newEventSink(t *testing.T) *eventSink {
	t.Helper()
	s := &eventSink{t: t}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.offline.Load() {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+sinkToken {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var p event.Payload
		if err := json.Unmarshal(body, &p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var raw struct {
			Records []struct {
				EventName string `json:"eventName"`
			} `json:"Records"`
		}
		_ = json.Unmarshal(body, &raw)
		s.mu.Lock()
		s.got = append(s.got, p)
		if len(raw.Records) == 1 {
			if s.wire == nil {
				s.wire = map[string]string{}
			}
			s.wire[string(p.EventName)+" "+p.Key] = raw.Records[0].EventName
		}
		s.mu.Unlock()
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// names returns "<event name> <bucket>/<key>" of the received payloads.
func (s *eventSink) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.got))
	for _, p := range s.got {
		out = append(out, string(p.EventName)+" "+p.Key)
	}
	return out
}

// find returns the record of the first payload with that event and key.
func (s *eventSink) find(name event.Name, key string) (event.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.got {
		if p.EventName == name && p.Key == key && len(p.Records) == 1 {
			return p.Records[0], true
		}
	}
	return event.Event{}, false
}

// waitFor waits until every expected "<event> <bucket>/<key>" arrived.
func (s *eventSink) waitFor(expected ...string) {
	s.t.Helper()
	require.Eventually(s.t, func() bool {
		got := map[string]bool{}
		for _, n := range s.names() {
			got[n] = true
		}
		for _, e := range expected {
			if !got[e] {
				return false
			}
		}
		return true
	}, 30*time.Second, 20*time.Millisecond, "expected %v, received %v", expected, s.names())
}

// webhookARN is the ARN of the target of the notify_webhook subsystem.
func (e *env) webhookARN() string {
	return fmt.Sprintf("arn:nexo:sqs:%s:%s:webhook", e.srv.Config.Region, event.DefaultTargetID)
}

// configureWebhook configures the notify_webhook target through the Admin
// API and waits until every node of the deployment has loaded it.
func (e *env) configureWebhook(sink *eventSink) {
	e.t.Helper()
	root := e.consoleLogin(e.rootCreds())
	body := fmt.Sprintf(`{"settings":{"enable":"on","endpoint":%q,"auth_token":%q}}`, sink.srv.URL, sinkToken)
	resp := root.do(http.MethodPut, "/config/notify_webhook", body)
	require.Equal(e.t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	id := event.TargetID{ID: event.DefaultTargetID, Type: event.TypeWebhook}
	require.Eventually(e.t, func() bool {
		for _, n := range e.srv.Nodes {
			if n.App == nil || n.App.Events() == nil {
				return false
			}
			if _, err := n.App.Events().GetTarget(e.ctx, id); err != nil {
				return false
			}
		}
		return true
	}, 30*time.Second, 20*time.Millisecond, "every node loads the target")
}

// notify subscribes the webhook to events of the keys under prefix.
func (e *env) notify(bucket, prefix string, events ...types.Event) {
	e.t.Helper()
	cfg := &types.NotificationConfiguration{QueueConfigurations: []types.QueueConfiguration{{
		Id: aws.String("to-webhook"), QueueArn: aws.String(e.webhookARN()), Events: events,
	}}}
	if prefix != "" {
		cfg.QueueConfigurations[0].Filter = &types.NotificationConfigurationFilter{Key: &types.S3KeyFilter{
			FilterRules: []types.FilterRule{{Name: types.FilterRuleNamePrefix, Value: aws.String(prefix)}},
		}}
	}
	_, err := e.root.PutBucketNotificationConfiguration(e.ctx, &s3.PutBucketNotificationConfigurationInput{Bucket: aws.String(bucket), NotificationConfiguration: cfg})
	require.NoError(e.t, err)
}

// TS-I-15: a webhook receives the events of every S3 operation that
// creates, reads, tags or removes an object, with the standard S3
// payload, once (in every mode: each node publishes the events of the
// requests it serves).
//
// covers: S-BKT-10
func TestBucketNotifications(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx
	sink := newEventSink(t)
	e.configureWebhook(sink)
	const bucket, versioned = "notified", "notified-ver"
	e.mkBucket(bucket)
	e.mkBucket(versioned)
	e.setVersioning(versioned, types.BucketVersioningStatusEnabled)

	// The configuration names configured targets only.
	_, err := e.root.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{Bucket: aws.String(bucket),
		NotificationConfiguration: &types.NotificationConfiguration{QueueConfigurations: []types.QueueConfiguration{{
			QueueArn: aws.String("arn:nexo:sqs:" + e.srv.Config.Region + ":missing:webhook"), Events: []types.Event{"s3:ObjectCreated:*"},
		}}}})
	require.Error(t, err)
	assert.Equal(t, "InvalidArgument", apiCode(err))
	_, err = e.root.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{Bucket: aws.String(bucket),
		NotificationConfiguration: &types.NotificationConfiguration{QueueConfigurations: []types.QueueConfiguration{{
			QueueArn: aws.String(e.webhookARN()), Events: []types.Event{"s3:Replication:*"},
		}}}})
	assert.Equal(t, "InvalidArgument", apiCode(err), "unknown event")

	e.notify(bucket, "watched/", "s3:ObjectCreated:*", "s3:ObjectRemoved:*", "s3:ObjectAccessed:*", "s3:ObjectTagging:*")
	e.notify(versioned, "", "s3:ObjectRemoved:*")
	got, err := e.root.GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, got.QueueConfigurations, 1)
	assert.Equal(t, e.webhookARN(), aws.ToString(got.QueueConfigurations[0].QueueArn))
	assert.Equal(t, "to-webhook", aws.ToString(got.QueueConfigurations[0].Id))
	assert.Len(t, got.QueueConfigurations[0].Events, 4)

	// Every operation that publishes.
	put, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("watched/a b.txt"),
		Body: strings.NewReader("hello"), ContentType: aws.String("text/plain"), Metadata: map[string]string{"color": "blue"}})
	require.NoError(t, err)
	e.get(bucket, "watched/a b.txt")
	_, err = e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("watched/a b.txt")})
	require.NoError(t, err)
	_, err = e.root.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(bucket), Key: aws.String("watched/copy.txt"),
		CopySource: aws.String(bucket + "/watched/a%20b.txt")})
	require.NoError(t, err)
	mpu, err := e.root.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("watched/mp.bin")})
	require.NoError(t, err)
	part, err := e.root.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String("watched/mp.bin"), UploadId: mpu.UploadId,
		PartNumber: aws.Int32(1), Body: bytes.NewReader([]byte("part one"))})
	require.NoError(t, err)
	_, err = e.root.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("watched/mp.bin"), UploadId: mpu.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{ETag: part.ETag, PartNumber: aws.Int32(1)}}}})
	require.NoError(t, err)
	_, err = e.root.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String("watched/a b.txt"),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("k"), Value: aws.String("v")}}}})
	require.NoError(t, err)
	_, err = e.root.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String("watched/a b.txt")})
	require.NoError(t, err)
	form := postPolicy{creds: e.rootCreds(), region: e.srv.Config.Region, bucket: bucket, expiration: time.Now().Add(time.Hour),
		conditions: []any{[]any{"starts-with", "$key", "watched/"}}}
	resp := e.postForm(bucket, merge(form.fields(t), map[string]string{"key": "watched/form.txt"}), "form.txt", []byte("form"))
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "%s", resp.Body)
	_, err = e.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("watched/copy.txt")})
	require.NoError(t, err)
	e.put(bucket, "other/x.txt", []byte("not watched"))
	e.putVersion(versioned, "doc", "v1")
	_, err = e.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(versioned), Key: aws.String("doc")})
	require.NoError(t, err)

	expected := []string{
		"s3:ObjectCreated:Put notified/watched/a b.txt",
		"s3:ObjectAccessed:Get notified/watched/a b.txt",
		"s3:ObjectAccessed:Head notified/watched/a b.txt",
		"s3:ObjectCreated:Copy notified/watched/copy.txt",
		"s3:ObjectCreated:CompleteMultipartUpload notified/watched/mp.bin",
		"s3:ObjectTagging:Put notified/watched/a b.txt",
		"s3:ObjectTagging:Delete notified/watched/a b.txt",
		"s3:ObjectCreated:Post notified/watched/form.txt",
		"s3:ObjectRemoved:Delete notified/watched/copy.txt",
		"s3:ObjectRemoved:DeleteMarkerCreated notified-ver/doc",
	}
	sink.waitFor(expected...)
	time.Sleep(300 * time.Millisecond)
	names := sink.names()
	sort.Strings(names)
	want := append([]string(nil), expected...)
	sort.Strings(want)
	assert.Equal(t, want, names, "each event once, nothing outside the filters")

	// The standard S3 record.
	rec, ok := sink.find(event.ObjectCreatedPut, "notified/watched/a b.txt")
	require.True(t, ok)
	assert.Equal(t, "2.1", rec.EventVersion)
	sink.mu.Lock()
	wireName := sink.wire["s3:ObjectCreated:Put notified/watched/a b.txt"]
	sink.mu.Unlock()
	assert.Equal(t, "ObjectCreated:Put", wireName, "the records name the event as S3 does, without the s3: prefix")
	assert.Equal(t, "nexo:s3", rec.EventSource)
	assert.Equal(t, e.srv.Config.Region, rec.AwsRegion)
	_, err = time.Parse("2006-01-02T15:04:05.000Z", rec.EventTime)
	assert.NoError(t, err)
	assert.Equal(t, e.srv.RootUser, rec.UserIdentity.PrincipalID)
	assert.Equal(t, "127.0.0.1", rec.RequestParameters["sourceIPAddress"])
	assert.NotEmpty(t, rec.ResponseElements["x-amz-request-id"])
	assert.Equal(t, "to-webhook", rec.S3.ConfigurationID)
	assert.Equal(t, "notified", rec.S3.Bucket.Name)
	assert.Equal(t, "arn:aws:s3:::notified", rec.S3.Bucket.ARN)
	assert.Equal(t, "watched/a+b.txt", rec.S3.Object.Key, "the key is URL-encoded")
	assert.Equal(t, int64(5), rec.S3.Object.Size)
	assert.Equal(t, strings.Trim(aws.ToString(put.ETag), `"`), rec.S3.Object.ETag)
	assert.Equal(t, "text/plain", rec.S3.Object.ContentType)
	assert.Equal(t, map[string]string{"color": "blue"}, rec.S3.Object.UserMetadata)
	assert.NotEmpty(t, rec.S3.Object.Sequencer)
	marker, ok := sink.find(event.ObjectRemovedDeleteMarkerCreated, "notified-ver/doc")
	require.True(t, ok)
	assert.NotEmpty(t, marker.S3.Object.VersionID, "the version of the delete marker")

	// An empty configuration stops the notifications.
	_, err = e.root.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{Bucket: aws.String(bucket),
		NotificationConfiguration: &types.NotificationConfiguration{}})
	require.NoError(t, err)
	e.put(bucket, "watched/after.txt", []byte("x"))
	time.Sleep(300 * time.Millisecond)
	assert.NotContains(t, sink.names(), "s3:ObjectCreated:Put notified/watched/after.txt")
}

// TS-I-15: the events of an offline target are queued and delivered when
// it comes back, also across a restart of the server.
//
// covers: S-BKT-10
func TestBucketNotificationsOfflineTarget(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sink := newEventSink(t)
	e.configureWebhook(sink)
	const bucket = "queued"
	e.mkBucket(bucket)
	e.notify(bucket, "", "s3:ObjectCreated:*", "s3:ObjectRemoved:*")

	sink.offline.Store(true)
	e.put(bucket, "first.txt", []byte("1"))
	e.put(bucket, "second.txt", []byte("2"))
	_, err := e.root.DeleteObject(e.ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("first.txt")})
	require.NoError(t, err)
	time.Sleep(500 * time.Millisecond)
	assert.Empty(t, sink.names(), "the target is offline")
	sink.offline.Store(false)
	sink.waitFor("s3:ObjectCreated:Put queued/first.txt", "s3:ObjectCreated:Put queued/second.txt", "s3:ObjectRemoved:Delete queued/first.txt")
	names := sink.names()
	require.Len(t, names, 3)
	if len(e.srv.Nodes) == 1 {
		assert.Equal(t, []string{"s3:ObjectCreated:Put queued/first.txt", "s3:ObjectCreated:Put queued/second.txt", "s3:ObjectRemoved:Delete queued/first.txt"},
			names, "the queue preserves the order")
	}

	// Queued across a restart.
	sink.offline.Store(true)
	e.put(bucket, "restart.txt", []byte("r"))
	time.Sleep(300 * time.Millisecond)
	e.srv.Restart(t)
	sink.offline.Store(false)
	sink.waitFor("s3:ObjectCreated:Put queued/restart.txt")
}
