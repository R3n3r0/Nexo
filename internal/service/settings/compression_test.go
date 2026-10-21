package settings_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/config"
	configmock "github.com/r3n3r0/nexo/internal/service/config/mock"
	"github.com/r3n3r0/nexo/internal/service/settings"
)

func TestCompressionSettings(t *testing.T) {
	read := func(t *testing.T, kv config.KV) settings.Compression {
		t.Helper()
		cfg := configmock.NewService(t)
		cfg.EXPECT().Get(mock.Anything, "compression").Return(kv, nil)
		s, err := settings.New(settings.Deps{Config: cfg})
		require.NoError(t, err)
		c, err := s.Compression(context.Background())
		require.NoError(t, err)
		return c
	}
	def := read(t, config.KV{})
	assert.False(t, def.Enabled, "off by default")
	assert.False(t, def.Selects("a.txt", "text/plain"))

	on := read(t, config.KV{"enable": "on"})
	for _, c := range []struct {
		key, ct string
		want    bool
	}{
		{"logs/app.LOG", "", true},
		{"data.json", "application/octet-stream", true},
		{"page", "text/html; charset=utf-8", true},
		{"doc", "application/json", true},
		{"blob", "application/octet-stream", false},
		{"archive.txt.gz", "text/plain", false},
		{"photo.jpg", "image/jpeg", false},
		{"clip", "video/mp4", false},
		{"notes.txt", "application/zip", false},
	} {
		assert.Equal(t, c.want, on.Selects(c.key, c.ct), "%s %s", c.key, c.ct)
	}

	custom := read(t, config.KV{"enable": "on", "extensions": ".dat", "mime_types": ""})
	assert.True(t, custom.Selects("x.dat", ""))
	assert.False(t, custom.Selects("x.txt", "text/plain"), "the lists replace the defaults")
	all := read(t, config.KV{"enable": "on", "extensions": "", "mime_types": ""})
	assert.True(t, all.Selects("anything", "application/octet-stream"), "empty lists select every version")
	assert.False(t, all.Selects("movie.mkv", ""), "except the compressed formats")
}
