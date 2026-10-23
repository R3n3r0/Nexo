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

// PoolPlanBody is the body of POST /pools/plan.
type PoolPlanBody struct {
	// Add are the arguments of the new pools, one with ellipses per pool.
	Add []string `json:"add,omitempty"`
	// Remove are the decommissioned pools to remove (1-based).
	Remove []int `json:"remove,omitempty"`
}

// PlannedPoolResponse is a pool of the planned topology.
type PlannedPoolResponse struct {
	// Index is the 1-based position in the planned command line; Current
	// the position today, absent for a new pool.
	Index        int      `json:"index"`
	Current      int      `json:"current,omitempty"`
	Arg          string   `json:"arg"`
	Nodes        []string `json:"nodes"`
	Drives       int      `json:"drives"`
	Sets         int      `json:"sets"`
	DrivesPerSet int      `json:"drivesPerSet"`
	Parity       int      `json:"parity"`
}

// PoolPlanResponse is the body of the answer to POST /pools/plan.
type PoolPlanResponse struct {
	Mode string `json:"mode"`
	// Args are the endpoint arguments of every node, in order.
	Args     []string              `json:"args"`
	Pools    []PlannedPoolResponse `json:"pools"`
	Removed  []string              `json:"removed"`
	Nodes    []string              `json:"nodes"`
	NewNodes []string              `json:"newNodes"`
}

func (h *clusterRoutes) planPools(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.poolsService(w, r)
	if !ok {
		return
	}
	var in PoolPlanBody
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	remove := make([]int, 0, len(in.Remove))
	for _, n := range in.Remove {
		if n < 1 {
			h.fail(w, r, fmt.Errorf("%w: a pool is its 1-based position in the command line", domain.ErrInvalidArgument))
			return
		}
		remove = append(remove, n-1)
	}
	plan, err := svc.Plan(r.Context(), pools.PlanInput{Add: in.Add, Remove: remove})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := PoolPlanResponse{Mode: plan.Mode.String(), Args: plan.Args, Pools: make([]PlannedPoolResponse, 0, len(plan.Pools)),
		Removed: nonNil(plan.Removed), Nodes: nonNil(plan.Nodes), NewNodes: nonNil(plan.NewNodes)}
	for _, p := range plan.Pools {
		out.Pools = append(out.Pools, PlannedPoolResponse{Index: p.Index + 1, Current: p.Current + 1, Arg: p.Arg, Nodes: p.Nodes,
			Drives: p.Drives, Sets: p.Sets, DrivesPerSet: p.DrivesPerSet, Parity: p.Parity})
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}
