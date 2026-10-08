package admin

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// ConfigDeps are the dependencies of the config routes.
type ConfigDeps struct {
	Config config.Service
	// Policy authorizes the admin:* actions; nil leaves the routes open (tests).
	Policy policy.Service
	Enc    httpx.Codec
}

type configRoutes struct {
	deps  ConfigDeps
	authz authorizer
}

var _ ConfigRoutes = (*configRoutes)(nil)

// NewConfigRoutes returns the dynamic configuration routes.
func NewConfigRoutes(deps ConfigDeps) (ConfigRoutes, error) {
	if deps.Config == nil {
		return nil, fmt.Errorf("%w: config routes require the config service", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &configRoutes{deps: deps, authz: authorizer{policy: deps.Policy, enc: deps.Enc}}, nil
}

// Register implements Routes.
func (h *configRoutes) Register(r chi.Router) {
	r.Get("/config", h.list)
	r.Put("/config", h.putAll)
	r.Get("/config/export", h.export)
	r.Post("/config/import", h.importAll)
	r.Get("/config/{subsys}", h.get)
	r.Put("/config/{subsys}", h.put)
	r.Delete("/config/{subsys}", h.remove)
}

// SubsystemResponse is the configuration of one subsystem.
type SubsystemResponse struct {
	Name     string    `json:"name"`
	Settings config.KV `json:"settings"`
}

// SubsystemRequest is the JSON body of PUT /config/{subsys}.
type SubsystemRequest struct {
	Settings config.KV `json:"settings"`
}

func newSubsystemResponse(name string, kv config.KV) SubsystemResponse {
	if kv == nil {
		kv = config.KV{}
	}
	return SubsystemResponse{Name: name, Settings: kv}
}

func (h *configRoutes) list(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminConfigRead, "") {
		return
	}
	ctx := r.Context()
	names, err := h.deps.Config.List(ctx)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	sort.Strings(names)
	out := make([]SubsystemResponse, 0, len(names))
	for _, name := range names {
		kv, err := h.deps.Config.Get(ctx, name)
		if err != nil {
			h.deps.Enc.WriteProblemError(w, r, err)
			return
		}
		out = append(out, newSubsystemResponse(name, kv))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

// putAll merges the settings of several subsystems: the body maps a
// subsystem name to its settings.
func (h *configRoutes) putAll(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminConfigWrite, "") {
		return
	}
	var in map[string]config.KV
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	ctx := r.Context()
	for name, kv := range in {
		if err := h.deps.Config.Set(ctx, name, kv); err != nil {
			h.deps.Enc.WriteProblemError(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *configRoutes) get(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminConfigRead, "") {
		return
	}
	name := chi.URLParam(r, "subsys")
	kv, err := h.deps.Config.Get(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newSubsystemResponse(name, kv))
}

func (h *configRoutes) put(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminConfigWrite, "") {
		return
	}
	name := chi.URLParam(r, "subsys")
	var in SubsystemRequest
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	ctx := r.Context()
	if err := h.deps.Config.Set(ctx, name, in.Settings); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	kv, err := h.deps.Config.Get(ctx, name)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newSubsystemResponse(name, kv))
}

// remove deletes a subsystem, or only the keys listed in the "keys" query
// parameter (comma separated).
func (h *configRoutes) remove(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminConfigWrite, "") {
		return
	}
	var keys []string
	if v := r.URL.Query().Get("keys"); v != "" {
		for _, k := range strings.Split(v, ",") {
			if k = strings.TrimSpace(k); k != "" {
				keys = append(keys, k)
			}
		}
	}
	if err := h.deps.Config.Delete(r.Context(), chi.URLParam(r, "subsys"), keys...); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *configRoutes) export(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminConfigRead, "") {
		return
	}
	data, err := h.deps.Config.Export(r.Context())
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", httpx.ContentTypeJSON)
	w.Header().Set("Content-Disposition", `attachment; filename="nexo-config.json"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *configRoutes) importAll(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminConfigWrite, "") {
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, fmt.Errorf("%w: read body: %v", domain.ErrInvalidArgument, err))
		return
	}
	if len(data) > maxJSONBody {
		badRequest(h.deps.Enc, w, r, fmt.Sprintf("body exceeds %d bytes", maxJSONBody))
		return
	}
	if err := h.deps.Config.Import(r.Context(), data); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
