package auth

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyCache(t *testing.T) {
	scope := func(date string) credentialScope {
		return credentialScope{accessKey: "AK", date: date, region: "us-east-1", service: ServiceS3}
	}
	var disabled *keyCache
	assert.Nil(t, newKeyCache(0))
	assert.Equal(t, deriveSigningKey("s", scope("20261008")), disabled.get("s", scope("20261008")))
	assert.Zero(t, disabled.len())

	c := newKeyCache(2)
	k1 := c.get("s", scope("20261008"))
	assert.Equal(t, deriveSigningKey("s", scope("20261008")), k1)
	assert.Same(t, &k1[0], &c.get("s", scope("20261008"))[0], "cache hit")
	assert.NotEqual(t, k1, c.get("rotated", scope("20261008")), "the secret is part of the key")
	c.get("s", scope("20261009"))
	assert.Equal(t, 2, c.len(), "bounded")
	c.get("s", scope("20261009"))
	assert.Equal(t, 2, c.len())
}

func TestTrimHeaderValue(t *testing.T) {
	for in, want := range map[string]string{
		"value":            "value",
		"  value  ":        "value",
		`"a   b   c"`:      `"a b c"`,
		" a  b ":           "a b",
		"\ta\t\tb\t":       "a\t\tb",
		"a \t  b":          "a \t b",
		"":                 "",
		"multi    space x": "multi space x",
	} {
		assert.Equal(t, want, trimHeaderValue(in), "%q", in)
	}
}

func TestURIEncode(t *testing.T) {
	assert.Equal(t, "/", encodePath(""))
	assert.Equal(t, "/a%20b/c%2Bd/%E2%82%AC/-_.~", encodePath("/a b/c+d/€/-_.~"))
	assert.Equal(t, "a%2Fb%3D%26", uriEncode("a/b=&", false))
	assert.Equal(t, "plain", uriEncode("plain", false))
}

func TestCanonicalURIs(t *testing.T) {
	u, err := url.Parse("http://h/bucket/a%2Fb")
	require.NoError(t, err)
	assert.Equal(t, []string{"/bucket/a/b", "/bucket/a%2Fb"}, canonicalURIs(u))
	u, err = url.Parse("http://h/bucket/a+b")
	require.NoError(t, err)
	assert.Equal(t, []string{"/bucket/a%2Bb", "/bucket/a+b"}, canonicalURIs(u))
	u, err = url.Parse("http://h/bucket/a%20b")
	require.NoError(t, err)
	assert.Equal(t, []string{"/bucket/a%20b"}, canonicalURIs(u))
	assert.Equal(t, []string{"/"}, canonicalURIs(&url.URL{Opaque: "//h/x"}))
}

func TestParseQuery(t *testing.T) {
	q, err := parseQuery("a=1&&b=x;y&c&a=2&d=%2B+e")
	require.NoError(t, err)
	assert.Equal(t, url.Values{"a": {"1", "2"}, "b": {"x;y"}, "c": {""}, "d": {"+ e"}}, q)
	assert.Equal(t, "a=1&a=2&b=x%3By&c=&d=%2B%20e", canonicalQuery(q, ""))
	assert.Equal(t, "a=1&a=2&c=&d=%2B%20e", canonicalQuery(q, "b"))
	assert.Equal(t, "k=", canonicalQuery(url.Values{"k": nil}, ""))
	_, err = parseQuery("%zz=1")
	assert.Error(t, err)
	_, err = parseQuery("a=%zz")
	assert.Error(t, err)
}

func TestHeaderValues(t *testing.T) {
	r := &http.Request{URL: &url.URL{Host: "fallback:9000"}, Header: http.Header{}, ContentLength: -1}
	vs, ok := headerValues(r, "host")
	assert.True(t, ok)
	assert.Equal(t, []string{"fallback:9000"}, vs)
	_, ok = headerValues(&http.Request{Header: http.Header{}}, "host")
	assert.False(t, ok)

	_, ok = headerValues(r, "content-length")
	assert.False(t, ok, "unknown length")
	r.ContentLength = 42
	vs, _ = headerValues(r, "content-length")
	assert.Equal(t, []string{"42"}, vs)
	r.Header.Set("Content-Length", "43")
	vs, _ = headerValues(r, "content-length")
	assert.Equal(t, []string{"43"}, vs)

	_, ok = headerValues(r, "transfer-encoding")
	assert.False(t, ok)
	r.TransferEncoding = []string{"chunked"}
	vs, _ = headerValues(r, "transfer-encoding")
	assert.Equal(t, []string{"chunked"}, vs)

	vs, _ = headerValues(r, "expect")
	assert.Equal(t, []string{"100-continue"}, vs, "stripped by a proxy")
	r.Header.Set("Expect", "100-Continue")
	vs, _ = headerValues(r, "expect")
	assert.Equal(t, []string{"100-Continue"}, vs)
}

func TestSmallHelpers(t *testing.T) {
	assert.False(t, isHex("abc"))
	assert.False(t, isHex("zz"))
	assert.True(t, isHex("aBc9"))
	assert.False(t, isToken(""))
	assert.False(t, isToken("a b"))
	assert.True(t, isToken("x-amz-meta-a_b.c~"))
	_, err := parseAmzDate("20130524T000000")
	assert.Error(t, err)
	_, err = parseAmzDate("2013052XT000000Z")
	assert.Error(t, err)

	h := http.Header{}
	removeAWSChunked(h)
	assert.Empty(t, h)
	h.Set("Content-Encoding", "aws-chunked")
	removeAWSChunked(h)
	assert.Empty(t, h.Values("Content-Encoding"))
	h.Add("Content-Encoding", "gzip, AWS-CHUNKED")
	h.Add("Content-Encoding", "br")
	removeAWSChunked(h)
	assert.Equal(t, "gzip, br", h.Get("Content-Encoding"))
	h.Set("Content-Encoding", "deflate,  gzip")
	removeAWSChunked(h)
	assert.Equal(t, "deflate,  gzip", h.Get("Content-Encoding"), "kept verbatim without aws-chunked")

	trailers, err := declaredTrailers([]string{" , x-amz-checksum-crc32 ", "X-Amz-Checksum-SHA256"})
	require.NoError(t, err)
	assert.Len(t, trailers, 2)
	_, _, ok := newChecksum("x-amz-checksum-md5")
	assert.False(t, ok)
	_, _, ok = newChecksum("content-md5")
	assert.False(t, ok)
}

// FuzzParseAuthorization checks the invariants of a successfully parsed
// Authorization header and that no input panics.
func FuzzParseAuthorization(f *testing.F) {
	zeros := strings.Repeat("0", 64)
	for _, seed := range []string{
		"AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=host;range;x-amz-date,Signature=" + zeros,
		"AWS4-HMAC-SHA256 Credential=a/b/20130524/r/sts/aws4_request, SignedHeaders=Host;X-Amz-Date;host, Signature=" + zeros,
		"AWS4-HMAC-SHA256 Credential=, SignedHeaders=, Signature=",
		"AWS AKID:sig",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, h string) {
		a, err := parseAuthorization(h)
		if err != nil {
			return
		}
		require.True(t, isSignature(a.signature))
		require.NotEmpty(t, a.scope.accessKey)
		require.Contains(t, []string{ServiceS3, ServiceSTS}, a.scope.service)
		require.Contains(t, a.signedHeaders, "host")
		for i := 1; i < len(a.signedHeaders); i++ {
			require.Less(t, a.signedHeaders[i-1], a.signedHeaders[i], "sorted and unique")
		}
		for _, name := range a.signedHeaders {
			require.Equal(t, strings.ToLower(name), name)
		}
	})
}

// FuzzParseQuery checks that a parsed query renders a canonical query
// whose decoding yields the same values.
func FuzzParseQuery(f *testing.F) {
	for _, seed := range []string{"a=1&b=2", "x;y=%20+", "%zz", "&&=&", "ሴ=bar", "k"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		q, err := parseQuery(raw)
		if err != nil {
			return
		}
		canonical := canonicalQuery(q, "")
		back, err := parseQuery(canonical)
		require.NoError(t, err)
		for k, vs := range q {
			require.ElementsMatch(t, vs, back[k], k)
		}
	})
}
