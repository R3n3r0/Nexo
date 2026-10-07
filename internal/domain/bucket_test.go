package domain

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateBucketName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{"simple", "photos", true},
		{"with digits", "bucket123", true},
		{"with dots", "my.bucket.name", true},
		{"with hyphens", "my-bucket-name", true},
		{"min length", "abc", true},
		{"max length", "a123456789b123456789c123456789d123456789e123456789f123456789abc", true},
		{"too short", "ab", false},
		{"too long", "a123456789b123456789c123456789d123456789e123456789f123456789abcd", false},
		{"uppercase", "MyBucket", false},
		{"underscore", "my_bucket", false},
		{"leading hyphen", "-bucket", false},
		{"trailing dot", "bucket.", false},
		{"double dot", "my..bucket", false},
		{"dot hyphen", "my.-bucket", false},
		{"hyphen dot", "my-.bucket", false},
		{"ip address", "192.168.1.1", false},
		{"xn prefix", "xn--bucket", false},
		{"sthree prefix", "sthree-bucket", false},
		{"s3alias suffix", "bucket-s3alias", false},
		{"ol-s3 suffix", "bucket--ol-s3", false},
		{"space", "my bucket", false},
		{"slash", "my/bucket", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBucketName(tc.input)
			if tc.valid {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidBucketName), "expected ErrInvalidBucketName, got %v", err)
		})
	}
}

func TestParseVersioning(t *testing.T) {
	for in, want := range map[string]Versioning{"": VersioningOff, "Enabled": VersioningEnabled, "Suspended": VersioningSuspended} {
		got, err := ParseVersioning(in)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		if in != "" {
			assert.Equal(t, in, got.String())
		}
	}
	_, err := ParseVersioning("maybe")
	assert.ErrorIs(t, err, ErrInvalidArgument)
	assert.True(t, VersioningSuspended.Versioned())
	assert.False(t, VersioningOff.Versioned())
}
