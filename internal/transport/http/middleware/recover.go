package middleware

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// ErrorWriter writes an error response; transports pick the S3 XML or the
// problem+json flavor.
type ErrorWriter func(w http.ResponseWriter, r *http.Request, err error)

// S3ErrorWriter returns an ErrorWriter producing S3 XML errors.
func S3ErrorWriter(enc httpx.Encoder) ErrorWriter {
	return enc.WriteS3Error
}

// ProblemErrorWriter returns an ErrorWriter producing problem+json.
func ProblemErrorWriter(enc httpx.Encoder) ErrorWriter {
	return enc.WriteProblemError
}

// RecoverDeps are the dependencies of the panic recovery middleware.
type RecoverDeps struct {
	// Logger records the panics (default slog.Default()).
	Logger *slog.Logger
	// OnError writes the 500 response (default: S3 XML errors).
	OnError ErrorWriter
}

type recoverer struct {
	deps RecoverDeps
}

var _ Middleware = (*recoverer)(nil)

// NewRecover returns a middleware that turns panics into 500 responses
// (written with OnError) and logs them with a stack trace.
// http.ErrAbortHandler is re-panicked as net/http expects.
func NewRecover(deps RecoverDeps) Middleware {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.OnError == nil {
		deps.OnError = S3ErrorWriter(httpx.New())
	}
	return &recoverer{deps: deps}
}

// Wrap implements Middleware.
func (m *recoverer) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			m.deps.Logger.ErrorContext(r.Context(), "panic serving request",
				"panic", fmt.Sprint(rec),
				"method", r.Method,
				"path", r.URL.Path,
				"request_id", httpx.RequestIDFrom(r.Context()),
				"stack", string(debug.Stack()),
			)
			m.deps.OnError(w, r, fmt.Errorf("panic: %v", rec))
		}()
		next.ServeHTTP(w, r)
	})
}
