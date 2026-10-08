package policy_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// covers: docs/09 §9.3, docs/12 §12.7 (one definition of the request
// condition keys for the S3 API and the console)
func TestRequestConditions(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		in   policy.Request
		want map[string][]string
	}{
		{name: "nothing", in: policy.Request{}, want: map[string][]string{}},
		{
			name: "client",
			in:   policy.Request{Client: &policy.RequestClient{IP: "192.0.2.7", Secure: true, UserAgent: "ua", Referer: "https://console"}},
			want: map[string][]string{
				"aws:SourceIp": {"192.0.2.7"}, "aws:SecureTransport": {"true"},
				"aws:UserAgent": {"ua"}, "aws:Referer": {"https://console"},
			},
		},
		{
			name: "unknown client values are absent",
			in:   policy.Request{Client: &policy.RequestClient{}},
			want: map[string][]string{"aws:SecureTransport": {"false"}},
		},
		{
			name: "listing and version parameters",
			in:   policy.Request{Params: map[string]string{"prefix": "", "delimiter": "/", "max-keys": "10", "versionId": "v1", "marker": "x"}},
			want: map[string][]string{"s3:prefix": {""}, "s3:delimiter": {"/"}, "s3:max-keys": {"10"}, "s3:versionid": {"v1"}},
		},
		{
			name: "headers and tags",
			in: policy.Request{Headers: http.Header{
				"X-Amz-Tagging": {"team=ops&env=prod"}, "X-Amz-Storage-Class": {"STANDARD"},
				"X-Amz-Meta-Owner": {"me"}, "X-Amz-Security-Token": {"t"}, "Content-Type": {"text/plain"},
			}},
			want: map[string][]string{
				"s3:x-amz-tagging": {"team=ops&env=prod"}, "s3:x-amz-storage-class": {"STANDARD"},
				"s3:RequestObjectTag/team": {"ops"}, "s3:RequestObjectTag/env": {"prod"},
				"s3:RequestObjectTagKeys": {"env", "team"},
			},
		},
		{
			name: "lower-case header names",
			in:   policy.Request{Headers: map[string][]string{"x-amz-tagging": {"a=1"}}},
			want: map[string][]string{"s3:x-amz-tagging": {"a=1"}, "s3:RequestObjectTag/a": {"1"}, "s3:RequestObjectTagKeys": {"a"}},
		},
		{
			name: "malformed tagging",
			in:   policy.Request{Headers: map[string][]string{"x-amz-tagging": {"%zz"}}},
			want: map[string][]string{"s3:x-amz-tagging": {"%zz"}},
		},
		{
			name: "signed request with time",
			in:   policy.Request{Now: now, AuthType: domain.AuthTypeQuery, SignedAt: now.Add(-10 * time.Minute)},
			want: map[string][]string{
				"aws:CurrentTime": {"2026-10-08T12:00:00Z"}, "aws:EpochTime": {"1791460800"},
				"s3:authType": {"REST-QUERY-STRING"}, "s3:signatureversion": {"AWS4-HMAC-SHA256"}, "s3:signatureAge": {"600000"},
			},
		},
		{
			name: "signature age needs the time",
			in:   policy.Request{AuthType: domain.AuthTypePost, SignedAt: now},
			want: map[string][]string{"s3:authType": {"POST"}, "s3:signatureversion": {"AWS4-HMAC-SHA256"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, policy.RequestConditions(tt.in))
		})
	}
}

func TestRequestConditionsCopiesHeaderValues(t *testing.T) {
	h := http.Header{"X-Amz-Acl": {"private"}}
	got := policy.RequestConditions(policy.Request{Headers: h})
	got["s3:x-amz-acl"][0] = "changed"
	assert.Equal(t, "private", h.Get("X-Amz-Acl"), "the request headers are not aliased")
}
