package clitest

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// memNode is the node name of the in-memory diagnostics.
const memNode = "mem:9000"

// allowedDiagnosticsLocked reports whether the caller may profile and run
// speed tests: an administrator or a user of the diagnostics policy.
func (s *memSession) allowedDiagnosticsLocked() bool {
	return s.isAdminLocked() || slices.Contains(s.effectiveLocked(s.caller), "diagnostics")
}

// profile returns a fake profile named after its type.
func (s *memSession) profile(_ context.Context, in nexoadmin.ProfileRequest) (*nexoadmin.Profile, error) {
	if err := s.begin("Profile"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if !s.allowedDiagnosticsLocked() {
		return nil, apiError(http.StatusForbidden, nexoadmin.CodeAccessDenied, "admin:Profiling: no policy allows admin:Profiling")
	}
	kind := in.Type
	if kind == "" {
		kind = nexoadmin.ProfileCPU
	}
	if in.Seconds > 120 {
		return nil, apiError(http.StatusBadRequest, "invalid_argument", "profile duration must be between 1s and 2m0s")
	}
	return &nexoadmin.Profile{Type: kind, Node: memNode, FileName: fmt.Sprintf("nexo-mem-9000-%s.pb.gz", kind),
		Data: []byte("pprof:" + kind)}, nil
}

// speedtest returns a fixed result for a drive or object test; the net
// test needs a cluster.
func (s *memSession) speedtest(_ context.Context, kind string, in nexoadmin.SpeedtestRequest) (*nexoadmin.SpeedtestResult, error) {
	if err := s.begin("Speedtest"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if !s.allowedDiagnosticsLocked() {
		return nil, apiError(http.StatusForbidden, nexoadmin.CodeAccessDenied, "admin:Speedtest: no policy allows admin:Speedtest")
	}
	if kind == nexoadmin.SpeedtestNet {
		return nil, apiError(http.StatusNotImplemented, "not_implemented", "the network test needs the other nodes of a cluster")
	}
	size, duration, concurrency := in.Size, in.DurationSeconds, in.Concurrency
	if size == 0 {
		size = 4 << 20
	}
	if duration == 0 {
		duration = 5
	}
	if concurrency == 0 {
		concurrency = 8
	}
	m := nexoadmin.SpeedtestMeasure{Bytes: 10 * size, Operations: 10, ElapsedSeconds: duration,
		BytesPerSecond: float64(10*size) / duration, OperationsPerSecond: 10 / duration, LatencyP50Ms: 2, LatencyP99Ms: 5}
	target := "objects"
	if kind == nexoadmin.SpeedtestDrive {
		target = "/data1"
	}
	return &nexoadmin.SpeedtestResult{Kind: kind, Node: memNode, Size: size, DurationSeconds: duration, Concurrency: concurrency,
		Targets: []nexoadmin.SpeedtestTarget{{Target: target, Write: m, Read: m}}}, nil
}
