package admin

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/replication"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// ReplicationRoutes exposes the replication of the buckets (S-BKT-12):
// /buckets/{bucket}/replication (the rules, in the console form),
// .../targets (the remote buckets the rules replicate to), .../status and
// .../resync.
type ReplicationRoutes interface{ Routes }

// ReplicationDeps are the dependencies of the replication routes.
type ReplicationDeps struct {
	// Replication manages the targets and runs the replication; Buckets
	// stores the configuration (both required).
	Replication replication.Service
	Buckets     bucket.Service
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

type replicationRoutes struct {
	deps  ReplicationDeps
	guard guard
}

var _ ReplicationRoutes = (*replicationRoutes)(nil)

// NewReplicationRoutes returns the replication routes.
func NewReplicationRoutes(deps ReplicationDeps) (ReplicationRoutes, error) {
	if deps.Replication == nil || deps.Buckets == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: replication routes require the replication and bucket services and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &replicationRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}, nil
}

// Register implements Routes.
func (h *replicationRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *replicationRoutes) routes() []route {
	return []route{
		{http.MethodGet, "/buckets/{bucket}/replication", onBucket(policy.ActionGetReplication), h.get},
		{http.MethodPut, "/buckets/{bucket}/replication", onBucket(policy.ActionPutReplication), h.put},
		{http.MethodDelete, "/buckets/{bucket}/replication", onBucket(policy.ActionPutReplication), h.remove},
		{http.MethodGet, "/buckets/{bucket}/replication/targets", onBucket(policy.ActionAdminGetBucketTarget), h.listTargets},
		{http.MethodPost, "/buckets/{bucket}/replication/targets", onBucket(policy.ActionAdminSetBucketTarget), h.setTarget},
		{http.MethodDelete, "/buckets/{bucket}/replication/targets", onBucket(policy.ActionAdminSetBucketTarget), h.removeTarget},
		{http.MethodGet, "/buckets/{bucket}/replication/status", onBucket(policy.ActionGetReplication), h.status},
		{http.MethodPost, "/buckets/{bucket}/replication/resync", onBucket(policy.ActionResetReplication), h.resync},
	}
}

// ReplicationRuleBody is one rule of the replication of a bucket.
type ReplicationRuleBody struct {
	ID       string `json:"id"`
	Priority int    `json:"priority"`
	Enabled  bool   `json:"enabled"`
	// Prefix and Tags select the keys (none: every key).
	Prefix string            `json:"prefix,omitempty"`
	Tags   map[string]string `json:"tags,omitempty"`
	// Target is the ARN of the replication target of the bucket.
	Target string `json:"target"`
	// DeleteMarkers, Deletes and Existing replicate the delete markers,
	// the permanent deletions of versions, and (on a resync) the versions
	// written before the rule.
	DeleteMarkers bool `json:"deleteMarkers"`
	Deletes       bool `json:"deletes"`
	Existing      bool `json:"existing"`
}

// ReplicationConfigBody is the replication configuration of a bucket, the
// body of GET and PUT /buckets/{bucket}/replication (no rule: none).
type ReplicationConfigBody struct {
	Rules []ReplicationRuleBody `json:"rules"`
}

func toggle(on bool) *domain.ReplicationToggle {
	if on {
		return &domain.ReplicationToggle{Status: domain.ReplicationEnabled}
	}
	return &domain.ReplicationToggle{Status: domain.ReplicationDisabled}
}

// replicationDocument builds the ReplicationConfiguration document of the
// console form.
func replicationDocument(in ReplicationConfigBody) ([]byte, error) {
	cfg := domain.ReplicationConfig{}
	for _, r := range in.Rules {
		rule := domain.ReplicationRule{
			ID: r.ID, Priority: r.Priority, Status: domain.ReplicationDisabled,
			DeleteMarkerReplication: toggle(r.DeleteMarkers), DeleteReplication: toggle(r.Deletes), ExistingObjectReplication: toggle(r.Existing),
			Destination: domain.ReplicationRuleDestination{Bucket: r.Target},
		}
		if r.Enabled {
			rule.Status = domain.ReplicationEnabled
		}
		prefix := r.Prefix
		switch {
		case len(r.Tags) == 0:
			rule.Filter = &domain.ReplicationFilter{Prefix: &prefix}
		case len(r.Tags) == 1 && prefix == "":
			for k, v := range r.Tags {
				rule.Filter = &domain.ReplicationFilter{Tag: &domain.ReplicationTag{Key: k, Value: v}}
			}
		default:
			and := &domain.ReplicationFilterAnd{Prefix: prefix}
			for _, k := range sortedTagKeys(r.Tags) {
				and.Tags = append(and.Tags, domain.ReplicationTag{Key: k, Value: r.Tags[k]})
			}
			rule.Filter = &domain.ReplicationFilter{And: and}
		}
		cfg.Rules = append(cfg.Rules, rule)
	}
	return cfg.Encode()
}

func sortedTagKeys(tags map[string]string) []string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// replicationBody is the console form of a stored document.
func replicationBody(doc []byte) (ReplicationConfigBody, error) {
	cfg, err := domain.ParseReplicationConfig(doc, nil)
	if err != nil {
		return ReplicationConfigBody{}, err
	}
	out := ReplicationConfigBody{Rules: make([]ReplicationRuleBody, 0, len(cfg.Rules))}
	for _, r := range cfg.Rules {
		body := ReplicationRuleBody{ID: r.ID, Priority: r.Priority, Enabled: r.Enabled(), Target: r.Destination.Bucket,
			DeleteMarkers: r.ReplicatesDeleteMarkers(), Deletes: r.ReplicatesDeletes(), Existing: r.ReplicatesExisting()}
		switch {
		case r.Filter != nil && r.Filter.And != nil:
			body.Prefix = r.Filter.And.Prefix
			body.Tags = map[string]string{}
			for _, t := range r.Filter.And.Tags {
				body.Tags[t.Key] = t.Value
			}
		case r.Filter != nil && r.Filter.Tag != nil:
			body.Tags = map[string]string{r.Filter.Tag.Key: r.Filter.Tag.Value}
		case r.Filter != nil && r.Filter.Prefix != nil:
			body.Prefix = *r.Filter.Prefix
		case r.Prefix != nil:
			body.Prefix = *r.Prefix
		}
		out.Rules = append(out.Rules, body)
	}
	return out, nil
}

func (h *replicationRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

func (h *replicationRoutes) get(w http.ResponseWriter, r *http.Request) {
	doc, err := h.deps.Buckets.GetReplication(r.Context(), bucketName(r))
	if errors.Is(err, domain.ErrPolicyNotFound) {
		h.deps.Enc.WriteJSON(w, http.StatusOK, ReplicationConfigBody{Rules: []ReplicationRuleBody{}})
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	body, err := replicationBody(doc)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, body)
}

func (h *replicationRoutes) put(w http.ResponseWriter, r *http.Request) {
	var in ReplicationConfigBody
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	name := bucketName(r)
	if len(in.Rules) == 0 {
		h.remove(w, r)
		return
	}
	doc, err := replicationDocument(in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if doc, err = h.deps.Replication.ValidateConfig(r.Context(), name, doc); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Buckets.SetReplication(r.Context(), name, doc); err != nil {
		h.fail(w, r, err)
		return
	}
	body, err := replicationBody(doc)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, body)
}

func (h *replicationRoutes) remove(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Buckets.DeleteReplication(r.Context(), bucketName(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ReplicationTargetBody is a replication target: the request of POST
// .../targets (with secretKey; arn to replace one) and, without the
// secret, its responses.
type ReplicationTargetBody struct {
	ARN       string    `json:"arn,omitempty"`
	Endpoint  string    `json:"endpoint"`
	Bucket    string    `json:"bucket"`
	Region    string    `json:"region,omitempty"`
	AccessKey string    `json:"accessKey"`
	SecretKey string    `json:"secretKey,omitempty"`
	Label     string    `json:"label,omitempty"`
	Created   time.Time `json:"created,omitzero"`
	Updated   time.Time `json:"updated,omitzero"`
}

// ReplicationTargetsResponse is the body of GET .../targets.
type ReplicationTargetsResponse struct {
	Targets []ReplicationTargetBody `json:"targets"`
}

func targetBody(t replication.Target) ReplicationTargetBody {
	return ReplicationTargetBody{ARN: t.ARN, Endpoint: t.Endpoint, Bucket: t.Bucket, Region: t.Region, AccessKey: t.AccessKey,
		Label: t.Label, Created: t.Created, Updated: t.Updated}
}

func (h *replicationRoutes) listTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := h.deps.Replication.ListTargets(r.Context(), bucketName(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := ReplicationTargetsResponse{Targets: make([]ReplicationTargetBody, 0, len(targets))}
	for _, t := range targets {
		out.Targets = append(out.Targets, targetBody(t))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *replicationRoutes) setTarget(w http.ResponseWriter, r *http.Request) {
	var in ReplicationTargetBody
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	name := bucketName(r)
	if _, err := h.deps.Buckets.Get(r.Context(), name); err != nil {
		h.fail(w, r, err)
		return
	}
	t, err := h.deps.Replication.SetTarget(r.Context(), name, replication.TargetInput{
		ARN: in.ARN, Endpoint: in.Endpoint, Bucket: in.Bucket, Region: in.Region, AccessKey: in.AccessKey, SecretKey: in.SecretKey, Label: in.Label,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if in.ARN != "" {
		status = http.StatusOK
	}
	h.deps.Enc.WriteJSON(w, status, targetBody(t))
}

func (h *replicationRoutes) removeTarget(w http.ResponseWriter, r *http.Request) {
	arn := r.URL.Query().Get("arn")
	if arn == "" {
		h.fail(w, r, fmt.Errorf("%w: the arn query parameter names the target", domain.ErrInvalidArgument))
		return
	}
	if err := h.deps.Replication.RemoveTarget(r.Context(), bucketName(r), arn); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ReplicationResyncBody is a resync: the request of POST .../resync
// (target) and the progress the status reports.
type ReplicationResyncBody struct {
	ID         string    `json:"id,omitempty"`
	Target     string    `json:"target"`
	State      string    `json:"state,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	UpdatedAt  time.Time `json:"updatedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	Scanned    int64     `json:"scanned"`
	Queued     int64     `json:"queued"`
	Error      string    `json:"error,omitempty"`
}

// ReplicationTargetStatusBody is the replication to one target as the
// node serving the request sees it.
type ReplicationTargetStatusBody struct {
	ARN        string    `json:"arn"`
	Online     bool      `json:"online"`
	Queued     int       `json:"queued"`
	Replicated int64     `json:"replicated"`
	Bytes      int64     `json:"bytes"`
	Deletes    int64     `json:"deletes"`
	Failed     int64     `json:"failed"`
	LastError  string    `json:"lastError,omitempty"`
	LastFailed time.Time `json:"lastFailed,omitzero"`
	LastOK     time.Time `json:"lastOk,omitzero"`
}

// ReplicationStatusResponse is the body of GET .../status.
type ReplicationStatusResponse struct {
	Targets []ReplicationTargetStatusBody `json:"targets"`
	Queued  int                           `json:"queued"`
	Resync  *ReplicationResyncBody        `json:"resync,omitempty"`
}

func resyncBody(r replication.Resync) *ReplicationResyncBody {
	return &ReplicationResyncBody{ID: r.ID, Target: r.Target, State: string(r.State), StartedAt: r.StartedAt, UpdatedAt: r.UpdatedAt,
		FinishedAt: r.FinishedAt, Scanned: r.Scanned, Queued: r.Queued, Error: r.Error}
}

func (h *replicationRoutes) status(w http.ResponseWriter, r *http.Request) {
	st, err := h.deps.Replication.Status(r.Context(), bucketName(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := ReplicationStatusResponse{Targets: make([]ReplicationTargetStatusBody, 0, len(st.Targets)), Queued: st.Queued}
	for _, t := range st.Targets {
		out.Targets = append(out.Targets, ReplicationTargetStatusBody{ARN: t.ARN, Online: t.Online, Queued: t.Queued, Replicated: t.Replicated,
			Bytes: t.Bytes, Deletes: t.Deletes, Failed: t.Failed, LastError: t.LastError, LastFailed: t.LastFailed, LastOK: t.LastOK})
	}
	if st.Resync != nil {
		out.Resync = resyncBody(*st.Resync)
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *replicationRoutes) resync(w http.ResponseWriter, r *http.Request) {
	var in ReplicationResyncBody
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.deps.Replication.StartResync(r.Context(), bucketName(r), in.Target)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusAccepted, resyncBody(res))
}
