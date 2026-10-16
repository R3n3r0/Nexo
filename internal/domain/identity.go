package domain

import (
	"fmt"
	"time"
)

// CredentialStatus is the enabled/disabled state of a credential or user.
type CredentialStatus string

// Credential statuses.
const (
	StatusEnabled  CredentialStatus = "enabled"
	StatusDisabled CredentialStatus = "disabled"
)

// ParseCredentialStatus parses "enabled" or "disabled"; any other value,
// the empty string included, is an invalid argument.
func ParseCredentialStatus(s string) (CredentialStatus, error) {
	switch st := CredentialStatus(s); st {
	case StatusEnabled, StatusDisabled:
		return st, nil
	default:
		return "", fmt.Errorf("%w: status must be %q or %q", ErrInvalidArgument, StatusEnabled, StatusDisabled)
	}
}

// Credentials are the secret material of a principal.
type Credentials struct {
	AccessKey string
	SecretKey string
	// SessionToken is set for temporary (STS) credentials.
	SessionToken string
	// Expiration is the zero value for permanent credentials.
	Expiration time.Time
	Status     CredentialStatus
	// RevocationEpoch changes every time the credentials, or the user they
	// derive from, are disabled. A console session records it at login
	// and ends when it changes, so that enabling the credentials again does
	// not bring back the sessions opened before (pentest PT-05).
	RevocationEpoch uint64
}

// IsTemporary reports whether the credentials come from STS.
func (c Credentials) IsTemporary() bool { return c.SessionToken != "" || !c.Expiration.IsZero() }

// IsExpired reports whether the credentials expired at the given time.
func (c Credentials) IsExpired(now time.Time) bool {
	return !c.Expiration.IsZero() && !now.Before(c.Expiration)
}

// IsEnabled reports whether the credentials are usable (enabled or status unset).
func (c Credentials) IsEnabled() bool { return c.Status == "" || c.Status == StatusEnabled }

// Identity is the authenticated principal of a request.
type Identity struct {
	// AccessKey is the key used to sign the request; empty for anonymous.
	AccessKey string
	// Parent is the owning user for service accounts and STS credentials.
	Parent      string
	IsRoot      bool
	IsAnonymous bool
	Groups      []string
	// Claims are the JWT claims of STS credentials.
	Claims map[string]any
	// PolicyNames are the IAM policies attached directly or via groups.
	PolicyNames []string
	// SessionPolicy is the inline policy embedded in service accounts or
	// STS sessions. The effective permissions are the intersection of the
	// parent's policies and the session policy.
	SessionPolicy *Policy
	// AuthType is how the request was authenticated, the value of the
	// s3:authType policy condition key. It is empty for identities that do
	// not come from request authentication (console sessions, internal
	// calls).
	AuthType AuthType
	// SignatureAge is the age of the request signature when it was
	// verified (s3:signatureAge, in milliseconds in policies); zero for
	// anonymous requests.
	SignatureAge time.Duration
	// SignedHeaders are the lowercase names, sorted, of the request headers
	// covered by the SigV4 signature (SignedHeaders); nil for anonymous
	// requests, POST policy uploads and identities that do not come from a
	// request signature.
	SignedHeaders []string
}

// AuthType is the authentication mechanism of a request.
type AuthType string

// Authentication mechanisms, as reported by the s3:authType condition key.
const (
	// AuthTypeHeader is a request signed with the Authorization header.
	AuthTypeHeader AuthType = "REST-HEADER"
	// AuthTypeQuery is a presigned URL (signature in the query string).
	AuthTypeQuery AuthType = "REST-QUERY-STRING"
	// AuthTypePost is a browser upload authenticated by a POST policy.
	AuthTypePost AuthType = "POST"
	// AuthTypeAnonymous is an unsigned request.
	AuthTypeAnonymous AuthType = "ANONYMOUS"
	// AuthTypeBearer is a request authenticated by a bearer token (the
	// Prometheus scrape tokens of the metrics endpoints); a Nexo extension,
	// never accepted by the S3 API.
	AuthTypeBearer AuthType = "BEARER"
)

// Anonymous returns the anonymous identity.
func Anonymous() *Identity { return &Identity{IsAnonymous: true} }

// EffectiveUser returns the user the identity acts as: the parent for
// derived credentials, the access key otherwise.
func (i *Identity) EffectiveUser() string {
	if i.Parent != "" {
		return i.Parent
	}
	return i.AccessKey
}
