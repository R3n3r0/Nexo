package idp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/r3n3r0/nexo/internal/domain"
)

// ldapTimeout bounds the connection and every request to the directory.
const ldapTimeout = 10 * time.Second

// maxLDAPGroups bounds the groups read for a user.
const maxLDAPGroups = 1000

// LDAP implements Service.
func (s *service) LDAP(ctx context.Context, username, password string) (Identity, error) {
	cfg, err := s.ldapConf(ctx)
	if err != nil {
		return Identity{}, err
	}
	if cfg == nil {
		return Identity{}, fmt.Errorf("%w: idp: no LDAP directory is configured", domain.ErrNotImplemented)
	}
	username = strings.TrimSpace(username)
	// An empty password would be an unauthenticated bind, which many
	// directories accept for any DN.
	if username == "" || password == "" || len(username) > 256 || len(password) > 1024 {
		return Identity{}, fmt.Errorf("%w: idp: invalid LDAP credentials", domain.ErrAccessDenied)
	}
	key := strings.ToLower(username)
	if s.throttle.blocked(key) {
		return Identity{}, fmt.Errorf("%w: idp: too many failed logins for %q, retry in a minute", domain.ErrTooManyRequests, username)
	}
	id, err := s.ldapLogin(ctx, cfg, username, password)
	if errors.Is(err, domain.ErrAccessDenied) {
		s.throttle.failed(key)
	} else if err == nil {
		s.throttle.succeeded(key)
	}
	return id, err
}

func (s *service) dial(ctx context.Context, cfg *ldapConfig) (*ldap.Conn, error) {
	host, _, err := net.SplitHostPort(cfg.addr)
	if err != nil {
		return nil, fmt.Errorf("%w: idp: invalid LDAP server address %q: %w", domain.ErrServiceUnavailable, cfg.addr, err)
	}
	tlsConf := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.skipVerify} // #nosec G402 -- tls_skip_verify is an explicit choice of the administrator
	dialer := &net.Dialer{Timeout: ldapTimeout}
	var conn *ldap.Conn
	if cfg.tls && !cfg.startTLS {
		conn, err = ldap.DialURL("ldaps://"+cfg.addr, ldap.DialWithDialer(dialer), ldap.DialWithTLSConfig(tlsConf))
	} else {
		conn, err = ldap.DialURL("ldap://"+cfg.addr, ldap.DialWithDialer(dialer))
		if err == nil && cfg.startTLS {
			if err = conn.StartTLS(tlsConf); err != nil {
				_ = conn.Close()
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: idp: LDAP server %s: %w", domain.ErrServiceUnavailable, cfg.addr, err)
	}
	conn.SetTimeout(ldapTimeout)
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	return conn, nil
}

func (s *service) ldapLogin(ctx context.Context, cfg *ldapConfig, username, password string) (Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*ldapTimeout)
	defer cancel()
	conn, err := s.dial(ctx, cfg)
	if err != nil {
		return Identity{}, err
	}
	defer func() { _ = conn.Close() }()
	if err := s.lookupBind(conn, cfg); err != nil {
		return Identity{}, err
	}
	filter := strings.ReplaceAll(cfg.userFilter, "%s", ldap.EscapeFilter(username))
	res, err := conn.Search(ldap.NewSearchRequest(cfg.userBase, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, int(ldapTimeout.Seconds()), false,
		filter, []string{"dn"}, nil))
	if err != nil && !ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
		return Identity{}, fmt.Errorf("%w: idp: LDAP user search: %w", domain.ErrServiceUnavailable, err)
	}
	if res == nil || len(res.Entries) != 1 {
		// Unknown or ambiguous: the same answer as a wrong password.
		return Identity{}, fmt.Errorf("%w: idp: invalid LDAP credentials", domain.ErrAccessDenied)
	}
	dn := res.Entries[0].DN
	if err := conn.Bind(dn, password); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
			return Identity{}, fmt.Errorf("%w: idp: invalid LDAP credentials", domain.ErrAccessDenied)
		}
		return Identity{}, fmt.Errorf("%w: idp: LDAP bind: %w", domain.ErrServiceUnavailable, err)
	}
	groups, err := s.ldapGroups(conn, cfg, dn, username)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Provider: ProviderLDAP, Subject: dn, Username: username, Groups: groups,
		Claims: map[string]any{"ldapUser": username, "ldapUserDN": dn, "ldapGroups": groups}}, nil
}

// lookupBind binds with the lookup account (anonymous when none is set).
func (s *service) lookupBind(conn *ldap.Conn, cfg *ldapConfig) error {
	var err error
	if cfg.bindDN != "" {
		err = conn.Bind(cfg.bindDN, cfg.bindPassword)
	} else {
		err = conn.UnauthenticatedBind("")
	}
	if err != nil {
		return fmt.Errorf("%w: idp: LDAP lookup bind: %w", domain.ErrServiceUnavailable, err)
	}
	return nil
}

// ldapGroups reads the groups of a user (as the lookup account).
func (s *service) ldapGroups(conn *ldap.Conn, cfg *ldapConfig, dn, username string) ([]string, error) {
	if cfg.groupBase == "" {
		return nil, nil
	}
	if err := s.lookupBind(conn, cfg); err != nil {
		return nil, err
	}
	filter := strings.ReplaceAll(cfg.groupFilter, "%d", ldap.EscapeFilter(dn))
	filter = strings.ReplaceAll(filter, "%s", ldap.EscapeFilter(username))
	res, err := conn.Search(ldap.NewSearchRequest(cfg.groupBase, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, maxLDAPGroups, int(ldapTimeout.Seconds()), false,
		filter, []string{cfg.name}, nil))
	if err != nil && !ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
		return nil, fmt.Errorf("%w: idp: LDAP group search: %w", domain.ErrServiceUnavailable, err)
	}
	var out []string
	if res != nil {
		for _, e := range res.Entries {
			if v := e.GetAttributeValue(cfg.name); v != "" {
				out = append(out, v)
			}
		}
	}
	return out, nil
}
