package object

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// lockCaller is who asks for a deletion in the TS-U-12 matrix: the
// transport turns the x-amz-bypass-governance-retention header into
// bypass only for the callers allowed s3:BypassGovernanceRetention (root
// always is: root bypasses the policies).
type lockCaller struct {
	name string
	// header is the x-amz-bypass-governance-retention header.
	header bool
	// allowed is the s3:BypassGovernanceRetention permission.
	allowed bool
}

func (c lockCaller) bypass() bool { return c.header && c.allowed }

func lockCallers() []lockCaller {
	return []lockCaller{
		{name: "user", header: false, allowed: false},
		{name: "user with header without permission", header: true, allowed: false},
		{name: "user with permission without header", header: false, allowed: true},
		{name: "user with permission and header", header: true, allowed: true},
		{name: "root without header", header: false, allowed: true},
		{name: "root with header", header: true, allowed: true},
	}
}

// TestObjectLockDeleteMatrix is TS-U-12: governance/compliance x active
// or expired retention x legal hold x caller, for the deletion of a
// version.
func TestObjectLockDeleteMatrix(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	modes := []domain.RetentionMode{"", domain.RetentionGovernance, domain.RetentionCompliance}
	holds := []domain.LegalHoldStatus{"", domain.LegalHoldOff, domain.LegalHoldOn}
	for _, mode := range modes {
		for _, active := range []bool{true, false} {
			for _, hold := range holds {
				for _, caller := range lockCallers() {
					name := fmt.Sprintf("mode=%q/active=%v/hold=%q/%s", mode, active, hold, caller.name)
					t.Run(name, func(t *testing.T) {
						info := domain.ObjectInfo{LegalHold: hold}
						if mode != "" {
							until := now.Add(time.Hour)
							if !active {
								until = now.Add(-time.Hour)
							}
							info.Retention = domain.ObjectRetention{Mode: mode, RetainUntil: until}
						}
						protected := mode != "" && active
						want := hold != domain.LegalHoldOn &&
							(!protected || (mode == domain.RetentionGovernance && caller.bypass()))
						err := checkDeletable(info, now, caller.bypass())
						if want {
							assert.NoError(t, err)
						} else {
							assert.ErrorIs(t, err, domain.ErrObjectLocked)
							var s3err *domain.S3Error
							require.ErrorAs(t, err, &s3err)
							assert.Equal(t, "AccessDenied", s3err.Code)
						}
						assert.Equal(t, hold == domain.LegalHoldOn || protected, isLocked(info, now))
					})
				}
			}
		}
	}
	// A delete marker is never protected.
	marker := domain.ObjectInfo{IsDeleteMarker: true, LegalHold: domain.LegalHoldOn,
		Retention: domain.ObjectRetention{Mode: domain.RetentionCompliance, RetainUntil: now.Add(time.Hour)}}
	assert.NoError(t, checkDeletable(marker, now, false))
	assert.False(t, isLocked(marker, now))
}

// TestObjectLockRetentionChangeMatrix is TS-U-12 for PutObjectRetention:
// extensions always, reductions and removals in governance with bypass
// only, never in compliance.
func TestObjectLockRetentionChangeMatrix(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	gov := func(d time.Duration) domain.ObjectRetention {
		return domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: now.Add(d)}
	}
	comp := func(d time.Duration) domain.ObjectRetention {
		return domain.ObjectRetention{Mode: domain.RetentionCompliance, RetainUntil: now.Add(d)}
	}
	none := domain.ObjectRetention{}
	tests := []struct {
		name string
		cur  domain.ObjectRetention
		next domain.ObjectRetention
		// allowed without and with bypass.
		plain, bypass bool
	}{
		{"set on a version without retention", none, gov(time.Hour), true, true},
		{"set compliance", none, comp(time.Hour), true, true},
		{"governance extended", gov(time.Hour), gov(2 * time.Hour), true, true},
		{"governance same date", gov(time.Hour), gov(time.Hour), true, true},
		{"governance to compliance extended", gov(time.Hour), comp(2 * time.Hour), true, true},
		{"governance shortened", gov(2 * time.Hour), gov(time.Hour), false, true},
		{"governance removed", gov(time.Hour), none, false, true},
		{"governance to compliance shortened", gov(2 * time.Hour), comp(time.Hour), false, true},
		{"compliance extended", comp(time.Hour), comp(2 * time.Hour), true, true},
		{"compliance same date", comp(time.Hour), comp(time.Hour), true, true},
		{"compliance shortened", comp(2 * time.Hour), comp(time.Hour), false, false},
		{"compliance removed", comp(time.Hour), none, false, false},
		{"compliance to governance", comp(time.Hour), gov(2 * time.Hour), false, false},
		{"expired compliance shortened", comp(-time.Hour), gov(time.Minute), true, true},
		{"expired compliance removed", comp(-time.Hour), none, true, true},
		{"expired governance removed", gov(-time.Hour), none, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for bypass, want := range map[bool]bool{false: tt.plain, true: tt.bypass} {
				err := checkRetentionChange(tt.cur, tt.next, now, bypass)
				if want {
					assert.NoError(t, err, "bypass=%v", bypass)
				} else {
					assert.ErrorIs(t, err, domain.ErrObjectLocked, "bypass=%v", bypass)
				}
			}
		})
	}
}

func TestCheckLockRequest(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	locked := BucketConfig{Versioning: domain.VersioningEnabled, ObjectLock: true}
	future := domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: now.Add(time.Hour)}
	tests := []struct {
		name string
		cfg  BucketConfig
		r    domain.ObjectRetention
		hold domain.LegalHoldStatus
		code string
	}{
		{"nothing requested", BucketConfig{}, domain.ObjectRetention{}, "", ""},
		{"retention", locked, future, "", ""},
		{"legal hold", locked, domain.ObjectRetention{}, domain.LegalHoldOn, ""},
		{"bucket without lock", BucketConfig{Versioning: domain.VersioningEnabled}, future, "", "InvalidRequest"},
		{"legal hold without lock", BucketConfig{}, domain.ObjectRetention{}, domain.LegalHoldOff, "InvalidRequest"},
		{"past date", locked, domain.ObjectRetention{Mode: domain.RetentionCompliance, RetainUntil: now.Add(-time.Second)}, "", "InvalidArgument"},
		{"date now", locked, domain.ObjectRetention{Mode: domain.RetentionCompliance, RetainUntil: now}, "", "InvalidArgument"},
		{"mode without date", locked, domain.ObjectRetention{Mode: domain.RetentionCompliance}, "", "InvalidArgument"},
		{"date without mode", locked, domain.ObjectRetention{RetainUntil: now.Add(time.Hour)}, "", "InvalidArgument"},
		{"bad legal hold", locked, domain.ObjectRetention{}, "MAYBE", "InvalidArgument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkLockRequest(tt.cfg, tt.r, tt.hold, now)
			if tt.code == "" {
				require.NoError(t, err)
				return
			}
			var s3err *domain.S3Error
			require.ErrorAs(t, err, &s3err)
			assert.Equal(t, tt.code, s3err.Code)
		})
	}
}

func TestApplyDefaultRetention(t *testing.T) {
	mod := time.Date(2026, 1, 31, 8, 0, 0, 0, time.UTC)
	cfg := BucketConfig{ObjectLock: true, DefaultRetention: domain.DefaultRetention{Mode: domain.RetentionGovernance, Days: 1}}

	meta := map[string]string{}
	applyDefaultRetention(meta, cfg, mod)
	assert.Equal(t, "GOVERNANCE", meta[metaLockMode])
	assert.Equal(t, "2026-02-01T08:00:00.000Z", meta[metaLockRetainUntil])

	explicit := map[string]string{metaLockMode: "COMPLIANCE", metaLockRetainUntil: "2030-01-01T00:00:00.000Z"}
	applyDefaultRetention(explicit, cfg, mod)
	assert.Equal(t, "COMPLIANCE", explicit[metaLockMode], "the requested retention wins")

	plain := map[string]string{}
	applyDefaultRetention(plain, BucketConfig{DefaultRetention: cfg.DefaultRetention}, mod)
	assert.Empty(t, plain, "no object lock, no default retention")

	info := objectInfo("b", "k", storage.FileInfo{Metadata: meta})
	assert.Equal(t, domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: mod.AddDate(0, 0, 1)}, info.Retention)
}
