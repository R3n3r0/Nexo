package admin

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// InfoDeps are the dependencies of the info routes.
type InfoDeps struct {
	Admin admin.Service
	// Sessions protects the endpoints with the console session; when nil
	// the endpoints are open (tests).
	//
	// TODO(M3): replace with the shared admin authorization middleware
	// (session cookie or SigV4 + policy "admin:ServerInfo").
	Sessions session.Service
	Enc      httpx.Encoder
}

type infoRoutes struct {
	deps InfoDeps
}

var _ InfoRoutes = (*infoRoutes)(nil)

// NewInfoRoutes returns the info routes.
func NewInfoRoutes(deps InfoDeps) InfoRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &infoRoutes{deps: deps}
}

// Register implements Routes.
func (h *infoRoutes) Register(r chi.Router) {
	r.Get("/info", h.authenticated(h.info))
	r.Get("/usage", h.authenticated(h.usage))
}

func (h *infoRoutes) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.deps.Sessions != nil {
			if _, err := SessionFromRequest(r, h.deps.Sessions); err != nil {
				h.deps.Enc.WriteProblem(w, r, httpx.Problem{Status: http.StatusUnauthorized, Code: "unauthenticated", Detail: "no valid session"})
				return
			}
		}
		next(w, r)
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
	info, err := h.deps.Admin.ServerInfo(r.Context())
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newServerInfoResponse(info))
}

func (h *infoRoutes) usage(w http.ResponseWriter, r *http.Request) {
	du, err := h.deps.Admin.DataUsage(r.Context())
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, du)
}
