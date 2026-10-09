package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCompatibleVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"v1.2.3", "v1.9.0", true},
		{"1.2.3", "v1.0.0", true},
		{"v1.2.3", "v2.0.0", false},
		{"dev", "v2.0.0", true},
		{"v3.0.0", "abc123", true},
		{"v-1.0.0", "v2.0.0", true},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, CompatibleVersions(tt.a, tt.b), "%s vs %s", tt.a, tt.b)
	}
}
