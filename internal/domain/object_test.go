package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateObjectKey(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		valid bool
	}{
		{"simple", "file.txt", true},
		{"nested", "dir/sub/file.txt", true},
		{"unicode", "cartella/città.txt", true},
		{"spaces and symbols", "my file (1) +=&.txt", true},
		{"leading slash", "/abs.txt", true},
		{"trailing slash (directory marker)", "dir/", true},
		{"max length", strings.Repeat("a", MaxObjectKeyLength), true},
		{"empty", "", false},
		{"too long", strings.Repeat("a", MaxObjectKeyLength+1), false},
		{"invalid utf8", "bad\xff", false},
		{"nul byte", "a\x00b", false},
		{"double slash", "a//b", false},
		{"dot segment", "a/./b", false},
		{"dotdot segment", "a/../b", false},
		{"dotdot only", "..", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateObjectKey(tc.key)
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, ErrInvalidObjectName)
			}
		})
	}
}

func TestValidateObjectPrefix(t *testing.T) {
	assert.NoError(t, ValidateObjectPrefix(""))
	assert.NoError(t, ValidateObjectPrefix("photos/2024/"))
	assert.ErrorIs(t, ValidateObjectPrefix("../x"), ErrInvalidObjectName)
}
