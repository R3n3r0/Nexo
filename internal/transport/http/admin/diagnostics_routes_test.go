package admin_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
)

func TestProfileRoute(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		deny     bool
		want     adminsvc.ProfileInput
		svcErr   error
		status   int
		code     string
		noCallTo bool
	}{
		{name: "cpu by default", query: "", want: adminsvc.ProfileInput{Type: adminsvc.ProfileCPU}, status: http.StatusOK},
		{name: "heap", query: "?type=heap", want: adminsvc.ProfileInput{Type: adminsvc.ProfileHeap}, status: http.StatusOK},
		{name: "mutex for 3 seconds", query: "?type=MUTEX&seconds=3", want: adminsvc.ProfileInput{Type: adminsvc.ProfileMutex, Duration: 3 * time.Second}, status: http.StatusOK},
		{name: "invalid seconds", query: "?seconds=ten", status: http.StatusBadRequest, code: "invalid_argument", noCallTo: true},
		{name: "limits of the service", query: "?type=cpu&seconds=600", want: adminsvc.ProfileInput{Type: adminsvc.ProfileCPU, Duration: 600 * time.Second},
			svcErr: fmt.Errorf("%w: too long", domain.ErrInvalidArgument), status: http.StatusBadRequest, code: "invalid_argument"},
		{name: "one at a time", query: "?type=block", want: adminsvc.ProfileInput{Type: adminsvc.ProfileBlock},
			svcErr: fmt.Errorf("%w: busy", domain.ErrConflict), status: http.StatusConflict, code: "conflict"},
		{name: "denied without admin:Profiling", deny: true, status: http.StatusForbidden, code: "access_denied", noCallTo: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.deny {
				f.denyAll()
			}
			if !tt.noCallTo {
				call := f.diag.EXPECT().Profile(mock.Anything, tt.want)
				if tt.svcErr != nil {
					call.Return(nil, tt.svcErr)
				} else {
					call.Return(&adminsvc.Profile{Type: tt.want.Type, Node: "127.0.0.1:9000", Duration: tt.want.Duration, Data: []byte("pprof")}, nil)
				}
			}
			rec := f.root(t).do(http.MethodPost, "/profile"+tt.query, nil)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			if tt.code != "" {
				assert.Equal(t, tt.code, problemOf(t, rec).Code)
				return
			}
			assert.Equal(t, "pprof", rec.Body.String())
			assert.Equal(t, "application/octet-stream", rec.Header().Get("Content-Type"))
			assert.Equal(t, `attachment; filename="nexo-127.0.0.1-9000-`+string(tt.want.Type)+`.pb.gz"`, rec.Header().Get("Content-Disposition"))
			assert.Equal(t, "127.0.0.1:9000", rec.Header().Get("X-Nexo-Node"))
			assert.Equal(t, []policy.Action{policy.ActionAdminProfiling}, f.lastCheck().Actions)
		})
	}
}

func TestSpeedtestRoute(t *testing.T) {
	result := &adminsvc.SpeedtestResult{
		Kind: adminsvc.SpeedtestObject, Node: "local", Size: 1 << 20, Duration: 2 * time.Second, Concurrency: 4,
		Targets: []adminsvc.SpeedtestTarget{{Target: "objects",
			Write: adminsvc.SpeedtestMeasure{Bytes: 8 << 20, Operations: 8, Elapsed: 2 * time.Second, BytesPerSecond: 4 << 20, OperationsPerSecond: 4,
				LatencyP50: 1500 * time.Microsecond, LatencyP99: 3 * time.Millisecond}}},
	}
	tests := []struct {
		name     string
		kind     string
		body     any
		deny     bool
		want     adminsvc.SpeedtestInput
		svcErr   error
		status   int
		code     string
		noCallTo bool
	}{
		{name: "object with options", kind: "object", body: `{"size":1048576,"durationSeconds":2,"concurrency":4}`,
			want: adminsvc.SpeedtestInput{Kind: adminsvc.SpeedtestObject, Size: 1 << 20, Duration: 2 * time.Second, Concurrency: 4}, status: http.StatusOK},
		{name: "drive with the defaults", kind: "drive", want: adminsvc.SpeedtestInput{Kind: adminsvc.SpeedtestDrive}, status: http.StatusOK},
		{name: "net out of a cluster", kind: "net", want: adminsvc.SpeedtestInput{Kind: adminsvc.SpeedtestNet},
			svcErr: fmt.Errorf("%w: no peers", domain.ErrNotImplemented), status: http.StatusNotImplemented, code: "not_implemented"},
		{name: "unknown kind", kind: "gpu", want: adminsvc.SpeedtestInput{Kind: "gpu"},
			svcErr: fmt.Errorf("%w: kind", domain.ErrInvalidArgument), status: http.StatusBadRequest, code: "invalid_argument"},
		{name: "duration over the limit", kind: "object", body: `{"durationSeconds":3600}`, status: http.StatusBadRequest, code: "invalid_argument", noCallTo: true},
		{name: "malformed body", kind: "object", body: `{"size":`, status: http.StatusBadRequest, noCallTo: true},
		{name: "one at a time", kind: "drive", want: adminsvc.SpeedtestInput{Kind: adminsvc.SpeedtestDrive},
			svcErr: fmt.Errorf("%w: running", domain.ErrConflict), status: http.StatusConflict, code: "conflict"},
		{name: "denied without admin:Speedtest", kind: "drive", deny: true, status: http.StatusForbidden, code: "access_denied", noCallTo: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.deny {
				f.denyAll()
			}
			if !tt.noCallTo {
				call := f.diag.EXPECT().Speedtest(mock.Anything, tt.want)
				if tt.svcErr != nil {
					call.Return(nil, tt.svcErr)
				} else {
					call.Return(result, nil)
				}
			}
			rec := f.root(t).do(http.MethodPost, "/speedtest/"+tt.kind, tt.body)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			if tt.code != "" {
				assert.Equal(t, tt.code, problemOf(t, rec).Code)
			}
			if tt.status != http.StatusOK {
				return
			}
			got := decode[admin.SpeedtestResponse](t, rec)
			assert.Equal(t, "object", got.Kind)
			assert.InDelta(t, 2.0, got.DurationSeconds, 1e-9)
			require.Len(t, got.Targets, 1)
			assert.Equal(t, int64(8<<20), got.Targets[0].Write.Bytes)
			assert.InDelta(t, 1.5, got.Targets[0].Write.LatencyP50Ms, 1e-9)
			assert.Equal(t, []policy.Action{policy.ActionAdminSpeedtest}, f.lastCheck().Actions)
		})
	}
}
