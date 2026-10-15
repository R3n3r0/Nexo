package trace

import (
	"net/url"
	"strings"
)

// redactedValue replaces the value of a sensitive query parameter.
const redactedValue = "REDACTED"

// sensitiveParam reports the query parameters that carry a credential or
// a capability: the SigV4 and SigV2 presigned signatures, credentials and
// tokens, the signed URLs of the distributions, and anything named like a
// secret. Names are compared case-insensitively.
func sensitiveParam(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "x-amz-signature", "x-amz-credential", "x-amz-security-token", "signature", "awsaccesskeyid",
		"key-pair-id", "policy", "x-amz-server-side-encryption-customer-key":
		return true
	}
	for _, s := range []string{"signature", "credential", "token", "secret", "password", "passwd"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// redactQuery returns the raw query with the values of the sensitive
// parameters replaced; the other pairs keep their encoding and order. A
// parameter name that does not decode is redacted with its value.
func redactQuery(raw string) string {
	if raw == "" {
		return ""
	}
	pairs := strings.Split(raw, "&")
	for i, pair := range pairs {
		k, v, hasValue := strings.Cut(pair, "=")
		name, err := url.QueryUnescape(k)
		switch {
		case err != nil:
			pairs[i] = redactedValue
		case sensitiveParam(name) && hasValue && v != "":
			pairs[i] = k + "=" + redactedValue
		}
	}
	return strings.Join(pairs, "&")
}
