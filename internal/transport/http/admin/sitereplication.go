package admin

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/siterepl"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// maxSiteBatch bounds the body of a batch of entries between sites.
const maxSiteBatch = 64 << 20

// SiteReplicationRoutes exposes the site replication (S-STO-11):
// /site-replication (the group), .../sync, and /site-replication/peer/...,
// the calls between the sites of a group, reserved to the root
// credentials.
type SiteReplicationRoutes interface{ Routes }

// SiteReplicationDeps are the dependencies of the site replication routes.
type SiteReplicationDeps struct {
	// SiteReplication runs the site replication (required).
	SiteReplication siterepl.Service
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

type siteReplicationRoutes struct {
	deps  SiteReplicationDeps
	guard guard
}

var _ SiteReplicationRoutes = (*siteReplicationRoutes)(nil)

// NewSiteReplicationRoutes returns the site replication routes.
func NewSiteReplicationRoutes(deps SiteReplicationDeps) (SiteReplicationRoutes, error) {
	if deps.SiteReplication == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: site replication routes require the site replication service and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &siteReplicationRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}, nil
}

// Register implements Routes.
func (h *siteReplicationRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *siteReplicationRoutes) routes() []route {
	op := global(policy.ActionAdminSiteReplicationOperation)
	return []route{
		{http.MethodGet, "/site-replication", global(policy.ActionAdminSiteReplicationInfo), h.info},
		{http.MethodPost, "/site-replication", global(policy.ActionAdminSiteReplicationAdd), h.add},
		{http.MethodDelete, "/site-replication", global(policy.ActionAdminSiteReplicationRemove), h.remove},
		{http.MethodPost, "/site-replication/sync", global(policy.ActionAdminSiteReplicationAdd), h.sync},
		{http.MethodGet, "/site-replication/peer", op, h.rootOnly(h.peer)},
		{http.MethodPost, "/site-replication/peer/join", op, h.rootOnly(h.join)},
		{http.MethodPost, "/site-replication/peer/leave", op, h.rootOnly(h.leave)},
		{http.MethodGet, "/site-replication/peer/state", op, h.rootOnly(h.state)},
		{http.MethodPost, "/site-replication/peer/items", op, h.rootOnly(h.items)},
		{http.MethodPost, "/site-replication/peer/apply", op, h.rootOnly(h.apply)},
	}
}

// SiteReplicationSiteBody is a site of a group.
type SiteReplicationSiteBody struct {
	Name          string `json:"name"`
	DeploymentID  string `json:"deploymentId,omitempty"`
	Endpoint      string `json:"endpoint"`
	AdminEndpoint string `json:"adminEndpoint"`
}

// SiteReplicationSitesBody is the body of POST /site-replication and of
// .../peer/join.
type SiteReplicationSitesBody struct {
	Sites []SiteReplicationSiteBody `json:"sites"`
}

// SiteReplicationInfoBody is the answer of GET /site-replication.
type SiteReplicationInfoBody struct {
	Enabled      bool                      `json:"enabled"`
	Name         string                    `json:"name,omitempty"`
	DeploymentID string                    `json:"deploymentId"`
	Sites        []SiteReplicationSiteBody `json:"sites"`
	Peers        []siterepl.PeerStatus     `json:"peers"`
	Entries      int                       `json:"entries"`
}

// SiteReplicationPeerBody is the answer of GET /site-replication/peer.
type SiteReplicationPeerBody struct {
	DeploymentID string                    `json:"deploymentId"`
	Enabled      bool                      `json:"enabled"`
	Sites        []SiteReplicationSiteBody `json:"sites,omitempty"`
}

// SiteReplicationStateBody is the answer of GET /site-replication/peer/state.
type SiteReplicationStateBody struct {
	DeploymentID string                    `json:"deploymentId"`
	Entries      map[string]siterepl.Entry `json:"entries"`
}

// SiteReplicationKeysBody is the body of POST /site-replication/peer/items.
type SiteReplicationKeysBody struct {
	Keys []string `json:"keys"`
}

// SiteReplicationSealedBody carries sealed entries.
type SiteReplicationSealedBody struct {
	Data []byte `json:"data"`
}

// SiteReplicationResultsBody is the answer of POST
// /site-replication/peer/apply.
type SiteReplicationResultsBody struct {
	Results []siterepl.Result `json:"results"`
}

func sitesBody(sites []siterepl.Site) []SiteReplicationSiteBody {
	out := make([]SiteReplicationSiteBody, 0, len(sites))
	for _, s := range sites {
		out = append(out, SiteReplicationSiteBody(s))
	}
	return out
}

func (h *siteReplicationRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

// rootOnly reserves a route to the root credentials: the calls between
// the sites carry the IAM entries, secrets included.
func (h *siteReplicationRoutes) rootOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !caller(r).IsRoot {
			h.fail(w, r, fmt.Errorf("%w: the calls between the sites need the root credentials", domain.ErrAccessDenied))
			return
		}
		next(w, r)
	}
}

func (h *siteReplicationRoutes) info(w http.ResponseWriter, r *http.Request) {
	in, err := h.deps.SiteReplication.Info(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, infoBody(in))
}

func infoBody(in siterepl.Info) SiteReplicationInfoBody {
	return SiteReplicationInfoBody{Enabled: in.Enabled, Name: in.Name, DeploymentID: in.DeploymentID, Sites: sitesBody(in.Sites),
		Peers: in.Peers, Entries: in.Entries}
}

func (h *siteReplicationRoutes) add(w http.ResponseWriter, r *http.Request) {
	var in SiteReplicationSitesBody
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	sites := make([]siterepl.SiteInput, 0, len(in.Sites))
	for _, s := range in.Sites {
		sites = append(sites, siterepl.SiteInput{Name: s.Name, Endpoint: s.Endpoint, AdminEndpoint: s.AdminEndpoint})
	}
	info, err := h.deps.SiteReplication.Add(r.Context(), sites)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, infoBody(info))
}

func (h *siteReplicationRoutes) remove(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.SiteReplication.Remove(r.Context()); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *siteReplicationRoutes) sync(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.SiteReplication.Sync(r.Context()); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *siteReplicationRoutes) peer(w http.ResponseWriter, r *http.Request) {
	in, err := h.deps.SiteReplication.Peer(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, SiteReplicationPeerBody{DeploymentID: in.DeploymentID, Enabled: in.Enabled, Sites: sitesBody(in.Sites)})
}

func (h *siteReplicationRoutes) join(w http.ResponseWriter, r *http.Request) {
	var in SiteReplicationSitesBody
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	sites := make([]siterepl.Site, 0, len(in.Sites))
	for _, s := range in.Sites {
		sites = append(sites, siterepl.Site(s))
	}
	if err := h.deps.SiteReplication.Join(r.Context(), sites); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *siteReplicationRoutes) leave(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.SiteReplication.Leave(r.Context()); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *siteReplicationRoutes) state(w http.ResponseWriter, r *http.Request) {
	entries, err := h.deps.SiteReplication.State(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	peer, err := h.deps.SiteReplication.Peer(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, SiteReplicationStateBody{DeploymentID: peer.DeploymentID, Entries: entries})
}

func (h *siteReplicationRoutes) items(w http.ResponseWriter, r *http.Request) {
	var in SiteReplicationKeysBody
	if err := h.deps.Enc.DecodeJSON(r.Body, maxSiteBatch, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	sealed, err := h.deps.SiteReplication.Items(r.Context(), in.Keys)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, SiteReplicationSealedBody{Data: sealed})
}

func (h *siteReplicationRoutes) apply(w http.ResponseWriter, r *http.Request) {
	var in SiteReplicationSealedBody
	if err := h.deps.Enc.DecodeJSON(r.Body, maxSiteBatch, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	results, err := h.deps.SiteReplication.Apply(r.Context(), in.Data)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, SiteReplicationResultsBody{Results: results})
}
