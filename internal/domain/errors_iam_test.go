package domain

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIAMSentinelsWrap checks that the specific IAM sentinels keep the
// generic mapping of the sentinel they wrap.
func TestIAMSentinelsWrap(t *testing.T) {
	tests := []struct {
		err, wraps error
	}{
		{ErrGroupAlreadyExists, ErrConflict},
		{ErrAccessKeyNotFound, ErrUserNotFound},
		{ErrAccessKeyAlreadyExists, ErrUserAlreadyExists},
	}
	for _, tt := range tests {
		wrapped := fmt.Errorf("op: %w", tt.err)
		assert.ErrorIs(t, wrapped, tt.err)
		assert.ErrorIs(t, wrapped, tt.wraps)
		assert.NotErrorIs(t, tt.wraps, tt.err)
	}
	assert.NotErrorIs(t, ErrShareNotFound, ErrObjectNotFound)
}
