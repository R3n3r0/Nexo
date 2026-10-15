package event_test

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
)

// FuzzParseBucketConfig checks the parser of the NotificationConfiguration
// documents (PUT ?notification, the console): no panic, failures are S3
// errors, an accepted configuration routes any event without panicking
// and its canonical document parses back to the same configuration.
func FuzzParseBucketConfig(f *testing.F) {
	for _, seed := range []string{
		`<NotificationConfiguration><QueueConfiguration><Id>q</Id><Queue>arn:minio:sqs::hook:webhook</Queue><Event>s3:ObjectCreated:*</Event><Filter><S3Key><FilterRule><Name>prefix</Name><Value>img/</Value></FilterRule><FilterRule><Name>suffix</Name><Value>.jpg</Value></FilterRule></S3Key></Filter></QueueConfiguration></NotificationConfiguration>`,
		`<NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><TopicConfiguration><Topic>arn:aws:sns:us-east-1:1:topic</Topic><Event>s3:ObjectRemoved:Delete</Event></TopicConfiguration><CloudFunctionConfiguration><CloudFunction>arn:minio:sqs:us-east-1:fn:webhook</CloudFunction><Event>s3:ObjectCreated:Put</Event><Event>s3:ObjectCreated:Put</Event></CloudFunctionConfiguration></NotificationConfiguration>`,
		`<NotificationConfiguration><EventBridgeConfiguration/></NotificationConfiguration>`,
		`<NotificationConfiguration><QueueConfiguration><Queue>bad</Queue><Event>s3:Nope</Event></QueueConfiguration></NotificationConfiguration>`,
		`<NotificationConfiguration/>`, ``, `<x>`,
	} {
		f.Add([]byte(seed), "img/a.jpg")
	}
	names := []event.Name{event.ObjectCreatedPut, event.ObjectRemovedDelete, event.ObjectCreatedAll}
	f.Fuzz(func(t *testing.T, doc []byte, key string) {
		cfg, err := event.ParseBucketConfig(doc, "us-east-1")
		if err != nil {
			var s3err *domain.S3Error
			if !errors.As(err, &s3err) {
				t.Fatalf("ParseBucketConfig: unexpected error %T %v", err, err)
			}
			return
		}
		for _, r := range cfg.Rules {
			require.NotEmpty(t, r.ID, "every rule has an id")
			require.NotEmpty(t, r.Events)
			for _, n := range names {
				e := event.Event{EventName: n}
				e.S3.Object.Key = key
				_ = r.Matches(e)
			}
		}
		out, err := event.EncodeBucketConfig(cfg)
		require.NoError(t, err)
		if !xmlSafe(cfg) {
			// The XML encoder replaces the characters a document cannot
			// carry: no exact round trip.
			return
		}
		back, err := event.ParseBucketConfig(out, "us-east-1")
		require.NoError(t, err, "the canonical document parses back: %s", out)
		require.Equal(t, cfg, back)
	})
}

// xmlSafe reports whether every string of cfg survives an XML round trip.
func xmlSafe(cfg *event.BucketConfig) bool {
	for _, r := range cfg.Rules {
		for _, s := range []string{r.ID, r.Prefix, r.Suffix} {
			if !utf8.ValidString(s) || strings.IndexFunc(s, func(c rune) bool {
				return c != '\t' && c != '\n' && c != '\r' && (c < 0x20 || c == 0xFFFE || c == 0xFFFF || (c >= 0xD800 && c <= 0xDFFF))
			}) >= 0 {
				return false
			}
		}
	}
	return true
}
