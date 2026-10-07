package domain

import "time"

// CredentialStatus is the enabled/disabled state of a credential or user.
type CredentialStatus string

// Credential statuses.
const (
	StatusEnabled  CredentialStatus = "enabled"
	StatusDisabled CredentialStatus = "disabled"
)

// Credentials are the secret material of a principal.
type Credentials struct {
	AccessKey string
	SecretKey string
	// SessionToken is set for temporary (STS) credentials.
	SessionToken string
	// Expiration is the zero value for permanent credentials.
	Expiration time.Time
	Status     CredentialStatus
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
}

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
