package admin_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
)

func TestClientConditions(t *testing.T) {
	got := admin.ClientConditions(admin.Client{IP: "192.0.2.7", Secure: true, UserAgent: "ua", Referer: "https://console"})
	assert.Equal(t, map[string][]string{
		"aws:SourceIp":        {"192.0.2.7"},
		"aws:SecureTransport": {"true"},
		"aws:UserAgent":       {"ua"},
		"aws:Referer":         {"https://console"},
		"s3:authType":         {"REST-HEADER"},
		"s3:signatureversion": {"AWS4-HMAC-SHA256"},
	}, got)
	got = admin.ClientConditions(admin.Client{})
	assert.Equal(t, []string{"false"}, got["aws:SecureTransport"])
	assert.NotContains(t, got, "aws:SourceIp", "an unknown value is absent, not empty")
}

// covers: docs/12 §12.7 (the console evaluates the condition values of
// the S3 request it stands for)
func TestObjectConditions(t *testing.T) {
	tests := []struct {
		name string
		in   admin.ObjectRequest
		want map[string][]string
	}{
		{name: "latest version", in: admin.ObjectRequest{}, want: map[string][]string{}},
		{name: "version", in: admin.ObjectRequest{VersionID: "v1"}, want: map[string][]string{"s3:versionid": {"v1"}}},
		{name: "forwarded headers", in: admin.ObjectRequest{Headers: map[string]string{
			"X-Amz-Tagging": "team=ops&env=prod", "x-amz-storage-class": "STANDARD",
			"x-amz-meta-owner": "me", "X-Amz-Security-Token": "t", "Content-Type": "text/plain",
		}}, want: map[string][]string{
			"s3:x-amz-tagging":         {"team=ops&env=prod"},
			"s3:x-amz-storage-class":   {"STANDARD"},
			"s3:RequestObjectTag/team": {"ops"},
			"s3:RequestObjectTag/env":  {"prod"},
			"s3:RequestObjectTagKeys":  {"env", "team"},
		}},
		{name: "malformed tagging", in: admin.ObjectRequest{Headers: map[string]string{"x-amz-tagging": "%zz"}},
			want: map[string][]string{"s3:x-amz-tagging": {"%zz"}}},
		{name: "presigned use", in: admin.ObjectRequest{VersionID: "v2", AuthType: domain.AuthTypeQuery},
			want: map[string][]string{"s3:versionid": {"v2"}, "s3:authType": {"REST-QUERY-STRING"}, "s3:signatureversion": {"AWS4-HMAC-SHA256"}}},
		{name: "POST policy", in: admin.ObjectRequest{AuthType: domain.AuthTypePost},
			want: map[string][]string{"s3:authType": {"POST"}, "s3:signatureversion": {"AWS4-HMAC-SHA256"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, admin.ObjectConditions(tt.in))
		})
	}
}

// covers: docs/12 §12.7 (the listings of the console are evaluated with
// the keys of the S3 listing)
func TestListingConditions(t *testing.T) {
	assert.Equal(t, map[string][]string{"s3:prefix": {""}}, admin.ListingConditions("", "", 0))
	assert.Equal(t, map[string][]string{"s3:prefix": {"home/gina/"}, "s3:delimiter": {"/"}, "s3:max-keys": {"50"}},
		admin.ListingConditions("home/gina/", "/", 50))
}

func TestParseTagging(t *testing.T) {
	tags, err := admin.ParseTagging("")
	require.NoError(t, err)
	assert.Nil(t, tags)
	tags, err = admin.ParseTagging("a=1&b=two%20words")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "1", "b": "two words"}, tags)
	for _, bad := range []string{"%zz", "a=1&a=2"} {
		_, err = admin.ParseTagging(bad)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, bad)
	}
}

func TestCopySourceOf(t *testing.T) {
	assert.Equal(t, "bkt/dir/a%20b.txt", admin.CopySourceOf("bkt", "dir/a b.txt", ""))
	assert.Equal(t, "bkt/k?versionId=v%2B1", admin.CopySourceOf("bkt", "k", "v+1"))
}
