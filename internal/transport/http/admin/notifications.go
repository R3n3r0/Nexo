package admin

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// NotificationRoutes exposes the event subscriptions of the buckets
// (/buckets/{bucket}/events) and the notification targets of the
// deployment (/notify/targets).
type NotificationRoutes interface{ Routes }

// NotificationDeps are the dependencies of the notification routes.
type NotificationDeps struct {
	// Notifications manages the subscriptions and the targets (required).
	Notifications adminsvc.NotificationManager
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// NotificationOption configures the notification routes.
type NotificationOption func(*notificationRoutes)

type notificationRoutes struct {
	deps  NotificationDeps
	guard guard
}

var _ NotificationRoutes = (*notificationRoutes)(nil)

// NewNotificationRoutes returns the notification routes.
func NewNotificationRoutes(deps NotificationDeps, opts ...NotificationOption) (NotificationRoutes, error) {
	if deps.Notifications == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: notification routes require the notification manager and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &notificationRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *notificationRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *notificationRoutes) routes() []route {
	// The targets are the notify_* subsystems of the configuration: like
	// the configuration, they need admin:ConfigUpdate.
	targets := global(policy.ActionAdminConfigWrite)
	return []route{
		{http.MethodGet, "/buckets/{bucket}/events", onBucket(policy.ActionGetBucketNotification), h.get},
		{http.MethodPut, "/buckets/{bucket}/events", onBucket(policy.ActionPutBucketNotification), h.put},
		{http.MethodDelete, "/buckets/{bucket}/events", onBucket(policy.ActionPutBucketNotification), h.remove},
		{http.MethodGet, "/notify/targets", targets, h.targets},
		{http.MethodPost, "/notify/targets/{id}/test", targets, h.test},
	}
}

// NotificationsResponse are the event subscriptions of a bucket, the body
// of GET and PUT /buckets/{bucket}/events.
type NotificationsResponse struct {
	Subscriptions []SubscriptionResponse `json:"subscriptions"`
}

// SubscriptionResponse routes the events of a bucket to a target.
type SubscriptionResponse struct {
	// ID is generated when empty.
	ID string `json:"id,omitempty"`
	// ARN names the target (NotifyTargetResponse.ARN).
	ARN string `json:"arn"`
	// Events are event names or families ("s3:ObjectCreated:*").
	Events []string `json:"events"`
	Prefix string   `json:"prefix,omitempty"`
	Suffix string   `json:"suffix,omitempty"`
}

// NotifyTargetResponse is a notification target with its delivery state
// on the node that answers.
type NotifyTargetResponse struct {
	// ID is "_" for the default target of a subsystem.
	ID      string `json:"id"`
	Type    string `json:"type"`
	ARN     string `json:"arn"`
	Enabled bool   `json:"enabled"`
	// Endpoint describes the destination without credentials.
	Endpoint      string `json:"endpoint,omitempty"`
	Online        bool   `json:"online"`
	PendingEvents int    `json:"pendingEvents"`
	LastError     string `json:"lastError,omitempty"`
}

// NotifyTargetListResponse is the body of GET /notify/targets.
type NotifyTargetListResponse struct {
	Items []NotifyTargetResponse `json:"items"`
}

// TargetTestResponse is the body of POST /notify/targets/{id}/test.
type TargetTestResponse struct {
	// Target is "<id>:<type>".
	Target    string `json:"target"`
	Delivered bool   `json:"delivered"`
	Error     string `json:"error,omitempty"`
}

func newNotificationsResponse(cfg *adminsvc.NotificationConfig) NotificationsResponse {
	out := NotificationsResponse{Subscriptions: make([]SubscriptionResponse, 0, len(cfg.Subscriptions))}
	for _, s := range cfg.Subscriptions {
		out.Subscriptions = append(out.Subscriptions, SubscriptionResponse{ID: s.ID, ARN: s.ARN, Events: nonNil(s.Events), Prefix: s.Prefix, Suffix: s.Suffix})
	}
	return out
}

func (h *notificationRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

func (h *notificationRoutes) get(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.deps.Notifications.Get(r.Context(), bucketName(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newNotificationsResponse(cfg))
}

func (h *notificationRoutes) put(w http.ResponseWriter, r *http.Request) {
	var in NotificationsResponse
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	cfg := adminsvc.NotificationConfig{Subscriptions: make([]adminsvc.Subscription, 0, len(in.Subscriptions))}
	for _, s := range in.Subscriptions {
		cfg.Subscriptions = append(cfg.Subscriptions, adminsvc.Subscription(s))
	}
	out, err := h.deps.Notifications.Set(r.Context(), bucketName(r), cfg)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newNotificationsResponse(out))
}

func (h *notificationRoutes) remove(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Notifications.Delete(r.Context(), bucketName(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *notificationRoutes) targets(w http.ResponseWriter, r *http.Request) {
	targets, err := h.deps.Notifications.Targets(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := NotifyTargetListResponse{Items: make([]NotifyTargetResponse, 0, len(targets))}
	for _, t := range targets {
		out.Items = append(out.Items, NotifyTargetResponse{
			ID: t.ID, Type: t.Type, ARN: t.ARN, Enabled: t.Enabled, Endpoint: t.Endpoint,
			Online: t.Online, PendingEvents: t.Pending, LastError: t.LastError,
		})
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

// test is POST /notify/targets/{id}/test, {id} being "<id>:<type>" or
// the ARN of the target.
func (h *notificationRoutes) test(w http.ResponseWriter, r *http.Request) {
	id, err := pathParam(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.deps.Notifications.TestTarget(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, TargetTestResponse(*res))
}
