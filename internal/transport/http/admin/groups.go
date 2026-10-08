package admin

import (
	"net/http"
	"time"

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
	groups, err := h.deps.Identities.ListGroups(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := GroupListResponse{Items: make([]GroupResponse, 0, len(groups.Items)), NextCursor: groups.NextCursor}
	for i := range groups.Items {
		out.Items = append(out.Items, newGroupResponse(&groups.Items[i]))
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
	name, err := pathParam(r, "group")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	g, err := h.deps.IAM.GetGroup(r.Context(), name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newGroupResponse(g))
}

func (h *userRoutes) updateGroup(w http.ResponseWriter, r *http.Request) {
	name, err := pathParam(r, "group")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var in UpdateGroupRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	upd := adminsvc.UpdateGroupInput{
		Requester: requesterOf(r), Name: name, PolicyNames: in.PolicyNames,
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
	name, err := pathParam(r, "group")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.IAM.DeleteGroup(r.Context(), name); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
