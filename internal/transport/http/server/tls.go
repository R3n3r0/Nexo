package server

import (
	"crypto/tls"
	"crypto/x509"
	"sync"
)

// ClientCASource returns the CAs verifying the client certificates
// (certs.Keystore implements it). ClientCAs is a pure accessor read at
// every handshake (no I/O, never blocks): a reload of the CAs applies to
// the next handshake.
type ClientCASource interface {
	ClientCAs() *x509.CertPool
}

// WithKeystore serves TLS with the certificates of ks, the certificates
// of the server itself reloaded at runtime (certs.Keystore: the default
// certificate and the ones of the sub-directories selected by SNI). It
// takes the place of WithTLS; the distribution certificates of
// WithCertificates still come first for the names they cover.
func WithKeystore(ks CertificateSource) Option {
	return func(s *service) { s.keystore = ks }
}

// WithClientCASource is WithClientCAs with CAs reloaded at runtime: every
// handshake verifies the client certificates with the current ones.
func WithClientCASource(src ClientCASource) Option {
	return func(s *service) { s.clientCASource = src }
}

// WithMinTLSVersion sets the oldest TLS version accepted
// (tls.VersionTLS12, the default and the floor, or tls.VersionTLS13); the
// newest common version is always negotiated, TLS 1.3 with the current
// clients.
func WithMinTLSVersion(v uint16) Option {
	return func(s *service) {
		if v >= tls.VersionTLS12 {
			s.minTLSVersion = v
		}
	}
}

// secureCipherSuites are the TLS 1.2 cipher suites of the listeners:
// forward secrecy (ECDHE) and AEAD only, the AES-GCM ones first (TLS 1.3
// suites are not configurable and all secure).
func secureCipherSuites() []uint16 {
	return []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	}
}

// harden applies the TLS policy of the listeners to cfg: at least
// TLS 1.2 (or the configured minimum), the secure TLS 1.2 cipher suites
// unless cfg chose its own, HTTP/2 then HTTP/1.1.
func (s *service) harden(cfg *tls.Config) {
	minVersion := s.minTLSVersion
	if minVersion < tls.VersionTLS12 {
		minVersion = tls.VersionTLS12
	}
	if cfg.MinVersion < minVersion {
		cfg.MinVersion = minVersion
	}
	if cfg.CipherSuites == nil {
		cfg.CipherSuites = secureCipherSuites()
	}
	cfg.NextProtos = appendMissing(cfg.NextProtos, "h2", "http/1.1")
}

// withClientCAs asks the TLS clients for a certificate and verifies the
// ones presented (tls.VerifyClientCertIfGiven) with the static CAs of
// WithClientCAs or the current ones of WithClientCASource.
func (s *service) withClientCAs(cfg *tls.Config) {
	switch {
	case s.clientCASource != nil:
		cfg.ClientAuth = tls.VerifyClientCertIfGiven
		cfg.ClientCAs = s.clientCASource.ClientCAs()
		cfg.GetConfigForClient = (&clientCAConfigs{base: cfg.Clone(), src: s.clientCASource}).forHello
	case s.clientCAs != nil:
		cfg.ClientAuth, cfg.ClientCAs = tls.VerifyClientCertIfGiven, s.clientCAs
	}
}

// clientCAConfigs serves the configuration of a handshake with the
// current client CAs: the base configuration while the CAs are the ones it
// was built with, a copy carrying the new ones after a reload (built once
// per reload). The session tickets keep the keys of the listener's
// configuration.
type clientCAConfigs struct {
	base *tls.Config
	src  ClientCASource

	mu   sync.Mutex
	pool *x509.CertPool
	cfg  *tls.Config
}

// forHello is the tls.Config.GetConfigForClient callback.
func (c *clientCAConfigs) forHello(*tls.ClientHelloInfo) (*tls.Config, error) {
	pool := c.src.ClientCAs()
	if pool == c.base.ClientCAs {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg == nil || c.pool != pool {
		cfg := c.base.Clone()
		cfg.ClientCAs = pool
		cfg.GetConfigForClient = nil
		c.pool, c.cfg = pool, cfg
	}
	return c.cfg, nil
}
