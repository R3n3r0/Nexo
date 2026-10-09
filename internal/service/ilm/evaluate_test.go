package ilm_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/ilm"
	ilmmock "github.com/r3n3r0/nexo/internal/service/ilm/mock"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
)

// newService returns the service over mocks of its dependencies (the
// object layer when objects is nil).
func newService(t *testing.T, clk clock.Clock, opts ...ilm.Option) ilm.Service {
	t.Helper()
	svc, err := ilm.New(ilm.Deps{Objects: objectmock.NewService(t), Buckets: ilmmock.NewBucketSettings(t), Clock: clk}, opts...)
	require.NoError(t, err)
	return svc
}

// created is the creation time of the versions of the evaluation tests:
// with 3 days they expire on Jan 19 at midnight UTC.
var created = time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)

func version(key, id string, modTime time.Time, latest bool) domain.ObjectInfo {
	return domain.ObjectInfo{Bucket: "b", Key: key, VersionID: id, ModTime: modTime, IsLatest: latest, Size: 100}
}

func marker(key, id string, modTime time.Time, latest bool) domain.ObjectInfo {
	v := version(key, id, modTime, latest)
	v.IsDeleteMarker, v.Size = true, 0
	return v
}

func days(n int) *ilm.Expiration { return &ilm.Expiration{Days: n} }

func ptr(n int64) *int64 { return &n }

// TestEvaluate covers TS-U-11: the rules evaluated with a fake clock on
// the versions of a key (filters by prefix, tags and size, days and
// dates, versioning, noncurrent versions, delete markers, Object Lock)
// and on the multipart uploads.
func TestEvaluate(t *testing.T) {
	due := time.Date(2026, 1, 19, 0, 0, 0, 0, time.UTC)
	justBefore := due.Add(-time.Second)
	tagged := version("logs/a", "", created, true)
	tagged.Tags = map[string]string{"class": "tmp", "team": "x"}
	lockedRetention := version("k", "", created, true)
	lockedRetention.Retention = domain.ObjectRetention{Mode: domain.RetentionCompliance, RetainUntil: due.AddDate(1, 0, 0)}
	pastRetention := version("k", "", created, true)
	pastRetention.Retention = domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: due.Add(-time.Hour)}
	legalHold := version("k", "", created, true)
	legalHold.LegalHold = domain.LegalHoldOn
	lockedNoncurrent := version("k", "v1", created, false)
	lockedNoncurrent.LegalHold = domain.LegalHoldOn
	lockedCurrentVersioned := version("k", "v2", created, true)
	lockedCurrentVersioned.Retention = domain.ObjectRetention{Mode: domain.RetentionCompliance, RetainUntil: due.AddDate(1, 0, 0)}
	lockedNull := version("k", "null", created, true)
	lockedNull.LegalHold = domain.LegalHoldOn
	// A null version written before versioning was enabled, as a caller
	// that does not label it lists it (without id).
	lockedPreVersioning := version("k", "", created, true)
	lockedPreVersioning.LegalHold = domain.LegalHoldOn

	rule := func(id string, f ilm.Filter, mutate func(r *ilm.Rule)) ilm.Rule {
		r := ilm.Rule{ID: id, Enabled: true, Filter: f}
		mutate(&r)
		return r
	}
	expire3 := func(r *ilm.Rule) { r.Expiration = days(3) }
	noncurrent := func(n, keep int) func(r *ilm.Rule) {
		return func(r *ilm.Rule) {
			r.NoncurrentExpiration = &ilm.NoncurrentExpiration{NoncurrentDays: n, NewerNoncurrentVersions: keep}
		}
	}
	size := func(gt, lt *int64) ilm.Filter { return ilm.Filter{SizeGreaterThan: gt, SizeLessThan: lt} }

	tests := []struct {
		name       string
		now        time.Time
		rules      []ilm.Rule
		versioning domain.Versioning
		versions   []domain.ObjectInfo
		uploads    []domain.MultipartUpload
		want       []ilm.Action
	}{
		{
			name: "unversioned: not yet due", now: justBefore, rules: []ilm.Rule{rule("r", ilm.Filter{}, expire3)},
			versions: []domain.ObjectInfo{version("k", "", created, true)},
		},
		{
			name: "unversioned: expired at midnight", now: due, rules: []ilm.Rule{rule("r", ilm.Filter{}, expire3)},
			versions: []domain.ObjectInfo{version("k", "", created, true)},
			want:     []ilm.Action{{Type: ilm.ActionDeleteVersion, RuleID: "r", Bucket: "b", Key: "k", ModTime: created, Due: due}},
		},
		{
			name: "disabled rule", now: due, rules: []ilm.Rule{{ID: "r", Expiration: days(3)}},
			versions: []domain.ObjectInfo{version("k", "", created, true)},
		},
		{
			name: "expiration date", now: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
			rules:    []ilm.Rule{{ID: "date", Enabled: true, Expiration: &ilm.Expiration{Date: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}}},
			versions: []domain.ObjectInfo{version("k", "", created, true)},
			want:     []ilm.Action{{Type: ilm.ActionDeleteVersion, RuleID: "date", Bucket: "b", Key: "k", ModTime: created, Due: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}},
		},
		{
			name: "expiration date in the future", now: due,
			rules:    []ilm.Rule{{ID: "date", Enabled: true, Expiration: &ilm.Expiration{Date: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}}},
			versions: []domain.ObjectInfo{version("k", "", created, true)},
		},
		{
			name: "prefix mismatch", now: due, rules: []ilm.Rule{rule("r", ilm.Filter{Prefix: "logs/"}, expire3)},
			versions: []domain.ObjectInfo{version("data/k", "", created, true)},
		},
		{
			name: "tags match", now: due, rules: []ilm.Rule{rule("r", ilm.Filter{Prefix: "logs/", Tags: map[string]string{"class": "tmp"}}, expire3)},
			versions: []domain.ObjectInfo{tagged},
			want:     []ilm.Action{{Type: ilm.ActionDeleteVersion, RuleID: "r", Bucket: "b", Key: "logs/a", ModTime: created, Due: due}},
		},
		{
			name: "tags mismatch", now: due, rules: []ilm.Rule{rule("r", ilm.Filter{Tags: map[string]string{"class": "keep"}}, expire3)},
			versions: []domain.ObjectInfo{tagged},
		},
		{
			name: "size within bounds", now: due, rules: []ilm.Rule{rule("r", size(i64(99), i64(101)), expire3)},
			versions: []domain.ObjectInfo{version("k", "", created, true)},
			want:     []ilm.Action{{Type: ilm.ActionDeleteVersion, RuleID: "r", Bucket: "b", Key: "k", ModTime: created, Due: due}},
		},
		{
			name: "size bounds are exclusive", now: due, rules: []ilm.Rule{rule("r", size(i64(100), nil), expire3), rule("s", size(nil, i64(100)), expire3)},
			versions: []domain.ObjectInfo{version("k", "", created, true)},
		},
		{
			name: "earliest rule wins", now: due.AddDate(0, 0, 10),
			rules:    []ilm.Rule{rule("late", ilm.Filter{}, func(r *ilm.Rule) { r.Expiration = days(10) }), rule("early", ilm.Filter{}, expire3)},
			versions: []domain.ObjectInfo{version("k", "", created, true)},
			want:     []ilm.Action{{Type: ilm.ActionDeleteVersion, RuleID: "early", Bucket: "b", Key: "k", ModTime: created, Due: due}},
		},
		{
			name: "unversioned locked by retention", now: due, rules: []ilm.Rule{rule("r", ilm.Filter{}, expire3)},
			versions: []domain.ObjectInfo{lockedRetention},
		},
		{
			name: "unversioned retention elapsed", now: due, rules: []ilm.Rule{rule("r", ilm.Filter{}, expire3)},
			versions: []domain.ObjectInfo{pastRetention},
			want:     []ilm.Action{{Type: ilm.ActionDeleteVersion, RuleID: "r", Bucket: "b", Key: "k", ModTime: created, Due: due}},
		},
		{
			name: "unversioned legal hold", now: due, rules: []ilm.Rule{rule("r", ilm.Filter{}, expire3)},
			versions: []domain.ObjectInfo{legalHold},
		},
		{
			name: "versioned: a delete marker over the current version", now: due, versioning: domain.VersioningEnabled,
			rules:    []ilm.Rule{rule("r", ilm.Filter{}, expire3)},
			versions: []domain.ObjectInfo{version("k", "v2", created, true), version("k", "v1", created.Add(-time.Hour), false)},
			want:     []ilm.Action{{Type: ilm.ActionDeleteMarker, RuleID: "r", Bucket: "b", Key: "k", VersionID: "v2", ModTime: created, Due: due}},
		},
		{
			name: "versioned: a locked version still gets its delete marker", now: due, versioning: domain.VersioningEnabled,
			rules:    []ilm.Rule{rule("r", ilm.Filter{}, expire3)},
			versions: []domain.ObjectInfo{lockedCurrentVersioned},
			want:     []ilm.Action{{Type: ilm.ActionDeleteMarker, RuleID: "r", Bucket: "b", Key: "k", VersionID: "v2", ModTime: created, Due: due}},
		},
		{
			name: "suspended: a null delete marker", now: due, versioning: domain.VersioningSuspended,
			rules:    []ilm.Rule{rule("r", ilm.Filter{}, expire3)},
			versions: []domain.ObjectInfo{version("k", "null", created, true)},
			want:     []ilm.Action{{Type: ilm.ActionDeleteMarker, RuleID: "r", Bucket: "b", Key: "k", VersionID: "null", ModTime: created, Due: due}},
		},
		{
			name: "suspended: a locked null version is kept", now: due, versioning: domain.VersioningSuspended,
			rules:    []ilm.Rule{rule("r", ilm.Filter{}, expire3)},
			versions: []domain.ObjectInfo{lockedNull},
		},
		{
			name: "suspended: a locked null version written before versioning is kept", now: due, versioning: domain.VersioningSuspended,
			rules:    []ilm.Rule{rule("r", ilm.Filter{}, expire3)},
			versions: []domain.ObjectInfo{lockedPreVersioning},
		},
		{
			name: "noncurrent: the null version written before versioning was enabled", now: due.AddDate(0, 1, 0), versioning: domain.VersioningEnabled,
			rules: []ilm.Rule{rule("nc", ilm.Filter{}, noncurrent(1, 0))},
			versions: []domain.ObjectInfo{
				version("k", "v2", created.AddDate(0, 0, 1), true), version("k", "v1", created, false), version("k", "null", created.AddDate(0, 0, -1), false),
			},
			want: []ilm.Action{
				{Type: ilm.ActionDeleteVersion, RuleID: "nc", Bucket: "b", Key: "k", VersionID: "v1", ModTime: created, Due: time.Date(2026, 1, 18, 0, 0, 0, 0, time.UTC)},
				{Type: ilm.ActionDeleteVersion, RuleID: "nc", Bucket: "b", Key: "k", VersionID: "null", ModTime: created.AddDate(0, 0, -1), Due: time.Date(2026, 1, 17, 0, 0, 0, 0, time.UTC)},
			},
		},
		{
			name: "noncurrent: an unlabeled null version is evaluated too", now: due.AddDate(0, 1, 0), versioning: domain.VersioningEnabled,
			rules:    []ilm.Rule{rule("nc", ilm.Filter{}, noncurrent(1, 0))},
			versions: []domain.ObjectInfo{version("k", "v1", created, true), version("k", "", created.AddDate(0, 0, -1), false)},
			want:     []ilm.Action{{Type: ilm.ActionDeleteVersion, RuleID: "nc", Bucket: "b", Key: "k", ModTime: created.AddDate(0, 0, -1), Due: time.Date(2026, 1, 17, 0, 0, 0, 0, time.UTC)}},
		},
		{
			name: "current delete marker is not expired by days", now: due.AddDate(1, 0, 0), versioning: domain.VersioningEnabled,
			rules:    []ilm.Rule{rule("r", ilm.Filter{}, expire3)},
			versions: []domain.ObjectInfo{marker("k", "m", created, true), version("k", "v1", created.Add(-time.Hour), false)},
		},
		{
			name: "noncurrent: days counted from the successor", now: due, versioning: domain.VersioningEnabled,
			rules: []ilm.Rule{rule("nc", ilm.Filter{}, noncurrent(3, 0))},
			versions: []domain.ObjectInfo{
				version("k", "v3", created.AddDate(0, 0, 1), true), version("k", "v2", created, false), version("k", "v1", created.AddDate(0, 0, -30), false),
			},
			want: []ilm.Action{{Type: ilm.ActionDeleteVersion, RuleID: "nc", Bucket: "b", Key: "k", VersionID: "v1", ModTime: created.AddDate(0, 0, -30), Due: due}},
		},
		{
			name: "noncurrent: newer noncurrent versions are kept", now: due.AddDate(0, 1, 0), versioning: domain.VersioningEnabled,
			rules: []ilm.Rule{rule("nc", ilm.Filter{}, noncurrent(1, 2))},
			versions: []domain.ObjectInfo{
				version("k", "v4", created, true), version("k", "v3", created, false), version("k", "v2", created, false), version("k", "v1", created, false),
			},
			want: []ilm.Action{{Type: ilm.ActionDeleteVersion, RuleID: "nc", Bucket: "b", Key: "k", VersionID: "v1", ModTime: created, Due: time.Date(2026, 1, 17, 0, 0, 0, 0, time.UTC)}},
		},
		{
			name: "noncurrent: a locked version is kept, a noncurrent marker goes", now: due.AddDate(0, 1, 0), versioning: domain.VersioningEnabled,
			rules:    []ilm.Rule{rule("nc", ilm.Filter{}, noncurrent(1, 0))},
			versions: []domain.ObjectInfo{version("k", "v3", created, true), marker("k", "m2", created, false), lockedNoncurrent},
			want:     []ilm.Action{{Type: ilm.ActionDeleteVersion, RuleID: "nc", Bucket: "b", Key: "k", VersionID: "m2", ModTime: created, Due: time.Date(2026, 1, 17, 0, 0, 0, 0, time.UTC)}},
		},
		{
			name: "noncurrent: a tag filter never selects a marker", now: due.AddDate(0, 1, 0), versioning: domain.VersioningEnabled,
			rules:    []ilm.Rule{rule("nc", ilm.Filter{Tags: map[string]string{"a": "b"}}, noncurrent(1, 0))},
			versions: []domain.ObjectInfo{version("k", "v3", created, true), marker("k", "m2", created, false)},
		},
		{
			name: "expired object delete marker", now: created, versioning: domain.VersioningEnabled,
			rules:    []ilm.Rule{rule("dm", ilm.Filter{}, func(r *ilm.Rule) { r.Expiration = &ilm.Expiration{ExpiredObjectDeleteMarker: true} })},
			versions: []domain.ObjectInfo{marker("k", "m", created, true)},
			want:     []ilm.Action{{Type: ilm.ActionDeleteVersion, RuleID: "dm", Bucket: "b", Key: "k", VersionID: "m", ModTime: created, Due: created}},
		},
		{
			// AWS: "When you specify the Days tag, Amazon S3 automatically
			// performs ExpiredObjectDeleteMarker cleanup when the delete
			// markers are old enough to satisfy the age criteria".
			name: "an expiration in days removes an expired object delete marker once old enough", now: due.AddDate(0, 1, 0), versioning: domain.VersioningEnabled,
			rules:    []ilm.Rule{rule("r", ilm.Filter{}, expire3), rule("nc", ilm.Filter{}, noncurrent(1, 0))},
			versions: []domain.ObjectInfo{marker("k", "m", created, true)},
			want:     []ilm.Action{{Type: ilm.ActionDeleteVersion, RuleID: "r", Bucket: "b", Key: "k", VersionID: "m", ModTime: created, Due: due}},
		},
		{
			name: "an expired object delete marker younger than the days stays", now: justBefore, versioning: domain.VersioningEnabled,
			rules:    []ilm.Rule{rule("r", ilm.Filter{}, expire3)},
			versions: []domain.ObjectInfo{marker("k", "m", created, true)},
		},
		{
			name: "an expiration date does not remove delete markers", now: due.AddDate(1, 0, 0), versioning: domain.VersioningEnabled,
			rules:    []ilm.Rule{{ID: "date", Enabled: true, Expiration: &ilm.Expiration{Date: due}}},
			versions: []domain.ObjectInfo{marker("k", "m", created, true)},
		},
		{
			// AWS example: ObjectSizeLessThan=1 expires the noncurrent
			// objects without data, delete markers included (0 bytes).
			name: "noncurrent: a size filter sees a marker as 0 bytes", now: due.AddDate(0, 1, 0), versioning: domain.VersioningEnabled,
			rules:    []ilm.Rule{rule("empty", size(nil, ptr(1)), noncurrent(1, 0))},
			versions: []domain.ObjectInfo{version("k", "v2", created, true), marker("k", "m1", created, false)},
			want:     []ilm.Action{{Type: ilm.ActionDeleteVersion, RuleID: "empty", Bucket: "b", Key: "k", VersionID: "m1", ModTime: created, Due: time.Date(2026, 1, 17, 0, 0, 0, 0, time.UTC)}},
		},
		{
			name: "noncurrent: a lower size bound never selects a marker", now: due.AddDate(0, 1, 0), versioning: domain.VersioningEnabled,
			rules:    []ilm.Rule{rule("big", size(ptr(0), nil), noncurrent(1, 0))},
			versions: []domain.ObjectInfo{version("k", "v2", created, true), marker("k", "m1", created, false)},
		},
		{
			name: "a delete marker with versions behind it is not expired", now: created, versioning: domain.VersioningEnabled,
			rules:    []ilm.Rule{rule("dm", ilm.Filter{}, func(r *ilm.Rule) { r.Expiration = &ilm.Expiration{ExpiredObjectDeleteMarker: true} })},
			versions: []domain.ObjectInfo{marker("k", "m", created, true), version("k", "v1", created.Add(-time.Hour), false)},
		},
		{
			name: "abort incomplete uploads by prefix", now: due,
			rules: []ilm.Rule{rule("mpu", ilm.Filter{Prefix: "up/"}, func(r *ilm.Rule) { r.AbortIncompleteUpload = &ilm.AbortIncompleteUpload{DaysAfterInitiation: 3} })},
			uploads: []domain.MultipartUpload{
				{Key: "up/a", UploadID: "u1", Initiated: created},
				{Key: "up/b", UploadID: "u2", Initiated: created.AddDate(0, 0, 1)},
				{Key: "other", UploadID: "u3", Initiated: created},
			},
			want: []ilm.Action{{Type: ilm.ActionAbortUpload, RuleID: "mpu", Bucket: "b", Key: "up/a", UploadID: "u1", Due: due}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := newService(t, clock.NewFake(tc.now))
			got, err := svc.Evaluate(context.Background(), ilm.EvaluateInput{
				Bucket: "b", Config: &ilm.Configuration{Rules: tc.rules}, Versioning: tc.versioning, Versions: tc.versions, Uploads: tc.uploads,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
	none, err := newService(t, clock.NewFake(due)).Evaluate(context.Background(), ilm.EvaluateInput{Versions: []domain.ObjectInfo{version("k", "", created, true)}})
	require.NoError(t, err)
	assert.Empty(t, none, "no configuration")
}
