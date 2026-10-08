package s3

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/health"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// HealthDeps are the dependencies of the health routes.
type HealthDeps struct {
	Health health.Service
	Enc    httpx.Encoder
}

type healthRoutes struct {
	deps HealthDeps
}

var _ HealthRoutes = (*healthRoutes)(nil)

// NewHealthRoutes returns the health route group.
func NewHealthRoutes(deps HealthDeps) HealthRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &healthRoutes{deps: deps}
}

// Register implements Routes.
func (h *healthRoutes) Register(r chi.Router) {
	r.Get(domain.HealthLivePath, h.live)
	r.Head(domain.HealthLivePath, h.live)
	r.Get(domain.HealthReadyPath, h.ready)
	r.Head(domain.HealthReadyPath, h.ready)
	r.Get(domain.HealthClusterPath, h.cluster)
	r.Head(domain.HealthClusterPath, h.cluster)
}

func (h *healthRoutes) live(w http.ResponseWriter, r *http.Request) {
	h.write(w, r, h.deps.Health.Live(r.Context()))
}

func (h *healthRoutes) ready(w http.ResponseWriter, r *http.Request) {
	h.write(w, r, h.deps.Health.Ready(r.Context()))
}

func (h *healthRoutes) cluster(w http.ResponseWriter, r *http.Request) {
	st := h.deps.Health.Cluster(r.Context())
	status := http.StatusOK
	if !st.OK {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	h.deps.Enc.WriteJSON(w, status, st)
}

func (h *healthRoutes) write(w http.ResponseWriter, r *http.Request, st health.Status) {
	status := http.StatusOK
	if !st.OK {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	h.deps.Enc.WriteJSON(w, status, st)
}
