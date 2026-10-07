package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// Range is a parsed HTTP byte range (RFC 9110 §14). Only single ranges are
// supported, as in S3. A Range is either a suffix range ("bytes=-N"), an
// open range ("bytes=S-") or a closed range ("bytes=S-E").
type Range struct {
	// Start is the first byte offset (ignored for suffix ranges).
	Start int64
	// End is the last byte offset, -1 for open ranges.
	End int64
	// IsSuffix reports a suffix range; SuffixLength is then its length.
	IsSuffix     bool
	SuffixLength int64
}

// ParseRange parses a Range header value such as "bytes=0-99". It returns
// ErrInvalidRange for syntactically invalid specs and multiple ranges. The
// range is validated against the content size by Resolve.
func ParseRange(spec string) (*Range, error) {
	spec = strings.TrimSpace(spec)
	const prefix = "bytes="
	if !strings.HasPrefix(spec, prefix) {
		return nil, fmt.Errorf("%w: %q does not start with %q", ErrInvalidRange, spec, prefix)
	}
	body := strings.TrimSpace(spec[len(prefix):])
	if body == "" || strings.Contains(body, ",") {
		return nil, fmt.Errorf("%w: %q must contain exactly one range", ErrInvalidRange, spec)
	}
	dash := strings.IndexByte(body, '-')
	if dash < 0 {
		return nil, fmt.Errorf("%w: %q has no '-'", ErrInvalidRange, spec)
	}
	first, last := strings.TrimSpace(body[:dash]), strings.TrimSpace(body[dash+1:])
	if first == "" {
		n, err := parseRangeInt(last)
		if err != nil {
			return nil, fmt.Errorf("%w: %q: bad suffix length", ErrInvalidRange, spec)
		}
		if n == 0 {
			return nil, fmt.Errorf("%w: %q: suffix length must be positive", ErrInvalidRange, spec)
		}
		return &Range{IsSuffix: true, SuffixLength: n, End: -1}, nil
	}
	start, err := parseRangeInt(first)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: bad start", ErrInvalidRange, spec)
	}
	if last == "" {
		return &Range{Start: start, End: -1}, nil
	}
	end, err := parseRangeInt(last)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: bad end", ErrInvalidRange, spec)
	}
	if end < start {
		return nil, fmt.Errorf("%w: %q: end before start", ErrInvalidRange, spec)
	}
	return &Range{Start: start, End: end}, nil
}

func parseRangeInt(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("not a number")
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

// Resolve computes the byte offset and length to serve for a resource of the
// given size. It returns ErrInvalidRange when the range is unsatisfiable
// (start beyond the end of the resource).
func (r *Range) Resolve(size int64) (offset, length int64, err error) {
	if r == nil {
		return 0, size, nil
	}
	if size < 0 {
		return 0, 0, fmt.Errorf("%w: negative size", ErrInvalidRange)
	}
	if r.IsSuffix {
		if r.SuffixLength >= size {
			return 0, size, nil
		}
		return size - r.SuffixLength, r.SuffixLength, nil
	}
	if r.Start >= size {
		return 0, 0, fmt.Errorf("%w: start %d beyond size %d", ErrInvalidRange, r.Start, size)
	}
	end := r.End
	if end < 0 || end >= size {
		end = size - 1
	}
	return r.Start, end - r.Start + 1, nil
}

// ContentRange renders the Content-Range header value for a resolved range.
func ContentRange(offset, length, size int64) string {
	if length <= 0 {
		return fmt.Sprintf("bytes */%d", size)
	}
	return fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, size)
}
