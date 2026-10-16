package httpx

import (
	"net"
	"strings"
)

// NormalizeHost lowercases a host and strips its spaces, its port and the
// surrounding dots (the trailing dot of a fully qualified name).
func NormalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.Trim(strings.ToLower(host), ".")
}

// VirtualHostBucket returns the bucket a virtual-host request addresses
// ("<bucket>.<domain>" for one of domains, NEXO_DOMAIN), both the host and
// the domains compared normalized (NormalizeHost): the S3 API and the
// middlewares that record the target of a request resolve it alike.
func VirtualHostBucket(domains []string, host string) (string, bool) {
	if len(domains) == 0 {
		return "", false
	}
	host = NormalizeHost(host)
	for _, d := range domains {
		if d = NormalizeHost(d); d == "" {
			continue
		}
		if bucket, ok := strings.CutSuffix(host, "."+d); ok && bucket != "" {
			return bucket, true
		}
	}
	return "", false
}
