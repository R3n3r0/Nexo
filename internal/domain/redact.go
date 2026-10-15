package domain

import (
	"net/url"
	"strings"
)

// redactedURLPart replaces the secrets of a URL in its redacted form.
const redactedURLPart = "REDACTED"

// RedactURL returns raw without the parts of a URL that may carry
// credentials, for the errors and the logs: the user info and the query
// are replaced, the fragment is dropped. A value that does not parse as a
// URL is replaced altogether (it could hold anything).
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[invalid URL]"
	}
	if u.User != nil {
		u.User = url.User(redactedURLPart)
	}
	if u.RawQuery != "" || u.ForceQuery {
		u.RawQuery, u.ForceQuery = redactedURLPart, false
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

// RedactedValue replaces the value of a sensitive parameter in the logs,
// the audit, the access logs and the live trace.
const RedactedValue = "REDACTED"

// sensitiveParamWords are the words that make a parameter name sensitive
// wherever they appear in it (X-Amz-Security-Token, access_token,
// client_secret...).
const sensitiveParamWords = "signature|credential|token|secret|password|passwd"

// sensitiveParamNames are the sensitive parameter names without one of
// those words: the access key of the SigV2 presigned URLs, the policy and
// key pair of the signed URLs of the distributions, the SSE-C key, and the
// usual names of the credentials of the applications a distribution
// serves (API keys, signatures, JWTs, session ids). Other parameters of an
// application stay in clear; "key" (the object key of S3 and of the Admin
// API) and "key-marker" are never redacted.
const sensitiveParamNames = "awsaccesskeyid|key-pair-id|policy|x-amz-server-side-encryption-customer-key" +
	"|apikey|api_key|api-key|x-api-key|auth|sig|jwt|session|sid"

// IsSensitiveParam reports a query parameter (or form field) whose value
// is a credential or a capability: a name containing signature,
// credential, token, secret, password or passwd, or one of the names of
// sensitiveParamNames; case-insensitive. It is the one predicate of every
// redaction of the server (logs, log stream, audit, access logs, trace).
func IsSensitiveParam(name string) bool {
	n := strings.ToLower(name)
	for _, w := range strings.Split(sensitiveParamWords, "|") {
		if strings.Contains(n, w) {
			return true
		}
	}
	for _, s := range strings.Split(sensitiveParamNames, "|") {
		if n == s {
			return true
		}
	}
	return false
}

// SensitiveParamPattern is the regular expression (RE2) matching
// "<sensitive name>=" in a text, with the name in group 1, for the
// redaction of free text (the values of the log entries): the names
// IsSensitiveParam reports.
func SensitiveParamPattern() string {
	return `(?i)((?:[\w.-]*(?:` + sensitiveParamWords + `)[\w.-]*|` + sensitiveParamNames + `)=)`
}

// RedactRawQuery returns the raw query string with the non-empty values of
// the sensitive parameters (IsSensitiveParam) replaced by RedactedValue;
// the other pairs keep their encoding and order, and a parameter without
// value stays as is (the "?policy" sub-resource of S3). A pair whose name
// does not decode is replaced altogether: it could hide anything.
func RedactRawQuery(raw string) string {
	if raw == "" {
		return ""
	}
	pairs := strings.Split(raw, "&")
	changed := false
	for i, pair := range pairs {
		k, v, _ := strings.Cut(pair, "=")
		name, err := url.QueryUnescape(k)
		switch {
		case err != nil:
			pairs[i], changed = RedactedValue, true
		case v != "" && IsSensitiveParam(name):
			pairs[i], changed = k+"="+RedactedValue, true
		}
	}
	if !changed {
		return raw
	}
	return strings.Join(pairs, "&")
}

// RedactQuery returns a copy of q where the non-empty values of the
// sensitive parameters (IsSensitiveParam) are RedactedValue.
func RedactQuery(q url.Values) url.Values {
	if q == nil {
		return nil
	}
	out := make(url.Values, len(q))
	for name, values := range q {
		cp := append([]string(nil), values...)
		if IsSensitiveParam(name) {
			for i, v := range cp {
				if v != "" {
					cp[i] = RedactedValue
				}
			}
		}
		out[name] = cp
	}
	return out
}
