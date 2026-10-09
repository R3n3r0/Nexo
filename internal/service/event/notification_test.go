package event_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
)

func TestParseARN(t *testing.T) {
	tests := []struct {
		in      string
		want    event.ARN
		wantErr bool
	}{
		{in: "arn:nexo:sqs:us-east-1:1:webhook", want: event.ARN{Region: "us-east-1", ID: "1", Type: event.TypeWebhook}},
		{in: "arn:nexo:sqs::_:kafka", want: event.ARN{ID: "_", Type: event.TypeKafka}},
		{in: "arn:nexo:sns:eu:x:nats", want: event.ARN{Region: "eu", ID: "x", Type: event.TypeNATS}},
		{in: "arn:nexo:lambda:eu:x:postgresql", want: event.ARN{Region: "eu", ID: "x", Type: event.TypePostgres}},
		{in: "arn:aws:sqs:us-east-1:1:webhook", wantErr: true},
		{in: "arn:nexo:s3:us-east-1:1:webhook", wantErr: true},
		{in: "arn:nexo:sqs:us-east-1::webhook", wantErr: true},
		{in: "arn:nexo:sqs:us-east-1:1:mysql", wantErr: true},
		{in: "arn:nexo:sqs:us-east-1:1", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := event.ParseARN(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, domain.ErrInvalidArgument)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseBucketConfig(t *testing.T) {
	const arn = "arn:nexo:sqs:us-east-1:1:webhook"
	tests := []struct {
		name    string
		doc     string
		check   func(t *testing.T, cfg *event.BucketConfig)
		wantErr error
		code    string
	}{
		{
			name: "empty configuration",
			doc:  `<NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"/>`,
			check: func(t *testing.T, cfg *event.BucketConfig) {
				assert.Empty(t, cfg.Rules)
			},
		},
		{
			name: "queue topic and function with filters",
			doc: `<NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
				<QueueConfiguration><Id>q</Id><Queue>` + arn + `</Queue><Event>s3:ObjectCreated:*</Event><Event>s3:ObjectCreated:*</Event>
					<Filter><S3Key><FilterRule><Name>Prefix</Name><Value>images/</Value></FilterRule><FilterRule><Name>suffix</Name><Value>.jpg</Value></FilterRule></S3Key></Filter>
				</QueueConfiguration>
				<TopicConfiguration><Topic>arn:nexo:sns:us-east-1:k:kafka</Topic><Event>s3:ObjectRemoved:Delete</Event></TopicConfiguration>
				<CloudFunctionConfiguration><Id>f</Id><CloudFunction>arn:nexo:lambda::n:nats</CloudFunction><Event>s3:LifecycleExpiration:*</Event><Event>s3:ObjectTagging:Put</Event></CloudFunctionConfiguration>
			</NotificationConfiguration>`,
			check: func(t *testing.T, cfg *event.BucketConfig) {
				require.Len(t, cfg.Rules, 3)
				q := cfg.Rules[0]
				assert.Equal(t, "q", q.ID)
				assert.Equal(t, event.KindQueue, q.Kind)
				assert.Equal(t, event.TargetID{ID: "1", Type: event.TypeWebhook}, q.ARN.TargetID())
				assert.Equal(t, []event.Name{event.ObjectCreatedAll}, q.Events, "duplicates dropped")
				assert.Equal(t, "images/", q.Prefix)
				assert.Equal(t, ".jpg", q.Suffix)
				topic := cfg.Rules[1]
				assert.Equal(t, event.KindTopic, topic.Kind)
				assert.True(t, strings.HasPrefix(topic.ID, "nexo-"), "generated id %q", topic.ID)
				assert.Equal(t, event.KindFunction, cfg.Rules[2].Kind)
				assert.Equal(t, []event.Name{event.LifecycleExpirationAll, event.ObjectTaggingPut}, cfg.Rules[2].Events)
			},
		},
		{name: "malformed xml", doc: `<NotificationConfiguration><QueueConfiguration>`, wantErr: domain.ErrMalformedXML, code: "MalformedXML"},
		{name: "empty document", doc: ``, wantErr: domain.ErrMalformedXML, code: "MalformedXML"},
		{name: "unknown event", doc: `<NotificationConfiguration><QueueConfiguration><Queue>` + arn + `</Queue><Event>s3:Replication:*</Event></QueueConfiguration></NotificationConfiguration>`, wantErr: domain.ErrInvalidArgument, code: "InvalidArgument"},
		{name: "no event", doc: `<NotificationConfiguration><QueueConfiguration><Queue>` + arn + `</Queue></QueueConfiguration></NotificationConfiguration>`, wantErr: domain.ErrInvalidArgument},
		{name: "bad filter name", doc: `<NotificationConfiguration><QueueConfiguration><Queue>` + arn + `</Queue><Event>s3:ObjectCreated:Put</Event><Filter><S3Key><FilterRule><Name>contains</Name><Value>x</Value></FilterRule></S3Key></Filter></QueueConfiguration></NotificationConfiguration>`, wantErr: domain.ErrInvalidArgument},
		{name: "two prefixes", doc: `<NotificationConfiguration><QueueConfiguration><Queue>` + arn + `</Queue><Event>s3:ObjectCreated:Put</Event><Filter><S3Key><FilterRule><Name>prefix</Name><Value>a</Value></FilterRule><FilterRule><Name>prefix</Name><Value>b</Value></FilterRule></S3Key></Filter></QueueConfiguration></NotificationConfiguration>`, wantErr: domain.ErrInvalidArgument},
		{name: "foreign arn", doc: `<NotificationConfiguration><QueueConfiguration><Queue>arn:aws:sqs:us-east-1:123:queue</Queue><Event>s3:ObjectCreated:Put</Event></QueueConfiguration></NotificationConfiguration>`, wantErr: domain.ErrInvalidArgument},
		{name: "other region", doc: `<NotificationConfiguration><QueueConfiguration><Queue>arn:nexo:sqs:eu-west-1:1:webhook</Queue><Event>s3:ObjectCreated:Put</Event></QueueConfiguration></NotificationConfiguration>`, wantErr: domain.ErrInvalidArgument},
		{name: "duplicate ids", doc: `<NotificationConfiguration><QueueConfiguration><Id>a</Id><Queue>` + arn + `</Queue><Event>s3:ObjectCreated:Put</Event></QueueConfiguration><QueueConfiguration><Id>a</Id><Queue>` + arn + `</Queue><Event>s3:ObjectRemoved:*</Event></QueueConfiguration></NotificationConfiguration>`, wantErr: domain.ErrInvalidArgument},
		{name: "event bridge", doc: `<NotificationConfiguration><EventBridgeConfiguration/></NotificationConfiguration>`, wantErr: domain.ErrNotImplemented},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := event.ParseBucketConfig([]byte(tc.doc), "us-east-1")
			if tc.wantErr != nil {
				require.Error(t, err)
				assert.ErrorIs(t, err, tc.wantErr)
				if tc.code != "" {
					var s3err *domain.S3Error
					require.True(t, errors.As(err, &s3err))
					assert.Equal(t, tc.code, s3err.Code)
				}
				return
			}
			require.NoError(t, err)
			tc.check(t, cfg)
		})
	}
}

func TestEncodeBucketConfigRoundTrip(t *testing.T) {
	doc := `<NotificationConfiguration>
		<QueueConfiguration><Queue>arn:nexo:sqs:us-east-1:1:webhook</Queue><Event>s3:ObjectCreated:*</Event>
			<Filter><S3Key><FilterRule><Name>prefix</Name><Value>a/</Value></FilterRule></S3Key></Filter></QueueConfiguration>
		<TopicConfiguration><Id>t</Id><Topic>arn:nexo:sqs::2:kafka</Topic><Event>s3:ObjectRemoved:*</Event>
			<Filter><S3Key><FilterRule><Name>suffix</Name><Value>.log</Value></FilterRule></S3Key></Filter></TopicConfiguration>
	</NotificationConfiguration>`
	cfg, err := event.ParseBucketConfig([]byte(doc), "us-east-1")
	require.NoError(t, err)
	out, err := event.EncodeBucketConfig(cfg)
	require.NoError(t, err)
	assert.Contains(t, string(out), `xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`)
	again, err := event.ParseBucketConfig(out, "us-east-1")
	require.NoError(t, err)
	assert.Equal(t, cfg, again, "the canonical document parses to the same configuration (generated ids kept)")
	empty, err := event.EncodeBucketConfig(nil)
	require.NoError(t, err)
	cfg, err = event.ParseBucketConfig(empty, "")
	require.NoError(t, err)
	assert.Empty(t, cfg.Rules)
}

func TestRuleMatches(t *testing.T) {
	r := event.Rule{Events: []event.Name{event.ObjectCreatedAll, event.ObjectRemovedDelete}, Prefix: "img/", Suffix: ".png"}
	tests := []struct {
		name event.Name
		key  string
		want bool
	}{
		{event.ObjectCreatedPut, "img/a.png", true},
		{event.ObjectRemovedDelete, "img/a.png", true},
		{event.ObjectRemovedDeleteMarkerCreated, "img/a.png", false},
		{event.ObjectCreatedPut, "doc/a.png", false},
		{event.ObjectCreatedPut, "img/a.jpg", false},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, r.Matches(objectEvent(tc.name, "b", tc.key)), "%s %s", tc.name, tc.key)
	}
}

func TestValidateBucketConfigResolvesTheDestinations(t *testing.T) {
	ctx := context.Background()
	d := event.NewDispatcher(event.Deps{}, event.WithRegion("us-east-1"))
	require.NoError(t, d.SetTarget(ctx, webhookTarget("1", "http://127.0.0.1:1/hook")))

	out, err := d.ValidateBucketConfig(ctx, []byte(`<NotificationConfiguration><QueueConfiguration><Queue>arn:nexo:sqs:us-east-1:1:webhook</Queue><Event>s3:ObjectCreated:Put</Event></QueueConfiguration></NotificationConfiguration>`))
	require.NoError(t, err)
	assert.Contains(t, string(out), "<Queue>arn:nexo:sqs:us-east-1:1:webhook</Queue>")
	assert.Contains(t, string(out), "<Id>nexo-", "the canonical document names the rule")

	_, err = d.ValidateBucketConfig(ctx, []byte(`<NotificationConfiguration><QueueConfiguration><Queue>arn:nexo:sqs:us-east-1:2:webhook</Queue><Event>s3:ObjectCreated:Put</Event></QueueConfiguration></NotificationConfiguration>`))
	require.Error(t, err, "no target 2")
	var s3err *domain.S3Error
	require.True(t, errors.As(err, &s3err))
	assert.Equal(t, "InvalidArgument", s3err.Code)
	_, err = d.ValidateBucketConfig(ctx, []byte(`<NotificationConfiguration><QueueConfiguration><Queue>arn:nexo:sqs:us-east-1:1:kafka</Queue><Event>s3:ObjectCreated:Put</Event></QueueConfiguration></NotificationConfiguration>`))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "same id, other type")
}

// TestValidateBucketConfigRefusesOverlaps: as S3, two configurations
// sharing an event type (a family covers its members) with overlapping key
// filters are refused; disjoint filters or event types are accepted.
func TestValidateBucketConfigRefusesOverlaps(t *testing.T) {
	ctx := context.Background()
	d := event.NewDispatcher(event.Deps{}, event.WithRegion("us-east-1"))
	require.NoError(t, d.SetTarget(ctx, webhookTarget("1", "http://127.0.0.1:1/hook")))
	queue := func(id, ev, prefix, suffix string) string {
		filter := ""
		if prefix != "" || suffix != "" {
			filter = "<Filter><S3Key>"
			if prefix != "" {
				filter += "<FilterRule><Name>prefix</Name><Value>" + prefix + "</Value></FilterRule>"
			}
			if suffix != "" {
				filter += "<FilterRule><Name>suffix</Name><Value>" + suffix + "</Value></FilterRule>"
			}
			filter += "</S3Key></Filter>"
		}
		return "<QueueConfiguration><Id>" + id + "</Id><Queue>arn:nexo:sqs:us-east-1:1:webhook</Queue><Event>" + ev + "</Event>" + filter + "</QueueConfiguration>"
	}
	tests := []struct {
		name    string
		a, b    string
		overlap bool
	}{
		{"family and member, nested prefixes", queue("q1", "s3:ObjectCreated:*", "a/", ""), queue("q2", "s3:ObjectCreated:Put", "a/b", ""), true},
		{"same event, no filters", queue("q1", "s3:ObjectRemoved:*", "", ""), queue("q2", "s3:ObjectRemoved:*", "", ""), true},
		{"prefix against suffix", queue("q1", "s3:ObjectCreated:Put", "images/", ""), queue("q2", "s3:ObjectCreated:Put", "", ".jpg"), true},
		{"nested suffixes", queue("q1", "s3:ObjectCreated:Put", "", ".jpg"), queue("q2", "s3:ObjectCreated:Put", "", "thumb.jpg"), true},
		{"disjoint prefixes", queue("q1", "s3:ObjectCreated:*", "a/", ""), queue("q2", "s3:ObjectCreated:*", "b/", ""), false},
		{"disjoint suffixes", queue("q1", "s3:ObjectCreated:*", "", ".jpg"), queue("q2", "s3:ObjectCreated:*", "", ".png"), false},
		{"other event types", queue("q1", "s3:ObjectCreated:*", "", ""), queue("q2", "s3:ObjectRemoved:*", "", ""), false},
		{"sibling events", queue("q1", "s3:ObjectCreated:Put", "", ""), queue("q2", "s3:ObjectCreated:Copy", "", ""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := d.ValidateBucketConfig(ctx, []byte("<NotificationConfiguration>"+tt.a+tt.b+"</NotificationConfiguration>"))
			if !tt.overlap {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, domain.ErrInvalidArgument)
			assert.Contains(t, err.Error(), "Configurations overlap")
		})
	}
}
