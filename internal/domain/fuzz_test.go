package domain

import (
	"errors"
	"testing"
)

// FuzzParseRange checks the Range header parser (RFC 9110 single ranges,
// as S3): no panic, every error is ErrInvalidRange, and a resolved range
// always lies within the resource.
func FuzzParseRange(f *testing.F) {
	for _, seed := range []string{
		"bytes=0-0", "bytes=-500", "bytes=100-", "bytes=0-99", "bytes= 5 - 9 ", "bytes=9-5", "bytes=0-1,2-3",
		"bytes=-0", "bytes=99999999999999999999-", "bytes=-9223372036854775807", "items=0-1", "bytes=", "bytes=--1",
	} {
		f.Add(seed, int64(1000))
	}
	f.Add("bytes=0-", int64(0))
	f.Fuzz(func(t *testing.T, spec string, size int64) {
		r, err := ParseRange(spec)
		if err != nil {
			if !errors.Is(err, ErrInvalidRange) {
				t.Fatalf("ParseRange(%q): error %v is not ErrInvalidRange", spec, err)
			}
			return
		}
		offset, length, err := r.Resolve(size)
		if err != nil {
			if !errors.Is(err, ErrInvalidRange) {
				t.Fatalf("Resolve(%q, %d): error %v is not ErrInvalidRange", spec, size, err)
			}
			return
		}
		if offset < 0 || length < 0 || offset > size || length > size-offset {
			t.Fatalf("Resolve(%q, %d) = %d, %d: outside of the resource", spec, size, offset, length)
		}
		if size > 0 && length == 0 {
			t.Fatalf("Resolve(%q, %d): empty satisfiable range", spec, size)
		}
		_ = ContentRange(offset, length, size)
	})
}

// FuzzParseEndpoint checks the parser of the drive arguments: no panic, and
// a parsed endpoint renders and parses again to the same endpoint.
func FuzzParseEndpoint(f *testing.F) {
	for _, seed := range []string{
		"/data", "./d1", "http://node1:9000/mnt/disk1", "https://[::1]:9000/d", "http://host/d", "C:\\data",
		"http://user:pass@h/d", "ftp://h/d", "http://h:99999/d", "http://h:9000", "", "http://%zz/d",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		ep, err := ParseEndpoint(raw)
		if err != nil {
			return
		}
		again, err := ParseEndpoint(ep.String())
		if err != nil {
			t.Fatalf("ParseEndpoint(%q) = %q, which does not parse again: %v", raw, ep.String(), err)
		}
		if again.String() != ep.String() {
			t.Fatalf("ParseEndpoint(%q): %q then %q", raw, ep.String(), again.String())
		}
	})
}
