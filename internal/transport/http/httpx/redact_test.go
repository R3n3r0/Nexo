package httpx_test

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

func TestIsSensitiveQueryParam(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{name: "X-Amz-Signature", want: true},
		{name: "x-amz-signature", want: true},
		{name: "X-Amz-Credential", want: true},
		{name: "X-Amz-Security-Token", want: true},
		{name: "Signature", want: true},
		{name: "AWSAccessKeyId", want: true},
		{name: "X-Amz-Date"},
		{name: "X-Amz-Expires"},
		{name: "X-Amz-SignedHeaders"},
		{name: "prefix"},
		{name: "versionId"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, httpx.IsSensitiveQueryParam(tt.name), tt.name)
	}
}

func TestRedactRawQuery(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: "", want: ""},
		{name: "nothing sensitive", raw: "list-type=2&prefix=a%2Fb", want: "list-type=2&prefix=a%2Fb"},
		{name: "presigned",
			raw:  "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIA%2F20261008%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20261008T120000Z&X-Amz-Expires=300&X-Amz-SignedHeaders=host&X-Amz-Security-Token=tok&X-Amz-Signature=f8de00",
			want: "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=REDACTED&X-Amz-Date=20261008T120000Z&X-Amz-Expires=300&X-Amz-SignedHeaders=host&X-Amz-Security-Token=REDACTED&X-Amz-Signature=REDACTED"},
		{name: "signature v2", raw: "AWSAccessKeyId=AKIA&Expires=1&Signature=abc%3D", want: "AWSAccessKeyId=REDACTED&Expires=1&Signature=REDACTED"},
		{name: "encoded name", raw: "X%2DAmz%2DSignature=abc&uploads", want: "X%2DAmz%2DSignature=REDACTED&uploads"},
		{name: "flag without value", raw: "Signature&x=1", want: "Signature=REDACTED&x=1"},
		{name: "undecodable name kept", raw: "%zz=1&X-Amz-Signature=a", want: "%zz=1&X-Amz-Signature=REDACTED"},
		{name: "repeated", raw: "X-Amz-Signature=a&X-Amz-Signature=b", want: "X-Amz-Signature=REDACTED&X-Amz-Signature=REDACTED"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, httpx.RedactRawQuery(tt.raw), tt.name)
	}
}

func TestRedactQuery(t *testing.T) {
	assert.Nil(t, httpx.RedactQuery(nil))
	q := url.Values{"X-Amz-Signature": {"a", "b"}, "prefix": {"p"}, "X-Amz-Credential": {"AKIA/x"}}
	got := httpx.RedactQuery(q)
	assert.Equal(t, url.Values{"X-Amz-Signature": {"REDACTED", "REDACTED"}, "prefix": {"p"}, "X-Amz-Credential": {"REDACTED"}}, got)
	assert.Equal(t, []string{"a", "b"}, q["X-Amz-Signature"], "the input is not modified")
	got["prefix"][0] = "changed"
	assert.Equal(t, "p", q.Get("prefix"), "the copy does not share the value slices")
}
