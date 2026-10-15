package match_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/r3n3r0/nexo/internal/service/match"
)

// TestNormalizeInvalidUTF8 checks the NFC form of keys with invalid
// UTF-8 bytes: the bytes are kept, the valid runs around them are
// normalized, and the normalization returns (GO-2026-5970).
func TestNormalizeInvalidUTF8(t *testing.T) {
	ctx := context.Background()
	svc := match.New(match.Deps{})
	tests := []struct {
		name, key, want string
	}{
		{"valid decomposed", "café/x", "café/x"},
		{"invalid byte after a decomposed run", "é\xff/x", "é\xff/x"},
		{"invalid bytes on both sides", "\xfeé\xffå", "\xfeé\xffå"},
		{"lone combining mark after an invalid byte", "\xff́", "\xff́"},
		{"truncated sequence", "a/\xe2\x82", "a/\xe2\x82"},
		{"encoded replacement character", "�/é", "�/é"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, svc.Normalize(ctx, tc.key))
		})
	}
}
