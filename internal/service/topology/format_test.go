package topology

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleFormat() *FormatV1 {
	return &FormatV1{
		Version: FormatVersion,
		ID:      "deployment-1",
		Mode:    FormatModeErasure,
		Erasure: FormatErasure{
			This:             "d2",
			Sets:             [][]string{{"d1", "d2", "d3", "d4"}, {"d5", "d6", "d7", "d8"}},
			DistributionAlgo: DistributionAlgoV1,
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

	back, err := UnmarshalFormat(data)
	require.NoError(t, err)
	assert.Equal(t, f, back)
	assert.True(t, f.SameTopology(back))

	// Compact JSON written by another tool is accepted too.
	compact, err := json.Marshal(f)
	require.NoError(t, err)
	back, err = UnmarshalFormat(compact)
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
	assert.ErrorIs(t, err, ErrInvalidFormat)
}

func TestUnmarshalFormat_Invalid(t *testing.T) {
	t.Parallel()
	mutate := func(f func(*FormatV1)) []byte {
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
		{"not json", []byte("{"), ErrInvalidFormat},
		{"empty", nil, ErrInvalidFormat},
		{"wrong type", []byte(`{"version":"1"}`), ErrInvalidFormat},
		{"unknown field", []byte(`{"version":1,"id":"x","mode":"erasure","erasure":{"this":"a","sets":[["a","b"]],"distributionAlgo":"SIPMOD+PARITY"},"extra":1}`), ErrInvalidFormat},
		{"version 0", mutate(func(f *FormatV1) { f.Version = 0 }), ErrFormatVersion},
		{"version 2", mutate(func(f *FormatV1) { f.Version = 2 }), ErrFormatVersion},
		{"empty id", mutate(func(f *FormatV1) { f.ID = "" }), ErrInvalidFormat},
		{"unknown mode", mutate(func(f *FormatV1) { f.Mode = "xl" }), ErrInvalidFormat},
		{"unknown algo", mutate(func(f *FormatV1) { f.Erasure.DistributionAlgo = "CRCMOD" }), ErrInvalidFormat},
		{"empty this", mutate(func(f *FormatV1) { f.Erasure.This = "" }), ErrInvalidFormat},
		{"this not in sets", mutate(func(f *FormatV1) { f.Erasure.This = "zz" }), ErrInvalidFormat},
		{"no sets", mutate(func(f *FormatV1) { f.Erasure.Sets = nil }), ErrInvalidFormat},
		{"empty set", mutate(func(f *FormatV1) { f.Erasure.Sets = [][]string{{}} }), ErrInvalidFormat},
		{"ragged sets", mutate(func(f *FormatV1) { f.Erasure.Sets[1] = f.Erasure.Sets[1][:3] }), ErrInvalidFormat},
		{"duplicate id", mutate(func(f *FormatV1) { f.Erasure.Sets[1][0] = "d1" }), ErrInvalidFormat},
		{"empty drive id", mutate(func(f *FormatV1) { f.Erasure.Sets[1][0] = "" }), ErrInvalidFormat},
		{"erasure set of one", mutate(func(f *FormatV1) { f.Erasure.Sets = [][]string{{"d2"}} }), ErrInvalidFormat},
		{"erasure set of seventeen", mutate(func(f *FormatV1) {
			set := make([]string, 17)
			for i := range set {
				set[i] = string(rune('a' + i))
			}
			f.Erasure.Sets = [][]string{set}
			f.Erasure.This = "a"
		}), ErrInvalidFormat},
		{"snsd with two drives", mutate(func(f *FormatV1) {
			f.Mode = FormatModeSNSD
			f.Erasure.Sets = [][]string{{"d1", "d2"}}
		}), ErrInvalidFormat},
		{"snsd with two sets", mutate(func(f *FormatV1) {
			f.Mode = FormatModeSNSD
			f.Erasure.Sets = [][]string{{"d2"}, {"d3"}}
		}), ErrInvalidFormat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := UnmarshalFormat(tc.data)
			require.Error(t, err)
			assert.Nil(t, f)
			assert.ErrorIs(t, err, tc.want)
		})
	}

	var nilFormat *FormatV1
	assert.ErrorIs(t, nilFormat.Validate(), ErrInvalidFormat)
	assert.Nil(t, nilFormat.Clone())
}

func TestFormat_SNSD(t *testing.T) {
	t.Parallel()
	f := &FormatV1{Version: 1, ID: "x", Mode: FormatModeSNSD, Erasure: FormatErasure{This: "a", Sets: [][]string{{"a"}}, DistributionAlgo: DistributionAlgoV1}}
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
	assert.ErrorIs(t, err, ErrInvalidFormat)

	var empty FormatV1
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
	var nilFormat *FormatV1
	assert.False(t, nilFormat.SameTopology(f))

	cases := map[string]func(*FormatV1){
		"id":      func(x *FormatV1) { x.ID = "other" },
		"mode":    func(x *FormatV1) { x.Mode = FormatModeSNSD },
		"version": func(x *FormatV1) { x.Version = 2 },
		"algo":    func(x *FormatV1) { x.Erasure.DistributionAlgo = "x" },
		"sets":    func(x *FormatV1) { x.Erasure.Sets = x.Erasure.Sets[:1] },
		"width":   func(x *FormatV1) { x.Erasure.Sets[0] = x.Erasure.Sets[0][:3] },
		"drive":   func(x *FormatV1) { x.Erasure.Sets[1][3] = "d9" },
		"order": func(x *FormatV1) {
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
	assert.Equal(t, FormatModeSNSD, FormatModeFor(ModeSNSD))
	assert.Equal(t, FormatModeErasure, FormatModeFor(ModeSNMD))
	assert.Equal(t, FormatModeErasure, FormatModeFor(ModeDistributed))
}

func TestFormat_MatchesPool(t *testing.T) {
	t.Parallel()
	f := sampleFormat()
	pool := Pool{SetCount: 2, SetDriveCount: 4}
	assert.NoError(t, f.MatchesPool(ModeSNMD, pool))
	assert.NoError(t, f.MatchesPool(ModeDistributed, pool))
	assert.ErrorIs(t, f.MatchesPool(ModeSNSD, pool), ErrFormatModeMismatch)
	assert.ErrorIs(t, f.MatchesPool(ModeSNMD, Pool{SetCount: 1, SetDriveCount: 8}), ErrFormatLayoutMismatch)
	assert.ErrorIs(t, f.MatchesPool(ModeSNMD, Pool{SetCount: 2, SetDriveCount: 8}), ErrFormatLayoutMismatch)
	bad := f.Clone()
	bad.ID = ""
	assert.ErrorIs(t, bad.MatchesPool(ModeSNMD, pool), ErrInvalidFormat)

	snsd := &FormatV1{Version: 1, ID: "x", Mode: FormatModeSNSD, Erasure: FormatErasure{This: "a", Sets: [][]string{{"a"}}, DistributionAlgo: DistributionAlgoV1}}
	assert.NoError(t, snsd.MatchesPool(ModeSNSD, Pool{SetCount: 1, SetDriveCount: 1}))
	assert.ErrorIs(t, snsd.MatchesPool(ModeSNMD, Pool{SetCount: 1, SetDriveCount: 4}), ErrFormatModeMismatch)
}

func TestCheckFormats(t *testing.T) {
	t.Parallel()
	ids := &counterIDs{}
	pool := Pool{Index: 0, SetCount: 2, SetDriveCount: 4, Endpoints: make([]Endpoint, 8)}
	formats, err := NewFormats(ModeSNMD, pool, "dep", ids)
	require.NoError(t, err)
	require.Len(t, formats, 8)

	t.Run("consistent", func(t *testing.T) {
		ref, err := CheckFormats(formats)
		require.NoError(t, err)
		assert.Same(t, formats[0], ref)
	})
	t.Run("nil skipped", func(t *testing.T) {
		partial := []*FormatV1{nil, nil, formats[2], nil, formats[4], nil, nil, nil}
		ref, err := CheckFormats(partial)
		require.NoError(t, err)
		assert.Same(t, formats[2], ref)
	})
	t.Run("all nil", func(t *testing.T) {
		_, err := CheckFormats([]*FormatV1{nil, nil})
		assert.ErrorIs(t, err, ErrInvalidFormat)
		_, err = CheckFormats(nil)
		assert.ErrorIs(t, err, ErrInvalidFormat)
	})
	t.Run("different deployment", func(t *testing.T) {
		mixed := append([]*FormatV1(nil), formats...)
		mixed[5] = formats[5].Clone()
		mixed[5].ID = "other"
		_, err := CheckFormats(mixed)
		assert.ErrorIs(t, err, ErrFormatMismatch)
		assert.Contains(t, err.Error(), "drive 5")
	})
	t.Run("different sets", func(t *testing.T) {
		mixed := append([]*FormatV1(nil), formats...)
		mixed[1] = formats[1].Clone()
		mixed[1].Erasure.Sets[1][3] = "foreign"
		_, err := CheckFormats(mixed)
		assert.ErrorIs(t, err, ErrFormatMismatch)
	})
	t.Run("different mode", func(t *testing.T) {
		mixed := append([]*FormatV1(nil), formats...)
		mixed[1] = formats[1].Clone()
		mixed[1].Mode = FormatModeSNSD
		_, err := CheckFormats(mixed)
		require.Error(t, err)
	})
	t.Run("duplicate this", func(t *testing.T) {
		mixed := append([]*FormatV1(nil), formats...)
		mixed[3] = formats[2].Clone()
		_, err := CheckFormats(mixed)
		assert.ErrorIs(t, err, ErrFormatMismatch)
		assert.Contains(t, err.Error(), "drives 2 and 3")
	})
	t.Run("invalid document", func(t *testing.T) {
		mixed := append([]*FormatV1(nil), formats...)
		mixed[6] = formats[6].Clone()
		mixed[6].Erasure.This = ""
		_, err := CheckFormats(mixed)
		assert.ErrorIs(t, err, ErrInvalidFormat)
		assert.Contains(t, err.Error(), "drive 6")
	})
}

func TestNewFormats(t *testing.T) {
	t.Parallel()
	ids := &counterIDs{}
	pool := Pool{Index: 2, SetCount: 2, SetDriveCount: 4, Endpoints: make([]Endpoint, 8)}
	formats, err := NewFormats(ModeDistributed, pool, "dep", ids)
	require.NoError(t, err)
	require.Len(t, formats, 8)
	assert.Equal(t, 8, ids.n, "one id per drive")
	seen := map[string]struct{}{}
	for i, f := range formats {
		require.NoError(t, f.Validate())
		assert.Equal(t, FormatModeErasure, f.Mode)
		assert.Equal(t, "dep", f.ID)
		set, disk, err := f.Position()
		require.NoError(t, err)
		assert.Equal(t, i/4, set)
		assert.Equal(t, i%4, disk)
		seen[f.Erasure.This] = struct{}{}
		assert.True(t, formats[0].SameTopology(f))
		// Sets are deep copies: mutating one document does not leak into the others.
		f.Erasure.Sets[0][0] = "mutated-" + f.Erasure.This
	}
	assert.Len(t, seen, 8)
	assert.NotEqual(t, formats[0].Erasure.Sets[0][0], formats[1].Erasure.Sets[0][0])

	single, err := NewFormats(ModeSNSD, Pool{SetCount: 1, SetDriveCount: 1, Endpoints: make([]Endpoint, 1)}, "dep", ids)
	require.NoError(t, err)
	require.Len(t, single, 1)
	assert.Equal(t, FormatModeSNSD, single[0].Mode)
	require.NoError(t, single[0].Validate())

	_, err = NewFormats(ModeSNMD, pool, "", ids)
	assert.ErrorIs(t, err, ErrInvalidFormat)
	_, err = NewFormats(ModeSNMD, Pool{SetCount: 1, SetDriveCount: 4, Endpoints: make([]Endpoint, 3)}, "dep", ids)
	assert.ErrorIs(t, err, ErrInvalidLayout)
	_, err = NewFormats(ModeSNMD, Pool{}, "dep", ids)
	assert.ErrorIs(t, err, ErrInvalidLayout)
}
