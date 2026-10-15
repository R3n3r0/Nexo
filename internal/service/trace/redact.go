package trace

import "github.com/r3n3r0/nexo/internal/domain"

// redactQuery returns the raw query with the values of the sensitive
// parameters replaced (domain.RedactRawQuery: the predicate the logs, the
// audit and the access logs share); the other pairs keep their encoding
// and order.
func redactQuery(raw string) string { return domain.RedactRawQuery(raw) }
