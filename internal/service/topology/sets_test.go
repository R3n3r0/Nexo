package topology

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultParity(t *testing.T) {
	t.Parallel()
	tests := map[int]int{
		0: 0, 1: 0, 2: 1, 3: 1, 4: 2, 5: 2, 6: 3, 7: 3, 8: 4, 9: 4, 10: 4, 12: 4, 15: 4, 16: 4,
	}
	for n, want := range tests {
		assert.Equal(t, want, DefaultParity(n), "n=%d", n)
	}
	for n := 2; n <= MaxSetDriveCount; n++ {
		p := DefaultParity(n)
		assert.GreaterOrEqual(t, p, 1, "n=%d", n)
		assert.LessOrEqual(t, p, n/2, "n=%d", n)
	}
}

func TestIsSymmetric(t *testing.T) {
	t.Parallel()
	tests := []struct {
		setSize, nodes int
		want           bool
	}{
		{16, 1, true}, {16, 4, true}, {16, 8, true}, {16, 16, true}, {16, 32, true},
		{8, 4, true}, {4, 16, true}, {2, 32, true}, {9, 18, true}, {3, 9, true},
		{6, 4, false}, {8, 3, false}, {16, 3, false}, {16, 12, false}, {4, 6, false},
		{0, 1, false}, {4, 0, false}, {-1, 2, false},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, isSymmetric(tc.setSize, tc.nodes), "set=%d nodes=%d", tc.setSize, tc.nodes)
	}
}

func TestChooseSetDriveCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		total, nodes int
		want         int // 0 = error expected
	}{
		{4, 1, 4}, {2, 1, 2}, {3, 1, 3}, {5, 1, 5}, {8, 1, 8}, {16, 1, 16}, {32, 1, 16},
		{20, 1, 10}, {24, 1, 12}, {30, 1, 15}, {17, 1, 0}, {19, 1, 0}, {23, 1, 0}, {1, 1, 0},
		{16, 4, 16}, {8, 4, 8}, {32, 4, 16}, {64, 4, 16}, {4, 4, 4}, {12, 4, 12},
		{6, 3, 6}, {9, 3, 9}, {21, 7, 7}, {7, 7, 7}, {14, 7, 14},
		{32, 32, 16}, {1024, 32, 16}, {18, 18, 9}, {36, 18, 9}, {20, 20, 10}, {22, 22, 11},
		{24, 24, 12}, {25, 25, 5}, {26, 26, 13}, {27, 27, 9}, {28, 28, 14}, {30, 30, 15},
		{17, 17, 0}, {34, 17, 0}, {289, 17, 0}, {19, 19, 0}, {31, 31, 0},
		{12, 2, 12}, {24, 2, 12}, {30, 2, 10}, {3, 2, 0},
		{48, 3, 12}, {48, 6, 12}, {48, 12, 12}, {48, 16, 16}, {48, 24, 12},
	}
	for _, tc := range tests {
		got, err := chooseSetDriveCount(tc.total, tc.nodes)
		if tc.want == 0 {
			require.Error(t, err, "total=%d nodes=%d", tc.total, tc.nodes)
			assert.True(t, errors.Is(err, ErrInvalidSetSize))
			continue
		}
		require.NoError(t, err, "total=%d nodes=%d", tc.total, tc.nodes)
		assert.Equal(t, tc.want, got, "total=%d nodes=%d", tc.total, tc.nodes)
	}
}

func TestChooseSetDriveCount_IsLargestValid(t *testing.T) {
	t.Parallel()
	for nodes := 1; nodes <= 32; nodes++ {
		for drives := 1; drives <= 32; drives++ {
			total := nodes * drives
			best := 0
			for s := MaxSetDriveCount; s >= MinSetDriveCount; s-- {
				if total%s == 0 && isSymmetric(s, nodes) {
					best = s
					break
				}
			}
			got, err := chooseSetDriveCount(total, nodes)
			if best == 0 {
				assert.Error(t, err, "%dx%d", nodes, drives)
				continue
			}
			require.NoError(t, err, "%dx%d", nodes, drives)
			assert.Equal(t, best, got, "%dx%d", nodes, drives)
		}
	}
}

func TestValidateSetDriveCount(t *testing.T) {
	t.Parallel()
	assert.NoError(t, validateSetDriveCount(4, 16, 1))
	assert.NoError(t, validateSetDriveCount(8, 16, 4))
	assert.NoError(t, validateSetDriveCount(4, 16, 16))
	assert.ErrorIs(t, validateSetDriveCount(1, 16, 1), ErrInvalidSetSize)
	assert.ErrorIs(t, validateSetDriveCount(17, 34, 1), ErrInvalidSetSize)
	assert.ErrorIs(t, validateSetDriveCount(5, 16, 1), ErrInvalidSetSize)
	assert.ErrorIs(t, validateSetDriveCount(6, 12, 4), ErrInvalidSetSize)
}

func TestValidateParity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		parity, n int
		ok        bool
	}{
		{0, 1, true}, {1, 1, false}, {1, 2, true}, {2, 2, false}, {0, 2, false},
		{1, 3, true}, {2, 4, true}, {3, 4, false}, {0, 4, false}, {4, 8, true},
		{5, 8, false}, {8, 16, true}, {9, 16, false}, {1, 16, true},
	}
	for _, tc := range tests {
		err := validateParity(tc.parity, tc.n)
		if tc.ok {
			assert.NoError(t, err, "parity=%d n=%d", tc.parity, tc.n)
		} else {
			assert.ErrorIs(t, err, ErrInvalidParity, "parity=%d n=%d", tc.parity, tc.n)
		}
	}
}

func TestDistributeRoundRobin(t *testing.T) {
	t.Parallel()
	ep := func(host, p string) Endpoint {
		return Endpoint{Scheme: "http", Host: host, Port: 9000, Path: p}
	}
	in := []Endpoint{ep("a", "/1"), ep("a", "/2"), ep("b", "/1"), ep("b", "/2"), ep("c", "/1"), ep("c", "/2")}
	ordered, keys, perNode := distributeRoundRobin(in)
	assert.Equal(t, []string{"a:9000", "b:9000", "c:9000"}, keys)
	assert.Len(t, perNode["a:9000"], 2)
	want := []Endpoint{ep("a", "/1"), ep("b", "/1"), ep("c", "/1"), ep("a", "/2"), ep("b", "/2"), ep("c", "/2")}
	assert.Equal(t, want, ordered)

	// Already interleaved input keeps its order.
	ordered, _, _ = distributeRoundRobin(want)
	assert.Equal(t, want, ordered)

	// Local paths share one node and keep their order.
	local := []Endpoint{{Path: "/d1"}, {Path: "/d2"}, {Path: "/d3"}}
	ordered, keys, _ = distributeRoundRobin(local)
	assert.Equal(t, []string{""}, keys)
	assert.Equal(t, local, ordered)

	// Uneven nodes: the shorter node simply runs out.
	uneven := []Endpoint{ep("a", "/1"), ep("a", "/2"), ep("a", "/3"), ep("b", "/1")}
	ordered, _, _ = distributeRoundRobin(uneven)
	assert.Equal(t, []Endpoint{ep("a", "/1"), ep("b", "/1"), ep("a", "/2"), ep("a", "/3")}, ordered)

	assert.Empty(t, fmt.Sprint(mustEmpty(distributeRoundRobin(nil))))
}

func mustEmpty(eps []Endpoint, _ []string, _ map[string][]Endpoint) []Endpoint { return eps }
