package cache

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// --- Duration ------------------------------------------------------------------------

const day = 24 * time.Hour

// ParseDuration parses a duration of the cache configuration: the Go
// syntax ("1h30m") or a number of days ("365d", "1.5d"). The settings
// refuse the negative ones (an age past the expiry is negative).
func ParseDuration(s string) (Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("%w: empty duration", domain.ErrInvalidArgument)
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil || math.Abs(n*float64(day)) > math.MaxInt64 {
			return 0, fmt.Errorf("%w: duration %q", domain.ErrInvalidArgument, s)
		}
		return Duration(time.Duration(n * float64(day))), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%w: duration %q", domain.ErrInvalidArgument, s)
	}
	return Duration(d), nil
}

// D returns the duration as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String formats whole days as "<n>d", other durations in the Go syntax.
func (d Duration) String() string {
	td := time.Duration(d)
	if td != 0 && td%day == 0 {
		return strconv.FormatInt(int64(td/day), 10) + "d"
	}
	return td.String()
}

// MarshalText implements encoding.TextMarshaler (JSON strings, YAML).
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// UnmarshalJSON accepts a string or a number of seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		return d.UnmarshalText([]byte(s))
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("%w: duration %s", domain.ErrInvalidArgument, b)
	}
	if n < 0 || n > math.MaxInt64/float64(time.Second) {
		return fmt.Errorf("%w: duration %s", domain.ErrInvalidArgument, b)
	}
	*d = Duration(time.Duration(n * float64(time.Second)))
	return nil
}

// --- Size ----------------------------------------------------------------------------

// sizeUnits are the units of ParseSize, longest suffix first.
func sizeUnits() []struct {
	suffix string
	mult   int64
} {
	return []struct {
		suffix string
		mult   int64
	}{
		{"tib", 1 << 40}, {"gib", 1 << 30}, {"mib", 1 << 20}, {"kib", 1 << 10},
		{"tb", 1e12}, {"gb", 1e9}, {"mb", 1e6}, {"kb", 1e3},
		{"t", 1 << 40}, {"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10},
		{"b", 1},
	}
}

// ParseSize parses a size: bytes, or a number with a unit (KiB, MiB,
// GiB, TiB, KB, MB, GB, TB, K, M, G, T: the single letters are binary).
func ParseSize(s string) (Size, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return 0, fmt.Errorf("%w: empty size", domain.ErrInvalidArgument)
	}
	mult := int64(1)
	for _, u := range sizeUnits() {
		if strings.HasSuffix(t, u.suffix) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suffix)), u.mult
			break
		}
	}
	n, err := strconv.ParseFloat(t, 64)
	if err != nil || n < 0 || n*float64(mult) > math.MaxInt64 {
		return 0, fmt.Errorf("%w: size %q", domain.ErrInvalidArgument, s)
	}
	return Size(int64(n * float64(mult))), nil
}

// String formats the size with the largest binary unit dividing it.
func (s Size) String() string {
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if s != 0 && int64(s)%u.mult == 0 {
			return strconv.FormatInt(int64(s)/u.mult, 10) + u.suffix
		}
	}
	return strconv.FormatInt(int64(s), 10)
}

// MarshalText implements encoding.TextMarshaler.
func (s Size) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Size) UnmarshalText(b []byte) error {
	v, err := ParseSize(string(b))
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// UnmarshalJSON accepts a string or a number of bytes.
func (s *Size) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		return s.UnmarshalText([]byte(str))
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil || n < 0 || n > math.MaxInt64 {
		return fmt.Errorf("%w: size %s", domain.ErrInvalidArgument, b)
	}
	*s = Size(int64(n))
	return nil
}

// --- QueryString ---------------------------------------------------------------------

// ParseQueryString parses the string form of a query string mode: "none",
// "all", "allowlist:v,w", "denylist:utm_source".
func ParseQueryString(s string) (QueryString, error) {
	mode, list, _ := strings.Cut(strings.TrimSpace(s), ":")
	q := QueryString{Mode: strings.ToLower(strings.TrimSpace(mode))}
	for _, p := range strings.Split(list, ",") {
		if p = strings.TrimSpace(p); p != "" {
			q.Params = append(q.Params, p)
		}
	}
	return q, validateQueryString(q)
}

// String is the string form of the mode ("allowlist:v,w").
func (q QueryString) String() string {
	if len(q.Params) == 0 {
		return q.Mode
	}
	return q.Mode + ":" + strings.Join(q.Params, ",")
}

// UnmarshalJSON accepts the string form, an object {"mode", "params"} or
// the short objects {"allowlist": [...]} and {"denylist": [...]}.
func (q *QueryString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := ParseQueryString(s)
		if err != nil {
			return err
		}
		*q = v
		return nil
	}
	var raw struct {
		Mode      string   `json:"mode"`
		Params    []string `json:"params"`
		Allowlist []string `json:"allowlist"`
		Denylist  []string `json:"denylist"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("%w: queryString: %v", domain.ErrInvalidArgument, err)
	}
	switch {
	case raw.Allowlist != nil:
		*q = QueryString{Mode: QueryAllowlist, Params: raw.Allowlist}
	case raw.Denylist != nil:
		*q = QueryString{Mode: QueryDenylist, Params: raw.Denylist}
	default:
		*q = QueryString{Mode: strings.ToLower(raw.Mode), Params: raw.Params}
	}
	return nil
}

// --- helpers ----------------------------------------------------------------------------

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// boolOf returns *p, or def when p is nil.
func boolOf(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}
