package admin

import (
	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// UserDeps are the dependencies of the user routes.
type UserDeps struct {
	IAM iam.Service
	Enc httpx.Encoder
}

type userRoutes struct{ deps UserDeps }

var _ UserRoutes = (*userRoutes)(nil)

// NewUserRoutes returns the user, group and access-key routes.
//
// TODO(M3): implement on iam.Service.
func NewUserRoutes(deps UserDeps) UserRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &userRoutes{deps: deps}
}

// Register implements Routes.
func (h *userRoutes) Register(r chi.Router) {
	ni := notImplemented(h.deps.Enc)
	for _, p := range []string{"/users", "/users/{user}", "/groups", "/groups/{group}", "/access-keys", "/access-keys/{key}"} {
		r.Get(p, ni)
		r.Post(p, ni)
		r.Put(p, ni)
		r.Delete(p, ni)
	}
}

// PolicyDeps are the dependencies of the policy routes.
type PolicyDeps struct {
	Policy policy.Service
	Enc    httpx.Encoder
}

type policyRoutes struct{ deps PolicyDeps }

var _ PolicyRoutes = (*policyRoutes)(nil)

// NewPolicyRoutes returns the policy routes.
//
// TODO(M3): implement on policy.Service.
func NewPolicyRoutes(deps PolicyDeps) PolicyRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &policyRoutes{deps: deps}
}

// Register implements Routes.
func (h *policyRoutes) Register(r chi.Router) {
	ni := notImplemented(h.deps.Enc)
	for _, p := range []string{"/policies", "/policies/{policy}"} {
		r.Get(p, ni)
		r.Post(p, ni)
		r.Put(p, ni)
		r.Delete(p, ni)
	}
	r.Post("/policies/attach", ni)
	r.Post("/policies/detach", ni)
}

// BucketDeps are the dependencies of the admin bucket routes.
type BucketDeps struct {
	Buckets bucket.Service
	Enc     httpx.Encoder
}

type bucketRoutes struct{ deps BucketDeps }

var _ BucketRoutes = (*bucketRoutes)(nil)

// NewBucketRoutes returns the admin bucket routes.
//
// TODO(M3): implement on bucket.Service and object.Service.
func NewBucketRoutes(deps BucketDeps) BucketRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &bucketRoutes{deps: deps}
}

// Register implements Routes.
func (h *bucketRoutes) Register(r chi.Router) {
	ni := notImplemented(h.deps.Enc)
	r.Get("/buckets", ni)
	r.Post("/buckets", ni)
	r.Get("/buckets/{bucket}", ni)
	r.Delete("/buckets/{bucket}", ni)
	for _, s := range []string{"versioning", "policy", "lifecycle", "encryption", "objectlock", "quota", "tags", "events", "replication"} {
		r.Get("/buckets/{bucket}/"+s, ni)
		r.Put("/buckets/{bucket}/"+s, ni)
	}
	r.Get("/buckets/{bucket}/objects", ni)
	r.Post("/buckets/{bucket}/objects/upload", ni)
	r.Get("/buckets/{bucket}/objects/download", ni)
	r.Post("/buckets/{bucket}/objects/download-zip", ni)
	r.Delete("/buckets/{bucket}/objects", ni)
	r.Post("/buckets/{bucket}/objects/share", ni)
}

// ConfigDeps are the dependencies of the config routes.
type ConfigDeps struct {
	Config config.Service
	Enc    httpx.Encoder
}

type configRoutes struct{ deps ConfigDeps }

var _ ConfigRoutes = (*configRoutes)(nil)

// NewConfigRoutes returns the configuration routes.
//
// TODO(M3): implement on config.Service.
func NewConfigRoutes(deps ConfigDeps) ConfigRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &configRoutes{deps: deps}
}

// Register implements Routes.
func (h *configRoutes) Register(r chi.Router) {
	ni := notImplemented(h.deps.Enc)
	r.Get("/config", ni)
	r.Put("/config", ni)
	r.Get("/config/{subsys}", ni)
	r.Put("/config/{subsys}", ni)
	r.Get("/config/export", ni)
	r.Post("/config/import", ni)
}
