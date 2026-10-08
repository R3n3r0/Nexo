package middleware

import (
	"context"
	"net/http"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
)

// verifierAdapter completes fakeVerifier into a full auth.Verifier.
type verifierAdapter struct{ fakeVerifier }

func (a verifierAdapter) Verify(ctx context.Context, r *http.Request) (*domain.Identity, error) {
	return a.fakeVerifier.Verify(ctx, r)
}

func (a verifierAdapter) VerifyPresigned(ctx context.Context, r *http.Request) (*domain.Identity, error) {
	return a.fakeVerifier.VerifyPresigned(ctx, r)
}

func (a verifierAdapter) VerifyPostPolicy(context.Context, auth.PostForm) (*domain.Identity, *auth.PostPolicy, error) {
	return a.id, nil, a.err
}

var _ auth.Verifier = verifierAdapter{}
