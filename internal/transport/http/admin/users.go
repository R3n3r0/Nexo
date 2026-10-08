package admin

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// UserDeps are the dependencies of the user routes.
type UserDeps struct {
	IAM iam.Service
	// Policy authorizes the admin:* actions; nil leaves the routes open (tests).
	Policy policy.Service
	Enc    httpx.Codec
}

type userRoutes struct {
	deps  UserDeps
	authz authorizer
}

var _ UserRoutes = (*userRoutes)(nil)

// NewUserRoutes returns the user, group and access-key routes.
func NewUserRoutes(deps UserDeps) (UserRoutes, error) {
	if deps.IAM == nil {
		return nil, fmt.Errorf("%w: user routes require the IAM service", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &userRoutes{deps: deps, authz: authorizer{policy: deps.Policy, enc: deps.Enc}}, nil
}

// Register implements Routes.
func (h *userRoutes) Register(r chi.Router) {
	r.Get("/users", h.listUsers)
	r.Post("/users", h.createUser)
	r.Get("/users/{user}", h.getUser)
	r.Put("/users/{user}", h.updateUser)
	r.Delete("/users/{user}", h.deleteUser)

	r.Get("/groups", h.listGroups)
	r.Post("/groups", h.createGroup)
	r.Get("/groups/{group}", h.getGroup)
	r.Put("/groups/{group}", h.updateGroup)
	r.Delete("/groups/{group}", h.deleteGroup)

	r.Get("/access-keys", h.listAccessKeys)
	r.Post("/access-keys", h.createAccessKey)
	r.Get("/access-keys/{key}", h.getAccessKey)
	r.Put("/access-keys/{key}", h.updateAccessKey)
	r.Delete("/access-keys/{key}", h.deleteAccessKey)
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

// CreateUserRequest is the JSON body of POST /users.
type CreateUserRequest struct {
	AccessKey   string   `json:"accessKey"`
	SecretKey   string   `json:"secretKey"`
	PolicyNames []string `json:"policyNames,omitempty"`
	// Status defaults to enabled.
	Status string `json:"status,omitempty"`
}

// UpdateUserRequest is the JSON body of PUT /users/{user}; absent fields
// are left unchanged.
type UpdateUserRequest struct {
	SecretKey   *string   `json:"secretKey,omitempty"`
	Status      *string   `json:"status,omitempty"`
	PolicyNames *[]string `json:"policyNames,omitempty"`
}

func (h *userRoutes) listUsers(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminListUsers, "") {
		return
	}
	users, err := h.deps.IAM.ListUsers(r.Context())
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	out := make([]UserResponse, 0, len(users))
	for i := range users {
		out = append(out, newUserResponse(&users[i]))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *userRoutes) createUser(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminCreateUser, "") {
		return
	}
	var in CreateUserRequest
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	input := iam.CreateUserInput{AccessKey: in.AccessKey, SecretKey: in.SecretKey, PolicyNames: in.PolicyNames}
	if in.Status != "" {
		status, err := domain.ParseCredentialStatus(in.Status)
		if err != nil {
			h.deps.Enc.WriteProblemError(w, r, err)
			return
		}
		input.Status = status
	}
	u, err := h.deps.IAM.CreateUser(r.Context(), input)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, newUserResponse(u))
}

func (h *userRoutes) getUser(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminGetUser, "") {
		return
	}
	u, err := h.deps.IAM.GetUser(r.Context(), chi.URLParam(r, "user"))
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newUserResponse(u))
}

func (h *userRoutes) updateUser(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminUpdateUser, "") {
		return
	}
	accessKey := chi.URLParam(r, "user")
	var in UpdateUserRequest
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	ctx := r.Context()
	if in.SecretKey != nil {
		if err := h.deps.IAM.SetUserSecret(ctx, accessKey, *in.SecretKey); err != nil {
			h.deps.Enc.WriteProblemError(w, r, err)
			return
		}
	}
	if in.Status != nil {
		status, err := domain.ParseCredentialStatus(*in.Status)
		if err != nil {
			h.deps.Enc.WriteProblemError(w, r, err)
			return
		}
		if err := h.deps.IAM.SetUserStatus(ctx, accessKey, status); err != nil {
			h.deps.Enc.WriteProblemError(w, r, err)
			return
		}
	}
	if in.PolicyNames != nil {
		if err := h.deps.IAM.SetUserPolicy(ctx, accessKey, *in.PolicyNames); err != nil {
			h.deps.Enc.WriteProblemError(w, r, err)
			return
		}
	}
	u, err := h.deps.IAM.GetUser(ctx, accessKey)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newUserResponse(u))
}

func (h *userRoutes) deleteUser(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminDeleteUser, "") {
		return
	}
	if err := h.deps.IAM.DeleteUser(r.Context(), chi.URLParam(r, "user")); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GroupResponse is an IAM group. It mirrors the Group DTO of the web console.
type GroupResponse struct {
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Members     []string  `json:"members"`
	PolicyNames []string  `json:"policyNames"`
	CreatedAt   time.Time `json:"createdAt"`
}

func newGroupResponse(g *iam.Group) GroupResponse {
	return GroupResponse{
		Name:        g.Name,
		Status:      string(g.Status),
		Members:     nonNil(g.Members),
		PolicyNames: nonNil(g.PolicyNames),
		CreatedAt:   g.CreatedAt,
	}
}

// CreateGroupRequest is the JSON body of POST /groups.
type CreateGroupRequest struct {
	Name        string   `json:"name"`
	Members     []string `json:"members,omitempty"`
	PolicyNames []string `json:"policyNames,omitempty"`
}

// UpdateGroupRequest is the JSON body of PUT /groups/{group}; absent
// fields are left unchanged.
type UpdateGroupRequest struct {
	Status        *string   `json:"status,omitempty"`
	PolicyNames   *[]string `json:"policyNames,omitempty"`
	AddMembers    []string  `json:"addMembers,omitempty"`
	RemoveMembers []string  `json:"removeMembers,omitempty"`
}

func (h *userRoutes) listGroups(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminListGroups, "") {
		return
	}
	groups, err := h.deps.IAM.ListGroups(r.Context())
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	out := make([]GroupResponse, 0, len(groups))
	for i := range groups {
		out = append(out, newGroupResponse(&groups[i]))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *userRoutes) createGroup(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminCreateGroup, "") {
		return
	}
	var in CreateGroupRequest
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	g, err := h.deps.IAM.CreateGroup(r.Context(), iam.CreateGroupInput{Name: in.Name, Members: in.Members, PolicyNames: in.PolicyNames})
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, newGroupResponse(g))
}

func (h *userRoutes) getGroup(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminGetGroup, "") {
		return
	}
	g, err := h.deps.IAM.GetGroup(r.Context(), chi.URLParam(r, "group"))
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newGroupResponse(g))
}

func (h *userRoutes) updateGroup(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminUpdateGroup, "") {
		return
	}
	name := chi.URLParam(r, "group")
	var in UpdateGroupRequest
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	ctx := r.Context()
	if in.Status != nil {
		status, err := domain.ParseCredentialStatus(*in.Status)
		if err != nil {
			h.deps.Enc.WriteProblemError(w, r, err)
			return
		}
		if err := h.deps.IAM.SetGroupStatus(ctx, name, status); err != nil {
			h.deps.Enc.WriteProblemError(w, r, err)
			return
		}
	}
	if in.PolicyNames != nil {
		if err := h.deps.IAM.SetGroupPolicy(ctx, name, *in.PolicyNames); err != nil {
			h.deps.Enc.WriteProblemError(w, r, err)
			return
		}
	}
	if len(in.AddMembers) > 0 {
		if err := h.deps.IAM.AddGroupMembers(ctx, name, in.AddMembers); err != nil {
			h.deps.Enc.WriteProblemError(w, r, err)
			return
		}
	}
	if len(in.RemoveMembers) > 0 {
		if err := h.deps.IAM.RemoveGroupMembers(ctx, name, in.RemoveMembers); err != nil {
			h.deps.Enc.WriteProblemError(w, r, err)
			return
		}
	}
	g, err := h.deps.IAM.GetGroup(ctx, name)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newGroupResponse(g))
}

func (h *userRoutes) deleteGroup(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminDeleteGroup, "") {
		return
	}
	if err := h.deps.IAM.DeleteGroup(r.Context(), chi.URLParam(r, "group")); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AccessKeyResponse is a service account. It mirrors the AccessKey DTO of
// the web console; SecretKey is only set in the response of the creation.
type AccessKeyResponse struct {
	AccessKey   string         `json:"accessKey"`
	SecretKey   string         `json:"secretKey,omitempty"`
	ParentUser  string         `json:"parentUser"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Status      string         `json:"status"`
	Expiration  *time.Time     `json:"expiration,omitempty"`
	Policy      *domain.Policy `json:"policy,omitempty"`
	CreatedAt   time.Time      `json:"createdAt"`
}

func newAccessKeyResponse(k *iam.AccessKey, withSecret bool) AccessKeyResponse {
	out := AccessKeyResponse{
		AccessKey:   k.AccessKey,
		ParentUser:  k.ParentUser,
		Name:        k.Name,
		Description: k.Description,
		Status:      string(k.Status),
		Policy:      k.Policy,
		CreatedAt:   k.CreatedAt,
	}
	if withSecret {
		out.SecretKey = k.SecretKey
	}
	if !k.Expiration.IsZero() {
		exp := k.Expiration
		out.Expiration = &exp
	}
	return out
}

// CreateAccessKeyRequest is the JSON body of POST /access-keys.
type CreateAccessKeyRequest struct {
	// ParentUser owns the key; it defaults to the caller and only root may
	// name another user (enforced by the IAM service).
	ParentUser string `json:"parentUser,omitempty"`
	// AccessKey and SecretKey are generated when empty.
	AccessKey   string         `json:"accessKey,omitempty"`
	SecretKey   string         `json:"secretKey,omitempty"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Expiration  *time.Time     `json:"expiration,omitempty"`
	Policy      *domain.Policy `json:"policy,omitempty"`
}

// UpdateAccessKeyRequest is the JSON body of PUT /access-keys/{key};
// absent fields are left unchanged.
type UpdateAccessKeyRequest struct {
	SecretKey   *string        `json:"secretKey,omitempty"`
	Name        *string        `json:"name,omitempty"`
	Description *string        `json:"description,omitempty"`
	Status      *string        `json:"status,omitempty"`
	Expiration  *time.Time     `json:"expiration,omitempty"`
	Policy      *domain.Policy `json:"policy,omitempty"`
	// ClearPolicy removes the inline policy.
	ClearPolicy bool `json:"clearPolicy,omitempty"`
}

func (h *userRoutes) listAccessKeys(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminListAccessKeys, "") {
		return
	}
	keys, err := h.deps.IAM.ListAccessKeys(r.Context(), *caller(r), r.URL.Query().Get("parent"))
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	out := make([]AccessKeyResponse, 0, len(keys))
	for i := range keys {
		out = append(out, newAccessKeyResponse(&keys[i], false))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *userRoutes) createAccessKey(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminCreateAccessKey, "") {
		return
	}
	var in CreateAccessKeyRequest
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	input := iam.CreateAccessKeyInput{
		Caller:      *caller(r),
		ParentUser:  in.ParentUser,
		AccessKey:   in.AccessKey,
		SecretKey:   in.SecretKey,
		Name:        in.Name,
		Description: in.Description,
		Policy:      in.Policy,
	}
	if in.Expiration != nil {
		input.Expiration = *in.Expiration
	}
	k, err := h.deps.IAM.CreateAccessKey(r.Context(), input)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, newAccessKeyResponse(k, true))
}

func (h *userRoutes) getAccessKey(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminGetAccessKey, "") {
		return
	}
	k, err := h.deps.IAM.GetAccessKey(r.Context(), *caller(r), chi.URLParam(r, "key"))
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newAccessKeyResponse(k, false))
}

func (h *userRoutes) updateAccessKey(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminUpdateAccessKey, "") {
		return
	}
	accessKey := chi.URLParam(r, "key")
	var in UpdateAccessKeyRequest
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	input := iam.UpdateAccessKeyInput{
		SecretKey:   in.SecretKey,
		Name:        in.Name,
		Description: in.Description,
		Expiration:  in.Expiration,
		Policy:      in.Policy,
		ClearPolicy: in.ClearPolicy,
	}
	if in.Status != nil {
		status, err := domain.ParseCredentialStatus(*in.Status)
		if err != nil {
			h.deps.Enc.WriteProblemError(w, r, err)
			return
		}
		input.Status = &status
	}
	k, err := h.deps.IAM.UpdateAccessKey(r.Context(), *caller(r), accessKey, input)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newAccessKeyResponse(k, false))
}

func (h *userRoutes) deleteAccessKey(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminDeleteAccessKey, "") {
		return
	}
	if err := h.deps.IAM.DeleteAccessKey(r.Context(), *caller(r), chi.URLParam(r, "key")); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
