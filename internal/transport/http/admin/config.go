package admin

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// ConfigDeps are the dependencies of the config routes.
type ConfigDeps struct {
	// Config validates and stores the dynamic configuration (required).
	Config adminsvc.ConfigManager
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// ConfigOption configures the config routes (none yet: the type keeps the
// constructor shape of every route group, docs/12 §12.3).
type ConfigOption func(*configRoutes)

type configRoutes struct {
	deps  ConfigDeps
	guard guard
}

var _ ConfigRoutes = (*configRoutes)(nil)

// NewConfigRoutes returns the dynamic configuration routes.
func NewConfigRoutes(deps ConfigDeps, opts ...ConfigOption) (ConfigRoutes, error) {
	if deps.Config == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: config routes require the config manager and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &configRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *configRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *configRoutes) routes() []route {
	// Like MinIO, reading the configuration needs admin:ConfigUpdate: it
	// holds the secrets of the notification targets.
	cfg := global(policy.ActionAdminConfigWrite)
	return []route{
		{http.MethodGet, "/config", cfg, h.list},
		{http.MethodPut, "/config", cfg, h.putAll},
		{http.MethodGet, "/config/schema", cfg, h.schema},
		{http.MethodGet, "/config/export", cfg, h.export},
		{http.MethodPost, "/config/import", cfg, h.importAll},
		{http.MethodGet, "/config/{subsys}", cfg, h.get},
		{http.MethodPut, "/config/{subsys}", cfg, h.put},
		{http.MethodDelete, "/config/{subsys}", cfg, h.remove},
	}
}

func (h *configRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

// SubsystemResponse is the configuration of one subsystem. Sensitive
// values are masked (adminsvc.SensitiveMask); sending the mask back keeps
// the stored secret.
type SubsystemResponse struct {
	Name     string    `json:"name"`
	Settings config.KV `json:"settings"`
}

// SubsystemRequest is the JSON body of PUT /config/{subsys}.
type SubsystemRequest struct {
	Settings config.KV `json:"settings"`
}

// ConfigKeyResponse describes a configuration key.
type ConfigKeyResponse struct {
	Name string `json:"name"`
	// Type is string, bool, int, duration, url, enum or list.
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Default     string   `json:"default,omitempty"`
	Values      []string `json:"values,omitempty"`
	Sensitive   bool     `json:"sensitive"`
	// Applied reports a key the server applies (on the next request); the
	// others are stored but have no effect yet.
	Applied bool `json:"applied"`
}

// ConfigSchemaResponse describes a configuration subsystem.
type ConfigSchemaResponse struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Keys        []ConfigKeyResponse `json:"keys"`
}

func newSubsystemResponse(name string, kv config.KV) SubsystemResponse {
	if kv == nil {
		kv = config.KV{}
	}
	return SubsystemResponse{Name: name, Settings: kv}
}

func (h *configRoutes) list(w http.ResponseWriter, r *http.Request) {
	all, err := h.deps.Config.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]SubsystemResponse, 0, len(all))
	for _, s := range all {
		out = append(out, newSubsystemResponse(s.Name, s.Settings))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *configRoutes) schema(w http.ResponseWriter, r *http.Request) {
	schema := h.deps.Config.Schema(r.Context())
	out := make([]ConfigSchemaResponse, 0, len(schema))
	for _, s := range schema {
		sr := ConfigSchemaResponse{Name: s.Name, Description: s.Description, Keys: make([]ConfigKeyResponse, 0, len(s.Keys))}
		for _, k := range s.Keys {
			sr.Keys = append(sr.Keys, ConfigKeyResponse{Name: k.Name, Type: string(k.Type), Description: k.Description, Default: k.Default, Values: k.Values, Sensitive: k.Sensitive, Applied: k.Applied})
		}
		out = append(out, sr)
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

// putAll merges the settings of several subsystems: the body maps a
// subsystem name to its settings.
func (h *configRoutes) putAll(w http.ResponseWriter, r *http.Request) {
	var in map[string]config.KV
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Config.SetAll(r.Context(), in); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *configRoutes) get(w http.ResponseWriter, r *http.Request) {
	name, err := pathParam(r, "subsys")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	kv, err := h.deps.Config.Get(r.Context(), name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newSubsystemResponse(name, kv))
}

func (h *configRoutes) put(w http.ResponseWriter, r *http.Request) {
	name, err := pathParam(r, "subsys")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var in SubsystemRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	kv, err := h.deps.Config.Set(r.Context(), name, in.Settings)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newSubsystemResponse(name, kv))
}

// remove deletes a subsystem, or only the keys listed in the "keys" query
// parameter (comma separated).
func (h *configRoutes) remove(w http.ResponseWriter, r *http.Request) {
	name, err := pathParam(r, "subsys")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Config.Delete(r.Context(), name, trimList(r.URL.Query().Get("keys"))); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *configRoutes) export(w http.ResponseWriter, r *http.Request) {
	data, err := h.deps.Config.Export(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", httpx.ContentTypeJSON)
	w.Header().Set("Content-Disposition", `attachment; filename="nexo-config.json"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *configRoutes) importAll(w http.ResponseWriter, r *http.Request) {
	data, err := readBody(r, maxJSONBody)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Config.Import(r.Context(), data); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
