// Package dns01 publishes the TXT records of the ACME DNS-01 challenge
// (RFC 8555 §8.4) in the DNS zones of the distributions and waits for
// them to be served by the authoritative nameservers of the zone, before
// the certificate service asks the CA to validate them. DNS-01 is the only
// challenge that proves the control of a whole domain: the certificates of
// the wildcard aliases ("*.example.com") need it.
//
// Three providers are built in:
//
//   - Cloudflare, through its v4 API with an API token (permission
//     Zone:DNS:Edit on the zone);
//   - AWS Route 53, with an access key or the ambient credentials of the
//     node (environment, shared files, instance role, web identity);
//   - RFC 2136 dynamic updates signed with a TSIG key, for BIND, Knot,
//     PowerDNS and any server accepting them.
//
// The credentials are fields of the configurations: the caller (the
// registry of the certificate service) stores them sealed and never logs
// nor returns them; the providers never write them in an error.
//
// The Checker queries the authoritative nameservers of the zone of the
// record (found through the recursive resolvers of the node), or the
// nameservers configured instead (split-horizon DNS, tests), until every
// one of them serves the value or the timeout of the provider expires.
package dns01

import (
	"context"
	"time"
)

// Provider publishes and removes the TXT records of the DNS-01 challenge.
//
// The names are fully qualified with their trailing dot
// ("_acme-challenge.example.com."). A record may hold several values at
// once (the order of "example.com" and "*.example.com" validates two
// values on the same name): Present adds its value to the record, CleanUp
// removes only its value.
type Provider interface {
	// Present publishes value in the TXT record fqdn.
	Present(ctx context.Context, fqdn, value string) error
	// CleanUp removes value from the TXT record fqdn; removing a value
	// that is not there is not an error.
	CleanUp(ctx context.Context, fqdn, value string) error
	// Propagation returns how long the Checker waits for a record of the
	// provider to reach every authoritative nameserver, and how often it
	// queries them.
	Propagation() (timeout, interval time.Duration)
}

// Checker waits for a TXT record to be served by the nameservers of its
// zone.
type Checker interface {
	// Wait returns when every nameserver serves value in the TXT record
	// fqdn, or an error when timeout expires first (or ctx is done).
	Wait(ctx context.Context, fqdn, value string, timeout, interval time.Duration) error
}
