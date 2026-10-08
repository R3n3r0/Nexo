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

type recoverer struct {
	logger  *slog.Logger
	onError ErrorWriter
}

var _ Middleware = (*recoverer)(nil)

// NewRecover returns a middleware that turns panics into 500 responses
// (written with onError) and logs them with a stack trace.
// http.ErrAbortHandler is re-panicked as net/http expects.
func NewRecover(logger *slog.Logger, onError ErrorWriter) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	if onError == nil {
		onError = S3ErrorWriter(httpx.New())
	}
	return &recoverer{logger: logger, onError: onError}
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
			m.logger.ErrorContext(r.Context(), "panic serving request",
				"panic", fmt.Sprint(rec),
				"method", r.Method,
				"path", r.URL.Path,
				"request_id", httpx.RequestIDFrom(r.Context()),
				"stack", string(debug.Stack()),
			)
			m.onError(w, r, fmt.Errorf("panic: %v", rec))
		}()
		next.ServeHTTP(w, r)
	})
}
