package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		raw     string
		want    Endpoint
		wantErr bool
	}{
		{"/data", Endpoint{Path: "/data", IsLocal: true}, false},
		{"data/", Endpoint{Path: "data", IsLocal: true}, false},
		{"/data/../data1/", Endpoint{Path: "/data1", IsLocal: true}, false},
		{"http://node1:9000/data1", Endpoint{Scheme: "http", Host: "node1:9000", Path: "/data1"}, false},
		{"https://node1/mnt/d1/", Endpoint{Scheme: "https", Host: "node1", Path: "/mnt/d1"}, false},
		{"", Endpoint{}, true},
		{"/", Endpoint{}, true},
		{".", Endpoint{}, true},
		{"ftp://node1/data", Endpoint{}, true},
		{"http:///data", Endpoint{}, true},
		{"http://node1:9000", Endpoint{}, true},
		{"http://node1:9000/", Endpoint{}, true},
		{"http://user@node1:9000/data", Endpoint{}, true},
		{"http://node1:9000/data?x=1", Endpoint{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := ParseEndpoint(tc.raw)
			if tc.wantErr {
				assert.ErrorIs(t, err, ErrInvalidArgument)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestEndpointAccessors(t *testing.T) {
	e, err := ParseEndpoint("http://node1:9000/data1")
	require.NoError(t, err)
	assert.True(t, e.IsURL())
	assert.Equal(t, "http://node1:9000/data1", e.String())
	assert.Equal(t, "node1", e.Hostname())
	assert.Equal(t, "9000", e.Port())

	l, err := ParseEndpoint("/data")
	require.NoError(t, err)
	assert.False(t, l.IsURL())
	assert.Equal(t, "/data", l.String())
	assert.Empty(t, l.Hostname())
	assert.Empty(t, l.Port())
}
