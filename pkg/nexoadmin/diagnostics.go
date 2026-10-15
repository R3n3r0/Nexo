package nexoadmin

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Profile types (ProfileRequest.Type).
const (
	ProfileCPU       = "cpu"
	ProfileHeap      = "heap"
	ProfileGoroutine = "goroutine"
	ProfileMutex     = "mutex"
	ProfileBlock     = "block"
)

// Speed tests (Speedtest kind).
const (
	SpeedtestDrive  = "drive"
	SpeedtestObject = "object"
	SpeedtestNet    = "net"
)

// diagnosticsMargin is added to the expected duration of a profile or a
// speed test for the timeout of its request.
const diagnosticsMargin = 2 * time.Minute

// ProfileRequest selects a runtime profile of the node that serves the
// request.
type ProfileRequest struct {
	// Type is ProfileCPU (default), ProfileHeap, ProfileGoroutine,
	// ProfileMutex or ProfileBlock.
	Type string
	// Seconds of sampling of the cpu, mutex and block profiles (0: the
	// server default, 10).
	Seconds int
}

// Profile is a profile in the pprof format (go tool pprof).
type Profile struct {
	Type string
	// Node is the node that collected it.
	Node string
	// FileName is the name the server suggests (nexo-<node>-<type>.pb.gz).
	FileName string
	Data     []byte
}

// SpeedtestRequest are the options of a speed test; zero values select
// the defaults of the server.
type SpeedtestRequest struct {
	Size            int64   `json:"size,omitempty"`
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
	Concurrency     int     `json:"concurrency,omitempty"`
}

// SpeedtestResult is the result of a speed test.
type SpeedtestResult struct {
	Kind            string            `json:"kind"`
	Node            string            `json:"node"`
	Size            int64             `json:"size"`
	DurationSeconds float64           `json:"durationSeconds"`
	Concurrency     int               `json:"concurrency"`
	Targets         []SpeedtestTarget `json:"targets"`
}

// SpeedtestTarget is the measure of a drive, a peer node or the object
// layer.
type SpeedtestTarget struct {
	Target string           `json:"target"`
	Write  SpeedtestMeasure `json:"write"`
	Read   SpeedtestMeasure `json:"read"`
	Error  string           `json:"error,omitempty"`
}

// SpeedtestMeasure is the throughput of a phase.
type SpeedtestMeasure struct {
	Bytes               int64   `json:"bytes"`
	Operations          int64   `json:"operations"`
	ElapsedSeconds      float64 `json:"elapsedSeconds"`
	BytesPerSecond      float64 `json:"bytesPerSecond"`
	OperationsPerSecond float64 `json:"operationsPerSecond"`
	LatencyP50Ms        float64 `json:"latencyP50Ms"`
	LatencyP99Ms        float64 `json:"latencyP99Ms"`
}

// Profile implements AdminClient.
func (c *client) Profile(ctx context.Context, in ProfileRequest) (*Profile, error) {
	q := url.Values{}
	if in.Type != "" {
		q.Set("type", in.Type)
	}
	if in.Seconds > 0 {
		q.Set("seconds", strconv.Itoa(in.Seconds))
	}
	data, hdr, err := c.send(ctx, request{method: http.MethodGet, path: "/profile", query: q,
		timeout: time.Duration(in.Seconds)*time.Second + diagnosticsMargin})
	if err != nil {
		return nil, err
	}
	p := &Profile{Type: hdr.Get("X-Nexo-Profile-Type"), Node: hdr.Get("X-Nexo-Node"), Data: data}
	if _, name, ok := strings.Cut(hdr.Get("Content-Disposition"), `filename="`); ok {
		p.FileName = strings.TrimSuffix(name, `"`)
	}
	return p, nil
}

// Speedtest implements AdminClient.
func (c *client) Speedtest(ctx context.Context, kind string, in SpeedtestRequest) (*SpeedtestResult, error) {
	var out SpeedtestResult
	// Two phases (write, read) plus the cleanup.
	budget := 2*time.Duration(in.DurationSeconds*float64(time.Second)) + diagnosticsMargin
	return &out, c.do(ctx, request{method: http.MethodPost, path: pathOf("speedtest", kind), body: in, timeout: budget}, &out)
}
