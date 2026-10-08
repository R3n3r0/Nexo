package cli_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
)

// covers: TCL-U-03
func TestHumanSize(t *testing.T) {
	tests := map[int64]string{
		0:               "0B",
		1:               "1B",
		1023:            "1023B",
		1024:            "1.0KiB",
		1536:            "1.5KiB",
		1048575:         "1.0MiB",
		5 * mib:         "5.0MiB",
		40000000:        "38.1MiB",
		3 * gib:         "3.0GiB",
		5 * tib:         "5.0TiB",
		-2048:           "-2.0KiB",
		1<<62 + 1<<61:   "6.0EiB",
		1024*1024 - 100: "1023.9KiB",
	}
	for in, want := range tests {
		assert.Equal(t, want, cli.HumanSize(in), in)
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		bad  bool
	}{
		{in: "100", want: 100},
		{in: "10MiB", want: 10 * mib},
		{in: "10mib", want: 10 * mib},
		{in: "1.5GB", want: 1500000000},
		{in: "512k", want: 512000},
		{in: "512KiB", want: 512 * kib},
		{in: "2 TiB", want: 2 * tib},
		{in: "1b", want: 1},
		{in: "", bad: true},
		{in: "abc", bad: true},
		{in: "10XB", bad: true},
		{in: "-1", bad: true},
		{in: "1e30TiB", bad: true},
	}
	for _, tc := range tests {
		got, err := cli.ParseSize(tc.in)
		if tc.bad {
			assert.ErrorIs(t, err, cli.ErrUsage, tc.in)
			continue
		}
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{in: "30s", want: 30 * time.Second},
		{in: "7d", want: 7 * 24 * time.Hour},
		{in: "1d12h", want: 36 * time.Hour},
		{in: "2w", want: 14 * 24 * time.Hour},
		{in: "90m", want: 90 * time.Minute},
		{in: "1.5h", want: 90 * time.Minute},
		{in: "0.5d", want: 12 * time.Hour},
		{in: "", bad: true},
		{in: "7", bad: true},
		{in: "d", bad: true},
		{in: "1y", bad: true},
		{in: "-1h", bad: true},
	}
	for _, tc := range tests {
		got, err := cli.ParseDuration(tc.in)
		if tc.bad {
			assert.ErrorIs(t, err, cli.ErrUsage, tc.in)
			continue
		}
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
	assert.Equal(t, "1h0m0s", cli.HumanDuration(time.Hour))
	assert.Equal(t, "7d0s", cli.HumanDuration(7*24*time.Hour))
	assert.Equal(t, "2d3h0m0s", cli.HumanDuration(51*time.Hour))
}

func TestParseMetadataAndTags(t *testing.T) {
	meta, err := cli.ParseMetadata("Owner=ops; x-amz-meta-Env = prod ;;")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"owner": "ops", "env": "prod"}, meta)
	_, err = cli.ParseMetadata("novalue")
	assert.ErrorIs(t, err, cli.ErrUsage)
	_, err = cli.ParseMetadata("=v")
	assert.ErrorIs(t, err, cli.ErrUsage)

	tags, err := cli.ParseTags("team=ops&env=prod%20eu&empty=")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"team": "ops", "env": "prod eu", "empty": ""}, tags)
	assert.Equal(t, "empty=&env=prod+eu&team=ops", cli.FormatTags(tags))
	_, err = cli.ParseTags("a=1&a=2")
	assert.ErrorIs(t, err, cli.ErrUsage)
	_, err = cli.ParseTags("=x")
	assert.ErrorIs(t, err, cli.ErrUsage)
	_, err = cli.ParseTags("a=%zz")
	assert.ErrorIs(t, err, cli.ErrUsage)
	assert.Equal(t, []string{"a", "b"}, cli.SortedKeys(map[string]int{"b": 1, "a": 2}))
}
