package admin

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/pools"
)

// The pool management routes (docs/03 §3.8, S-STO-09, S-STO-10):
// GET /pools, POST/DELETE /pools/{pool}/decommission and POST/DELETE
// /rebalance. {pool} is the 1-based position of the pool in the command
// line. They answer 501 in single-drive mode (no pools to manage).

// PoolJobResponse is the progress of a decommission or of the rebalance.
type PoolJobResponse struct {
	State      string     `json:"state"`
	StartedAt  time.Time  `json:"startedAt"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Pass       int        `json:"pass"`
	// Pool is the source pool of a rebalance (1-based).
	Pool           int    `json:"pool,omitempty"`
	Bucket         string `json:"bucket,omitempty"`
	Object         string `json:"object,omitempty"`
	Objects        int64  `json:"objects"`
	Versions       int64  `json:"versions"`
	Bytes          int64  `json:"bytes"`
	Failed         int64  `json:"failed"`
	PendingUploads int    `json:"pendingUploads"`
	Error          string `json:"error,omitempty"`
}

// PoolStatusResponse is one pool of GET /pools.
type PoolStatusResponse struct {
	// Index is the 1-based position of the pool in the command line.
	Index        int              `json:"index"`
	ID           string           `json:"id"`
	Total        uint64           `json:"total"`
	Free         uint64           `json:"free"`
	Draining     bool             `json:"draining"`
	Decommission *PoolJobResponse `json:"decommission,omitempty"`
}

// PoolsResponse is the body of GET /pools.
type PoolsResponse struct {
	Pools     []PoolStatusResponse `json:"pools"`
	Rebalance *PoolJobResponse     `json:"rebalance,omitempty"`
}

func newPoolJobResponse(j *pools.Job, rebalance bool) *PoolJobResponse {
	if j == nil {
		return nil
	}
	out := &PoolJobResponse{
		State: string(j.State), StartedAt: j.StartedAt, UpdatedAt: optTime(j.UpdatedAt), FinishedAt: optTime(j.FinishedAt),
		Pass: j.Pass, Bucket: j.Bucket, Object: j.Object, Objects: j.Objects, Versions: j.Versions, Bytes: j.Bytes,
		Failed: j.Failed, PendingUploads: j.PendingUploads, Error: j.Error,
	}
	if rebalance {
		out.Pool = j.Pool + 1
	}
	return out
}

func (h *clusterRoutes) poolsService(w http.ResponseWriter, r *http.Request) (pools.Service, bool) {
	if h.deps.Pools == nil {
		h.fail(w, r, fmt.Errorf("%w: pool management needs an erasure-coded deployment", domain.ErrNotImplemented))
		return nil, false
	}
	return h.deps.Pools, true
}

func (h *clusterRoutes) listPools(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.poolsService(w, r)
	if !ok {
		return
	}
	st, err := svc.Status(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := PoolsResponse{Pools: make([]PoolStatusResponse, 0, len(st.Pools)), Rebalance: newPoolJobResponse(st.Rebalance, true)}
	for _, p := range st.Pools {
		out.Pools = append(out.Pools, PoolStatusResponse{
			Index: p.Index + 1, ID: p.ID, Total: p.Total, Free: p.Free, Draining: p.Draining,
			Decommission: newPoolJobResponse(p.Decommission, false),
		})
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

// poolParam returns the 0-based pool of the {pool} path parameter.
func poolParam(r *http.Request) (int, error) {
	n, err := strconv.Atoi(chi.URLParam(r, "pool"))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%w: the pool is its 1-based position in the command line", domain.ErrInvalidArgument)
	}
	return n - 1, nil
}

func (h *clusterRoutes) poolAction(action func(svc pools.Service, r *http.Request, pool int) error, status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := h.poolsService(w, r)
		if !ok {
			return
		}
		pool, err := poolParam(r)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if err := action(svc, r, pool); err != nil {
			h.fail(w, r, err)
			return
		}
		w.WriteHeader(status)
	}
}

func (h *clusterRoutes) rebalanceAction(action func(svc pools.Service, r *http.Request) error, status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := h.poolsService(w, r)
		if !ok {
			return
		}
		if err := action(svc, r); err != nil {
			h.fail(w, r, err)
			return
		}
		w.WriteHeader(status)
	}
}
