package admin

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// InfoDeps are the dependencies of the info routes.
type InfoDeps struct {
	Admin admin.Service
	// Policy authorizes the admin:* actions; nil leaves the routes open (tests).
	Policy policy.Service
	Enc    httpx.Codec
}

type infoRoutes struct {
	deps  InfoDeps
	authz authorizer
}

var _ InfoRoutes = (*infoRoutes)(nil)

// NewInfoRoutes returns the server information, data usage, healing and
// service control routes.
func NewInfoRoutes(deps InfoDeps) (InfoRoutes, error) {
	if deps.Admin == nil {
		return nil, fmt.Errorf("%w: info routes require the admin service", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &infoRoutes{deps: deps, authz: authorizer{policy: deps.Policy, enc: deps.Enc}}, nil
}

// Register implements Routes.
func (h *infoRoutes) Register(r chi.Router) {
	r.Get("/info", h.info)
	r.Get("/usage", h.usage)
	r.Post("/heal", h.heal)
	r.Get("/heal/{id}", h.healStatus)
	r.Post("/service/{action}", h.serviceAction)
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
	Error    string `json:"error,omitempty"`
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
				Healing: d.Healing, Error: d.Error,
			})
		}
		resp.Nodes = append(resp.Nodes, node)
	}
	return resp
}

func (h *infoRoutes) info(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminServerInfo, "") {
		return
	}
	info, err := h.deps.Admin.ServerInfo(r.Context())
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newServerInfoResponse(info))
}

func (h *infoRoutes) usage(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminDataUsage, "") {
		return
	}
	du, err := h.deps.Admin.DataUsage(r.Context())
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
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
}

func (h *infoRoutes) heal(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminHeal, "") {
		return
	}
	var in HealRequest
	if r.ContentLength != 0 {
		if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
			h.deps.Enc.WriteProblemError(w, r, err)
			return
		}
	}
	res, err := h.deps.Admin.Heal(r.Context(), admin.HealInput{Bucket: in.Bucket, Prefix: in.Prefix, Recursive: in.Recursive, DryRun: in.DryRun})
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusAccepted, res)
}

func (h *infoRoutes) healStatus(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminHeal, "") {
		return
	}
	res, err := h.deps.Admin.Heal(r.Context(), admin.HealInput{ID: chi.URLParam(r, "id")})
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, res)
}

func (h *infoRoutes) serviceAction(w http.ResponseWriter, r *http.Request) {
	action := admin.Action(chi.URLParam(r, "action"))
	var required policy.Action
	switch action {
	case admin.ActionRestart:
		required = policy.ActionAdminServiceRestart
	case admin.ActionStop:
		required = policy.ActionAdminServiceStop
	default:
		badRequest(h.deps.Enc, w, r, fmt.Sprintf("unknown service action %q", action))
		return
	}
	if !h.authz.authorize(w, r, required, "") {
		return
	}
	if err := h.deps.Admin.ServiceAction(r.Context(), action); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
