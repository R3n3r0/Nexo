package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// AccessKeyManagerDeps are the dependencies of the access key manager.
type AccessKeyManagerDeps struct {
	// IAM stores the service accounts and enforces ownership (required).
	IAM iam.Service
	// Authz decides between self-service and administration (required).
	Authz  Authorizer
	Logger *slog.Logger
}

// AccessKeyManagerOption configures the access key manager.
type AccessKeyManagerOption func(*accessKeyManager)

type accessKeyManager struct {
	deps AccessKeyManagerDeps
}

var _ AccessKeyManager = (*accessKeyManager)(nil)

// NewAccessKeyManager returns the access key manager. Missing
// dependencies are a wiring error (domain.ErrInvalidArgument).
func NewAccessKeyManager(deps AccessKeyManagerDeps, opts ...AccessKeyManagerOption) (AccessKeyManager, error) {
	if deps.IAM == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: access key manager requires the IAM service and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &accessKeyManager{deps: deps}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// keyCaller is the identity the manager presents to IAM for a request.
type keyCaller struct {
	id domain.Identity
	// delegated marks a non-root administrator that IAM sees as the system
	// actor (see elevate): the manager keeps it away from the keys of root,
	// which carry the powers of root.
	delegated bool
}

// errRootKeys is the denial of a delegated administrator acting on the
// access keys of root.
func errRootKeys() error {
	return fmt.Errorf("%w: only root manages the access keys of root", domain.ErrAccessDenied)
}

// caller authorizes action with the self-service rule and returns the
// identity to present to IAM (see elevate).
func (m *accessKeyManager) caller(ctx context.Context, req Requester, action policy.Action) (keyCaller, error) {
	d, err := m.deps.Authz.Authorize(ctx, AuthorizeInput{Requester: req, Actions: []policy.Action{action}, SelfService: true})
	if err != nil {
		return keyCaller{}, err
	}
	if !d.Allowed {
		return keyCaller{}, denied(d)
	}
	return keyCaller{id: elevate(req, d), delegated: delegated(req, d)}, nil
}

// rootOwned reports whether parent is root (or a credential of root):
// keys owned by it resolve to root identities that bypass every policy.
// Unknown and disabled parents are not root; IAM reports them.
func (m *accessKeyManager) rootOwned(ctx context.Context, parent string) (bool, error) {
	if parent == "" {
		return false, nil
	}
	_, id, err := m.deps.IAM.LookupCredentials(ctx, parent)
	switch {
	case err == nil:
		return id.IsRoot, nil
	case errors.Is(err, domain.ErrInvalidAccessKey), errors.Is(err, domain.ErrAccessDenied):
		return false, nil
	default:
		return false, fmt.Errorf("admin: owner of access keys %q: %w", parent, err)
	}
}

// checkParent denies a delegated administrator the keys owned by root.
func (m *accessKeyManager) checkParent(ctx context.Context, c keyCaller, parent string) error {
	if !c.delegated {
		return nil
	}
	root, err := m.rootOwned(ctx, parent)
	if err != nil {
		return err
	}
	if root {
		return errRootKeys()
	}
	return nil
}

// checkKey reads accessKey with the identity of c and denies a delegated
// administrator a key owned by root. The parent of a key never changes.
func (m *accessKeyManager) checkKey(ctx context.Context, c keyCaller, accessKey string) (*iam.AccessKey, error) {
	k, err := m.deps.IAM.GetAccessKey(ctx, c.id, accessKey)
	if err != nil {
		return nil, err
	}
	if err := m.checkParent(ctx, c, k.ParentUser); err != nil {
		return nil, err
	}
	return k, nil
}

// List implements AccessKeyManager.
func (m *accessKeyManager) List(ctx context.Context, req Requester, parent string) ([]iam.AccessKey, error) {
	c, err := m.caller(ctx, req, policy.ActionAdminListAccessKeys)
	if err != nil {
		return nil, err
	}
	if err := m.checkParent(ctx, c, parent); err != nil {
		return nil, err
	}
	keys, err := m.deps.IAM.ListAccessKeys(ctx, c.id, parent)
	if err != nil || !c.delegated || parent != "" {
		return keys, err
	}
	// A listing of every user hides the keys of root from a delegated
	// administrator.
	owners := map[string]bool{}
	out := make([]iam.AccessKey, 0, len(keys))
	for _, k := range keys {
		root, seen := owners[k.ParentUser]
		if !seen {
			if root, err = m.rootOwned(ctx, k.ParentUser); err != nil {
				return nil, err
			}
			owners[k.ParentUser] = root
		}
		if !root {
			out = append(out, k)
		}
	}
	return out, nil
}

// Get implements AccessKeyManager.
func (m *accessKeyManager) Get(ctx context.Context, req Requester, accessKey string) (*iam.AccessKey, error) {
	c, err := m.caller(ctx, req, policy.ActionAdminListAccessKeys)
	if err != nil {
		return nil, err
	}
	return m.checkKey(ctx, c, accessKey)
}

// Create implements AccessKeyManager.
func (m *accessKeyManager) Create(ctx context.Context, req Requester, in iam.CreateAccessKeyInput) (*iam.AccessKey, error) {
	c, err := m.caller(ctx, req, policy.ActionAdminCreateAccessKey)
	if err != nil {
		return nil, err
	}
	if in.ParentUser == "" {
		// The key belongs to the requester, never to the system actor.
		in.ParentUser = req.Identity.EffectiveUser()
	}
	if err := m.checkParent(ctx, c, in.ParentUser); err != nil {
		return nil, err
	}
	in.Caller = c.id
	k, err := m.deps.IAM.CreateAccessKey(ctx, in)
	if err != nil {
		return nil, err
	}
	m.deps.Logger.InfoContext(ctx, "admin: access key created", "accessKey", k.AccessKey, "parent", k.ParentUser, "by", req.Identity.AccessKey)
	return k, nil
}

// Update implements AccessKeyManager.
func (m *accessKeyManager) Update(ctx context.Context, req Requester, accessKey string, in iam.UpdateAccessKeyInput) (*iam.AccessKey, error) {
	c, err := m.caller(ctx, req, policy.ActionAdminUpdateAccessKey)
	if err != nil {
		return nil, err
	}
	if c.delegated {
		if _, err := m.checkKey(ctx, c, accessKey); err != nil {
			return nil, err
		}
	}
	return m.deps.IAM.UpdateAccessKey(ctx, c.id, accessKey, in)
}

// Delete implements AccessKeyManager.
func (m *accessKeyManager) Delete(ctx context.Context, req Requester, accessKey string) error {
	c, err := m.caller(ctx, req, policy.ActionAdminRemoveServiceAccount)
	if err != nil {
		return err
	}
	if c.delegated {
		if _, err := m.checkKey(ctx, c, accessKey); err != nil {
			return err
		}
	}
	if err := m.deps.IAM.DeleteAccessKey(ctx, c.id, accessKey); err != nil {
		return err
	}
	m.deps.Logger.InfoContext(ctx, "admin: access key deleted", "accessKey", accessKey, "by", req.Identity.AccessKey)
	return nil
}
