package ilm_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/ilm"
)

// doc wraps rules in a LifecycleConfiguration.
func doc(rules ...string) string {
	return `<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` + strings.Join(rules, "") + `</LifecycleConfiguration>`
}

func i64(v int64) *int64 { return &v }

func TestParse(t *testing.T) {
	svc := newService(t, nil)
	ctx := context.Background()
	tests := []struct {
		name string
		doc  string
		want []ilm.Rule
	}{
		{
			name: "expiration days with prefix filter",
			doc:  doc(`<Rule><ID>logs</ID><Filter><Prefix>logs/</Prefix></Filter><Status>Enabled</Status><Expiration><Days>30</Days></Expiration></Rule>`),
			want: []ilm.Rule{{ID: "logs", Enabled: true, Filter: ilm.Filter{Prefix: "logs/"}, Expiration: &ilm.Expiration{Days: 30}}},
		},
		{
			name: "legacy prefix and date",
			doc:  doc(`<Rule><ID>d</ID><Prefix>tmp/</Prefix><Status>Disabled</Status><Expiration><Date>2027-01-01T00:00:00Z</Date></Expiration></Rule>`),
			want: []ilm.Rule{{ID: "d", Filter: ilm.Filter{Prefix: "tmp/"}, Expiration: &ilm.Expiration{Date: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}}},
		},
		{
			name: "tag filter and noncurrent",
			doc: doc(`<Rule><ID>t</ID><Filter><Tag><Key>class</Key><Value>tmp</Value></Tag></Filter><Status>Enabled</Status>
				<NoncurrentVersionExpiration><NoncurrentDays>7</NoncurrentDays><NewerNoncurrentVersions>2</NewerNoncurrentVersions></NoncurrentVersionExpiration></Rule>`),
			want: []ilm.Rule{{ID: "t", Enabled: true, Filter: ilm.Filter{Tags: map[string]string{"class": "tmp"}},
				NoncurrentExpiration: &ilm.NoncurrentExpiration{NoncurrentDays: 7, NewerNoncurrentVersions: 2}}},
		},
		{
			name: "and filter with sizes",
			doc: doc(`<Rule><ID>a</ID><Filter><And><Prefix>p/</Prefix><Tag><Key>k1</Key><Value>v1</Value></Tag><Tag><Key>k2</Key><Value>v2</Value></Tag>
				<ObjectSizeGreaterThan>10</ObjectSizeGreaterThan><ObjectSizeLessThan>100</ObjectSizeLessThan></And></Filter><Status>Enabled</Status>
				<Expiration><Days>1</Days></Expiration></Rule>`),
			want: []ilm.Rule{{ID: "a", Enabled: true, Filter: ilm.Filter{Prefix: "p/", Tags: map[string]string{"k1": "v1", "k2": "v2"},
				SizeGreaterThan: i64(10), SizeLessThan: i64(100)}, Expiration: &ilm.Expiration{Days: 1}}},
		},
		{
			name: "size filter alone",
			doc:  doc(`<Rule><ID>s</ID><Filter><ObjectSizeGreaterThan>0</ObjectSizeGreaterThan></Filter><Status>Enabled</Status><Expiration><Days>2</Days></Expiration></Rule>`),
			want: []ilm.Rule{{ID: "s", Enabled: true, Filter: ilm.Filter{SizeGreaterThan: i64(0)}, Expiration: &ilm.Expiration{Days: 2}}},
		},
		{
			name: "expired delete marker and abort uploads, empty filter",
			doc: doc(`<Rule><ID>m</ID><Filter></Filter><Status>Enabled</Status><Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>
				<AbortIncompleteMultipartUpload><DaysAfterInitiation>3</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule>`),
			want: []ilm.Rule{{ID: "m", Enabled: true, Expiration: &ilm.Expiration{ExpiredObjectDeleteMarker: true},
				AbortIncompleteUpload: &ilm.AbortIncompleteUpload{DaysAfterInitiation: 3}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := svc.Parse(ctx, []byte(tc.doc))
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.Rules)
		})
	}
}

func TestParseErrors(t *testing.T) {
	svc := newService(t, nil)
	ctx := context.Background()
	exp := `<Expiration><Days>1</Days></Expiration>`
	many := make([]string, 1001)
	for i := range many {
		many[i] = fmt.Sprintf(`<Rule><ID>r%d</ID><Status>Enabled</Status>%s</Rule>`, i, exp)
	}
	tests := []struct {
		name     string
		doc      string
		sentinel error
		code     string
	}{
		{"not xml", `{}`, domain.ErrMalformedXML, "MalformedXML"},
		{"empty", ``, domain.ErrMalformedXML, "MalformedXML"},
		{"no rule", doc(), domain.ErrMalformedXML, "MalformedXML"},
		{"too many rules", doc(many...), domain.ErrInvalidArgument, "InvalidArgument"},
		{"duplicate ids", doc(`<Rule><ID>x</ID><Status>Enabled</Status>`+exp+`</Rule>`, `<Rule><ID>x</ID><Status>Enabled</Status>`+exp+`</Rule>`), domain.ErrInvalidArgument, "InvalidArgument"},
		{"long id", doc(`<Rule><ID>` + strings.Repeat("x", 256) + `</ID><Status>Enabled</Status>` + exp + `</Rule>`), domain.ErrInvalidArgument, "InvalidArgument"},
		{"bad status", doc(`<Rule><Status>On</Status>` + exp + `</Rule>`), domain.ErrMalformedXML, "MalformedXML"},
		{"filter and prefix", doc(`<Rule><Prefix>a</Prefix><Filter><Prefix>b</Prefix></Filter><Status>Enabled</Status>` + exp + `</Rule>`), domain.ErrMalformedXML, "MalformedXML"},
		{"two filter criteria", doc(`<Rule><Filter><Prefix>a</Prefix><Tag><Key>k</Key><Value>v</Value></Tag></Filter><Status>Enabled</Status>` + exp + `</Rule>`), domain.ErrMalformedXML, "MalformedXML"},
		{"empty tag key", doc(`<Rule><Filter><Tag><Key></Key><Value>v</Value></Tag></Filter><Status>Enabled</Status>` + exp + `</Rule>`), domain.ErrInvalidArgument, "InvalidArgument"},
		{"duplicate tag", doc(`<Rule><Filter><And><Tag><Key>k</Key><Value>1</Value></Tag><Tag><Key>k</Key><Value>2</Value></Tag></And></Filter><Status>Enabled</Status>` + exp + `</Rule>`), domain.ErrInvalidArgument, "InvalidArgument"},
		{"size bounds inverted", doc(`<Rule><Filter><And><ObjectSizeGreaterThan>10</ObjectSizeGreaterThan><ObjectSizeLessThan>5</ObjectSizeLessThan></And></Filter><Status>Enabled</Status>` + exp + `</Rule>`), domain.ErrInvalidArgument, "InvalidArgument"},
		{"negative size", doc(`<Rule><Filter><ObjectSizeLessThan>-1</ObjectSizeLessThan></Filter><Status>Enabled</Status>` + exp + `</Rule>`), domain.ErrInvalidArgument, "InvalidArgument"},
		{"no action", doc(`<Rule><Status>Enabled</Status></Rule>`), domain.ErrInvalidArgument, "InvalidRequest"},
		{"zero days", doc(`<Rule><Status>Enabled</Status><Expiration><Days>0</Days></Expiration></Rule>`), domain.ErrInvalidArgument, "InvalidArgument"},
		{"days and date", doc(`<Rule><Status>Enabled</Status><Expiration><Days>1</Days><Date>2027-01-01T00:00:00Z</Date></Expiration></Rule>`), domain.ErrInvalidArgument, "InvalidRequest"},
		{"days and delete marker", doc(`<Rule><Status>Enabled</Status><Expiration><Days>1</Days><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration></Rule>`), domain.ErrInvalidArgument, "InvalidRequest"},
		{"date not at midnight", doc(`<Rule><Status>Enabled</Status><Expiration><Date>2027-01-01T10:00:00Z</Date></Expiration></Rule>`), domain.ErrInvalidArgument, "InvalidArgument"},
		{"bad date", doc(`<Rule><Status>Enabled</Status><Expiration><Date>tomorrow</Date></Expiration></Rule>`), domain.ErrInvalidArgument, "InvalidArgument"},
		{"delete marker with tags", doc(`<Rule><Filter><Tag><Key>k</Key><Value>v</Value></Tag></Filter><Status>Enabled</Status><Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration></Rule>`), domain.ErrInvalidArgument, "InvalidRequest"},
		{"empty expiration", doc(`<Rule><Status>Enabled</Status><Expiration><ExpiredObjectDeleteMarker>false</ExpiredObjectDeleteMarker></Expiration></Rule>`), domain.ErrInvalidArgument, "InvalidRequest"},
		{"noncurrent without days", doc(`<Rule><Status>Enabled</Status><NoncurrentVersionExpiration></NoncurrentVersionExpiration></Rule>`), domain.ErrInvalidArgument, "InvalidArgument"},
		{"too many newer versions", doc(`<Rule><Status>Enabled</Status><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays><NewerNoncurrentVersions>101</NewerNoncurrentVersions></NoncurrentVersionExpiration></Rule>`), domain.ErrInvalidArgument, "InvalidArgument"},
		{"abort without days", doc(`<Rule><Status>Enabled</Status><AbortIncompleteMultipartUpload></AbortIncompleteMultipartUpload></Rule>`), domain.ErrInvalidArgument, "InvalidArgument"},
		{"abort with tags", doc(`<Rule><Filter><Tag><Key>k</Key><Value>v</Value></Tag></Filter><Status>Enabled</Status><AbortIncompleteMultipartUpload><DaysAfterInitiation>1</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule>`), domain.ErrInvalidArgument, "InvalidRequest"},
		{"transition without tier", doc(`<Rule><Status>Enabled</Status><Transition><Days>30</Days><StorageClass>GLACIER</StorageClass></Transition></Rule>`), domain.ErrNotImplemented, "NotImplemented"},
		{"noncurrent transition without tier", doc(`<Rule><Status>Enabled</Status><NoncurrentVersionTransition><NoncurrentDays>30</NoncurrentDays><StorageClass>WARM</StorageClass></NoncurrentVersionTransition></Rule>`), domain.ErrNotImplemented, "NotImplemented"},
		{"transition without class", doc(`<Rule><Status>Enabled</Status><Transition><Days>30</Days></Transition></Rule>`), domain.ErrMalformedXML, "MalformedXML"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Parse(ctx, []byte(tc.doc))
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.sentinel)
			var s3err *domain.S3Error
			require.True(t, errors.As(err, &s3err), "%v", err)
			assert.Equal(t, tc.code, s3err.Code)
		})
	}
}

func TestParseTransitionWithTier(t *testing.T) {
	svc := newService(t, nil, ilm.WithStorageClasses("WARM"))
	cfg, err := svc.Parse(context.Background(), []byte(doc(`<Rule><ID>t</ID><Status>Enabled</Status>
		<Transition><Days>30</Days><StorageClass>WARM</StorageClass></Transition>
		<NoncurrentVersionTransition><NoncurrentDays>5</NoncurrentDays><StorageClass>WARM</StorageClass></NoncurrentVersionTransition></Rule>`)))
	require.NoError(t, err)
	assert.Equal(t, []ilm.Transition{{Days: 30, StorageClass: "WARM"}}, cfg.Rules[0].Transitions)
	assert.Equal(t, []ilm.NoncurrentTransition{{NoncurrentDays: 5, StorageClass: "WARM"}}, cfg.Rules[0].NoncurrentTransitions)
}

// TestValidateCanonical: the canonical document names every rule and
// parses back to the same configuration.
func TestValidateCanonical(t *testing.T) {
	svc := newService(t, nil, ilm.WithStorageClasses("WARM"))
	ctx := context.Background()
	in := doc(
		`<Rule><Filter><Prefix>logs/</Prefix></Filter><Status>Enabled</Status><Expiration><Days>30</Days></Expiration></Rule>`,
		`<Rule><ID>tags</ID><Filter><And><Prefix>a/</Prefix><Tag><Key>b</Key><Value>2</Value></Tag><Tag><Key>a</Key><Value>1</Value></Tag></And></Filter><Status>Disabled</Status>
			<Expiration><Date>2027-03-01T00:00:00.000Z</Date></Expiration><NoncurrentVersionExpiration><NoncurrentDays>3</NoncurrentDays><NewerNoncurrentVersions>1</NewerNoncurrentVersions></NoncurrentVersionExpiration></Rule>`,
		`<Rule><ID>tag</ID><Filter><Tag><Key>k</Key><Value>v</Value></Tag></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule>`,
		`<Rule><ID>size</ID><Filter><ObjectSizeLessThan>1024</ObjectSizeLessThan></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule>`,
		`<Rule><ID>misc</ID><Status>Enabled</Status><Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>
			<AbortIncompleteMultipartUpload><DaysAfterInitiation>2</DaysAfterInitiation></AbortIncompleteMultipartUpload>
			<Transition><Date>2027-01-01T00:00:00Z</Date><StorageClass>WARM</StorageClass></Transition>
			<NoncurrentVersionTransition><NoncurrentDays>4</NoncurrentDays><NewerNoncurrentVersions>2</NewerNoncurrentVersions><StorageClass>WARM</StorageClass></NoncurrentVersionTransition></Rule>`,
	)
	canonical, err := svc.Validate(ctx, []byte(in))
	require.NoError(t, err)
	assert.Contains(t, string(canonical), `<ID>nexo-`, "a generated id")
	assert.Contains(t, string(canonical), `<Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>b</Key>`, "tags sorted")
	first, err := svc.Parse(ctx, []byte(in))
	require.NoError(t, err)
	again, err := svc.Parse(ctx, canonical)
	require.NoError(t, err)
	assert.Equal(t, first, again)
	twice, err := svc.Validate(ctx, canonical)
	require.NoError(t, err)
	assert.Equal(t, string(canonical), string(twice), "the canonical form is stable")
}
