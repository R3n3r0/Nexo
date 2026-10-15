package domain

import (
	"net/url"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://kes.example.com:7373", "https://kes.example.com:7373"},
		{"https://user:s3cret@kes:7373", "https://REDACTED@kes:7373"},
		{"http://token@kes/path?key=s3cret#frag", "http://REDACTED@kes/path?REDACTED"},
		{"https://user:s3cret@kes host", "[invalid URL]"},
	}
	for _, tt := range tests {
		got := RedactURL(tt.in)
		assert.Equal(t, tt.want, got, tt.in)
		assert.NotContains(t, got, "s3cret")
	}
}

// TestRedactQueryShared: one predicate for every redaction of the server.
func TestRedactQueryShared(t *testing.T) {
	for _, name := range []string{"X-Amz-Signature", "x-amz-credential", "X-Amz-Security-Token", "Signature", "AWSAccessKeyId",
		"token", "access_token", "password", "passwd", "client_secret", "Policy", "Key-Pair-Id",
		"X-Amz-Server-Side-Encryption-Customer-Key",
		// The credentials of the applications served by the
		// distributions (regression: in clear in the trace and the logs).
		"api_key", "apikey", "API-Key", "x-api-key", "auth", "sig", "jwt", "session", "SID"} {
		assert.True(t, IsSensitiveParam(name), name)
	}
	for _, name := range []string{"prefix", "versionId", "X-Amz-Date", "X-Amz-Expires", "list-type", "uploads", "key", "key-marker",
		"marker", "uploadId", "author", "signed"} {
		assert.False(t, IsSensitiveParam(name), name)
	}
	raw := "list-type=2&token=MYTOKEN&password=hunter2&client_secret=CS&policy&X%2DAmz%2DSignature=s&%zz=x"
	assert.Equal(t, "list-type=2&token=REDACTED&password=REDACTED&client_secret=REDACTED&policy&X%2DAmz%2DSignature=REDACTED&REDACTED",
		RedactRawQuery(raw))
	q := RedactQuery(url.Values{"password": {"hunter2", ""}, "prefix": {"p"}})
	assert.Equal(t, url.Values{"password": {RedactedValue, ""}, "prefix": {"p"}}, q)
	re := regexp.MustCompile(SensitiveParamPattern() + `[^&\s]+`)
	assert.Equal(t, "url?a=1&X-Amz-Security-Token=REDACTED&my_password=REDACTED&policy=REDACTED",
		re.ReplaceAllString("url?a=1&X-Amz-Security-Token=TOK&my_password=pw&policy=p", "${1}"+RedactedValue))
}
