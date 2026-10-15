package ilm

import (
	"errors"
	"testing"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// FuzzParseLifecycle checks the parser of the LifecycleConfiguration
// documents (PUT ?lifecycle, the console): no panic, failures are S3
// errors (malformed XML, invalid arguments, unsupported tiers), and an accepted
// configuration evaluates any version and upload without panicking.
func FuzzParseLifecycle(f *testing.F) {
	for _, seed := range []string{
		`<LifecycleConfiguration><Rule><ID>a</ID><Filter><Prefix>logs/</Prefix></Filter><Status>Enabled</Status><Expiration><Days>30</Days></Expiration></Rule></LifecycleConfiguration>`,
		`<LifecycleConfiguration><Rule><Prefix>tmp/</Prefix><Status>Disabled</Status><Expiration><Date>2027-01-01T00:00:00Z</Date></Expiration></Rule></LifecycleConfiguration>`,
		`<LifecycleConfiguration><Rule><Filter><And><Prefix>a/</Prefix><Tag><Key>k</Key><Value>v</Value></Tag><ObjectSizeGreaterThan>10</ObjectSizeGreaterThan></And></Filter><Status>Enabled</Status><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays><NewerNoncurrentVersions>2</NewerNoncurrentVersions></NoncurrentVersionExpiration><AbortIncompleteMultipartUpload><DaysAfterInitiation>7</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule></LifecycleConfiguration>`,
		`<LifecycleConfiguration><Rule><Filter></Filter><Status>Enabled</Status><Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration><Transition><Days>1</Days><StorageClass>COLD</StorageClass></Transition></Rule></LifecycleConfiguration>`,
		`<LifecycleConfiguration><Rule><Status>Enabled</Status><Expiration><Days>-1</Days></Expiration></Rule></LifecycleConfiguration>`,
		`<LifecycleConfiguration>`, ``, `<x/>`,
	} {
		f.Add([]byte(seed), int64(1<<20), int64(3*24))
	}
	tiers := map[string]bool{"COLD": true}
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, doc []byte, size int64, ageHours int64) {
		cfg, err := parseConfiguration(doc, tiers)
		if err != nil {
			var s3err *domain.S3Error
			if !errors.As(err, &s3err) {
				t.Fatalf("parseConfiguration: unexpected error %v", err)
			}
			return
		}
		if ageHours < 0 {
			ageHours = -ageHours
		}
		modTime := now.Add(-time.Duration(ageHours%(24*3650)) * time.Hour)
		versions := []domain.ObjectInfo{
			{Bucket: "b", Key: "logs/a", VersionID: "v2", Size: size, ModTime: modTime, IsLatest: true, Tags: map[string]string{"k": "v"}},
			{Bucket: "b", Key: "logs/a", VersionID: "v1", Size: size / 2, ModTime: modTime.Add(-time.Hour)},
			{Bucket: "b", Key: "logs/a", VersionID: "v0", IsDeleteMarker: true, ModTime: modTime.Add(-2 * time.Hour)},
		}
		for _, v := range []domain.Versioning{domain.VersioningOff, domain.VersioningEnabled, domain.VersioningSuspended} {
			_ = evaluate(now, EvaluateInput{Bucket: "b", Config: cfg, Versioning: v, Versions: versions,
				Uploads: []domain.MultipartUpload{{Bucket: "b", Key: "logs/u", UploadID: "u1", Initiated: modTime}}})
		}
	})
}
