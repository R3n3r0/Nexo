package admin

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// DiagnosticsRoutes exposes the diagnostics of the node that serves the
// request (S-OPS-11): GET /profile (a pprof profile, admin:Profiling) and
// POST /speedtest/{kind} (drive, object and net speed tests,
// admin:Speedtest).
type DiagnosticsRoutes interface{ Routes }

// DiagnosticsDeps are the dependencies of the diagnostics routes.
type DiagnosticsDeps struct {
	Diagnostics adminsvc.Diagnostics
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// DiagnosticsOption configures the diagnostics routes (none yet: the type
// keeps the constructor shape of every route group, docs/12 §12.3).
type DiagnosticsOption func(*diagnosticsRoutes)

type diagnosticsRoutes struct {
	deps  DiagnosticsDeps
	guard guard
}

var _ DiagnosticsRoutes = (*diagnosticsRoutes)(nil)

// NewDiagnosticsRoutes returns the profiling and speed test routes.
func NewDiagnosticsRoutes(deps DiagnosticsDeps, opts ...DiagnosticsOption) (DiagnosticsRoutes, error) {
	if deps.Diagnostics == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: diagnostics routes require the diagnostics service and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &diagnosticsRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *diagnosticsRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *diagnosticsRoutes) routes() []route {
	return []route{
		{http.MethodGet, "/profile", global(policy.ActionAdminProfiling), h.profile},
		{http.MethodPost, "/speedtest/{kind}", global(policy.ActionAdminSpeedtest), h.speedtest},
	}
}

// profile answers GET /profile?type=cpu|heap|goroutine|mutex|block&seconds=N
// with the profile in the pprof format.
func (h *diagnosticsRoutes) profile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	in := adminsvc.ProfileInput{Type: adminsvc.ProfileType(strings.ToLower(q.Get("type")))}
	if in.Type == "" {
		in.Type = adminsvc.ProfileCPU
	}
	if v := q.Get("seconds"); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil || secs < 0 {
			writeError(h.deps.Enc, w, r, fmt.Errorf("%w: seconds must be a non-negative integer", domain.ErrInvalidArgument))
			return
		}
		in.Duration = time.Duration(secs) * time.Second
	}
	p, err := h.deps.Diagnostics.Profile(r.Context(), in)
	if err != nil {
		writeError(h.deps.Enc, w, r, err)
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, profileFileName(p)))
	hdr.Set("Content-Length", strconv.Itoa(len(p.Data)))
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Nexo-Node", p.Node)
	hdr.Set("X-Nexo-Profile-Type", string(p.Type))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(p.Data)
}

// profileFileName names a profile download: nexo-<node>-<type>.pb.gz,
// the node reduced to letters, digits, '.' and '-'.
func profileFileName(p *adminsvc.Profile) string {
	node := strings.Map(func(c rune) rune {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-':
			return c
		}
		return '-'
	}, p.Node)
	return "nexo-" + node + "-" + string(p.Type) + ".pb.gz"
}

// SpeedtestRequest is the JSON body of POST /speedtest/{kind}; every
// field is optional.
type SpeedtestRequest struct {
	// Size of each file or object, in bytes.
	Size int64 `json:"size,omitempty"`
	// DurationSeconds of each phase (write, then read).
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
	// Concurrency of the object test and of the reads of the net test.
	Concurrency int `json:"concurrency,omitempty"`
}

// SpeedtestResponse is the result of a speed test.
type SpeedtestResponse struct {
	Kind            string                    `json:"kind"`
	Node            string                    `json:"node"`
	Size            int64                     `json:"size"`
	DurationSeconds float64                   `json:"durationSeconds"`
	Concurrency     int                       `json:"concurrency"`
	Targets         []SpeedtestTargetResponse `json:"targets"`
}

// SpeedtestTargetResponse is the measure of a drive, a peer node or the
// object layer.
type SpeedtestTargetResponse struct {
	Target string                   `json:"target"`
	Write  SpeedtestMeasureResponse `json:"write"`
	Read   SpeedtestMeasureResponse `json:"read"`
	Error  string                   `json:"error,omitempty"`
}

// SpeedtestMeasureResponse is the throughput of a phase.
type SpeedtestMeasureResponse struct {
	Bytes               int64   `json:"bytes"`
	Operations          int64   `json:"operations"`
	ElapsedSeconds      float64 `json:"elapsedSeconds"`
	BytesPerSecond      float64 `json:"bytesPerSecond"`
	OperationsPerSecond float64 `json:"operationsPerSecond"`
	LatencyP50Ms        float64 `json:"latencyP50Ms"`
	LatencyP99Ms        float64 `json:"latencyP99Ms"`
}

func (h *diagnosticsRoutes) speedtest(w http.ResponseWriter, r *http.Request) {
	kind, err := pathParam(r, "kind")
	if err != nil {
		writeError(h.deps.Enc, w, r, err)
		return
	}
	var in SpeedtestRequest
	if err := decodeJSON(h.deps.Enc, r, &in, true); err != nil {
		writeError(h.deps.Enc, w, r, err)
		return
	}
	if in.DurationSeconds < 0 || in.DurationSeconds > adminsvc.MaxSpeedtestDuration.Seconds() {
		writeError(h.deps.Enc, w, r, fmt.Errorf("%w: durationSeconds must be between 0 and %g", domain.ErrInvalidArgument,
			adminsvc.MaxSpeedtestDuration.Seconds()))
		return
	}
	res, err := h.deps.Diagnostics.Speedtest(r.Context(), adminsvc.SpeedtestInput{
		Kind: adminsvc.SpeedtestKind(kind), Size: in.Size, Concurrency: in.Concurrency,
		Duration: time.Duration(in.DurationSeconds * float64(time.Second)),
	})
	if err != nil {
		writeError(h.deps.Enc, w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newSpeedtestResponse(res))
}

func newSpeedtestResponse(res *adminsvc.SpeedtestResult) SpeedtestResponse {
	out := SpeedtestResponse{
		Kind: string(res.Kind), Node: res.Node, Size: res.Size, DurationSeconds: res.Duration.Seconds(),
		Concurrency: res.Concurrency, Targets: make([]SpeedtestTargetResponse, 0, len(res.Targets)),
	}
	for _, t := range res.Targets {
		out.Targets = append(out.Targets, SpeedtestTargetResponse{
			Target: t.Target, Write: newSpeedtestMeasure(t.Write), Read: newSpeedtestMeasure(t.Read), Error: t.Error,
		})
	}
	return out
}

func newSpeedtestMeasure(m adminsvc.SpeedtestMeasure) SpeedtestMeasureResponse {
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	return SpeedtestMeasureResponse{
		Bytes: m.Bytes, Operations: m.Operations, ElapsedSeconds: m.Elapsed.Seconds(),
		BytesPerSecond: m.BytesPerSecond, OperationsPerSecond: m.OperationsPerSecond,
		LatencyP50Ms: ms(m.LatencyP50), LatencyP99Ms: ms(m.LatencyP99),
	}
}
