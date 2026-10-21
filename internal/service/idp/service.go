// Package idp authenticates the users of external identity providers
// (S-IAM-08, S-IAM-09): the ID tokens of an OpenID Connect provider
// (AssumeRoleWithWebIdentity and the SSO login of the console, with the
// authorization code flow and PKCE) and the passwords of an LDAP directory
// (AssumeRoleWithLDAPIdentity and the console login). The providers are
// configured in the identity_openid and identity_ldap subsystems of the
// dynamic configuration, read at every use.
//
// An authenticated user is an Identity: its subject, its name, its groups
// at the provider and the policies the provider assigns. The IAM service
// turns it into federated credentials, whose policies are those policies
// and those of the Nexo groups named like its groups.
package idp

import (
	"context"
	"time"
)

// Service authenticates the users of the external providers.
type Service interface {
	// Providers reports the providers that are enabled (the login page of
	// the console offers them).
	Providers(ctx context.Context) (Providers, error)
	// WebIdentity verifies an ID token of the OpenID Connect provider:
	// signature (the keys of the provider), issuer, audience (the client
	// id) and validity. A provider not configured is
	// domain.ErrNotImplemented, an invalid token domain.ErrAccessDenied.
	WebIdentity(ctx context.Context, token string) (Identity, error)
	// AuthCodeURL returns the URL of the provider starting the login of
	// the console (authorization code flow with PKCE).
	AuthCodeURL(ctx context.Context, in AuthCodeInput) (string, error)
	// Exchange completes the login: it trades the code for the ID token
	// and verifies it, its nonce included.
	Exchange(ctx context.Context, in ExchangeInput) (Identity, error)
	// LDAP authenticates a user of the directory by binding with its
	// password, and reads its groups. Wrong credentials are
	// domain.ErrAccessDenied; repeated failures of a name are throttled.
	LDAP(ctx context.Context, username, password string) (Identity, error)
}

// Provider names.
const (
	ProviderOpenID = "openid"
	ProviderLDAP   = "ldap"
)

// Providers are the enabled providers.
type Providers struct {
	OpenID bool
	// OpenIDName is the name of the SSO button of the console.
	OpenIDName string
	// OpenIDRedirect is the redirect URI configured (empty: the console
	// derives it from the request).
	OpenIDRedirect string
	LDAP           bool
}

// Identity is a user authenticated by a provider.
type Identity struct {
	Provider string
	// Subject is the stable identifier at the provider (sub, the DN).
	Subject  string
	Username string
	Groups   []string
	// PolicyNames are the policies the provider assigns (the policy claim).
	PolicyNames []string
	// Claims are the claims of the ID token (OpenID Connect).
	Claims map[string]any
	// Expiry is the end of the validity of the ID token (zero for LDAP).
	Expiry time.Time
}

// AuthCodeInput are the parameters of the authorization request.
type AuthCodeInput struct {
	RedirectURI   string
	State         string
	Nonce         string
	CodeChallenge string
}

// ExchangeInput are the parameters of the code exchange.
type ExchangeInput struct {
	Code         string
	RedirectURI  string
	CodeVerifier string
	Nonce        string
}
