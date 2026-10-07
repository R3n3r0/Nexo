package topology

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TS-U-01: ellipsis expansion, valid and invalid patterns.
func TestExpandArg_Valid(t *testing.T) {
	t.Parallel()
	seq := func(prefix string, lo, hi int, suffix string) []string {
		out := make([]string, 0, hi-lo+1)
		for i := lo; i <= hi; i++ {
			out = append(out, fmt.Sprintf("%s%d%s", prefix, i, suffix))
		}
		return out
	}
	tests := []struct {
		name string
		arg  string
		want []string
	}{
		{"no ellipsis", "/data", []string{"/data"}},
		{"no ellipsis url", "http://node1:9000/data", []string{"http://node1:9000/data"}},
		{"numeric", "/data{1...4}", seq("/data", 1, 4, "")},
		{"numeric single", "/data{7...7}", []string{"/data7"}},
		{"numeric from zero", "/data{0...3}", []string{"/data0", "/data1", "/data2", "/data3"}},
		{"numeric two digits", "/data{10...12}", []string{"/data10", "/data11", "/data12"}},
		{"zero padded", "/data{01...04}", []string{"/data01", "/data02", "/data03", "/data04"}},
		{"zero padded crossing ten", "/data{08...11}", []string{"/data08", "/data09", "/data10", "/data11"}},
		{"zero padded short end", "/data{001...3}", []string{"/data001", "/data002", "/data003"}},
		{"zero padded wider end", "/d{09...10}", []string{"/d09", "/d10"}},
		{"alpha lower", "/data{a...d}", []string{"/dataa", "/datab", "/datac", "/datad"}},
		{"alpha upper", "/data{A...C}", []string{"/dataA", "/dataB", "/dataC"}},
		{"alpha single", "/data{x...x}", []string{"/datax"}},
		{"suffix", "/data{1...3}/nexo", []string{"/data1/nexo", "/data2/nexo", "/data3/nexo"}},
		{"middle of path", "/mnt/disk{1...2}/data", []string{"/mnt/disk1/data", "/mnt/disk2/data"}},
		{
			"two ellipses first varies fastest",
			"http://node{1...2}:9000/data{1...2}",
			[]string{
				"http://node1:9000/data1", "http://node2:9000/data1",
				"http://node1:9000/data2", "http://node2:9000/data2",
			},
		},
		{
			"ellipsis in port",
			"http://node{1...2}.lan:900{1...2}/d",
			[]string{
				"http://node1.lan:9001/d", "http://node2.lan:9001/d",
				"http://node1.lan:9002/d", "http://node2.lan:9002/d",
			},
		},
		{
			"alpha then numeric",
			"/{a...b}/data{1...2}",
			[]string{"/a/data1", "/b/data1", "/a/data2", "/b/data2"},
		},
		{
			"three ellipses",
			"/p{1...2}/q{1...2}/r{1...2}",
			[]string{
				"/p1/q1/r1", "/p2/q1/r1", "/p1/q2/r1", "/p2/q2/r1",
				"/p1/q1/r2", "/p2/q1/r2", "/p1/q2/r2", "/p2/q2/r2",
			},
		},
		{"mixed padding", "http://n{01...02}:9000/d{1...2}", []string{"http://n01:9000/d1", "http://n02:9000/d1", "http://n01:9000/d2", "http://n02:9000/d2"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := expandArg(tc.arg)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestExpandArg_LargeRanges(t *testing.T) {
	t.Parallel()
	got, err := expandArg("/data{1...100}")
	require.NoError(t, err)
	require.Len(t, got, 100)
	assert.Equal(t, "/data1", got[0])
	assert.Equal(t, "/data100", got[99])

	got, err = expandArg("/data{001...1000}")
	require.NoError(t, err)
	require.Len(t, got, 1000)
	assert.Equal(t, "/data001", got[0])
	assert.Equal(t, "/data010", got[9])
	assert.Equal(t, "/data100", got[99])
	assert.Equal(t, "/data1000", got[999])

	got, err = expandArg("http://node{1...32}:9000/data{1...32}")
	require.NoError(t, err)
	require.Len(t, got, 1024)
	assert.Equal(t, "http://node1:9000/data1", got[0])
	assert.Equal(t, "http://node32:9000/data1", got[31])
	assert.Equal(t, "http://node1:9000/data2", got[32])
	assert.Equal(t, "http://node32:9000/data32", got[1023])
}

func TestExpandArg_Invalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		arg  string
		want error
	}{
		{"empty", "", ErrInvalidEndpoint},
		{"two dots", "/data{1..4}", ErrInvalidEllipsis},
		{"four dots", "/data{1....4}", ErrInvalidEllipsis},
		{"reversed numeric", "/data{4...1}", ErrInvalidEllipsis},
		{"reversed alpha", "/data{d...a}", ErrInvalidEllipsis},
		{"mixed case", "/data{a...D}", ErrInvalidEllipsis},
		{"mixed kinds", "/data{1...d}", ErrInvalidEllipsis},
		{"mixed kinds reversed", "/data{a...4}", ErrInvalidEllipsis},
		{"multi letter", "/data{aa...dd}", ErrInvalidEllipsis},
		{"empty bounds", "/data{...}", ErrInvalidEllipsis},
		{"missing end", "/data{1...}", ErrInvalidEllipsis},
		{"missing start", "/data{...4}", ErrInvalidEllipsis},
		{"unclosed", "/data{1...4", ErrInvalidEllipsis},
		{"unopened", "/data1...4}", ErrInvalidEllipsis},
		{"nested", "/data{{1...4}}", ErrInvalidEllipsis},
		{"stray open", "/data{1...4}{", ErrInvalidEllipsis},
		{"stray close", "/data{1...4}}", ErrInvalidEllipsis},
		{"lonely open", "/data{", ErrInvalidEllipsis},
		{"lonely close", "/data}", ErrInvalidEllipsis},
		{"empty braces", "/data{}", ErrInvalidEllipsis},
		{"space in bound", "/data{ 1...4}", ErrInvalidEllipsis},
		{"negative", "/data{-1...4}", ErrInvalidEllipsis},
		{"decimal", "/data{1.5...4}", ErrInvalidEllipsis},
		{"plus sign", "/data{+1...4}", ErrInvalidEllipsis},
		{"hex", "/data{0x1...0x4}", ErrInvalidEllipsis},
		{"unicode letters", "/data{à...è}", ErrInvalidEllipsis},
		{"huge number", "/data{1...99999999999}", ErrInvalidEllipsis},
		{"too large range", "/data{1...70000}", ErrTooManyEndpoints},
		{"too large product", "/d{1...300}/{1...300}", ErrTooManyEndpoints},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := expandArg(tc.arg)
			require.Error(t, err, "arg %q", tc.arg)
			assert.Nil(t, got)
			assert.True(t, errors.Is(err, tc.want), "arg %q: got %v, want %v", tc.arg, err, tc.want)
		})
	}
}

func TestHasEllipsis(t *testing.T) {
	t.Parallel()
	tests := []struct {
		arg  string
		want bool
	}{
		{"/data", false},
		{"/data{1...4}", true},
		{"/data{01...16}", true},
		{"/data{a...d}", true},
		{"/data{1..4}", false},
		{"/data{}", false},
		{"http://node{1...4}:9000/data", true},
		{"", false},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, hasEllipsis(tc.arg), "arg %q", tc.arg)
	}
}

func TestExpandRange_Padding(t *testing.T) {
	t.Parallel()
	got, err := expandRange("01", "16")
	require.NoError(t, err)
	require.Len(t, got, 16)
	for i, v := range got {
		assert.Equal(t, fmt.Sprintf("%02d", i+1), v)
	}
	got, err = expandRange("1", "16")
	require.NoError(t, err)
	assert.Equal(t, "1", got[0])
	assert.Equal(t, "16", got[15])
	assert.False(t, strings.HasPrefix(got[0], "0"))
}
