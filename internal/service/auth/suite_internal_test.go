package auth

import (
	"bufio"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The aws-sig-v4-test-suite vectors in testdata/aws-sig-v4-test-suite were
// published by AWS for the generic SigV4 algorithm (service "service",
// credentials AKIDEXAMPLE / wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY,
// 20150830T123600Z, us-east-1). Each directory holds the raw request
// (.req), the canonical request (.creq), the string to sign (.sts) and the
// Authorization header (.authz). Only the vectors whose canonicalization
// is the S3 one are kept (no path normalization). The verifier only
// accepts the s3 and sts services, so the vectors exercise the canonical
// request, string to sign and key derivation directly.
const (
	suiteSecret  = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	suiteAmzDate = "20150830T123600Z"
)

// suiteRequest parses a .req file: request line, headers, empty line, body.
func suiteRequest(t *testing.T, raw string) (*http.Request, string) {
	t.Helper()
	br := bufio.NewReader(strings.NewReader(raw))
	line, err := br.ReadString('\n')
	require.NoError(t, err)
	line = strings.TrimSuffix(line, "\n")
	method, rest, ok := strings.Cut(line, " ")
	require.True(t, ok)
	target := rest[:strings.LastIndex(rest, " ")]
	path, query, _ := strings.Cut(target, "?")
	req := &http.Request{Method: method, URL: &url.URL{Path: path, RawQuery: query}, Header: http.Header{}}
	for {
		h, err := br.ReadString('\n')
		h = strings.TrimSuffix(h, "\n")
		if h == "" {
			break
		}
		name, value, ok := strings.Cut(h, ":")
		require.True(t, ok, h)
		if strings.EqualFold(name, "host") {
			req.Host = value
			continue
		}
		req.Header.Add(name, value)
		if err != nil {
			break
		}
	}
	body, err := io.ReadAll(br)
	require.NoError(t, err)
	return req, string(body)
}

func TestAWSSigV4TestSuite(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("testdata", "aws-sig-v4-test-suite", "*"))
	require.NoError(t, err)
	require.NotEmpty(t, dirs)
	scope := credentialScope{accessKey: "AKIDEXAMPLE", date: "20150830", region: "us-east-1", service: "service"}
	key := deriveSigningKey(suiteSecret, scope)
	read := func(t *testing.T, dir, ext string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, filepath.Base(dir)+ext))
		require.NoError(t, err)
		return string(b)
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			req, body := suiteRequest(t, read(t, dir, ".req"))
			authz := read(t, dir, ".authz")
			_, signedPart, _ := strings.Cut(authz, "SignedHeaders=")
			signedList, signature, _ := strings.Cut(signedPart, ", Signature=")
			signed, err := parseSignedHeaders(signedList)
			require.NoError(t, err)

			q, err := parseQuery(req.URL.RawQuery)
			require.NoError(t, err)
			headers, missing := canonicalHeaders(req, signed)
			require.Empty(t, missing)
			creq := canonicalRequest(req.Method, canonicalURIs(req.URL)[0], canonicalQuery(q, ""), headers, signed, sha256Hex([]byte(body)))
			assert.Equal(t, read(t, dir, ".creq"), creq)
			sts := stringToSign(suiteAmzDate, scope, creq)
			assert.Equal(t, read(t, dir, ".sts"), sts)
			got := signatureHex(key, sts)
			assert.Equal(t, signature, got)
			assert.Equal(t, authz, algorithm+" Credential="+scope.String()+", SignedHeaders="+strings.Join(signed, ";")+", Signature="+got)
		})
	}
}
