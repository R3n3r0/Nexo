package middleware

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

type (
	identityKey       struct{}
	identityHolderKey struct{}
)

// identityHolder carries the identity of a request up to the middlewares
// that run before its authentication (audit).
type identityHolder struct {
	mu sync.Mutex
	id *domain.Identity
}

// WithIdentity stores the identity in the context. When the context
// derives from one carrying an identity holder (WithIdentityHolder), the
// identity is recorded there too: the handlers that authenticate their own
// requests (presigned URLs, POST policy) report their signer this way.
func WithIdentity(ctx context.Context, id *domain.Identity) context.Context {
	if h, ok := ctx.Value(identityHolderKey{}).(*identityHolder); ok {
		h.mu.Lock()
		h.id = id
		h.mu.Unlock()
	}
	return context.WithValue(ctx, identityKey{}, id)
}

// WithIdentityHolder returns a context carrying a fresh identity holder: a
// middleware wrapping the authentication installs it before calling the
// next handler and reads, with HeldIdentity, the identity established
// below it (nil when the authentication failed).
func WithIdentityHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, identityHolderKey{}, &identityHolder{})
}

// HeldIdentity returns the last identity recorded in the holder of ctx by
// WithIdentity; the boolean is false when there is no holder or no
// identity.
func HeldIdentity(ctx context.Context) (*domain.Identity, bool) {
	h, ok := ctx.Value(identityHolderKey{}).(*identityHolder)
	if !ok {
		return nil, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.id, h.id != nil
}

// IdentityFromContext returns the identity set by the auth middleware. The
// boolean is false when no middleware ran (the identity is then nil).
func IdentityFromContext(ctx context.Context) (*domain.Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(*domain.Identity)
	return id, ok && id != nil
}

// AuthDeps are the dependencies of the authentication middleware.
type AuthDeps struct {
	// Verifier authenticates the requests (required).
	Verifier auth.Verifier
	// OnError writes authentication failures (default: S3 XML errors).
	OnError ErrorWriter
	// Skip selects the requests the middleware leaves alone (no
	// verification, no identity): routes that authenticate their own
	// requests, such as the metrics endpoints and their bearer tokens
	// (s3.MetricsRequest, which tells them from virtual-host object keys
	// with the same path). Nil skips nothing.
	Skip func(r *http.Request) bool
	// Service names the SigV4 service a request is addressed to
	// (auth.ServiceS3 or auth.ServiceSTS, see s3.SigningService): a
	// signature scoped to another service is refused. Nil addresses every
	// request to auth.ServiceS3.
	Service func(r *http.Request) string
}

type authMiddleware struct {
	deps AuthDeps
}

var _ Middleware = (*authMiddleware)(nil)

// NewAuth returns a middleware that authenticates requests with the
// verifier and stores the resulting identity in the context. Failures are
// written with OnError. Anonymous requests pass through with the anonymous
// identity: authorization is the policy engine's job. A missing verifier is
// a wiring error (domain.ErrInvalidArgument).
func NewAuth(deps AuthDeps) (Middleware, error) {
	if deps.Verifier == nil {
		return nil, fmt.Errorf("%w: auth middleware requires a verifier", domain.ErrInvalidArgument)
	}
	if deps.OnError == nil {
		deps.OnError = S3ErrorWriter(httpx.New())
	}
	return &authMiddleware{deps: deps}, nil
}

// Wrap implements Middleware.
func (m *authMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.deps.Skip != nil && m.deps.Skip(r) {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		if m.deps.Service != nil {
			ctx = auth.WithExpectedService(ctx, m.deps.Service(r))
		}
		id, err := m.deps.Verifier.Verify(ctx, r)
		if err != nil {
			m.deps.OnError(w, r, err)
			return
		}
		if id == nil {
			id = domain.Anonymous()
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(ctx, id)))
	})
}
