package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRange(t *testing.T) {
	tests := []struct {
		spec    string
		want    Range
		wantErr bool
	}{
		{"bytes=0-99", Range{Start: 0, End: 99}, false},
		{"bytes=100-", Range{Start: 100, End: -1}, false},
		{"bytes=-500", Range{IsSuffix: true, SuffixLength: 500, End: -1}, false},
		{" bytes=5-5 ", Range{Start: 5, End: 5}, false},
		{"bytes=0-0", Range{Start: 0, End: 0}, false},
		{"bytes=-0", Range{}, true},
		{"bytes=10-5", Range{}, true},
		{"bytes=a-b", Range{}, true},
		{"bytes=0-99,200-299", Range{}, true},
		{"bytes=", Range{}, true},
		{"bytes=-", Range{}, true},
		{"items=0-9", Range{}, true},
		{"", Range{}, true},
		{"bytes=+1-5", Range{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			got, err := ParseRange(tc.spec)
			if tc.wantErr {
				assert.ErrorIs(t, err, ErrInvalidRange)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, *got)
		})
	}
}

func TestRangeResolve(t *testing.T) {
	const size = 1000
	tests := []struct {
		name       string
		spec       string
		wantOffset int64
		wantLength int64
		wantErr    bool
	}{
		{"closed", "bytes=0-99", 0, 100, false},
		{"closed clamped", "bytes=900-1999", 900, 100, false},
		{"open", "bytes=500-", 500, 500, false},
		{"suffix", "bytes=-100", 900, 100, false},
		{"suffix larger than size", "bytes=-5000", 0, 1000, false},
		{"last byte", "bytes=999-999", 999, 1, false},
		{"start beyond size", "bytes=1000-", 0, 0, true},
		{"start beyond size closed", "bytes=1000-1001", 0, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := ParseRange(tc.spec)
			require.NoError(t, err)
			off, n, err := r.Resolve(size)
			if tc.wantErr {
				assert.ErrorIs(t, err, ErrInvalidRange)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantOffset, off)
			assert.Equal(t, tc.wantLength, n)
		})
	}

	// Regression: no range of an empty resource is satisfiable, suffix
	// ranges included (a 206 cannot describe zero bytes).
	for _, spec := range []string{"bytes=0-", "bytes=0-0", "bytes=-1", "bytes=-100"} {
		r, err := ParseRange(spec)
		require.NoError(t, err)
		_, _, err = r.Resolve(0)
		assert.ErrorIs(t, err, ErrInvalidRange, spec)
	}
	_, _, err := (&Range{IsSuffix: true}).Resolve(size)
	assert.ErrorIs(t, err, ErrInvalidRange, "an empty suffix serves nothing")

	var nilRange *Range
	off, n, err := nilRange.Resolve(size)
	require.NoError(t, err)
	assert.Equal(t, int64(0), off)
	assert.Equal(t, int64(size), n)

	assert.Equal(t, "bytes 0-99/1000", ContentRange(0, 100, size))
	assert.Equal(t, "bytes */1000", ContentRange(0, 0, size))
}
