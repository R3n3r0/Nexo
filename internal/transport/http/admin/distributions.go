package admin

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// DistributionRoutes exposes the distributions (docs/10 §10.2, §10.5):
// /distributions (CRUD with If-Match on the ETag, test of a URL, rotation
// of the origin secret, signed URLs and cookies), the key groups of the
// signed behaviors (/keygroups, the private key of a generated key pair
// returned once) and the diagnostics of the host routing
// (/routing/resolve).
//
// The body of a distribution is the document of distribution.Distribution
// (the JSON form stored in the system store and exported by nexoctl dist
// export) carried as is, so that every field of the model round-trips.
type DistributionRoutes interface{ Routes }

// DistributionDeps are the dependencies of the distribution routes.
type DistributionDeps struct {
	// Distributions manages the distributions and key groups (required).
	Distributions adminsvc.DistributionManager
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// DistributionOption configures the distribution routes.
type DistributionOption func(*distributionRoutes)

type distributionRoutes struct {
	deps  DistributionDeps
	guard guard
}

var _ DistributionRoutes = (*distributionRoutes)(nil)

// NewDistributionRoutes returns the distribution routes.
func NewDistributionRoutes(deps DistributionDeps, opts ...DistributionOption) (DistributionRoutes, error) {
	if deps.Distributions == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: distribution routes require the distribution manager and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &distributionRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *distributionRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *distributionRoutes) routes() []route {
	manage := global(policy.ActionAdminManageDistributions)
	return []route{
		{http.MethodGet, "/distributions", manage, h.list},
		// The manager also authorizes the action on the origin bucket of
		// the saved distribution.
		{http.MethodPost, "/distributions", manage, h.create},
		{http.MethodGet, "/distributions/{id}", manage, h.get},
		{http.MethodPut, "/distributions/{id}", manage, h.update},
		{http.MethodDelete, "/distributions/{id}", manage, h.remove},
		{http.MethodPost, "/distributions/{id}/test", manage, h.test},
		{http.MethodPost, "/distributions/{id}/origin-secret/rotate", manage, h.rotate},
		{http.MethodPost, "/distributions/{id}/sign", global(policy.ActionAdminSignDistributionURL), h.sign},
		{http.MethodGet, "/keygroups", manage, h.listKeyGroups},
		{http.MethodPost, "/keygroups", manage, h.createKeyGroup},
		{http.MethodGet, "/keygroups/{id}", manage, h.getKeyGroup},
		{http.MethodDelete, "/keygroups/{id}", manage, h.deleteKeyGroup},
		{http.MethodGet, "/keygroups/{id}/keys", manage, h.listKeys},
		{http.MethodPost, "/keygroups/{id}/keys", manage, h.addKey},
		{http.MethodDelete, "/keygroups/{id}/keys/{key}", manage, h.deleteKey},
		{http.MethodGet, "/routing/resolve", manage, h.resolve},
	}
}

// DistributionListResponse is the body of GET /distributions.
type DistributionListResponse struct {
	Items []distribution.Distribution `json:"items"`
}

// RouteResponse is the resolution of a host (GET /routing/resolve, the
// route of a test).
type RouteResponse struct {
	// Kind is distribution, virtual-host, main, path-style or misdirected.
	Kind distribution.RouteKind `json:"kind"`
	// Host is the normalized host.
	Host string `json:"host"`
	// Alias is the alias that matched (a wildcard alias included).
	Alias string `json:"alias,omitempty"`
	// Distribution is the id of the distribution serving the host, Mode
	// its mode and Enabled whether it serves requests.
	Distribution string            `json:"distribution,omitempty"`
	Mode         distribution.Mode `json:"mode,omitempty"`
	Enabled      *bool             `json:"enabled,omitempty"`
	// Bucket is the bucket the host addresses.
	Bucket string `json:"bucket,omitempty"`
}

func newRouteResponse(rt distribution.Route) RouteResponse {
	out := RouteResponse{Kind: rt.Kind, Host: rt.Host, Alias: rt.Alias, Bucket: rt.Bucket}
	if d := rt.Distribution; d != nil {
		enabled := d.Enabled
		out.Distribution, out.Mode, out.Enabled = d.ID, d.Mode, &enabled
	}
	return out
}

// DistributionTestRequest is the body of POST /distributions/{id}/test.
type DistributionTestRequest struct {
	// URL is an absolute URL on an alias of the distribution, or a path
	// (completed with https and the first exact alias).
	URL string `json:"url"`
	// Method defaults to GET.
	Method string `json:"method,omitempty"`
	// ClientIP is the address of the viewer.
	ClientIP string `json:"clientIp,omitempty"`
}

// DistributionTestResponse is how the distribution would serve a URL.
type DistributionTestResponse struct {
	Route RouteResponse `json:"route"`
	// BehaviorIndex is the index of the behavior that matched, -1 for the
	// default behavior; Behavior is the effective one (inherited fields
	// resolved).
	BehaviorIndex int                   `json:"behaviorIndex"`
	Behavior      distribution.Behavior `json:"behavior"`
	// Bucket and Key are the object the path maps to.
	Bucket string `json:"bucket,omitempty"`
	Key    string `json:"key,omitempty"`
	// RedirectStatus and RedirectLocation describe a redirection.
	RedirectStatus   int    `json:"redirectStatus,omitempty"`
	RedirectLocation string `json:"redirectLocation,omitempty"`
	// RefusalStatus and Refusal explain a refusal before the object is
	// read (method, restrictions, deny, invalid path).
	RefusalStatus int    `json:"refusalStatus,omitempty"`
	Refusal       string `json:"refusal,omitempty"`
	// ResponseHeaders are the headers the distribution adds.
	ResponseHeaders map[string]string `json:"responseHeaders"`
}

// RotateOriginSecretRequest is the body of POST
// /distributions/{id}/origin-secret/rotate.
type RotateOriginSecretRequest struct {
	// Header names the header (default: the current one, or
	// X-Origin-Secret).
	Header string `json:"header,omitempty"`
	// Keep is how many previous secrets stay active (default 1);
	// DropPrevious keeps none.
	Keep         int  `json:"keep,omitempty"`
	DropPrevious bool `json:"dropPrevious,omitempty"`
}

// RotateOriginSecretResponse is the new origin secret, in clear: the only
// time it is shown.
type RotateOriginSecretResponse struct {
	Secret       string                    `json:"secret"`
	Header       string                    `json:"header"`
	Distribution distribution.Distribution `json:"distribution"`
}

// SignDistributionRequest is the body of POST /distributions/{id}/sign.
type SignDistributionRequest struct {
	// URL is an absolute URL on an alias of the distribution, or a path
	// completed with Scheme (default https) and Host (default: the first
	// exact alias).
	URL    string `json:"url"`
	Scheme string `json:"scheme,omitempty"`
	Host   string `json:"host,omitempty"`
	// PrivateKeyPEM is the private key of a key of the key group of the
	// signed behavior serving the URL; KeyPairID names its public key
	// (default: found from the private key).
	PrivateKeyPEM string `json:"privateKeyPem"`
	KeyPairID     string `json:"keyPairId,omitempty"`
	// ExpiresIn (seconds) or ExpiresAt: when the signature expires.
	ExpiresIn int64      `json:"expiresIn,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// NotBefore, IPRange and Resource make a custom policy.
	NotBefore *time.Time `json:"notBefore,omitempty"`
	IPRange   string     `json:"ipRange,omitempty"`
	Resource  string     `json:"resource,omitempty"`
	// Cookies signs cookies instead of the URL.
	Cookies bool `json:"cookies,omitempty"`
}

// SignedCookieResponse is a signed cookie.
type SignedCookieResponse struct {
	Name    string    `json:"name"`
	Value   string    `json:"value"`
	Expires time.Time `json:"expires"`
}

// SignDistributionResponse is a signed URL, or signed cookies.
type SignDistributionResponse struct {
	// URL is the signed URL (the URL, unsigned, for cookies).
	URL       string                 `json:"url"`
	Cookies   []SignedCookieResponse `json:"cookies"`
	KeyGroup  string                 `json:"keyGroup"`
	KeyPairID string                 `json:"keyPairId"`
	ExpiresAt time.Time              `json:"expiresAt"`
}

// KeyGroupListResponse is the body of GET /keygroups.
type KeyGroupListResponse struct {
	Items []distribution.KeyGroup `json:"items"`
}

// GenerateKeyRequest describes a key pair to generate.
type GenerateKeyRequest struct {
	// Algorithm is ed25519 (default) or rsa.
	Algorithm distribution.KeyAlgorithm `json:"algorithm,omitempty"`
	// Bits is the size of an RSA key (2048 by default).
	Bits    int    `json:"bits,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// CreateKeyGroupRequest is the body of POST /keygroups.
type CreateKeyGroupRequest struct {
	ID      string `json:"id"`
	Comment string `json:"comment,omitempty"`
	// GenerateKey generates the first key pair of the group.
	GenerateKey *GenerateKeyRequest `json:"generateKey,omitempty"`
}

// CreateKeyGroupResponse is the created key group with its generated key
// pair, whose private key is returned once.
type CreateKeyGroupResponse struct {
	KeyGroup distribution.KeyGroup      `json:"keyGroup"`
	Key      *distribution.GeneratedKey `json:"key,omitempty"`
}

// KeyListResponse is the body of GET /keygroups/{id}/keys.
type KeyListResponse struct {
	Items []distribution.PublicKey `json:"items"`
}

// AddKeyRequest is the body of POST /keygroups/{id}/keys: a public key
// provided by the user, or (without it) a key pair to generate.
type AddKeyRequest struct {
	PublicKeyPEM string                    `json:"publicKeyPem,omitempty"`
	Algorithm    distribution.KeyAlgorithm `json:"algorithm,omitempty"`
	Bits         int                       `json:"bits,omitempty"`
	Comment      string                    `json:"comment,omitempty"`
}

// AddKeyResponse is the added key; PrivateKeyPEM is set for a generated
// key pair, returned once.
type AddKeyResponse struct {
	PublicKey     distribution.PublicKey `json:"publicKey"`
	PrivateKeyPEM string                 `json:"privateKeyPem,omitempty"`
}

func (h *distributionRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

func (h *distributionRoutes) writeDistribution(w http.ResponseWriter, status int, d distribution.Distribution) {
	setETag(w, d.ETag)
	h.deps.Enc.WriteJSON(w, status, d)
}

func (h *distributionRoutes) list(w http.ResponseWriter, r *http.Request) {
	list, err := h.deps.Distributions.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, DistributionListResponse{Items: nonNil(list)})
}

func (h *distributionRoutes) create(w http.ResponseWriter, r *http.Request) {
	var d distribution.Distribution
	if err := decodeJSON(h.deps.Enc, r, &d, false); err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.deps.Distributions.Create(r.Context(), adminsvc.SaveDistributionInput{Requester: requesterOf(r), Distribution: d})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeDistribution(w, http.StatusCreated, out)
}

func (h *distributionRoutes) get(w http.ResponseWriter, r *http.Request) {
	d, err := h.deps.Distributions.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeDistribution(w, http.StatusOK, d)
}

func (h *distributionRoutes) update(w http.ResponseWriter, r *http.Request) {
	var d distribution.Distribution
	if err := decodeJSON(h.deps.Enc, r, &d, false); err != nil {
		h.fail(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	if d.ID == "" {
		d.ID = id
	}
	if d.ID != id {
		h.fail(w, r, fmt.Errorf("%w: the id of the body (%s) is not the one of the path (%s)", domain.ErrInvalidArgument, d.ID, id))
		return
	}
	out, err := h.deps.Distributions.Update(r.Context(), adminsvc.SaveDistributionInput{Requester: requesterOf(r), Distribution: d, IfMatch: ifMatch(r)})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeDistribution(w, http.StatusOK, out)
}

func (h *distributionRoutes) remove(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Distributions.Delete(r.Context(), requesterOf(r), chi.URLParam(r, "id")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *distributionRoutes) test(w http.ResponseWriter, r *http.Request) {
	var in DistributionTestRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.deps.Distributions.Test(r.Context(), adminsvc.TestDistributionInput{ID: chi.URLParam(r, "id"), URL: in.URL, Method: in.Method, ClientIP: in.ClientIP})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, DistributionTestResponse{
		Route: newRouteResponse(res.Route), BehaviorIndex: res.BehaviorIndex, Behavior: res.Behavior, Bucket: res.Bucket, Key: res.Key,
		RedirectStatus: res.RedirectStatus, RedirectLocation: res.RedirectLocation, RefusalStatus: res.RefusalStatus, Refusal: res.Refusal,
		ResponseHeaders: nonNilMap(res.ResponseHeaders),
	})
}

func (h *distributionRoutes) rotate(w http.ResponseWriter, r *http.Request) {
	var in RotateOriginSecretRequest
	if err := decodeJSON(h.deps.Enc, r, &in, true); err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.deps.Distributions.RotateOriginSecret(r.Context(), requesterOf(r), distribution.RotateSecretInput{
		ID: chi.URLParam(r, "id"), Header: in.Header, Keep: in.Keep, DropPrevious: in.DropPrevious,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	setETag(w, res.Distribution.ETag)
	h.deps.Enc.WriteJSON(w, http.StatusOK, RotateOriginSecretResponse{Secret: res.Secret, Header: res.Header, Distribution: res.Distribution})
}

// maxSignExpiresIn bounds the relative expiration of a signature (no
// overflow of the duration).
const maxSignExpiresIn = int64(100 * 365 * 24 * 3600)

func (h *distributionRoutes) sign(w http.ResponseWriter, r *http.Request) {
	var in SignDistributionRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	if in.ExpiresIn < 0 || in.ExpiresIn > maxSignExpiresIn {
		h.fail(w, r, fmt.Errorf("%w: expiresIn must be between 1 and %d seconds", domain.ErrInvalidArgument, maxSignExpiresIn))
		return
	}
	sin := adminsvc.SignDistributionInput{
		ID: chi.URLParam(r, "id"), URL: in.URL, Scheme: in.Scheme, Host: in.Host, PrivateKeyPEM: in.PrivateKeyPEM, KeyPairID: in.KeyPairID,
		ExpiresIn: time.Duration(in.ExpiresIn) * time.Second, IPRange: in.IPRange, Resource: in.Resource, Cookies: in.Cookies,
	}
	if in.ExpiresAt != nil {
		sin.Expires = *in.ExpiresAt
	}
	if in.NotBefore != nil {
		sin.NotBefore = *in.NotBefore
	}
	res, err := h.deps.Distributions.Sign(r.Context(), sin)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := SignDistributionResponse{URL: res.URL, Cookies: make([]SignedCookieResponse, 0, len(res.Cookies)), KeyGroup: res.KeyGroup,
		KeyPairID: res.KeyPairID, ExpiresAt: res.Expires.UTC()}
	for _, c := range res.Cookies {
		out.Cookies = append(out.Cookies, SignedCookieResponse{Name: c.Name, Value: c.Value, Expires: c.Expires.UTC()})
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *distributionRoutes) listKeyGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.deps.Distributions.ListKeyGroups(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	for i := range groups {
		groups[i].Keys = nonNil(groups[i].Keys)
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, KeyGroupListResponse{Items: nonNil(groups)})
}

func (h *distributionRoutes) createKeyGroup(w http.ResponseWriter, r *http.Request) {
	var in CreateKeyGroupRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	cin := adminsvc.CreateKeyGroupInput{ID: in.ID, Comment: in.Comment}
	if g := in.GenerateKey; g != nil {
		cin.Key = &adminsvc.GenerateKeySpec{Algorithm: g.Algorithm, Bits: g.Bits, Comment: g.Comment}
	}
	res, err := h.deps.Distributions.CreateKeyGroup(r.Context(), cin)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res.KeyGroup.Keys = nonNil(res.KeyGroup.Keys)
	h.deps.Enc.WriteJSON(w, http.StatusCreated, CreateKeyGroupResponse{KeyGroup: res.KeyGroup, Key: res.Key})
}

func (h *distributionRoutes) getKeyGroup(w http.ResponseWriter, r *http.Request) {
	g, err := h.deps.Distributions.GetKeyGroup(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	g.Keys = nonNil(g.Keys)
	h.deps.Enc.WriteJSON(w, http.StatusOK, g)
}

func (h *distributionRoutes) deleteKeyGroup(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Distributions.DeleteKeyGroup(r.Context(), requesterOf(r), chi.URLParam(r, "id")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *distributionRoutes) listKeys(w http.ResponseWriter, r *http.Request) {
	g, err := h.deps.Distributions.GetKeyGroup(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, KeyListResponse{Items: nonNil(g.Keys)})
}

func (h *distributionRoutes) addKey(w http.ResponseWriter, r *http.Request) {
	var in AddKeyRequest
	if err := decodeJSON(h.deps.Enc, r, &in, true); err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.deps.Distributions.AddKey(r.Context(), adminsvc.AddKeyInput{
		Requester: requesterOf(r), KeyGroup: chi.URLParam(r, "id"), PublicKeyPEM: in.PublicKeyPEM, Algorithm: in.Algorithm, Bits: in.Bits, Comment: in.Comment,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, AddKeyResponse{PublicKey: res.PublicKey, PrivateKeyPEM: res.PrivateKeyPEM})
}

func (h *distributionRoutes) deleteKey(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Distributions.DeleteKey(r.Context(), requesterOf(r), chi.URLParam(r, "id"), chi.URLParam(r, "key")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *distributionRoutes) resolve(w http.ResponseWriter, r *http.Request) {
	rt, err := h.deps.Distributions.Resolve(r.Context(), r.URL.Query().Get("host"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newRouteResponse(rt))
}
