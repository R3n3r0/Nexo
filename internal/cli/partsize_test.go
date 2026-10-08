package cli_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
)

const (
	kib = int64(1) << 10
	mib = int64(1) << 20
	gib = int64(1) << 30
	tib = int64(1) << 40
)

// covers: TCL-U-06
func TestPartSizeAutomatic(t *testing.T) {
	tests := []struct {
		size     int64
		partSize int64
		parts    int
	}{
		{0, cli.DefaultPartSize, 1},
		{1, cli.DefaultPartSize, 1},
		{5 * mib, cli.DefaultPartSize, 1},
		{cli.DefaultPartSize, cli.DefaultPartSize, 1},
		{cli.DefaultPartSize + 1, cli.DefaultPartSize, 2},
		{1 * gib, cli.DefaultPartSize, 64},
		{10000 * cli.DefaultPartSize, cli.DefaultPartSize, 10000},
		{10000*cli.DefaultPartSize + 1, 17 * mib, 9412},
		{1 * tib, 105 * mib, 9987},
		{5 * tib, 525 * mib, 9987},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprint(tc.size), func(t *testing.T) {
			ps, parts, err := cli.PartSize(tc.size, 0)
			require.NoError(t, err)
			assert.Equal(t, tc.partSize, ps)
			assert.Equal(t, tc.parts, parts)
		})
	}
}

// covers: TCL-U-06
func TestPartSizeInvariants(t *testing.T) {
	// Every size from 1 B to 5 TiB (sampled on powers of two and their
	// neighbors) fits in at most 10000 parts of 5 MiB to 5 GiB.
	for exp := 0; exp <= 42; exp++ {
		base := int64(1) << exp
		for _, size := range []int64{base - 1, base, base + 1, base * 3 / 2} {
			if size < 1 || size > cli.MaxObjectSize {
				continue
			}
			ps, parts, err := cli.PartSize(size, 0)
			require.NoError(t, err, size)
			assert.LessOrEqual(t, parts, cli.MaxParts, size)
			assert.GreaterOrEqual(t, ps, cli.MinPartSize, size)
			assert.LessOrEqual(t, ps, cli.MaxPartSize, size)
			assert.GreaterOrEqual(t, ps*int64(parts), size, size)
			assert.Less(t, ps*int64(parts-1), size, "no empty trailing part for %d", size)
			assert.Zero(t, ps%mib, "part sizes are whole MiB")
		}
	}
}

// covers: TCL-U-06
func TestPartSizeRequested(t *testing.T) {
	ps, parts, err := cli.PartSize(100*mib, 5*mib)
	require.NoError(t, err)
	assert.Equal(t, 5*mib, ps)
	assert.Equal(t, 20, parts)

	_, _, err = cli.PartSize(100*mib, 4*mib)
	assert.ErrorIs(t, err, cli.ErrUsage, "below 5 MiB")
	_, _, err = cli.PartSize(100*mib, 6*gib)
	assert.ErrorIs(t, err, cli.ErrUsage, "above 5 GiB")
	_, _, err = cli.PartSize(1*tib, 5*mib)
	assert.ErrorIs(t, err, cli.ErrUsage, "more than 10000 parts")
	_, _, err = cli.PartSize(cli.MaxObjectSize+1, 0)
	assert.ErrorIs(t, err, domain.ErrEntityTooLarge)
	_, _, err = cli.PartSize(-1, 0)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)

	assert.NoError(t, cli.CheckPartSize(0))
	assert.NoError(t, cli.CheckPartSize(cli.MinPartSize))
	assert.Error(t, cli.CheckPartSize(cli.MinPartSize-1))
}

// covers: TCL-U-06
func TestStreamPartSizeReachesMaxObjectSize(t *testing.T) {
	assert.Equal(t, cli.DefaultPartSize, cli.StreamPartSize(1, 0))
	assert.Equal(t, cli.DefaultPartSize, cli.StreamPartSize(1000, 0))
	assert.Equal(t, 2*cli.DefaultPartSize, cli.StreamPartSize(1001, 0))
	assert.Equal(t, 10*mib, cli.StreamPartSize(1, 10*mib))
	var total int64
	for n := 1; n <= cli.MaxParts; n++ {
		size := cli.StreamPartSize(n, 0)
		assert.LessOrEqual(t, size, cli.MaxPartSize)
		total += size
	}
	assert.GreaterOrEqual(t, total, cli.MaxObjectSize, "a stream may reach 5 TiB within 10000 parts")
}
