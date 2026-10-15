package httpx

import (
	"net/url"
	"strings"
)

// RedactedValue replaces the value of a sensitive query parameter in logs
// and audit entries.
const RedactedValue = "REDACTED"

// IsSensitiveQueryParam reports the query parameters that carry a
// credential or a capability: the SigV4 presigned signature, credential
// and session token, and the Signature Version 2 signature and access key.
// Whoever reads a log holding them could rebuild and replay presigned URLs
// and share links until they expire. Names are compared case-insensitively.
func IsSensitiveQueryParam(name string) bool {
	switch strings.ToLower(name) {
	case "x-amz-signature", "x-amz-credential", "x-amz-security-token", "signature", "awsaccesskeyid":
		return true
	default:
		return false
	}
}

// RedactQuery returns a copy of q where the values of the sensitive
// parameters (IsSensitiveQueryParam) are RedactedValue.
func RedactQuery(q url.Values) url.Values {
	if q == nil {
		return nil
	}
	out := make(url.Values, len(q))
	for name, values := range q {
		if !IsSensitiveQueryParam(name) {
			out[name] = append([]string(nil), values...)
			continue
		}
		redacted := make([]string, len(values))
		for i := range redacted {
			redacted[i] = RedactedValue
		}
		out[name] = redacted
	}
	return out
}

// RedactRawQuery returns the raw query string with the values of the
// sensitive parameters replaced by RedactedValue; everything else keeps
// its original encoding and order. A name that does not decode is kept.
func RedactRawQuery(raw string) string {
	return RedactRawQueryFunc(raw, IsSensitiveQueryParam)
}

// IsSensitiveSignedURLParam reports, beyond IsSensitiveQueryParam, the
// parameters of the signed URLs of the distributions (Signature, Policy,
// Key-Pair-Id): their access logs never keep a replayable URL. Policy is
// not redacted by the generic logs, where "?policy" is an S3 subresource.
func IsSensitiveSignedURLParam(name string) bool {
	switch strings.ToLower(name) {
	case "policy", "key-pair-id":
		return true
	}
	return IsSensitiveQueryParam(name)
}

// RedactRawQueryFunc is RedactRawQuery with the parameters sensitive
// selects.
func RedactRawQueryFunc(raw string, sensitive func(name string) bool) string {
	if raw == "" {
		return ""
	}
	pairs := strings.Split(raw, "&")
	changed := false
	for i, pair := range pairs {
		k, _, _ := strings.Cut(pair, "=")
		name, err := url.QueryUnescape(k)
		if err != nil || !sensitive(name) {
			continue
		}
		pairs[i] = k + "=" + RedactedValue
		changed = true
	}
	if !changed {
		return raw
	}
	return strings.Join(pairs, "&")
}
