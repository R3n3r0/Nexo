package domain

import (
	"fmt"
	"strings"
)

// Mode is the deployment mode, derived from the server endpoints.
type Mode int

const (
	// ModeUnknown is the zero value: the mode has not been determined.
	ModeUnknown Mode = iota
	// ModeSNSD is single node, single drive: no erasure coding.
	ModeSNSD
	// ModeSNMD is single node, multiple drives with erasure coding.
	ModeSNMD
	// ModeDistributed is multiple nodes, multiple drives (cluster).
	ModeDistributed
)

var modeNames = map[Mode]string{
	ModeUnknown:     "unknown",
	ModeSNSD:        "snsd",
	ModeSNMD:        "snmd",
	ModeDistributed: "distributed",
}

// String returns the canonical lowercase name of the mode.
func (m Mode) String() string {
	if s, ok := modeNames[m]; ok {
		return s
	}
	return fmt.Sprintf("Mode(%d)", int(m))
}

// IsErasure reports whether the mode uses erasure coding.
func (m Mode) IsErasure() bool { return m == ModeSNMD || m == ModeDistributed }

// IsDistributed reports whether the mode spans multiple nodes.
func (m Mode) IsDistributed() bool { return m == ModeDistributed }

// ParseMode parses a mode name. It accepts the canonical names plus the
// aliases "cluster" and "mnmd" for ModeDistributed and "erasure" for ModeSNMD.
// The comparison is case-insensitive.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "snsd", "single", "fs":
		return ModeSNSD, nil
	case "snmd", "erasure":
		return ModeSNMD, nil
	case "distributed", "cluster", "mnmd":
		return ModeDistributed, nil
	default:
		return ModeUnknown, fmt.Errorf("%w: unknown mode %q", ErrInvalidArgument, s)
	}
}

// MarshalText implements encoding.TextMarshaler.
func (m Mode) MarshalText() ([]byte, error) { return []byte(m.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (m *Mode) UnmarshalText(b []byte) error {
	parsed, err := ParseMode(string(b))
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
