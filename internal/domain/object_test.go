package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateObjectKey(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		valid bool
	}{
		{"simple", "file.txt", true},
		{"nested", "dir/sub/file.txt", true},
		{"unicode", "folder/città.txt", true},
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

func TestValidateVersionID(t *testing.T) {
	tests := []struct {
		id string
		ok bool
	}{
		{"", true},
		{"null", true},
		{"0192f0f0-0000-7000-8000-000000000001", true},
		{"0192F0F0-0000-7000-8000-00000000ABCD", true},
		{"notavalid", false},
		{"zzz", false},
		{"0192f0f0-0000-7000-8000-00000000000", false},
		{"0192f0f0-0000-7000-8000-0000000000012", false},
		{"0192f0f0x0000-7000-8000-000000000001", false},
		{"0192f0f0-0000-7000-8000-00000000000g", false},
		{"NULL", false},
	}
	for _, tt := range tests {
		err := ValidateVersionID(tt.id)
		if tt.ok {
			assert.NoError(t, err, tt.id)
			continue
		}
		assert.ErrorIs(t, err, ErrInvalidArgument, tt.id)
		var s3err *S3Error
		require.ErrorAs(t, err, &s3err)
		assert.Equal(t, "InvalidArgument", s3err.Code)
	}
}
