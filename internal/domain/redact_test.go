package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://kes.example.com:7373", "https://kes.example.com:7373"},
		{"https://user:s3cret@kes:7373", "https://REDACTED@kes:7373"},
		{"http://token@kes/path?key=s3cret#frag", "http://REDACTED@kes/path?REDACTED"},
		{"https://user:s3cret@kes host", "[invalid URL]"},
	}
	for _, tt := range tests {
		got := RedactURL(tt.in)
		assert.Equal(t, tt.want, got, tt.in)
		assert.NotContains(t, got, "s3cret")
	}
}
