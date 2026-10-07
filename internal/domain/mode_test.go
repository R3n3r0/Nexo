package domain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModeParseAndString(t *testing.T) {
	for in, want := range map[string]Mode{
		"snsd": ModeSNSD, "SNMD": ModeSNMD, "erasure": ModeSNMD,
		"distributed": ModeDistributed, "cluster": ModeDistributed, " mnmd ": ModeDistributed,
	} {
		got, err := ParseMode(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	_, err := ParseMode("nope")
	assert.ErrorIs(t, err, ErrInvalidArgument)

	assert.Equal(t, "snsd", ModeSNSD.String())
	assert.Equal(t, "unknown", ModeUnknown.String())
	assert.Equal(t, "Mode(42)", Mode(42).String())
	assert.True(t, ModeSNMD.IsErasure())
	assert.False(t, ModeSNSD.IsErasure())
	assert.True(t, ModeDistributed.IsDistributed())

	b, err := json.Marshal(struct{ M Mode }{ModeSNMD})
	require.NoError(t, err)
	assert.JSONEq(t, `{"M":"snmd"}`, string(b))
	var v struct{ M Mode }
	require.NoError(t, json.Unmarshal([]byte(`{"M":"cluster"}`), &v))
	assert.Equal(t, ModeDistributed, v.M)
}
