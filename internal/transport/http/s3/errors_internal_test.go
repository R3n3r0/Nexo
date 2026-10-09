package s3

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// TestWriteErrorLogsServerFailures checks that the server failures (5xx)
// are logged with their cause, which the response never carries, and that
// the client errors are not.
func TestWriteErrorLogsServerFailures(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		status    int
		wantLevel string
	}{
		{name: "internal", err: errors.New("disk exploded"), status: http.StatusInternalServerError, wantLevel: "level=ERROR"},
		{name: "quorum", err: fmt.Errorf("delete: %w", domain.ErrWriteQuorum), status: http.StatusServiceUnavailable, wantLevel: "level=WARN"},
		{name: "client error", err: domain.ErrBucketNotFound, status: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodDelete, "/bkt", nil)
			writeError(httpx.New(), logger, w, r, tt.err)
			assert.Equal(t, tt.status, w.Code)
			if tt.wantLevel == "" {
				assert.Empty(t, logs.String())
				return
			}
			assert.Contains(t, logs.String(), tt.wantLevel)
			assert.Contains(t, logs.String(), tt.err.Error())
			assert.NotContains(t, w.Body.String(), tt.err.Error(), "the cause stays in the logs")
		})
	}
}
