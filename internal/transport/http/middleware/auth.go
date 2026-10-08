package middleware

import (
	"context"
	"net/http"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

type identityKey struct{}

// WithIdentity stores the identity in the context.
func WithIdentity(ctx context.Context, id *domain.Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFromContext returns the identity set by the auth middleware. The
// boolean is false when no middleware ran (the identity is then nil).
func IdentityFromContext(ctx context.Context) (*domain.Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(*domain.Identity)
	return id, ok && id != nil
}

type authMiddleware struct {
	verifier auth.Verifier
	onError  ErrorWriter
}

var _ Middleware = (*authMiddleware)(nil)

// NewAuth returns a middleware that authenticates requests with verifier and
// stores the resulting identity in the context. Failures are written with
// onError (S3 XML by default). Anonymous requests pass through with the
// anonymous identity: authorization is the policy engine's job.
func NewAuth(verifier auth.Verifier, onError ErrorWriter) Middleware {
	if onError == nil {
		onError = S3ErrorWriter(httpx.New())
	}
	return &authMiddleware{verifier: verifier, onError: onError}
}

// Wrap implements Middleware.
func (m *authMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.verifier == nil {
			// TODO(M2): the SigV4 verifier is not wired yet; everyone is anonymous.
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), domain.Anonymous())))
			return
		}
		id, err := m.verifier.Verify(r.Context(), r)
		if err != nil {
			m.onError(w, r, err)
			return
		}
		if id == nil {
			id = domain.Anonymous()
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
	})
}
