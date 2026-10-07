// Package iam manages identities: users, groups, service accounts (access
// keys with inline policies and expiry) and STS temporary credentials.
//
//go:generate mockery --name Service --output ./mock --outpkg mock --with-expecter
package iam

import (
	"context"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Service is the identity store.
type Service interface {
	// Bootstrap installs the root credentials. It is called at startup and
	// is idempotent.
	Bootstrap(ctx context.Context, root domain.Credentials) error

	// Users.
	CreateUser(ctx context.Context, in CreateUserInput) (*User, error)
	GetUser(ctx context.Context, accessKey string) (*User, error)
	ListUsers(ctx context.Context) ([]User, error)
	DeleteUser(ctx context.Context, accessKey string) error
	SetUserSecret(ctx context.Context, accessKey, secretKey string) error
	SetUserStatus(ctx context.Context, accessKey string, status domain.CredentialStatus) error
	// SetUserPolicy replaces the policies attached directly to the user.
	SetUserPolicy(ctx context.Context, accessKey string, policyNames []string) error

	// Groups.
	CreateGroup(ctx context.Context, in CreateGroupInput) (*Group, error)
	GetGroup(ctx context.Context, name string) (*Group, error)
	ListGroups(ctx context.Context) ([]Group, error)
	DeleteGroup(ctx context.Context, name string) error
	AddGroupMembers(ctx context.Context, name string, members []string) error
	RemoveGroupMembers(ctx context.Context, name string, members []string) error
	SetGroupStatus(ctx context.Context, name string, status domain.CredentialStatus) error
	SetGroupPolicy(ctx context.Context, name string, policyNames []string) error

	// Access keys (service accounts).
	CreateAccessKey(ctx context.Context, in CreateAccessKeyInput) (*AccessKey, error)
	GetAccessKey(ctx context.Context, accessKey string) (*AccessKey, error)
	// ListAccessKeys lists the keys owned by parent; empty lists all.
	ListAccessKeys(ctx context.Context, parent string) ([]AccessKey, error)
	UpdateAccessKey(ctx context.Context, accessKey string, in UpdateAccessKeyInput) (*AccessKey, error)
	DeleteAccessKey(ctx context.Context, accessKey string) error

	// LookupCredentials resolves any access key (root, user, service
	// account, STS) to its credentials and identity. Unknown keys return
	// domain.ErrInvalidAccessKey.
	LookupCredentials(ctx context.Context, accessKey string) (domain.Credentials, domain.Identity, error)

	// AssumeRole issues temporary credentials for the caller.
	AssumeRole(ctx context.Context, in AssumeRoleInput) (*domain.Credentials, error)
}

// User is an IAM user.
type User struct {
	AccessKey   string
	Status      domain.CredentialStatus
	PolicyNames []string
	Groups      []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CreateUserInput are the parameters of CreateUser.
type CreateUserInput struct {
	AccessKey   string
	SecretKey   string
	Status      domain.CredentialStatus
	PolicyNames []string
}

// Group is an IAM group.
type Group struct {
	Name        string
	Status      domain.CredentialStatus
	Members     []string
	PolicyNames []string
	CreatedAt   time.Time
}

// CreateGroupInput are the parameters of CreateGroup.
type CreateGroupInput struct {
	Name        string
	Members     []string
	PolicyNames []string
}

// AccessKey is a service account derived from a parent user.
type AccessKey struct {
	AccessKey string
	// SecretKey is only populated by CreateAccessKey.
	SecretKey   string
	ParentUser  string
	Name        string
	Description string
	Status      domain.CredentialStatus
	// Expiration is zero for non-expiring keys.
	Expiration time.Time
	// Policy is the inline session policy; nil means "inherit the parent".
	Policy    *domain.Policy
	CreatedAt time.Time
}

// CreateAccessKeyInput are the parameters of CreateAccessKey.
type CreateAccessKeyInput struct {
	// ParentUser owns the key. It is the caller for non-root callers.
	ParentUser string
	// AccessKey and SecretKey are generated when empty.
	AccessKey   string
	SecretKey   string
	Name        string
	Description string
	Expiration  time.Time
	Policy      *domain.Policy
}

// UpdateAccessKeyInput are the parameters of UpdateAccessKey; nil fields
// are left unchanged.
type UpdateAccessKeyInput struct {
	SecretKey   *string
	Name        *string
	Description *string
	Status      *domain.CredentialStatus
	Expiration  *time.Time
	Policy      *domain.Policy
	// ClearPolicy removes the inline policy.
	ClearPolicy bool
}

// AssumeRoleInput are the parameters of AssumeRole.
type AssumeRoleInput struct {
	// Caller is the authenticated identity requesting the role.
	Caller domain.Identity
	// Policy is the optional session policy restricting the credentials.
	Policy *domain.Policy
	// Duration is the validity (15 minutes to 7 days).
	Duration time.Duration
	// RoleSessionName is informational.
	RoleSessionName string
}
