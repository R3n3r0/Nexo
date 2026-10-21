package admin

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/pools"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// contentTypeZip is the media type of the cluster archives.
const contentTypeZip = "application/zip"

// ClusterDeps are the dependencies of the cluster routes.
type ClusterDeps struct {
	// Cluster exports and imports the state of the deployment (required).
	Cluster adminsvc.ClusterManager
	// Locks lists and releases the namespace locks (required).
	Locks lock.Manager
	// Pools runs the decommission and rebalance jobs (nil: the routes
	// answer 501, single-drive mode).
	Pools pools.Service
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// ClusterOption configures the cluster routes (none yet: the type keeps
// the constructor shape of every route group, docs/12 §12.3).
type ClusterOption func(*clusterRoutes)

type clusterRoutes struct {
	deps  ClusterDeps
	guard guard
}

var _ ClusterRoutes = (*clusterRoutes)(nil)

// NewClusterRoutes returns the cluster routes: the export and import of
// the state of the deployment and the lock diagnostics.
func NewClusterRoutes(deps ClusterDeps, opts ...ClusterOption) (ClusterRoutes, error) {
	if deps.Cluster == nil || deps.Locks == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: cluster routes require the cluster manager, the lock manager and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &clusterRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *clusterRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *clusterRoutes) routes() []route {
	return []route{
		{http.MethodGet, "/cluster/export", global(policy.ActionAdminExportCluster), h.export},
		{http.MethodPost, "/cluster/import", global(policy.ActionAdminImportCluster), h.importArchive},
		{http.MethodGet, "/locks", global(policy.ActionAdminListLocks), h.listLocks},
		{http.MethodDelete, "/locks", global(policy.ActionAdminForceUnlock), h.forceUnlock},
		{http.MethodGet, "/pools", global(policy.ActionAdminServerInfo), h.listPools},
		{http.MethodPost, "/pools/{pool}/decommission", global(policy.ActionAdminDecommission), h.poolAction(func(svc pools.Service, r *http.Request, pool int) error {
			return svc.Decommission(r.Context(), pool)
		}, http.StatusAccepted)},
		{http.MethodDelete, "/pools/{pool}/decommission", global(policy.ActionAdminDecommission), h.poolAction(func(svc pools.Service, r *http.Request, pool int) error {
			return svc.CancelDecommission(r.Context(), pool)
		}, http.StatusNoContent)},
		{http.MethodPost, "/rebalance", global(policy.ActionAdminRebalance), h.rebalanceAction(func(svc pools.Service, r *http.Request) error {
			return svc.StartRebalance(r.Context())
		}, http.StatusAccepted)},
		{http.MethodDelete, "/rebalance", global(policy.ActionAdminRebalance), h.rebalanceAction(func(svc pools.Service, r *http.Request) error {
			return svc.StopRebalance(r.Context())
		}, http.StatusNoContent)},
	}
}

func (h *clusterRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

// ClusterImportResponse is the outcome of POST /cluster/import.
type ClusterImportResponse struct {
	Mode             string   `json:"mode"`
	DryRun           bool     `json:"dryRun"`
	Users            int      `json:"users"`
	Groups           int      `json:"groups"`
	Policies         int      `json:"policies"`
	ConfigSubsystems int      `json:"configSubsystems"`
	Buckets          int      `json:"buckets"`
	BucketsCreated   []string `json:"bucketsCreated"`
	Removed          []string `json:"removed"`
}

// LockResponse is one namespace lock of GET /locks.
type LockResponse struct {
	Resource string `json:"resource"`
	UID      string `json:"uid"`
	Owner    string `json:"owner"`
	Source   string `json:"source,omitempty"`
	// Type is "write" or "read".
	Type      string    `json:"type"`
	Quorum    int       `json:"quorum"`
	Since     time.Time `json:"since"`
	Refreshed time.Time `json:"refreshed"`
	// Servers is the number of lock servers holding the lock, of Total.
	Servers int `json:"servers"`
	Total   int `json:"total"`
}

func (h *clusterRoutes) export(w http.ResponseWriter, r *http.Request) {
	data, err := h.deps.Cluster.Export(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", contentTypeZip)
	w.Header().Set("Content-Disposition", `attachment; filename="nexo-cluster.zip"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// importArchive imports the zip archive of the body; the "mode" (merge or
// replace) and "dryRun" query parameters tune it.
func (h *clusterRoutes) importArchive(w http.ResponseWriter, r *http.Request) {
	dryRun, err := queryBool(r, "dryRun")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	data, err := readBody(r, adminsvc.MaxClusterArchive)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sum, err := h.deps.Cluster.Import(r.Context(), data, adminsvc.ImportOptions{
		Mode: adminsvc.ImportMode(r.URL.Query().Get("mode")), DryRun: dryRun,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, ClusterImportResponse{
		Mode: string(sum.Mode), DryRun: sum.DryRun, Users: sum.Users, Groups: sum.Groups, Policies: sum.Policies,
		ConfigSubsystems: sum.ConfigSubsystems, Buckets: sum.Buckets,
		BucketsCreated: nonNil(sum.BucketsCreated), Removed: nonNil(sum.Removed),
	})
}

func (h *clusterRoutes) listLocks(w http.ResponseWriter, r *http.Request) {
	all, err := h.deps.Locks.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]LockResponse, 0, len(all))
	for _, l := range all {
		typ := "read"
		if l.Writer {
			typ = "write"
		}
		out = append(out, LockResponse{
			Resource: l.Resource, UID: l.UID, Owner: l.Owner, Source: l.Source, Type: typ, Quorum: l.Quorum,
			Since: l.Since.UTC(), Refreshed: l.Refreshed.UTC(), Servers: l.Servers, Total: l.Total,
		})
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

// forceUnlock releases the resources named by the "resource" query
// parameters ("bucket/key"), whoever holds them.
func (h *clusterRoutes) forceUnlock(w http.ResponseWriter, r *http.Request) {
	resources := r.URL.Query()["resource"]
	if len(resources) == 0 {
		h.fail(w, r, fmt.Errorf("%w: at least one resource is required", domain.ErrInvalidArgument))
		return
	}
	for _, res := range resources {
		if res == "" {
			h.fail(w, r, fmt.Errorf("%w: empty resource", domain.ErrInvalidArgument))
			return
		}
	}
	if err := h.deps.Locks.ForceUnlock(r.Context(), resources...); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
