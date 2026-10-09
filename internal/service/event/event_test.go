package event_test

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/event"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files of testdata")

func TestNameMatch(t *testing.T) {
	tests := []struct {
		name    event.Name
		pattern event.Name
		want    bool
	}{
		{event.ObjectCreatedPut, event.ObjectCreatedAll, true},
		{event.ObjectCreatedPut, event.ObjectCreatedPut, true},
		{event.ObjectCreatedPut, "*", true},
		{event.ObjectCreatedPut, event.ObjectRemovedAll, false},
		{event.ObjectCreatedPut, event.ObjectCreatedCopy, false},
		{event.LifecycleExpirationDelete, event.LifecycleExpirationAll, true},
		{event.ObjectTaggingPut, event.ObjectTaggingAll, true},
		{event.ObjectTaggingPut, event.ObjectCreatedAll, false},
		{event.ObjectCreatedPutTagging, event.ObjectTaggingAll, true},
	}
	for _, tc := range tests {
		t.Run(string(tc.name)+"~"+string(tc.pattern), func(t *testing.T) {
			assert.Equal(t, tc.want, tc.name.Match(tc.pattern))
		})
	}
}

func TestNameIsNotifiable(t *testing.T) {
	for _, n := range []event.Name{
		event.ObjectCreatedAll, event.ObjectCreatedPut, event.ObjectCreatedPost, event.ObjectCreatedCopy,
		event.ObjectCreatedCompleteMultipartUpload, event.ObjectRemovedAll, event.ObjectRemovedDelete,
		event.ObjectRemovedDeleteMarkerCreated, event.ObjectAccessedGet, event.ObjectAccessedHead,
		event.LifecycleExpirationAll, event.LifecycleExpirationDelete, event.LifecycleExpirationDeleteMarkerCreated,
		event.ObjectTaggingAll, event.ObjectTaggingPut, event.ObjectTaggingDelete,
	} {
		assert.True(t, n.IsNotifiable(), n)
	}
	for _, n := range []event.Name{event.BucketCreated, "s3:Replication:*", "", "*"} {
		assert.False(t, n.IsNotifiable(), n)
	}
}

func TestFilterMatches(t *testing.T) {
	e := event.NewEvent(event.ObjectCreatedPut, "us-east-1", time.Now())
	e.S3.Bucket.Name = "photos"
	e.S3.Object.Key = "2024/img.jpg"

	tests := []struct {
		name   string
		filter event.Filter
		want   bool
	}{
		{"zero", event.Filter{}, true},
		{"all criteria", event.Filter{Bucket: "photos", Prefix: "2024/", Suffix: ".jpg", Events: []event.Name{event.ObjectCreatedAll}}, true},
		{"other bucket", event.Filter{Bucket: "other"}, false},
		{"other prefix", event.Filter{Prefix: "2023/"}, false},
		{"other suffix", event.Filter{Suffix: ".png"}, false},
		{"other events", event.Filter{Events: []event.Name{event.ObjectRemovedAll}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.filter.Matches(e))
		})
	}
	assert.True(t, event.Filter{}.IsZero())
	assert.False(t, event.Filter{Bucket: "photos"}.IsZero())
}

func TestTargetIDAndARN(t *testing.T) {
	id := event.TargetConfig{ID: "primary", Type: event.TypeWebhook}.TargetID()
	assert.Equal(t, event.TargetID{ID: "primary", Type: event.TypeWebhook}, id)
	assert.Equal(t, "primary:webhook", id.String())
	assert.Equal(t, "arn:nexo:sqs:eu-west-1:primary:webhook", id.ARN("eu-west-1"))
	assert.Equal(t, "arn:nexo:sqs::primary:webhook", id.ARN(""))
}

// TestEventPayloadGolden pins the JSON document delivered for an event
// (docs/06 TS-I-15: the S3 standard payload): eventVersion 2.1 and the
// eventName without the "s3:" prefix in the records (the top-level
// EventName keeps it, as MinIO does), the
// request parameters and response elements of the request, the
// configuration id of the rule and the URL-encoded key.
func TestEventPayloadGolden(t *testing.T) {
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
	}))
	defer srv.Close()

	ctx := context.Background()
	rules := bucketRules(t, map[string]string{"photos": `<NotificationConfiguration>
  <QueueConfiguration><Id>new-photos</Id><Queue>arn:nexo:sqs:us-east-1:golden:webhook</Queue><Event>s3:ObjectCreated:*</Event></QueueConfiguration>
</NotificationConfiguration>`})
	d := event.NewDispatcher(event.Deps{Rules: rules, Factory: event.NewWebhookFactory(event.WebhookDeps{Client: srv.Client()})}, event.WithRegion("us-east-1"))
	require.NoError(t, d.SetTarget(ctx, webhookTarget("golden", srv.URL)))
	require.NoError(t, d.Start(ctx))
	defer stop(t, d)

	e := event.NewEvent(event.ObjectCreatedPut, "us-east-1", time.Date(2026, 10, 9, 12, 30, 45, 123e6, time.UTC))
	e.S3.Bucket = event.Bucket{Name: "photos", ARN: "arn:aws:s3:::photos"}
	e.S3.Object = event.Object{
		Key: "2026/summer holiday+1.jpg", Size: 1234, ETag: "d41d8cd98f00b204e9800998ecf8427e", ContentType: "image/jpeg",
		UserMetadata: map[string]string{"camera": "x100"}, VersionID: "0193a1b2-c3d4", Sequencer: "18a2f3c4d5e6f701",
	}
	reqCtx := event.WithRequest(ctx, event.Request{
		PrincipalID: "alice", SourceIP: "192.0.2.10", Port: "51234", RequestID: "17C2A3B4D5E6F708", HostID: "aG9zdC1pZA==",
		UserAgent: "aws-sdk-go-v2/1.36.3",
	})
	require.NoError(t, d.Publish(reqCtx, e))

	var body []byte
	select {
	case body = <-bodies:
	case <-time.After(5 * time.Second):
		t.Fatal("webhook not called")
	}
	var pretty any
	require.NoError(t, json.Unmarshal(body, &pretty))
	got, err := json.MarshalIndent(pretty, "", "  ")
	require.NoError(t, err)
	got = append(got, '\n')
	golden := filepath.Join("testdata", "webhook_payload.golden.json")
	if *updateGolden {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(golden, got, 0o644))
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(got))
	assert.Equal(t, string(want), string(got))
}

func TestPublishCompletesTheEventFromTheRequest(t *testing.T) {
	tests := []struct {
		name string
		req  *event.Request
		in   event.Name
		want event.Name
		who  string
	}{
		{name: "no request", in: event.ObjectCreatedPut, want: event.ObjectCreatedPut},
		{name: "put", req: &event.Request{PrincipalID: "bob"}, in: event.ObjectCreatedPut, want: event.ObjectCreatedPut, who: "bob"},
		{name: "post upload", req: &event.Request{Operation: func() string { return "PostObject" }}, in: event.ObjectCreatedPut, want: event.ObjectCreatedPost},
		{name: "presigned put", req: &event.Request{Operation: func() string { return "PresignedPutObject" }}, in: event.ObjectCreatedPut, want: event.ObjectCreatedPut},
		{name: "lifecycle delete", req: &event.Request{Lifecycle: true}, in: event.ObjectRemovedDelete, want: event.LifecycleExpirationDelete, who: event.LifecyclePrincipal},
		{name: "lifecycle marker", req: &event.Request{Lifecycle: true}, in: event.ObjectRemovedDeleteMarkerCreated, want: event.LifecycleExpirationDeleteMarkerCreated, who: event.LifecyclePrincipal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			got := make(chan event.Event, 1)
			target := newCaptureTarget(t, "cap", got)
			d := event.NewDispatcher(event.Deps{Factory: factoryOf(t, target)})
			cfg := event.TargetConfig{ID: "cap", Type: event.TypeWebhook, Enabled: true, Filter: event.Filter{Bucket: "b"}}
			require.NoError(t, d.SetTarget(ctx, cfg))
			require.NoError(t, d.Start(ctx))
			defer stop(t, d)
			pubCtx := ctx
			if tc.req != nil {
				pubCtx = event.WithRequest(ctx, *tc.req)
			}
			e := event.NewEvent(tc.in, "r", time.Now())
			e.S3.Bucket.Name = "b"
			e.S3.Object.Key = "k"
			require.NoError(t, d.Publish(pubCtx, e))
			rec := receive(t, got)
			assert.Equal(t, tc.want, rec.EventName)
			assert.Equal(t, tc.who, rec.UserIdentity.PrincipalID)
			assert.Equal(t, event.ConfigID, rec.S3.ConfigurationID)
			assert.Equal(t, "arn:aws:s3:::b", rec.S3.Bucket.ARN)
			assert.Equal(t, "nexo", rec.S3.Bucket.OwnerIdentity.PrincipalID)
		})
	}
}

// TestEventRecordJSON: the records carry the eventName as S3 does
// ("ObjectCreated:Put"); reading a record restores the configuration event
// type, whatever the form it was queued in.
func TestEventRecordJSON(t *testing.T) {
	e := event.NewEvent(event.ObjectRemovedDeleteMarkerCreated, "us-east-1", time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
	data, err := json.Marshal(e)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"eventName":"ObjectRemoved:DeleteMarkerCreated"`)
	assert.Contains(t, string(data), `"eventVersion":"2.1"`)
	tests := []struct {
		name, json string
		want       event.Name
	}{
		{"S3 form", `{"eventName":"ObjectCreated:Put"}`, event.ObjectCreatedPut},
		{"queued by an earlier build", `{"eventName":"s3:ObjectCreated:Put"}`, event.ObjectCreatedPut},
		{"no name", `{}`, ""},
	}
	for _, tt := range tests {
		var got event.Event
		require.NoError(t, json.Unmarshal([]byte(tt.json), &got), tt.name)
		assert.Equal(t, tt.want, got.EventName, tt.name)
	}
	var back event.Event
	require.NoError(t, json.Unmarshal(data, &back))
	assert.Equal(t, e.EventName, back.EventName)
	assert.True(t, back.EventName.Match(event.ObjectRemovedAll))
}
