package domain

import "net/url"

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
