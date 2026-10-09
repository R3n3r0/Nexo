package dist

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/certs"
)

// ChallengePrefix is the path prefix of the HTTP-01 challenges.
const ChallengePrefix = "/.well-known/acme-challenge/"

// ChallengeDeps are the dependencies of the challenge routes.
type ChallengeDeps struct {
	// Certs answers the challenges (required).
	Certs  certs.Service
	Logger *slog.Logger
}

type challengeRoutes struct {
	deps ChallengeDeps
}

var _ ChallengeRoutes = (*challengeRoutes)(nil)

// NewChallengeRoutes returns the HTTP-01 challenge routes. A missing
// certificate service is a wiring error (domain.ErrInvalidArgument).
func NewChallengeRoutes(deps ChallengeDeps) (ChallengeRoutes, error) {
	if deps.Certs == nil {
		return nil, fmt.Errorf("%w: the challenge routes need the certificate service", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &challengeRoutes{deps: deps}, nil
}

// Register implements Routes.
func (h *challengeRoutes) Register(r chi.Router) {
	r.Get(ChallengePrefix+"{token}", h.serve)
}

func (h *challengeRoutes) serve(w http.ResponseWriter, r *http.Request) {
	keyAuth, err := h.deps.Certs.HTTPChallenge(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			h.deps.Logger.WarnContext(r.Context(), "dist: ACME challenge lookup failed", "err", err)
		}
		writePlain(w, r, http.StatusNotFound, "no such challenge")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(keyAuth))
}
