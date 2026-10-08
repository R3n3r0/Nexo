package topology_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/topology"
	topologymock "github.com/r3n3r0/nexo/internal/service/topology/mock"
)

func sampleFormat() *topology.FormatV1 {
	return &topology.FormatV1{
		Version: topology.FormatVersion,
		ID:      "deployment-1",
		Mode:    topology.FormatModeErasure,
		Erasure: topology.FormatErasure{
			This:             "d2",
			Sets:             [][]string{{"d1", "d2", "d3", "d4"}, {"d5", "d6", "d7", "d8"}},
			DistributionAlgo: topology.DistributionAlgoV1,
		},
	}
}

func TestFormat_RoundTrip(t *testing.T) {
	t.Parallel()
	f := sampleFormat()
	data, err := f.Marshal()
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(string(data), "\n"))
	assert.Contains(t, string(data), `"version": 1`)
	assert.Contains(t, string(data), `"distributionAlgo": "SIPMOD+PARITY"`)

	var generic map[string]any
	require.NoError(t, json.Unmarshal(data, &generic))
	assert.ElementsMatch(t, []string{"version", "id", "mode", "erasure"}, keysOf(generic))
	assert.ElementsMatch(t, []string{"this", "sets", "distributionAlgo"}, keysOf(generic["erasure"].(map[string]any)))

	back, err := topology.UnmarshalFormat(data)
	require.NoError(t, err)
	assert.Equal(t, f, back)
	assert.True(t, f.SameTopology(back))

	// Compact JSON written by another tool is accepted too.
	compact, err := json.Marshal(f)
	require.NoError(t, err)
	back, err = topology.UnmarshalFormat(compact)
	require.NoError(t, err)
	assert.Equal(t, f, back)
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestFormat_MarshalInvalid(t *testing.T) {
	t.Parallel()
	f := sampleFormat()
	f.ID = ""
	_, err := f.Marshal()
	assert.ErrorIs(t, err, topology.ErrInvalidFormat)
}

func TestUnmarshalFormat_Invalid(t *testing.T) {
	t.Parallel()
	mutate := func(f func(*topology.FormatV1)) []byte {
		s := sampleFormat()
		f(s)
		data, err := json.Marshal(s)
		require.NoError(t, err)
		return data
	}
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"not json", []byte("{"), topology.ErrInvalidFormat},
		{"empty", nil, topology.ErrInvalidFormat},
		{"wrong type", []byte(`{"version":"1"}`), topology.ErrInvalidFormat},
		{"unknown field", []byte(`{"version":1,"id":"x","mode":"erasure","erasure":{"this":"a","sets":[["a","b"]],"distributionAlgo":"SIPMOD+PARITY"},"extra":1}`), topology.ErrInvalidFormat},
		{"version 0", mutate(func(f *topology.FormatV1) { f.Version = 0 }), topology.ErrFormatVersion},
		{"version 2", mutate(func(f *topology.FormatV1) { f.Version = 2 }), topology.ErrFormatVersion},
		{"empty id", mutate(func(f *topology.FormatV1) { f.ID = "" }), topology.ErrInvalidFormat},
		{"unknown mode", mutate(func(f *topology.FormatV1) { f.Mode = "xl" }), topology.ErrInvalidFormat},
		{"unknown algo", mutate(func(f *topology.FormatV1) { f.Erasure.DistributionAlgo = "CRCMOD" }), topology.ErrInvalidFormat},
		{"empty this", mutate(func(f *topology.FormatV1) { f.Erasure.This = "" }), topology.ErrInvalidFormat},
		{"this not in sets", mutate(func(f *topology.FormatV1) { f.Erasure.This = "zz" }), topology.ErrInvalidFormat},
		{"no sets", mutate(func(f *topology.FormatV1) { f.Erasure.Sets = nil }), topology.ErrInvalidFormat},
		{"empty set", mutate(func(f *topology.FormatV1) { f.Erasure.Sets = [][]string{{}} }), topology.ErrInvalidFormat},
		{"ragged sets", mutate(func(f *topology.FormatV1) { f.Erasure.Sets[1] = f.Erasure.Sets[1][:3] }), topology.ErrInvalidFormat},
		{"duplicate id", mutate(func(f *topology.FormatV1) { f.Erasure.Sets[1][0] = "d1" }), topology.ErrInvalidFormat},
		{"empty drive id", mutate(func(f *topology.FormatV1) { f.Erasure.Sets[1][0] = "" }), topology.ErrInvalidFormat},
		{"erasure set of one", mutate(func(f *topology.FormatV1) { f.Erasure.Sets = [][]string{{"d2"}} }), topology.ErrInvalidFormat},
		{"erasure set of seventeen", mutate(func(f *topology.FormatV1) {
			set := make([]string, 17)
			for i := range set {
				set[i] = string(rune('a' + i))
			}
			f.Erasure.Sets = [][]string{set}
			f.Erasure.This = "a"
		}), topology.ErrInvalidFormat},
		{"snsd with two drives", mutate(func(f *topology.FormatV1) {
			f.Mode = topology.FormatModeSNSD
			f.Erasure.Sets = [][]string{{"d1", "d2"}}
		}), topology.ErrInvalidFormat},
		{"snsd with two sets", mutate(func(f *topology.FormatV1) {
			f.Mode = topology.FormatModeSNSD
			f.Erasure.Sets = [][]string{{"d2"}, {"d3"}}
		}), topology.ErrInvalidFormat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := topology.UnmarshalFormat(tc.data)
			require.Error(t, err)
			assert.Nil(t, f)
			assert.ErrorIs(t, err, tc.want)
		})
	}

	var nilFormat *topology.FormatV1
	assert.ErrorIs(t, nilFormat.Validate(), topology.ErrInvalidFormat)
	assert.Nil(t, nilFormat.Clone())
}

func TestFormat_SNSD(t *testing.T) {
	t.Parallel()
	f := &topology.FormatV1{Version: 1, ID: "x", Mode: topology.FormatModeSNSD, Erasure: topology.FormatErasure{This: "a", Sets: [][]string{{"a"}}, DistributionAlgo: topology.DistributionAlgoV1}}
	require.NoError(t, f.Validate())
	assert.Equal(t, 1, f.SetCount())
	assert.Equal(t, 1, f.SetDriveCount())
	set, disk, err := f.Position()
	require.NoError(t, err)
	assert.Equal(t, 0, set)
	assert.Equal(t, 0, disk)
}

func TestFormat_Accessors(t *testing.T) {
	t.Parallel()
	f := sampleFormat()
	assert.Equal(t, 2, f.SetCount())
	assert.Equal(t, 4, f.SetDriveCount())
	set, disk, err := f.Position()
	require.NoError(t, err)
	assert.Equal(t, 0, set)
	assert.Equal(t, 1, disk)

	f.Erasure.This = "d7"
	set, disk, err = f.Position()
	require.NoError(t, err)
	assert.Equal(t, 1, set)
	assert.Equal(t, 2, disk)

	f.Erasure.This = "nope"
	_, _, err = f.Position()
	assert.ErrorIs(t, err, topology.ErrInvalidFormat)

	var empty topology.FormatV1
	assert.Equal(t, 0, empty.SetCount())
	assert.Equal(t, 0, empty.SetDriveCount())
}

func TestFormat_Clone(t *testing.T) {
	t.Parallel()
	f := sampleFormat()
	c := f.Clone()
	assert.Equal(t, f, c)
	c.Erasure.Sets[0][0] = "changed"
	c.Erasure.This = "other"
	assert.Equal(t, "d1", f.Erasure.Sets[0][0])
	assert.Equal(t, "d2", f.Erasure.This)
}

func TestFormat_SameTopology(t *testing.T) {
	t.Parallel()
	f := sampleFormat()
	o := f.Clone()
	o.Erasure.This = "d8"
	assert.True(t, f.SameTopology(o))
	assert.True(t, f.SameTopology(f))
	assert.False(t, f.SameTopology(nil))
	var nilFormat *topology.FormatV1
	assert.False(t, nilFormat.SameTopology(f))

	cases := map[string]func(*topology.FormatV1){
		"id":      func(x *topology.FormatV1) { x.ID = "other" },
		"mode":    func(x *topology.FormatV1) { x.Mode = topology.FormatModeSNSD },
		"version": func(x *topology.FormatV1) { x.Version = 2 },
		"algo":    func(x *topology.FormatV1) { x.Erasure.DistributionAlgo = "x" },
		"sets":    func(x *topology.FormatV1) { x.Erasure.Sets = x.Erasure.Sets[:1] },
		"width":   func(x *topology.FormatV1) { x.Erasure.Sets[0] = x.Erasure.Sets[0][:3] },
		"drive":   func(x *topology.FormatV1) { x.Erasure.Sets[1][3] = "d9" },
		"order": func(x *topology.FormatV1) {
			x.Erasure.Sets[0][0], x.Erasure.Sets[0][1] = x.Erasure.Sets[0][1], x.Erasure.Sets[0][0]
		},
	}
	for name, mutate := range cases {
		x := f.Clone()
		mutate(x)
		assert.False(t, f.SameTopology(x), name)
	}
}

func TestFormatModeFor(t *testing.T) {
	t.Parallel()
	assert.Equal(t, topology.FormatModeSNSD, topology.FormatModeFor(domain.ModeSNSD))
	assert.Equal(t, topology.FormatModeErasure, topology.FormatModeFor(domain.ModeSNMD))
	assert.Equal(t, topology.FormatModeErasure, topology.FormatModeFor(domain.ModeDistributed))
}

func TestFormat_MatchesPool(t *testing.T) {
	t.Parallel()
	f := sampleFormat()
	pool := topology.Pool{SetCount: 2, SetDriveCount: 4}
	assert.NoError(t, f.MatchesPool(domain.ModeSNMD, pool))
	assert.NoError(t, f.MatchesPool(domain.ModeDistributed, pool))
	assert.ErrorIs(t, f.MatchesPool(domain.ModeSNSD, pool), topology.ErrFormatModeMismatch)
	assert.ErrorIs(t, f.MatchesPool(domain.ModeSNMD, topology.Pool{SetCount: 1, SetDriveCount: 8}), topology.ErrFormatLayoutMismatch)
	assert.ErrorIs(t, f.MatchesPool(domain.ModeSNMD, topology.Pool{SetCount: 2, SetDriveCount: 8}), topology.ErrFormatLayoutMismatch)
	bad := f.Clone()
	bad.ID = ""
	assert.ErrorIs(t, bad.MatchesPool(domain.ModeSNMD, pool), topology.ErrInvalidFormat)

	snsd := &topology.FormatV1{Version: 1, ID: "x", Mode: topology.FormatModeSNSD, Erasure: topology.FormatErasure{This: "a", Sets: [][]string{{"a"}}, DistributionAlgo: topology.DistributionAlgoV1}}
	assert.NoError(t, snsd.MatchesPool(domain.ModeSNSD, topology.Pool{SetCount: 1, SetDriveCount: 1}))
	assert.ErrorIs(t, snsd.MatchesPool(domain.ModeSNMD, topology.Pool{SetCount: 1, SetDriveCount: 4}), topology.ErrFormatModeMismatch)
}

func TestCheckFormats(t *testing.T) {
	t.Parallel()
	ids := counterIDs(t)
	pool := topology.Pool{Index: 0, SetCount: 2, SetDriveCount: 4, Endpoints: make([]domain.Endpoint, 8)}
	formats, err := topology.NewFormats(domain.ModeSNMD, pool, "dep", ids)
	require.NoError(t, err)
	require.Len(t, formats, 8)

	t.Run("consistent", func(t *testing.T) {
		ref, err := topology.CheckFormats(formats)
		require.NoError(t, err)
		assert.Same(t, formats[0], ref)
	})
	t.Run("nil skipped", func(t *testing.T) {
		partial := []*topology.FormatV1{nil, nil, formats[2], nil, formats[4], nil, nil, nil}
		ref, err := topology.CheckFormats(partial)
		require.NoError(t, err)
		assert.Same(t, formats[2], ref)
	})
	t.Run("all nil", func(t *testing.T) {
		_, err := topology.CheckFormats([]*topology.FormatV1{nil, nil})
		assert.ErrorIs(t, err, topology.ErrInvalidFormat)
		_, err = topology.CheckFormats(nil)
		assert.ErrorIs(t, err, topology.ErrInvalidFormat)
	})
	t.Run("different deployment", func(t *testing.T) {
		mixed := append([]*topology.FormatV1(nil), formats...)
		mixed[5] = formats[5].Clone()
		mixed[5].ID = "other"
		_, err := topology.CheckFormats(mixed)
		assert.ErrorIs(t, err, topology.ErrFormatMismatch)
		assert.Contains(t, err.Error(), "drive 5")
	})
	t.Run("different sets", func(t *testing.T) {
		mixed := append([]*topology.FormatV1(nil), formats...)
		mixed[1] = formats[1].Clone()
		mixed[1].Erasure.Sets[1][3] = "foreign"
		_, err := topology.CheckFormats(mixed)
		assert.ErrorIs(t, err, topology.ErrFormatMismatch)
	})
	t.Run("different mode", func(t *testing.T) {
		mixed := append([]*topology.FormatV1(nil), formats...)
		mixed[1] = formats[1].Clone()
		mixed[1].Mode = topology.FormatModeSNSD
		_, err := topology.CheckFormats(mixed)
		require.Error(t, err)
	})
	t.Run("duplicate this", func(t *testing.T) {
		mixed := append([]*topology.FormatV1(nil), formats...)
		mixed[3] = formats[2].Clone()
		_, err := topology.CheckFormats(mixed)
		assert.ErrorIs(t, err, topology.ErrFormatMismatch)
		assert.Contains(t, err.Error(), "drives 2 and 3")
	})
	t.Run("invalid document", func(t *testing.T) {
		mixed := append([]*topology.FormatV1(nil), formats...)
		mixed[6] = formats[6].Clone()
		mixed[6].Erasure.This = ""
		_, err := topology.CheckFormats(mixed)
		assert.ErrorIs(t, err, topology.ErrInvalidFormat)
		assert.Contains(t, err.Error(), "drive 6")
	})
}

func TestNewFormats(t *testing.T) {
	t.Parallel()
	// One id per drive: the mock fails the test on a ninth call.
	ids := topologymock.NewIDGenerator(t)
	n := 0
	ids.EXPECT().NewID().RunAndReturn(func() string { n++; return fmt.Sprintf("id-%03d", n) }).Times(8)
	pool := topology.Pool{Index: 2, SetCount: 2, SetDriveCount: 4, Endpoints: make([]domain.Endpoint, 8)}
	formats, err := topology.NewFormats(domain.ModeDistributed, pool, "dep", ids)
	require.NoError(t, err)
	require.Len(t, formats, 8)
	seen := map[string]struct{}{}
	pristine := formats[0].Clone()
	for i, f := range formats {
		require.NoError(t, f.Validate())
		assert.Equal(t, topology.FormatModeErasure, f.Mode)
		assert.Equal(t, "dep", f.ID)
		set, disk, err := f.Position()
		require.NoError(t, err)
		assert.Equal(t, i/4, set)
		assert.Equal(t, i%4, disk)
		seen[f.Erasure.This] = struct{}{}
		assert.True(t, pristine.SameTopology(f))
		// Sets are deep copies: mutating one document does not leak into the others.
		f.Erasure.Sets[0][0] = "mutated-" + f.Erasure.This
	}
	assert.Len(t, seen, 8)
	assert.NotEqual(t, formats[0].Erasure.Sets[0][0], formats[1].Erasure.Sets[0][0])

	more := counterIDs(t)
	single, err := topology.NewFormats(domain.ModeSNSD, topology.Pool{SetCount: 1, SetDriveCount: 1, Endpoints: make([]domain.Endpoint, 1)}, "dep", more)
	require.NoError(t, err)
	require.Len(t, single, 1)
	assert.Equal(t, topology.FormatModeSNSD, single[0].Mode)
	require.NoError(t, single[0].Validate())

	_, err = topology.NewFormats(domain.ModeSNMD, pool, "", more)
	assert.ErrorIs(t, err, topology.ErrInvalidFormat)
	_, err = topology.NewFormats(domain.ModeSNMD, topology.Pool{SetCount: 1, SetDriveCount: 4, Endpoints: make([]domain.Endpoint, 3)}, "dep", more)
	assert.ErrorIs(t, err, topology.ErrInvalidLayout)
	_, err = topology.NewFormats(domain.ModeSNMD, topology.Pool{}, "dep", more)
	assert.ErrorIs(t, err, topology.ErrInvalidLayout)
}
