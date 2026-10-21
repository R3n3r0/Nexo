package idp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/config"
)

// Subsystems and keys of the dynamic configuration.
const (
	SubsystemOpenID = "identity_openid"
	SubsystemLDAP   = "identity_ldap"

	KeyEnable        = "enable"
	KeyConfigURL     = "config_url"
	KeyClientID      = "client_id"
	KeyClientSecret  = "client_secret"
	KeyScopes        = "scopes"
	KeyClaimName     = "claim_name"
	KeyGroupsClaim   = "groups_claim"
	KeyUsernameClaim = "username_claim"
	KeyDisplayName   = "display_name"
	KeyRedirectURI   = "redirect_uri"

	KeyServerAddr         = "server_addr"
	KeyTLS                = "tls"
	KeyStartTLS           = "starttls"
	KeyTLSSkipVerify      = "tls_skip_verify"
	KeyLookupBindDN       = "lookup_bind_dn"
	KeyLookupBindPassword = "lookup_bind_password"
	KeyUserBaseDN         = "user_dn_search_base_dn"
	KeyUserFilter         = "user_dn_search_filter"
	KeyGroupBaseDN        = "group_search_base_dn"
	KeyGroupFilter        = "group_search_filter"
	KeyGroupNameAttribute = "group_name_attribute"
)

// Deps are the dependencies of the service.
type Deps struct {
	Config config.Service
	// HTTP calls the OpenID provider (default: a client with a 10 s
	// timeout).
	HTTP   *http.Client
	Clock  clock.Clock
	Logger *slog.Logger
}

type service struct {
	deps Deps

	mu sync.Mutex
	// discovery caches the provider metadata and keys, by config URL.
	discovery map[string]*provider
	throttle  *throttle
}

var _ Service = (*service)(nil)

// New returns the identity provider service.
func New(deps Deps) (Service, error) {
	if deps.Config == nil {
		return nil, fmt.Errorf("%w: idp: the config service is required", domain.ErrInvalidArgument)
	}
	if deps.HTTP == nil {
		deps.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &service{deps: deps, discovery: map[string]*provider{}, throttle: newThrottle(deps.Clock)}, nil
}

// openIDConfig is the identity_openid subsystem.
type openIDConfig struct {
	configURL, clientID, clientSecret string
	scopes                            []string
	claimName, groupsClaim            string
	usernameClaim, displayName        string
	redirectURI                       string
}

func flag(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "1", "yes":
		return true
	}
	return false
}

func orDefault(v, def string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return def
}

func list(v string) []string {
	var out []string
	for _, s := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// openID returns the OpenID configuration, nil when disabled.
func (s *service) openID(ctx context.Context) (*openIDConfig, error) {
	kv, err := s.deps.Config.Get(ctx, SubsystemOpenID)
	if err != nil {
		return nil, fmt.Errorf("idp: %s: %w", SubsystemOpenID, err)
	}
	if !flag(kv[KeyEnable]) {
		return nil, nil
	}
	c := &openIDConfig{
		configURL: strings.TrimSpace(kv[KeyConfigURL]), clientID: strings.TrimSpace(kv[KeyClientID]), clientSecret: kv[KeyClientSecret],
		scopes:      list(orDefault(kv[KeyScopes], "openid,profile,email")),
		claimName:   orDefault(kv[KeyClaimName], "policy"),
		groupsClaim: orDefault(kv[KeyGroupsClaim], "groups"), usernameClaim: orDefault(kv[KeyUsernameClaim], "preferred_username"),
		displayName: orDefault(kv[KeyDisplayName], "SSO"), redirectURI: strings.TrimSpace(kv[KeyRedirectURI]),
	}
	if c.configURL == "" || c.clientID == "" {
		return nil, fmt.Errorf("%w: idp: %s needs %s and %s", domain.ErrNotImplemented, SubsystemOpenID, KeyConfigURL, KeyClientID)
	}
	return c, nil
}

// ldapConfig is the identity_ldap subsystem.
type ldapConfig struct {
	addr                         string
	tls, startTLS, skipVerify    bool
	bindDN, bindPassword         string
	userBase, userFilter         string
	groupBase, groupFilter, name string
}

// ldap returns the LDAP configuration, nil when disabled.
func (s *service) ldapConf(ctx context.Context) (*ldapConfig, error) {
	kv, err := s.deps.Config.Get(ctx, SubsystemLDAP)
	if err != nil {
		return nil, fmt.Errorf("idp: %s: %w", SubsystemLDAP, err)
	}
	if !flag(kv[KeyEnable]) {
		return nil, nil
	}
	c := &ldapConfig{
		addr: strings.TrimSpace(kv[KeyServerAddr]), tls: kv[KeyTLS] == "" || flag(kv[KeyTLS]), startTLS: flag(kv[KeyStartTLS]),
		skipVerify: flag(kv[KeyTLSSkipVerify]), bindDN: kv[KeyLookupBindDN], bindPassword: kv[KeyLookupBindPassword],
		userBase: kv[KeyUserBaseDN], userFilter: orDefault(kv[KeyUserFilter], "(uid=%s)"),
		groupBase: kv[KeyGroupBaseDN], groupFilter: orDefault(kv[KeyGroupFilter], "(&(objectClass=groupOfNames)(member=%d))"),
		name: orDefault(kv[KeyGroupNameAttribute], "cn"),
	}
	if c.addr == "" || c.userBase == "" {
		return nil, fmt.Errorf("%w: idp: %s needs %s and %s", domain.ErrNotImplemented, SubsystemLDAP, KeyServerAddr, KeyUserBaseDN)
	}
	return c, nil
}

// Providers implements Service.
func (s *service) Providers(ctx context.Context) (Providers, error) {
	var out Providers
	oc, err := s.openID(ctx)
	if err == nil && oc != nil {
		out.OpenID, out.OpenIDName, out.OpenIDRedirect = true, oc.displayName, oc.redirectURI
	}
	lc, err := s.ldapConf(ctx)
	if err == nil && lc != nil {
		out.LDAP = true
	}
	return out, nil
}

// throttle locks out an LDAP name after repeated failures: 5 failures
// within a minute block it for a minute.
type throttle struct {
	clk  clock.Clock
	mu   sync.Mutex
	fail map[string][]time.Time
}

const (
	throttleFailures = 5
	throttleWindow   = time.Minute
)

func newThrottle(clk clock.Clock) *throttle {
	return &throttle{clk: clk, fail: map[string][]time.Time{}}
}

// blocked reports whether name is locked out.
func (t *throttle) blocked(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.recentLocked(name)) >= throttleFailures
}

func (t *throttle) recentLocked(name string) []time.Time {
	now := t.clk.Now()
	kept := t.fail[name][:0]
	for _, at := range t.fail[name] {
		if now.Sub(at) < throttleWindow {
			kept = append(kept, at)
		}
	}
	if len(kept) == 0 {
		delete(t.fail, name)
		return nil
	}
	t.fail[name] = kept
	return kept
}

func (t *throttle) failed(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.fail) > 100000 {
		// Bounded memory under a spray of names: start over.
		t.fail = map[string][]time.Time{}
	}
	t.fail[name] = append(t.recentLocked(name), t.clk.Now())
}

func (t *throttle) succeeded(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fail, name)
}
