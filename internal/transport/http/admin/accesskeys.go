package admin

import (
	"net/http"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
)

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
	UpdatedAt   time.Time      `json:"updatedAt"`
}

func newAccessKeyResponse(k *iam.AccessKey, withSecret bool) AccessKeyResponse {
	out := AccessKeyResponse{
		AccessKey:   k.AccessKey,
		ParentUser:  k.ParentUser,
		Name:        k.Name,
		Description: k.Description,
		Status:      string(k.Status),
		Expiration:  optTime(k.Expiration),
		Policy:      k.Policy,
		CreatedAt:   k.CreatedAt,
		UpdatedAt:   k.UpdatedAt,
	}
	if withSecret {
		out.SecretKey = k.SecretKey
	}
	return out
}

// AccessKeyListResponse is a page of access keys.
type AccessKeyListResponse struct {
	Items      []AccessKeyResponse `json:"items"`
	NextCursor string              `json:"nextCursor,omitempty"`
}

// CreateAccessKeyRequest is the JSON body of POST /access-keys.
type CreateAccessKeyRequest struct {
	// ParentUser owns the key; it defaults to the caller and naming
	// another user requires admin:CreateServiceAccount.
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
	// ClearExpiration removes the expiration.
	ClearExpiration bool `json:"clearExpiration,omitempty"`
}

// listAccessKeys lists the keys of ?user= (alias ?parent=), the caller's
// own keys by default.
func (h *userRoutes) listAccessKeys(w http.ResponseWriter, r *http.Request) {
	p, err := readPage(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	parent := q.Get("user")
	if parent == "" {
		parent = q.Get("parent")
	}
	keys, err := h.deps.AccessKeys.List(r.Context(), requesterOf(r), parent, p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := AccessKeyListResponse{Items: make([]AccessKeyResponse, 0, len(keys.Items)), NextCursor: keys.NextCursor}
	for i := range keys.Items {
		out.Items = append(out.Items, newAccessKeyResponse(&keys.Items[i], false))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *userRoutes) createAccessKey(w http.ResponseWriter, r *http.Request) {
	var in CreateAccessKeyRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	input := iam.CreateAccessKeyInput{
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
	k, err := h.deps.AccessKeys.Create(r.Context(), requesterOf(r), input)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, newAccessKeyResponse(k, true))
}

func (h *userRoutes) getAccessKey(w http.ResponseWriter, r *http.Request) {
	key, err := pathParam(r, "key")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	k, err := h.deps.AccessKeys.Get(r.Context(), requesterOf(r), key)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newAccessKeyResponse(k, false))
}

func (h *userRoutes) updateAccessKey(w http.ResponseWriter, r *http.Request) {
	key, err := pathParam(r, "key")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var in UpdateAccessKeyRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
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
	if in.ClearExpiration {
		input.Expiration = &time.Time{}
	}
	if in.Status != nil {
		status, err := domain.ParseCredentialStatus(*in.Status)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		input.Status = &status
	}
	k, err := h.deps.AccessKeys.Update(r.Context(), requesterOf(r), key, input)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newAccessKeyResponse(k, false))
}

func (h *userRoutes) deleteAccessKey(w http.ResponseWriter, r *http.Request) {
	key, err := pathParam(r, "key")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.AccessKeys.Delete(r.Context(), requesterOf(r), key); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
