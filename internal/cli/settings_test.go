package cli_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/cli/clitest"
	"github.com/r3n3r0/nexo/internal/domain"
)

// covers: S-BKT-07 (ilm rule import/export)
func TestParseLifecycle(t *testing.T) {
	enabled, disabled := true, false
	tests := []struct {
		name    string
		doc     string
		want    cli.LifecycleConfiguration
		wantErr string
	}{
		{
			name: "admin JSON",
			doc:  `{"rules":[{"id":"a","filter":{"prefix":"logs/"},"expiration":{"days":3}}]}`,
			want: cli.LifecycleConfiguration{Rules: []cli.LifecycleRule{{ID: "a", Filter: &cli.LifecycleFilter{Prefix: "logs/"}, Expiration: &cli.LifecycleExpiration{Days: intPtr(3)}}}},
		},
		{
			name: "S3 XML with a legacy prefix",
			doc:  `<LifecycleConfiguration><Rule><ID>old</ID><Prefix>tmp/</Prefix><Status>Disabled</Status><Expiration><Date>2030-01-01T00:00:00Z</Date></Expiration></Rule></LifecycleConfiguration>`,
			want: cli.LifecycleConfiguration{Rules: []cli.LifecycleRule{{ID: "old", Enabled: &disabled, Filter: &cli.LifecycleFilter{Prefix: "tmp/"},
				Expiration: &cli.LifecycleExpiration{Date: "2030-01-01"}}}},
		},
		{
			name: "S3 XML with an And filter",
			doc: ` <?xml version="1.0"?><LifecycleConfiguration><Rule><ID>and</ID><Filter><And><Prefix>p/</Prefix><Tag><Key>a</Key><Value>1</Value></Tag>
				<ObjectSizeLessThan>10</ObjectSizeLessThan></And></Filter><Status>Enabled</Status>
				<NoncurrentVersionExpiration><NoncurrentDays>2</NoncurrentDays><NewerNoncurrentVersions>1</NewerNoncurrentVersions></NoncurrentVersionExpiration>
				<AbortIncompleteMultipartUpload><DaysAfterInitiation>4</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule></LifecycleConfiguration>`,
			want: cli.LifecycleConfiguration{Rules: []cli.LifecycleRule{{ID: "and", Enabled: &enabled,
				Filter:                &cli.LifecycleFilter{Prefix: "p/", Tags: map[string]string{"a": "1"}, SizeLessThan: int64Ptr(10)},
				NoncurrentExpiration:  &cli.LifecycleNoncurrentExpiration{NoncurrentDays: intPtr(2), NewerNoncurrentVersions: intPtr(1)},
				AbortIncompleteUpload: &cli.LifecycleAbortUpload{DaysAfterInitiation: intPtr(4)}}}},
		},
		{name: "no rules", doc: `{}`, want: cli.LifecycleConfiguration{Rules: []cli.LifecycleRule{}}},
		{name: "unknown member", doc: `{"rules":[{"id":"a","expire":{"days":1}}]}`, wantErr: "unknown field"},
		{name: "bad status", doc: `<LifecycleConfiguration><Rule><Status>On</Status></Rule></LifecycleConfiguration>`, wantErr: "Status"},
		{name: "bad date", doc: `{"rules":[{"expiration":{"date":"soon"}}]}`, wantErr: "invalid date"},
		{name: "date not at midnight", doc: `<LifecycleConfiguration><Rule><Status>Enabled</Status><Expiration><Date>2030-01-01T10:00:00Z</Date></Expiration></Rule></LifecycleConfiguration>`, wantErr: "midnight"},
		{name: "trailing data", doc: `{"rules":[]} {}`, wantErr: "after the document"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cli.ParseLifecycle([]byte(tt.doc))
			if tt.wantErr != "" {
				require.ErrorIs(t, err, cli.ErrUsage)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLifecycleXMLRoundTrip(t *testing.T) {
	enabled := true
	gt := int64(5)
	cfg := cli.LifecycleConfiguration{Rules: []cli.LifecycleRule{
		{ID: "all", Enabled: &enabled, Expiration: &cli.LifecycleExpiration{Days: intPtr(1)}},
		{ID: "tag", Enabled: &enabled, Filter: &cli.LifecycleFilter{Tags: map[string]string{"k": "v"}}, Expiration: &cli.LifecycleExpiration{ExpiredObjectDeleteMarker: true}},
		{ID: "size", Enabled: &enabled, Filter: &cli.LifecycleFilter{SizeGreaterThan: &gt}, Transitions: []cli.LifecycleTransition{{Date: "2031-02-03", StorageClass: "COLD"}},
			NoncurrentTransitions: []cli.LifecycleNoncurrentTransition{{NoncurrentDays: intPtr(9), StorageClass: "COLD"}}},
	}}
	doc, err := cli.EncodeLifecycleXML(cfg)
	require.NoError(t, err)
	assert.Contains(t, string(doc), `<Date>2031-02-03T00:00:00Z</Date>`)
	assert.Contains(t, string(doc), `<Tag>`)
	got, err := cli.ParseLifecycle(doc)
	require.NoError(t, err)
	assert.Equal(t, cfg, got)
}

func TestNextLifecycleRuleID(t *testing.T) {
	assert.Equal(t, "rule-1", cli.NextLifecycleRuleID(nil))
	assert.Equal(t, "rule-3", cli.NextLifecycleRuleID([]cli.LifecycleRule{{ID: "rule-1"}, {ID: "rule-2"}, {ID: "x"}}))
	assert.Equal(t, "rule-2", cli.NextLifecycleRuleID([]cli.LifecycleRule{{ID: "rule-1"}, {ID: "rule-3"}}))
}

// covers: S-BKT-06 (cors set)
func TestParseCORS(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		want    []cli.CORSRule
		wantErr string
	}{
		{name: "admin JSON", doc: `{"rules":[{"id":"w","allowedOrigins":["*"],"allowedMethods":["GET"],"maxAgeSeconds":5}]}`,
			want: []cli.CORSRule{{ID: "w", AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET"}, MaxAgeSeconds: intPtr(5)}}},
		{name: "AWS CLI JSON", doc: `{"CORSRules":[{"AllowedOrigins":["https://a.test"],"AllowedMethods":["PUT"],"AllowedHeaders":["*"]}]}`,
			want: []cli.CORSRule{{AllowedOrigins: []string{"https://a.test"}, AllowedMethods: []string{"PUT"}, AllowedHeaders: []string{"*"}}}},
		{name: "S3 XML", doc: `<CORSConfiguration><CORSRule><ID>x</ID><AllowedOrigin>*</AllowedOrigin><AllowedMethod>HEAD</AllowedMethod><ExposeHeader>ETag</ExposeHeader></CORSRule></CORSConfiguration>`,
			want: []cli.CORSRule{{ID: "x", AllowedOrigins: []string{"*"}, AllowedMethods: []string{"HEAD"}, ExposeHeaders: []string{"ETag"}}}},
		{name: "no rule", doc: `{"rules":[]}`, wantErr: "no rule"},
		{name: "unsupported method", doc: `{"rules":[{"allowedOrigins":["*"],"allowedMethods":["PATCH"]}]}`, wantErr: "PATCH"},
		{name: "no origin", doc: `{"rules":[{"allowedMethods":["GET"]}]}`, wantErr: "invalid CORS configuration"},
		{name: "malformed XML", doc: `<CORSConfiguration><CORSRule>`, wantErr: "invalid CORS XML"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cli.ParseCORS([]byte(tt.doc))
			if tt.wantErr != "" {
				require.ErrorIs(t, err, cli.ErrUsage)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Rules)
			doc, err := cli.EncodeCORSXML(got)
			require.NoError(t, err)
			again, err := cli.ParseCORS(doc)
			require.NoError(t, err)
			assert.Equal(t, got, again, "the XML form round-trips")
		})
	}
}

// covers: S-BKT-10 (event add)
func TestParseEventTypesAndARN(t *testing.T) {
	tests := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{in: "put", want: []string{"s3:ObjectCreated:*"}},
		{in: "delete, put,put", want: []string{"s3:ObjectCreated:*", "s3:ObjectRemoved:*"}},
		{in: "get,s3:ObjectCreated:Put", want: []string{"s3:ObjectAccessed:*", "s3:ObjectCreated:Put"}},
		{in: "ilm", want: []string{"s3:LifecycleExpiration:*", "s3:LifecycleTransition"}},
		{in: "teleport", wantErr: true},
		{in: " , ", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := cli.ParseEventTypes(tt.in)
			if tt.wantErr {
				assert.ErrorIs(t, err, cli.ErrUsage)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
	assert.True(t, cli.SameEvents([]string{"b", "a"}, []string{"a", "b"}))
	assert.False(t, cli.SameEvents([]string{"a"}, []string{"a", "b"}))

	for arn, valid := range map[string]bool{
		"arn:nexo:sqs:us-east-1:_:webhook":   true,
		"arn:nexo:sns::id:kafka":             true,
		"arn:aws:lambda:eu-west-1:1:fn:name": true,
		"arn:nexo:s3:us-east-1:_:webhook":    false,
		"arn::sqs:r:i:t":                     false,
		"not-an-arn":                         false,
	} {
		if valid {
			assert.NoError(t, cli.ValidateARN(arn), arn)
		} else {
			assert.ErrorIs(t, cli.ValidateARN(arn), cli.ErrUsage, arn)
		}
	}
	assert.Equal(t, "sqs", cli.ARNService("arn:nexo:sqs:us-east-1:_:webhook"))
}

// covers: S-OBJ-13 (--enc-c, --enc-s3, --enc-kms)
func TestEncryptionFlags(t *testing.T) {
	parser := cli.NewTargetParser(func(name string) bool { return name == "local" }, "/home/u")
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	rules, err := cli.EncryptionFlags{
		S3:  []string{"local/b"},
		KMS: []string{"local/b/kms/=app-key,local/other"},
		C:   []string{"local/b/kms/secret/=" + key},
	}.Rules(parser.Parse)
	require.NoError(t, err)
	tests := []struct {
		target string
		want   domain.SSEType
		keyID  string
	}{
		{target: "local/b/x.txt", want: domain.SSES3},
		{target: "local/b/kms/x.txt", want: domain.SSEKMS, keyID: "app-key"},
		{target: "local/b/kms/secret/x.txt", want: domain.SSEC},
		{target: "local/other/x", want: domain.SSEKMS},
		{target: "local/b2/x.txt", want: domain.SSENone},
		{target: "./b/x.txt", want: domain.SSENone},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			target, err := parser.Parse(tt.target)
			require.NoError(t, err)
			got := rules.For(target)
			assert.Equal(t, tt.want, got.Type)
			assert.Equal(t, tt.keyID, got.KMSKeyID)
			assert.Equal(t, tt.want == domain.SSEC, rules.CustomerKeyFor(target).Type == domain.SSEC)
		})
	}

	invalid := []cli.EncryptionFlags{
		{S3: []string{"local/b=key"}},
		{C: []string{"local/b"}},
		{C: []string{"local/b=short"}},
		{S3: []string{"local"}},
		{S3: []string{"elsewhere/b"}},
		{S3: []string{"local/b"}, C: []string{"local/b=" + key}},
	}
	for _, f := range invalid {
		_, err := f.Rules(parser.Parse)
		assert.ErrorIs(t, err, cli.ErrUsage, "%+v", f)
	}
	none, err := cli.EncryptionFlags{}.Rules(parser.Parse)
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestParseSSECustomerKey(t *testing.T) {
	raw := []byte(strings.Repeat("z", 32))
	for _, in := range []string{
		base64.StdEncoding.EncodeToString(raw),
		base64.RawStdEncoding.EncodeToString(raw),
		base64.URLEncoding.EncodeToString(raw),
		"7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a",
	} {
		k, err := cli.ParseSSECustomerKey(in)
		require.NoError(t, err, in)
		assert.Equal(t, raw, k.Key)
		assert.NotEmpty(t, k.KeyMD5)
	}
	for _, in := range []string{"", "c2hvcnQ=", strings.Repeat("ab", 31), "not base64!"} {
		_, err := cli.ParseSSECustomerKey(in)
		assert.ErrorIs(t, err, cli.ErrUsage, in)
	}
}

// covers: S-OBJ-12 (retention set --validity)
func TestParseValidity(t *testing.T) {
	now := time.Date(2026, 1, 31, 10, 0, 0, 500, time.UTC)
	tests := []struct {
		in    string
		want  cli.Validity
		until time.Time
		text  string
	}{
		{in: "30d", want: cli.Validity{Days: 30}, until: time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC), text: "30d"},
		{in: "1Y", want: cli.Validity{Years: 1}, until: time.Date(2027, 1, 31, 10, 0, 0, 0, time.UTC), text: "1y"},
		{in: " 7d ", want: cli.Validity{Days: 7}, until: time.Date(2026, 2, 7, 10, 0, 0, 0, time.UTC), text: "7d"},
	}
	for _, tt := range tests {
		got, err := cli.ParseValidity(tt.in)
		require.NoError(t, err, tt.in)
		assert.Equal(t, tt.want, got)
		assert.Equal(t, tt.until, got.Until(now))
		assert.Equal(t, tt.text, got.String())
	}
	for _, in := range []string{"", "d", "0d", "-1d", "3w", "1.5y", "12h"} {
		_, err := cli.ParseValidity(in)
		assert.ErrorIs(t, err, cli.ErrUsage, in)
	}
}

// covers: S-OBJ-12 (retention and legalhold targets, rb --force)
func TestLockTargetsAndLockedVersions(t *testing.T) {
	ctx := context.Background()
	clk := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mem := clitest.NewMemS3(nil)
	mem.MakeLockedBucket("vault", domain.DefaultRetention{})
	v1 := mem.PutObject("vault", "docs/a.txt", "a1", clitest.ObjectSpec{})
	v2 := mem.PutObject("vault", "docs/a.txt", "a2", clitest.ObjectSpec{LegalHold: domain.LegalHoldOn})
	mem.PutObject("vault", "docs/b.txt", "b", clitest.ObjectSpec{Retention: domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: clk.Add(time.Hour)}})
	mem.PutObject("vault", "docsx.txt", "x", clitest.ObjectSpec{Retention: domain.ObjectRetention{Mode: domain.RetentionCompliance, RetainUntil: clk.Add(-time.Hour)}})
	client := mem.Client(t)
	res, err := client.Remove(ctx, "vault", []cli.ObjectVersion{{Key: "docsx.txt"}}, cli.DeleteOptions{})
	require.NoError(t, err)
	require.NoError(t, res[0].Err, "a delete marker on docsx.txt")

	collect := func(key string, opts cli.LockTargetOptions) ([]cli.ObjectVersion, error) {
		var out []cli.ObjectVersion
		for v, err := range cli.LockTargets(ctx, client, "vault", key, opts) {
			if err != nil {
				return out, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	tests := []struct {
		name    string
		key     string
		opts    cli.LockTargetOptions
		want    []cli.ObjectVersion
		wantErr error
	}{
		{name: "one object", key: "docs/a.txt", want: []cli.ObjectVersion{{Key: "docs/a.txt"}}},
		{name: "one version", key: "docs/a.txt", opts: cli.LockTargetOptions{VersionID: v1}, want: []cli.ObjectVersion{{Key: "docs/a.txt", VersionID: v1}}},
		{name: "every version of an object", key: "docs/a.txt", opts: cli.LockTargetOptions{Versions: true},
			want: []cli.ObjectVersion{{Key: "docs/a.txt", VersionID: v2}, {Key: "docs/a.txt", VersionID: v1}}},
		{name: "a folder", key: "docs", opts: cli.LockTargetOptions{Recursive: true}, want: []cli.ObjectVersion{{Key: "docs/a.txt"}, {Key: "docs/b.txt"}}},
		{name: "a prefix", key: "docs/", opts: cli.LockTargetOptions{Recursive: true}, want: []cli.ObjectVersion{{Key: "docs/a.txt"}, {Key: "docs/b.txt"}}},
		{name: "the bucket, delete markers skipped", key: "", opts: cli.LockTargetOptions{Recursive: true, Versions: true}, want: []cli.ObjectVersion{
			{Key: "docs/a.txt", VersionID: v2}, {Key: "docs/a.txt", VersionID: v1}, {Key: "docs/b.txt", VersionID: "v0003"}, {Key: "docsx.txt", VersionID: "v0004"}}},
		{name: "missing object with versions", key: "nope", opts: cli.LockTargetOptions{Versions: true}, wantErr: domain.ErrObjectNotFound},
		{name: "a prefix without --recursive", key: "docs/", wantErr: cli.ErrUsage},
		{name: "version and recursive", key: "docs/", opts: cli.LockTargetOptions{Recursive: true, VersionID: v1}, wantErr: cli.ErrUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := collect(tt.key, tt.opts)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	locked, err := cli.LockedVersions(ctx, client, "vault", clk, 0)
	require.NoError(t, err)
	require.Len(t, locked, 2, "the legal hold and the active retention, not the expired one")
	assert.Equal(t, "legal hold", locked[0].Reason(clk))
	assert.Equal(t, v2, locked[0].VersionID)
	assert.Contains(t, locked[1].Reason(clk), "GOVERNANCE retention until")
	first, err := cli.LockedVersions(ctx, client, "vault", clk, 1)
	require.NoError(t, err)
	assert.Len(t, first, 1, "the limit")

	mem.MakeBucket("plain")
	none, err := cli.LockedVersions(ctx, client, "plain", clk, 0)
	require.NoError(t, err)
	assert.Empty(t, none)
}

func int64Ptr(n int64) *int64 { return &n }
