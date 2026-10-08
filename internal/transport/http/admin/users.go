package admin

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// UserDeps are the dependencies of the user, group and access key routes.
type UserDeps struct {
	// IAM serves the reads and the deletions (required).
	IAM iam.Service
	// Identities creates and updates users and groups (required).
	Identities adminsvc.IdentityManager
	// AccessKeys manages the service accounts on behalf of the caller
	// (required).
	AccessKeys adminsvc.AccessKeyManager
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

type userRoutes struct {
	deps  UserDeps
	guard guard
}

var _ UserRoutes = (*userRoutes)(nil)

// NewUserRoutes returns the user, group and access-key routes.
func NewUserRoutes(deps UserDeps) (UserRoutes, error) {
	if deps.IAM == nil || deps.Identities == nil || deps.AccessKeys == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: user routes require the IAM service, the identity and access key managers and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &userRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}, nil
}

// Register implements Routes.
func (h *userRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *userRoutes) routes() []route {
	// Access keys are self-service: the access key manager authorizes them
	// (own keys always, those of others with the admin action).
	return []route{
		{http.MethodGet, "/users", global(policy.ActionAdminListUsers), h.listUsers},
		{http.MethodPost, "/users", byService(policy.ActionAdminCreateUser, policy.ActionAdminAddUserToGroup, policy.ActionAdminAttachUserOrGroupPolicy), h.createUser},
		{http.MethodGet, "/users/{user}", global(policy.ActionAdminGetUser), h.getUser},
		{http.MethodPut, "/users/{user}", byService(policy.ActionAdminCreateUser, policy.ActionAdminEnableUser, policy.ActionAdminDisableUser,
			policy.ActionAdminAttachUserOrGroupPolicy, policy.ActionAdminAddUserToGroup, policy.ActionAdminRemoveUserFromGroup, policy.ActionAdminGetUser), h.updateUser},
		{http.MethodDelete, "/users/{user}", global(policy.ActionAdminDeleteUser), h.deleteUser},

		{http.MethodGet, "/groups", global(policy.ActionAdminListGroups), h.listGroups},
		{http.MethodPost, "/groups", byService(policy.ActionAdminAddUserToGroup, policy.ActionAdminAttachUserOrGroupPolicy), h.createGroup},
		{http.MethodGet, "/groups/{group}", global(policy.ActionAdminGetGroup), h.getGroup},
		{http.MethodPut, "/groups/{group}", byService(policy.ActionAdminEnableGroup, policy.ActionAdminDisableGroup, policy.ActionAdminAttachUserOrGroupPolicy,
			policy.ActionAdminAddUserToGroup, policy.ActionAdminRemoveUserFromGroup, policy.ActionAdminGetGroup), h.updateGroup},
		{http.MethodDelete, "/groups/{group}", global(policy.ActionAdminRemoveUserFromGroup), h.deleteGroup},

		{http.MethodGet, "/access-keys", byService(policy.ActionAdminListAccessKeys), h.listAccessKeys},
		{http.MethodPost, "/access-keys", byService(policy.ActionAdminCreateAccessKey), h.createAccessKey},
		{http.MethodGet, "/access-keys/{key}", byService(policy.ActionAdminListAccessKeys), h.getAccessKey},
		{http.MethodPut, "/access-keys/{key}", byService(policy.ActionAdminUpdateAccessKey), h.updateAccessKey},
		{http.MethodDelete, "/access-keys/{key}", byService(policy.ActionAdminRemoveServiceAccount), h.deleteAccessKey},
	}
}

func (h *userRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

// UserResponse is an IAM user. It mirrors the User DTO of the web console.
type UserResponse struct {
	AccessKey   string    `json:"accessKey"`
	Status      string    `json:"status"`
	PolicyNames []string  `json:"policyNames"`
	Groups      []string  `json:"groups"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func newUserResponse(u *iam.User) UserResponse {
	return UserResponse{
		AccessKey:   u.AccessKey,
		Status:      string(u.Status),
		PolicyNames: nonNil(u.PolicyNames),
		Groups:      nonNil(u.Groups),
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
	}
}

// UserListResponse is a page of users.
type UserListResponse struct {
	Items      []UserResponse `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

// CreateUserRequest is the JSON body of POST /users.
type CreateUserRequest struct {
	AccessKey   string   `json:"accessKey"`
	SecretKey   string   `json:"secretKey"`
	PolicyNames []string `json:"policyNames,omitempty"`
	// Groups are existing groups the user joins.
	Groups []string `json:"groups,omitempty"`
	// Status defaults to enabled.
	Status string `json:"status,omitempty"`
}

// UpdateUserRequest is the JSON body of PUT /users/{user}; absent fields
// are left unchanged.
type UpdateUserRequest struct {
	SecretKey   *string   `json:"secretKey,omitempty"`
	Status      *string   `json:"status,omitempty"`
	PolicyNames *[]string `json:"policyNames,omitempty"`
	// Groups replaces the group memberships.
	Groups *[]string `json:"groups,omitempty"`
}

// parseStatus parses an optional status: "" is the zero status.
func parseStatus(s string) (domain.CredentialStatus, error) {
	if s == "" {
		return "", nil
	}
	return domain.ParseCredentialStatus(s)
}

func (h *userRoutes) listUsers(w http.ResponseWriter, r *http.Request) {
	p, err := readPage(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	users, err := h.deps.IAM.ListUsers(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items, next := paginate(users, func(u iam.User) string { return u.AccessKey }, p)
	out := UserListResponse{Items: make([]UserResponse, 0, len(items)), NextCursor: next}
	for i := range items {
		out.Items = append(out.Items, newUserResponse(&items[i]))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *userRoutes) createUser(w http.ResponseWriter, r *http.Request) {
	var in CreateUserRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	status, err := parseStatus(in.Status)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	u, err := h.deps.Identities.CreateUser(r.Context(), adminsvc.CreateUserInput{
		Requester: requesterOf(r), AccessKey: in.AccessKey, SecretKey: in.SecretKey, Status: status, PolicyNames: in.PolicyNames, Groups: in.Groups,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, newUserResponse(u))
}

func (h *userRoutes) getUser(w http.ResponseWriter, r *http.Request) {
	u, err := h.deps.IAM.GetUser(r.Context(), chi.URLParam(r, "user"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newUserResponse(u))
}

func (h *userRoutes) updateUser(w http.ResponseWriter, r *http.Request) {
	var in UpdateUserRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	upd := adminsvc.UpdateUserInput{Requester: requesterOf(r), AccessKey: chi.URLParam(r, "user"), SecretKey: in.SecretKey, PolicyNames: in.PolicyNames, Groups: in.Groups}
	if in.Status != nil {
		status, err := domain.ParseCredentialStatus(*in.Status)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		upd.Status = &status
	}
	u, err := h.deps.Identities.UpdateUser(r.Context(), upd)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newUserResponse(u))
}

func (h *userRoutes) deleteUser(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.IAM.DeleteUser(r.Context(), chi.URLParam(r, "user")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
