package iam

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// Deps are the dependencies of the IAM implementations.
type Deps struct {
	Clock  clock.Clock
	Logger *slog.Logger
}

// Option configures an IAM implementation.
type Option func(*rootOnly)

// rootOnly is a minimal IAM that knows only the root credentials installed
// by Bootstrap. It lets the console session service and the smoke tests
// run before the real IAM store exists.
//
// TODO(M2): replace with the encrypted IAM store (.nexo.sys/config/iam).
type rootOnly struct {
	deps Deps
	mu   sync.RWMutex
	root *domain.Credentials
}

var _ Service = (*rootOnly)(nil)

// NewRootOnly returns an IAM service that only resolves the root user.
func NewRootOnly(deps Deps, opts ...Option) Service {
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	s := &rootOnly{deps: deps}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Bootstrap implements Service.
func (s *rootOnly) Bootstrap(_ context.Context, root domain.Credentials) error {
	if root.AccessKey == "" || root.SecretKey == "" {
		return fmt.Errorf("%w: root credentials must not be empty", domain.ErrInvalidArgument)
	}
	root.Status = domain.StatusEnabled
	s.mu.Lock()
	defer s.mu.Unlock()
	s.root = &root
	return nil
}

// LookupCredentials implements Service.
func (s *rootOnly) LookupCredentials(_ context.Context, accessKey string) (domain.Credentials, domain.Identity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.root == nil || accessKey == "" || accessKey != s.root.AccessKey {
		return domain.Credentials{}, domain.Identity{}, fmt.Errorf("%w: %q", domain.ErrInvalidAccessKey, accessKey)
	}
	return *s.root, domain.Identity{AccessKey: accessKey, IsRoot: true}, nil
}

func notImplemented(op string) error {
	return fmt.Errorf("iam: %s: %w", op, domain.ErrNotImplemented)
}

// CreateUser implements Service.
func (s *rootOnly) CreateUser(context.Context, CreateUserInput) (*User, error) {
	return nil, notImplemented("CreateUser")
}

// GetUser implements Service.
func (s *rootOnly) GetUser(_ context.Context, accessKey string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.root != nil && accessKey == s.root.AccessKey {
		return &User{AccessKey: accessKey, Status: domain.StatusEnabled}, nil
	}
	return nil, fmt.Errorf("%w: %q", domain.ErrUserNotFound, accessKey)
}

// ListUsers implements Service.
func (s *rootOnly) ListUsers(context.Context) ([]User, error) {
	return nil, notImplemented("ListUsers")
}

// DeleteUser implements Service.
func (s *rootOnly) DeleteUser(context.Context, string) error { return notImplemented("DeleteUser") }

// SetUserSecret implements Service.
func (s *rootOnly) SetUserSecret(context.Context, string, string) error {
	return notImplemented("SetUserSecret")
}

// SetUserStatus implements Service.
func (s *rootOnly) SetUserStatus(context.Context, string, domain.CredentialStatus) error {
	return notImplemented("SetUserStatus")
}

// SetUserPolicy implements Service.
func (s *rootOnly) SetUserPolicy(context.Context, string, []string) error {
	return notImplemented("SetUserPolicy")
}

// CreateGroup implements Service.
func (s *rootOnly) CreateGroup(context.Context, CreateGroupInput) (*Group, error) {
	return nil, notImplemented("CreateGroup")
}

// GetGroup implements Service.
func (s *rootOnly) GetGroup(context.Context, string) (*Group, error) {
	return nil, notImplemented("GetGroup")
}

// ListGroups implements Service.
func (s *rootOnly) ListGroups(context.Context) ([]Group, error) {
	return nil, notImplemented("ListGroups")
}

// DeleteGroup implements Service.
func (s *rootOnly) DeleteGroup(context.Context, string) error { return notImplemented("DeleteGroup") }

// AddGroupMembers implements Service.
func (s *rootOnly) AddGroupMembers(context.Context, string, []string) error {
	return notImplemented("AddGroupMembers")
}

// RemoveGroupMembers implements Service.
func (s *rootOnly) RemoveGroupMembers(context.Context, string, []string) error {
	return notImplemented("RemoveGroupMembers")
}

// SetGroupStatus implements Service.
func (s *rootOnly) SetGroupStatus(context.Context, string, domain.CredentialStatus) error {
	return notImplemented("SetGroupStatus")
}

// SetGroupPolicy implements Service.
func (s *rootOnly) SetGroupPolicy(context.Context, string, []string) error {
	return notImplemented("SetGroupPolicy")
}

// CreateAccessKey implements Service.
func (s *rootOnly) CreateAccessKey(context.Context, CreateAccessKeyInput) (*AccessKey, error) {
	return nil, notImplemented("CreateAccessKey")
}

// GetAccessKey implements Service.
func (s *rootOnly) GetAccessKey(context.Context, string) (*AccessKey, error) {
	return nil, notImplemented("GetAccessKey")
}

// ListAccessKeys implements Service.
func (s *rootOnly) ListAccessKeys(context.Context, string) ([]AccessKey, error) {
	return nil, notImplemented("ListAccessKeys")
}

// UpdateAccessKey implements Service.
func (s *rootOnly) UpdateAccessKey(context.Context, string, UpdateAccessKeyInput) (*AccessKey, error) {
	return nil, notImplemented("UpdateAccessKey")
}

// DeleteAccessKey implements Service.
func (s *rootOnly) DeleteAccessKey(context.Context, string) error {
	return notImplemented("DeleteAccessKey")
}

// AssumeRole implements Service.
func (s *rootOnly) AssumeRole(context.Context, AssumeRoleInput) (*domain.Credentials, error) {
	return nil, notImplemented("AssumeRole")
}
