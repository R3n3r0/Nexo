package httpx

import (
	"net/url"

	"github.com/r3n3r0/nexo/internal/domain"
)

// RedactedValue replaces the value of a sensitive query parameter in logs
// and audit entries.
const RedactedValue = domain.RedactedValue

// IsSensitiveQueryParam reports the query parameters that carry a
// credential or a capability (domain.IsSensitiveParam: signatures,
// credentials, tokens, secrets, passwords, the signed URLs of the
// distributions): whoever reads a log holding them could rebuild and
// replay presigned URLs and share links until they expire. The logs, the
// log stream, the audit, the access logs and the live trace share it.
func IsSensitiveQueryParam(name string) bool { return domain.IsSensitiveParam(name) }

// RedactQuery returns a copy of q where the values of the sensitive
// parameters (IsSensitiveQueryParam) are RedactedValue.
func RedactQuery(q url.Values) url.Values { return domain.RedactQuery(q) }

// RedactRawQuery returns the raw query string with the values of the
// sensitive parameters replaced by RedactedValue; everything else keeps
// its original encoding and order (domain.RedactRawQuery).
func RedactRawQuery(raw string) string { return domain.RedactRawQuery(raw) }
