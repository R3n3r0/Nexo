package admin

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/heal"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// InfoDeps are the dependencies of the info routes.
type InfoDeps struct {
	Admin adminsvc.Service
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// InfoOption configures the info routes (none yet: the type keeps the
// constructor shape of every route group, docs/12 §12.3).
type InfoOption func(*infoRoutes)

type infoRoutes struct {
	deps  InfoDeps
	guard guard
}

var _ InfoRoutes = (*infoRoutes)(nil)

// NewInfoRoutes returns the server information, data usage, healing and
// service control routes.
func NewInfoRoutes(deps InfoDeps, opts ...InfoOption) (InfoRoutes, error) {
	if deps.Admin == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: info routes require the admin service and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &infoRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *infoRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *infoRoutes) routes() []route {
	ni := notImplemented(h.deps.Enc)
	return []route{
		{http.MethodGet, "/info", global(policy.ActionAdminServerInfo), h.info},
		{http.MethodGet, "/usage", global(policy.ActionAdminDataUsage), h.usage},
		{http.MethodPost, "/heal", global(policy.ActionAdminHeal), h.heal},
		{http.MethodGet, "/heal", global(policy.ActionAdminHeal), h.healList},
		{http.MethodGet, "/heal/{id}", global(policy.ActionAdminHeal), h.healStatus},
		{http.MethodPost, "/service/restart", global(policy.ActionAdminServiceRestart), h.serviceAction(adminsvc.ActionRestart)},
		{http.MethodPost, "/service/stop", global(policy.ActionAdminServiceStop), h.serviceAction(adminsvc.ActionStop)},
		// Live streams (SSE) arrive with the observability milestone.
		{http.MethodGet, "/trace", global(policy.ActionAdminServerTrace), ni},
		{http.MethodGet, "/logs", global(policy.ActionAdminConsoleLog), ni},
	}
}

// ServerInfoResponse is the JSON body of GET /info. It mirrors the
// ServerInfo DTO of the web console (web/src/services/types.ts).
type ServerInfoResponse struct {
	Version      string         `json:"version"`
	Commit       string         `json:"commit"`
	BuildDate    string         `json:"buildDate"`
	Mode         string         `json:"mode"`
	Region       string         `json:"region"`
	DeploymentID string         `json:"deploymentId"`
	StartedAt    time.Time      `json:"startedAt"`
	UptimeSec    int64          `json:"uptimeSeconds"`
	Buckets      uint64         `json:"buckets"`
	Objects      uint64         `json:"objects"`
	Usage        uint64         `json:"usage"`
	Pools        []PoolResponse `json:"pools"`
	Nodes        []NodeResponse `json:"nodes"`
}

// PoolResponse is an erasure pool in ServerInfoResponse.
type PoolResponse struct {
	Index        int `json:"index"`
	Sets         int `json:"sets"`
	DrivesPerSet int `json:"drivesPerSet"`
	Parity       int `json:"parity"`
}

// NodeResponse is a node in ServerInfoResponse.
type NodeResponse struct {
	Endpoint  string         `json:"endpoint"`
	State     string         `json:"state"`
	Version   string         `json:"version"`
	Commit    string         `json:"commit"`
	UptimeSec int64          `json:"uptimeSeconds"`
	Disks     []DiskResponse `json:"disks"`
}

// DiskResponse is a drive in NodeResponse.
type DiskResponse struct {
	Endpoint string `json:"endpoint"`
	Path     string `json:"path"`
	ID       string `json:"id"`
	PoolIdx  int    `json:"poolIdx"`
	SetIdx   int    `json:"setIdx"`
	DiskIdx  int    `json:"diskIdx"`
	State    string `json:"state"`
	Total    uint64 `json:"total"`
	Free     uint64 `json:"free"`
	Used     uint64 `json:"used"`
	Healing  bool   `json:"healing"`
	// HealingProgress is the progress of the drive healing, when healing.
	HealingProgress *HealingProgressResponse `json:"healingProgress,omitempty"`
	Error           string                   `json:"error,omitempty"`
}

// HealingProgressResponse is the progress of the healing of a drive in
// DiskResponse.
type HealingProgressResponse struct {
	Started       time.Time `json:"started"`
	LastUpdate    time.Time `json:"lastUpdate"`
	Bucket        string    `json:"bucket,omitempty"`
	Object        string    `json:"object,omitempty"`
	HealedBuckets int       `json:"healedBuckets"`
	ObjectsHealed uint64    `json:"objectsHealed"`
	ObjectsFailed uint64    `json:"objectsFailed"`
	BytesDone     uint64    `json:"bytesDone"`
	BytesFailed   uint64    `json:"bytesFailed"`
}

func newHealingProgressResponse(p *domain.HealingProgress) *HealingProgressResponse {
	if p == nil {
		return nil
	}
	return &HealingProgressResponse{
		Started: p.Started, LastUpdate: p.LastUpdate, Bucket: p.Bucket, Object: p.Object, HealedBuckets: p.HealedBuckets,
		ObjectsHealed: p.ObjectsHealed, ObjectsFailed: p.ObjectsFailed, BytesDone: p.BytesDone, BytesFailed: p.BytesFailed,
	}
}

func newServerInfoResponse(info *domain.ServerInfo) ServerInfoResponse {
	resp := ServerInfoResponse{
		Version:      info.Version,
		Commit:       info.Commit,
		BuildDate:    info.BuildDate,
		Mode:         info.Mode.String(),
		Region:       info.Region,
		DeploymentID: info.DeploymentID,
		StartedAt:    info.StartedAt,
		UptimeSec:    int64(info.Uptime.Seconds()),
		Buckets:      info.Buckets,
		Objects:      info.Objects,
		Usage:        info.Usage,
		Pools:        make([]PoolResponse, 0, len(info.Pools)),
		Nodes:        make([]NodeResponse, 0, len(info.Nodes)),
	}
	for _, p := range info.Pools {
		resp.Pools = append(resp.Pools, PoolResponse{Index: p.Index, Sets: p.Sets, DrivesPerSet: p.DrivesPerSet, Parity: p.Parity})
	}
	for _, n := range info.Nodes {
		node := NodeResponse{
			Endpoint:  n.Endpoint,
			State:     n.State,
			Version:   n.Version,
			Commit:    n.Commit,
			UptimeSec: int64(n.Uptime.Seconds()),
			Disks:     make([]DiskResponse, 0, len(n.Disks)),
		}
		for _, d := range n.Disks {
			node.Disks = append(node.Disks, DiskResponse{
				Endpoint: d.Endpoint, Path: d.Path, ID: d.ID,
				PoolIdx: d.PoolIdx, SetIdx: d.SetIdx, DiskIdx: d.DiskIdx,
				State: d.State, Total: d.Total, Free: d.Free, Used: d.Used,
				Healing: d.Healing, HealingProgress: newHealingProgressResponse(d.HealingProgress), Error: d.Error,
			})
		}
		resp.Nodes = append(resp.Nodes, node)
	}
	return resp
}

func (h *infoRoutes) info(w http.ResponseWriter, r *http.Request) {
	info, err := h.deps.Admin.ServerInfo(r.Context())
	if err != nil {
		writeError(h.deps.Enc, w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newServerInfoResponse(info))
}

func (h *infoRoutes) usage(w http.ResponseWriter, r *http.Request) {
	du, err := h.deps.Admin.DataUsage(r.Context())
	if err != nil {
		writeError(h.deps.Enc, w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, du)
}

// HealRequest is the JSON body of POST /heal.
type HealRequest struct {
	Bucket    string `json:"bucket,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
	DryRun    bool   `json:"dryRun,omitempty"`
	Deep      bool   `json:"deep,omitempty"`
	Remove    bool   `json:"remove,omitempty"`
}

// HealJobResponse is a healing job (POST /heal, GET /heal/{id}).
type HealJobResponse struct {
	ID      string      `json:"id"`
	State   string      `json:"state"`
	Request HealRequest `json:"request"`
	// Finished reports a job that is no longer running.
	Finished   bool       `json:"finished"`
	Node       string     `json:"node,omitempty"`
	StartedAt  time.Time  `json:"startedAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Current    string     `json:"current,omitempty"`
	Scanned    uint64     `json:"scanned"`
	Healed     uint64     `json:"healed"`
	Failed     uint64     `json:"failed"`
	Dangling   uint64     `json:"dangling"`
	Removed    uint64     `json:"removed"`
	Bytes      uint64     `json:"bytes"`
	// Drives sums up the job drive by drive.
	Drives []HealJobDriveResponse `json:"drives"`
	// Items are the items that needed healing, were dangling or failed.
	Items          []HealItemResponse `json:"items"`
	ItemsTruncated bool               `json:"itemsTruncated,omitempty"`
	Error          string             `json:"error,omitempty"`
}

// HealJobDriveResponse is the summary of a job on one drive.
type HealJobDriveResponse struct {
	Endpoint string `json:"endpoint"`
	Pool     int    `json:"pool"`
	Set      int    `json:"set"`
	Disk     int    `json:"disk"`
	State    string `json:"state"`
	Missing  uint64 `json:"missing"`
	Corrupt  uint64 `json:"corrupt"`
	Offline  uint64 `json:"offline"`
	Healed   uint64 `json:"healed"`
}

// HealItemResponse is an item of a job that needed healing.
type HealItemResponse struct {
	Kind     string                  `json:"kind"`
	Bucket   string                  `json:"bucket,omitempty"`
	Object   string                  `json:"object,omitempty"`
	Healed   int                     `json:"healed"`
	Dangling bool                    `json:"dangling,omitempty"`
	Removed  bool                    `json:"removed,omitempty"`
	Error    string                  `json:"error,omitempty"`
	Drives   []HealItemDriveResponse `json:"drives"`
}

// HealItemDriveResponse is the state of an item on one drive, before and
// after the healing (ok, missing, corrupt, offline).
type HealItemDriveResponse struct {
	Endpoint string `json:"endpoint"`
	Pool     int    `json:"pool"`
	Set      int    `json:"set"`
	Disk     int    `json:"disk"`
	Before   string `json:"before"`
	After    string `json:"after"`
}

// HealJobListResponse is the body of GET /heal.
type HealJobListResponse struct {
	Items      []HealJobResponse `json:"items"`
	NextCursor string            `json:"nextCursor"`
}

func newHealJobResponse(j heal.Job) HealJobResponse {
	resp := HealJobResponse{
		ID: j.ID, State: string(j.State), Finished: j.State != heal.JobRunning, Node: j.Node,
		Request: HealRequest{
			Bucket: j.Request.Bucket, Prefix: j.Request.Prefix, Recursive: j.Request.Recursive,
			DryRun: j.Request.DryRun, Deep: j.Request.Deep, Remove: j.Request.Remove,
		},
		StartedAt: j.StartedAt, UpdatedAt: j.UpdatedAt, Current: j.Current,
		Scanned: j.Scanned, Healed: j.Healed, Failed: j.Failed, Dangling: j.Dangling, Removed: j.Removed, Bytes: j.Bytes,
		Drives: make([]HealJobDriveResponse, 0, len(j.Drives)), Items: make([]HealItemResponse, 0, len(j.Items)),
		ItemsTruncated: j.ItemsTruncated, Error: j.Error,
	}
	if !j.FinishedAt.IsZero() {
		at := j.FinishedAt
		resp.FinishedAt = &at
	}
	for _, d := range j.Drives {
		resp.Drives = append(resp.Drives, HealJobDriveResponse{
			Endpoint: d.Endpoint, Pool: d.Pool, Set: d.Set, Disk: d.Disk, State: d.State,
			Missing: d.Missing, Corrupt: d.Corrupt, Offline: d.Offline, Healed: d.Healed,
		})
	}
	for _, it := range j.Items {
		item := HealItemResponse{
			Kind: it.Kind, Bucket: it.Bucket, Object: it.Object, Healed: it.Healed, Dangling: it.Dangling,
			Removed: it.Removed, Error: it.Error, Drives: make([]HealItemDriveResponse, 0, len(it.Drives)),
		}
		for _, d := range it.Drives {
			item.Drives = append(item.Drives, HealItemDriveResponse{
				Endpoint: d.Endpoint, Pool: d.Pool, Set: d.Set, Disk: d.Disk, Before: string(d.Before), After: string(d.After),
			})
		}
		resp.Items = append(resp.Items, item)
	}
	return resp
}

func (h *infoRoutes) heal(w http.ResponseWriter, r *http.Request) {
	var in HealRequest
	if err := decodeJSON(h.deps.Enc, r, &in, true); err != nil {
		writeError(h.deps.Enc, w, r, err)
		return
	}
	j, err := h.deps.Admin.StartHeal(r.Context(), adminsvc.HealInput{
		Bucket: in.Bucket, Prefix: in.Prefix, Recursive: in.Recursive, DryRun: in.DryRun, Deep: in.Deep, Remove: in.Remove,
	})
	if err != nil {
		writeError(h.deps.Enc, w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusAccepted, newHealJobResponse(j))
}

func (h *infoRoutes) healList(w http.ResponseWriter, r *http.Request) {
	jobs, err := h.deps.Admin.ListHeals(r.Context())
	if err != nil {
		writeError(h.deps.Enc, w, r, err)
		return
	}
	resp := HealJobListResponse{Items: make([]HealJobResponse, 0, len(jobs))}
	for _, j := range jobs {
		resp.Items = append(resp.Items, newHealJobResponse(j))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, resp)
}

func (h *infoRoutes) healStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathParam(r, "id")
	if err != nil {
		writeError(h.deps.Enc, w, r, err)
		return
	}
	j, err := h.deps.Admin.HealStatus(r.Context(), id)
	if err != nil {
		writeError(h.deps.Enc, w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newHealJobResponse(j))
}

// serviceAction restarts or stops the server; the response is sent
// before the process acts on it.
func (h *infoRoutes) serviceAction(action adminsvc.Action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h.deps.Admin.ServiceAction(r.Context(), action); err != nil {
			writeError(h.deps.Enc, w, r, err)
			return
		}
		h.deps.Enc.WriteJSON(w, http.StatusAccepted, ServiceActionResponse{Action: string(action)})
	}
}

// ServiceActionResponse acknowledges a service action.
type ServiceActionResponse struct {
	Action string `json:"action"`
}
