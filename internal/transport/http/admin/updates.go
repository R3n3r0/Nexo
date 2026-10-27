package admin

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/update"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// UpdateRoutes expose the update of the binary of every node and the
// coordinated restart and stop of the deployment (S-OPS-09, C-ADM-09):
// /service/update and /service/cluster/{restart,stop}.
type UpdateRoutes interface{ Routes }

// UpdateDeps are the dependencies of the update routes.
type UpdateDeps struct {
	// Updates orchestrates the jobs (required).
	Updates update.Service
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

type updateRoutes struct {
	deps  UpdateDeps
	guard guard
}

var _ UpdateRoutes = (*updateRoutes)(nil)

// NewUpdateRoutes returns the update routes.
func NewUpdateRoutes(deps UpdateDeps) (UpdateRoutes, error) {
	if deps.Updates == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: update routes require the update service and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &updateRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}, nil
}

// Register implements Routes.
func (h *updateRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *updateRoutes) routes() []route {
	return []route{
		{http.MethodGet, "/service/update", global(policy.ActionAdminServerInfo), h.job},
		{http.MethodPost, "/service/update", global(policy.ActionAdminServerUpdate), h.update},
		{http.MethodPost, "/service/cluster/restart", global(policy.ActionAdminServiceRestart), h.restart},
		{http.MethodPost, "/service/cluster/stop", global(policy.ActionAdminServiceStop), h.stop},
	}
}

// UpdateBody is the body of POST /service/update.
type UpdateBody struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	// Mode is "rolling" (default) or "all".
	Mode string `json:"mode,omitempty"`
}

// RestartBody is the body of POST /service/cluster/restart.
type RestartBody struct {
	Mode string `json:"mode,omitempty"`
}

func (h *updateRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

func (h *updateRoutes) job(w http.ResponseWriter, r *http.Request) {
	j, err := h.deps.Updates.Job(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, j)
}

func (h *updateRoutes) update(w http.ResponseWriter, r *http.Request) {
	var in UpdateBody
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	j, err := h.deps.Updates.Update(r.Context(), update.UpdateInput{URL: in.URL, SHA256: in.SHA256, Mode: update.Mode(in.Mode)})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusAccepted, j)
}

func (h *updateRoutes) restart(w http.ResponseWriter, r *http.Request) {
	var in RestartBody
	if err := decodeJSON(h.deps.Enc, r, &in, true); err != nil {
		h.fail(w, r, err)
		return
	}
	j, err := h.deps.Updates.Restart(r.Context(), update.RestartInput{Mode: update.Mode(in.Mode)})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusAccepted, j)
}

func (h *updateRoutes) stop(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Updates.StopAll(r.Context()); err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusAccepted, ServiceActionResponse{Action: "stop"})
}
