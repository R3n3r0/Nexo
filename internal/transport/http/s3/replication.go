package s3

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/replication"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// ReplicationRoutes exposes Put/Get/DeleteBucketReplication (S-BKT-12).
// The destinations of the rules are the ARNs of the replication targets of
// the bucket (Admin API /buckets/{bucket}/replication/targets).
type ReplicationRoutes interface {
	Routes
	Group
}

// ReplicationDeps are the dependencies of the replication routes. Without
// Buckets, Replication or Policy the operations answer NotImplemented.
type ReplicationDeps struct {
	Buckets     bucket.Service
	Replication replication.Service
	Policy      policy.Service
	Enc         httpx.Codec
	Clock       clock.Clock
	Logger      *slog.Logger
}

type replicationRoutes struct {
	deps  ReplicationDeps
	authz authorizer
}

var _ ReplicationRoutes = (*replicationRoutes)(nil)

// NewReplicationRoutes returns the replication route group.
func NewReplicationRoutes(deps ReplicationDeps) ReplicationRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &replicationRoutes{deps: deps, authz: authorizer{policy: deps.Policy, enc: deps.Enc, clock: deps.Clock}}
}

func (h *replicationRoutes) available() bool {
	return h.deps.Buckets != nil && h.deps.Replication != nil && h.deps.Policy != nil
}

// Mount implements Group.
func (h *replicationRoutes) Mount(d Dispatcher) {
	get, put, del := http.MethodGet, http.MethodPut, http.MethodDelete
	mountAll(d, h.deps.Enc, map[Operation]http.HandlerFunc{
		{Name: "GetBucketReplication", Method: get, Scope: ScopeBucket, Query: "replication"}:    h.get,
		{Name: "PutBucketReplication", Method: put, Scope: ScopeBucket, Query: "replication"}:    h.put,
		{Name: "DeleteBucketReplication", Method: del, Scope: ScopeBucket, Query: "replication"}: h.delete,
	}, h.available())
}

// Register implements Routes.
func (h *replicationRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }

func (h *replicationRoutes) get(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	doc, err := h.deps.Buckets.GetReplication(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	writeRawXML(w, doc)
}

func (h *replicationRoutes) put(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	body, err := readConfigBody(r)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if _, err := h.deps.Buckets.Get(r.Context(), name); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	doc, err := h.deps.Replication.ValidateConfig(r.Context(), name, body)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if err := h.deps.Buckets.SetReplication(r.Context(), name, doc); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *replicationRoutes) delete(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	if err := h.deps.Buckets.DeleteReplication(r.Context(), name); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
