package server

import (
	"crypto/tls"
)

// acmeTLSALPN is the ALPN protocol of the TLS-ALPN-01 challenge of ACME
// (RFC 8737).
const acmeTLSALPN = "acme-tls/1"

// CertificateSource selects the certificate of a TLS handshake by SNI
// (certs.Service implements it): it returns nil, nil when it has none for
// the name, so that the static certificate of the server is used. It is
// the tls.Config callback: the context is hello.Context().
type CertificateSource interface {
	GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error)
}

// WithCertificates serves, by SNI, the certificates of src (the domains of
// the distributions) besides the static certificate of WithTLS or
// WithTLSConfig, which remains the fallback; without a static one the
// server serves TLS with src alone, and a handshake for a name src does
// not cover fails. The listener also answers the TLS-ALPN-01 challenges
// of ACME ("acme-tls/1").
func WithCertificates(src CertificateSource) Option {
	return func(s *service) { s.certificates = src }
}

// withCertificates adds the certificates of the source to cfg (nil: a
// TLS configuration of the source alone).
func (s *service) withCertificates(cfg *tls.Config) *tls.Config {
	if s.certificates == nil {
		return cfg
	}
	if cfg == nil {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}
	}
	src := s.certificates
	static := cfg.GetCertificate
	cfg.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		c, err := src.GetCertificate(hello)
		if c != nil || err != nil || static == nil {
			return c, err
		}
		return static(hello)
	}
	// The protocol of the ACME validation comes last: an HTTP client never
	// offers it, a validator offers nothing else.
	cfg.NextProtos = appendMissing(cfg.NextProtos, acmeTLSALPN)
	return cfg
}
