package admin

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/iam"
)

// GroupResponse is an IAM group. It mirrors the Group DTO of the web console.
type GroupResponse struct {
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Members     []string  `json:"members"`
	PolicyNames []string  `json:"policyNames"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func newGroupResponse(g *iam.Group) GroupResponse {
	return GroupResponse{
		Name:        g.Name,
		Status:      string(g.Status),
		Members:     nonNil(g.Members),
		PolicyNames: nonNil(g.PolicyNames),
		CreatedAt:   g.CreatedAt,
		UpdatedAt:   g.UpdatedAt,
	}
}

// GroupListResponse is a page of groups.
type GroupListResponse struct {
	Items      []GroupResponse `json:"items"`
	NextCursor string          `json:"nextCursor,omitempty"`
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
	p, err := readPage(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	groups, err := h.deps.IAM.ListGroups(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items, next := paginate(groups, func(g iam.Group) string { return g.Name }, p)
	out := GroupListResponse{Items: make([]GroupResponse, 0, len(items)), NextCursor: next}
	for i := range items {
		out.Items = append(out.Items, newGroupResponse(&items[i]))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *userRoutes) createGroup(w http.ResponseWriter, r *http.Request) {
	var in CreateGroupRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	g, err := h.deps.Identities.CreateGroup(r.Context(), adminsvc.CreateGroupInput{
		Requester: requesterOf(r), Name: in.Name, Members: in.Members, PolicyNames: in.PolicyNames,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, newGroupResponse(g))
}

func (h *userRoutes) getGroup(w http.ResponseWriter, r *http.Request) {
	g, err := h.deps.IAM.GetGroup(r.Context(), chi.URLParam(r, "group"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newGroupResponse(g))
}

func (h *userRoutes) updateGroup(w http.ResponseWriter, r *http.Request) {
	var in UpdateGroupRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	upd := adminsvc.UpdateGroupInput{
		Requester: requesterOf(r), Name: chi.URLParam(r, "group"), PolicyNames: in.PolicyNames,
		AddMembers: in.AddMembers, RemoveMembers: in.RemoveMembers,
	}
	if in.Status != nil {
		status, err := domain.ParseCredentialStatus(*in.Status)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		upd.Status = &status
	}
	g, err := h.deps.Identities.UpdateGroup(r.Context(), upd)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newGroupResponse(g))
}

func (h *userRoutes) deleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.IAM.DeleteGroup(r.Context(), chi.URLParam(r, "group")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
