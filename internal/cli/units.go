package cli

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// sizeUnits are the binary units of HumanSize.
const sizeUnits = "KMGTPE"

// HumanSize renders a byte count with binary units: "0B", "1023B",
// "1.0KiB", "5.5MiB".
func HumanSize(n int64) string {
	if n < 0 {
		return "-" + HumanSize(-n)
	}
	if n < 1024 {
		return strconv.FormatInt(n, 10) + "B"
	}
	v := float64(n)
	i := -1
	for v >= 1024 && i < len(sizeUnits)-1 {
		v /= 1024
		i++
	}
	// Avoid "1024.0KiB" after rounding.
	if math.Round(v*10)/10 >= 1024 && i < len(sizeUnits)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f%ciB", v, sizeUnits[i])
}

// ParseSize parses a size: a number of bytes, optionally followed by a
// decimal (KB, MB, GB, TB) or binary (KiB, MiB, GiB, TiB) unit, case
// insensitive ("10MiB", "1.5GB", "512k", "100").
func ParseSize(s string) (int64, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return 0, UsageError("empty size")
	}
	i := 0
	for i < len(in) && (in[i] == '.' || (in[i] >= '0' && in[i] <= '9')) {
		i++
	}
	num, unit := in[:i], strings.ToLower(strings.TrimSpace(in[i:]))
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v < 0 {
		return 0, UsageError("invalid size %q", s)
	}
	mult := float64(1)
	switch strings.TrimSuffix(unit, "b") {
	case "":
	case "k":
		mult = 1e3
	case "m":
		mult = 1e6
	case "g":
		mult = 1e9
	case "t":
		mult = 1e12
	case "ki":
		mult = 1 << 10
	case "mi":
		mult = 1 << 20
	case "gi":
		mult = 1 << 30
	case "ti":
		mult = 1 << 40
	default:
		return 0, UsageError("invalid size unit in %q (B, KB, MB, GB, TB, KiB, MiB, GiB, TiB)", s)
	}
	out := v * mult
	if out > math.MaxInt64 {
		return 0, UsageError("size %q is too large", s)
	}
	return int64(math.Round(out)), nil
}

// ParseDuration parses a duration with the units of time.ParseDuration
// plus "d" (24h) and "w" (7d): "7d", "1d12h", "90m", "2w".
func ParseDuration(s string) (time.Duration, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return 0, UsageError("empty duration")
	}
	var total time.Duration
	rest := in
	for rest != "" {
		i := 0
		for i < len(rest) && (rest[i] == '.' || unicode.IsDigit(rune(rest[i]))) {
			i++
		}
		j := i
		for j < len(rest) && unicode.IsLetter(rune(rest[j])) {
			j++
		}
		if i == 0 || j == i {
			return 0, UsageError("invalid duration %q (e.g. 30s, 15m, 2h, 7d, 1w)", s)
		}
		num, unit := rest[:i], rest[i:j]
		rest = rest[j:]
		switch unit {
		case "d", "w":
			v, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, UsageError("invalid duration %q", s)
			}
			day := 24 * time.Hour
			if unit == "w" {
				day *= 7
			}
			total += time.Duration(v * float64(day))
		default:
			d, err := time.ParseDuration(num + unit)
			if err != nil {
				return 0, UsageError("invalid duration %q (e.g. 30s, 15m, 2h, 7d, 1w)", s)
			}
			total += d
		}
	}
	if total < 0 {
		return 0, UsageError("negative duration %q", s)
	}
	return total, nil
}

// HumanDuration renders a duration rounded to the second, with days when
// longer than one: "1h0m0s", "7d0s", "2d3h0m0s".
func HumanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	if days == 0 {
		return d.String()
	}
	return fmt.Sprintf("%dd%s", days, d.String())
}

// ParseMetadata parses user metadata "k1=v1;k2=v2" (the keys may carry
// the "x-amz-meta-" prefix, which is removed; keys are lower-cased).
func ParseMetadata(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		k = strings.TrimPrefix(k, "x-amz-meta-")
		if !ok || k == "" {
			return nil, UsageError("invalid metadata %q: use key=value pairs separated by ';'", pair)
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, nil
}

// ParseTags parses tags in URL query form "k1=v1&k2=v2".
func ParseTags(s string) (map[string]string, error) {
	values, err := url.ParseQuery(s)
	if err != nil {
		return nil, UsageError("invalid tags %q: use key=value pairs separated by '&'", s)
	}
	out := make(map[string]string, len(values))
	for k, vs := range values {
		if k == "" {
			return nil, UsageError("invalid tags %q: empty key", s)
		}
		if len(vs) > 1 {
			return nil, UsageError("invalid tags %q: duplicate key %q", s, k)
		}
		out[k] = vs[0]
	}
	return out, nil
}

// FormatTags renders tags in URL query form, sorted by key.
func FormatTags(tags map[string]string) string {
	keys := SortedKeys(tags)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(tags[k]))
	}
	return strings.Join(parts, "&")
}

// SortedKeys returns the keys of a map, sorted.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
