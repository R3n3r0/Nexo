package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRetentionMode(t *testing.T) {
	tests := []struct {
		in   string
		want RetentionMode
		ok   bool
	}{
		{"GOVERNANCE", RetentionGovernance, true},
		{"compliance", RetentionCompliance, true},
		{" Governance ", RetentionGovernance, true},
		{"", "", false},
		{"LEGAL", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseRetentionMode(tt.in)
			if !tt.ok {
				assert.ErrorIs(t, err, ErrInvalidArgument)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseLegalHold(t *testing.T) {
	for in, want := range map[string]LegalHoldStatus{"ON": LegalHoldOn, "off": LegalHoldOff} {
		got, err := ParseLegalHold(in)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	for _, in := range []string{"", "yes", "true"} {
		_, err := ParseLegalHold(in)
		assert.ErrorIs(t, err, ErrInvalidArgument, in)
	}
}

func TestObjectRetentionActive(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		r    ObjectRetention
		want bool
	}{
		{"none", ObjectRetention{}, false},
		{"future governance", ObjectRetention{Mode: RetentionGovernance, RetainUntil: now.Add(time.Second)}, true},
		{"future compliance", ObjectRetention{Mode: RetentionCompliance, RetainUntil: now.Add(time.Hour)}, true},
		{"expired", ObjectRetention{Mode: RetentionCompliance, RetainUntil: now.Add(-time.Second)}, false},
		{"ends now", ObjectRetention{Mode: RetentionCompliance, RetainUntil: now}, false},
		{"date without mode", ObjectRetention{RetainUntil: now.Add(time.Hour)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.r.Active(now))
		})
	}
	assert.True(t, ObjectRetention{}.IsZero())
	assert.False(t, ObjectRetention{Mode: RetentionGovernance}.IsZero())
}

func TestDefaultRetention(t *testing.T) {
	created := time.Date(2024, 2, 29, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		d       DefaultRetention
		valid   bool
		wantEnd time.Time
	}{
		{"none", DefaultRetention{}, true, time.Time{}},
		{"days", DefaultRetention{Mode: RetentionGovernance, Days: 3}, true, created.AddDate(0, 0, 3)},
		{"years", DefaultRetention{Mode: RetentionCompliance, Years: 1}, true, created.AddDate(1, 0, 0)},
		{"both", DefaultRetention{Mode: RetentionCompliance, Years: 1, Days: 1}, false, time.Time{}},
		{"no period", DefaultRetention{Mode: RetentionCompliance}, false, time.Time{}},
		{"bad mode", DefaultRetention{Mode: "X", Days: 1}, false, time.Time{}},
		{"negative", DefaultRetention{Mode: RetentionGovernance, Days: -1}, false, time.Time{}},
		{"period without mode", DefaultRetention{Days: 1}, false, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.d.Validate()
			if !tt.valid {
				assert.ErrorIs(t, err, ErrMalformedXML)
				return
			}
			require.NoError(t, err)
			got := tt.d.RetentionAt(created)
			if tt.wantEnd.IsZero() {
				assert.True(t, got.IsZero())
				return
			}
			assert.Equal(t, tt.d.Mode, got.Mode)
			assert.Equal(t, tt.wantEnd, got.RetainUntil)
		})
	}
}

func TestChecksumAlgorithmsAndTypes(t *testing.T) {
	for _, a := range ChecksumAlgorithms() {
		got, err := ParseChecksumAlgorithm(string(a))
		require.NoError(t, err)
		assert.Equal(t, a, got)
		assert.True(t, a.IsValid())
	}
	got, err := ParseChecksumAlgorithm("crc64nvme")
	require.NoError(t, err)
	assert.Equal(t, ChecksumCRC64NVME, got)
	none, err := ParseChecksumAlgorithm("")
	require.NoError(t, err)
	assert.Equal(t, ChecksumNone, none)
	_, err = ParseChecksumAlgorithm("MD5")
	assert.ErrorIs(t, err, ErrInvalidArgument)
	assert.True(t, ChecksumCRC64NVME.IsCRC())
	assert.False(t, ChecksumSHA256.IsCRC())

	ct, err := ParseChecksumType("full_object")
	require.NoError(t, err)
	assert.Equal(t, ChecksumTypeFullObject, ct)
	_, err = ParseChecksumType("PARTIAL")
	assert.ErrorIs(t, err, ErrInvalidArgument)
}

func TestDeleteMarkerError(t *testing.T) {
	inner := NewS3Error(ErrObjectNotFound, "NoSuchKey", "The specified key does not exist.")
	err := error(&DeleteMarkerError{VersionID: "v1", Err: inner})
	assert.ErrorIs(t, err, ErrObjectNotFound)
	var s3err *S3Error
	require.ErrorAs(t, err, &s3err)
	assert.Equal(t, "NoSuchKey", s3err.Code)
	var dm *DeleteMarkerError
	require.True(t, errors.As(err, &dm))
	assert.Equal(t, "v1", dm.VersionID)
	assert.Equal(t, inner.Error(), err.Error())
	assert.Equal(t, "delete marker", (&DeleteMarkerError{}).Error())
}
