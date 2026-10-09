package commands_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/cli/clitest"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The scenarios of the M7 settings commands (C-OBJ-09, T-7.10): Object
// Lock, lifecycle, events, CORS, server-side encryption and checksums
// against the in-memory store.

// covers: C-OBJ-09, S-BKT-08, S-OBJ-12, TS-U-12
func TestScenarioObjectLock(t *testing.T) {
	h := newHarness(t)
	h.write("doc.txt", "locked content\n")
	h.write("draft.txt", "draft\n")
	s := h.session()
	s.run("mb", "--with-lock", "mem/vault")
	s.run("mb", "mem/plain")
	s.run("stat", "mem/vault")
	s.run("retention", "info", "mem/vault")
	s.run("retention", "info", "mem/plain")
	s.run("retention", "set", "--default", "--mode", "governance", "--validity", "7d", "mem/vault")
	s.run("retention", "info", "--default", "mem/vault")
	s.run("retention", "set", "--default", "--mode", "governance", "--validity", "7d", "mem/plain")
	s.run("cp", "doc.txt", "draft.txt", "mem/vault/")
	s.run("retention", "info", "mem/vault/doc.txt")
	s.run("stat", "mem/vault/doc.txt")
	s.run("retention", "set", "--mode", "compliance", "--validity", "1y", "mem/vault/doc.txt")
	s.run("retention", "clear", "--bypass", "mem/vault/doc.txt")
	s.run("legalhold", "set", "mem/vault/draft.txt")
	s.run("legalhold", "info", "-r", "mem/vault/")
	s.run("retention", "info", "-r", "--json", "mem/vault")
	s.run("rm", "--versions", "mem/vault/draft.txt")
	s.run("rb", "--force", "mem/vault")
	s.run("legalhold", "clear", "mem/vault/draft.txt")
	s.run("rm", "--versions", "mem/vault/draft.txt")
	s.run("rm", "--versions", "--bypass", "mem/vault/draft.txt")
	s.run("ls", "--versions", "--recursive", "mem/vault")
	s.run("retention", "clear", "--default", "mem/vault")
	s.run("retention", "info", "mem/vault")
	// Usage errors (exit code 2).
	s.run("retention", "set", "--mode", "governance", "mem/vault/doc.txt")
	s.run("retention", "set", "--mode", "forever", "--validity", "1d", "mem/vault/doc.txt")
	s.run("retention", "set", "--mode", "governance", "--validity", "3w", "mem/vault/doc.txt")
	s.run("retention", "set", "--mode", "governance", "--validity", "1d", "mem/vault")
	s.run("retention", "set", "--default", "--bypass", "--mode", "governance", "--validity", "1d", "mem/vault")
	s.run("legalhold", "set", "mem/vault")
	s.run("legalhold", "set", "--version-id", "v1", "-r", "mem/vault/")
	s.run("legalhold", "info", "mem/plain/missing")
	golden(t, "scenario/object_lock", s.b.String())

	state, ok := h.mem.State("vault", "doc.txt", "")
	require.True(t, ok)
	assert.Equal(t, domain.RetentionCompliance, state.Retention.Mode)
	assert.Equal(t, t0.AddDate(1, 0, 0), state.Retention.RetainUntil)
}

// covers: C-OBJ-09, S-BKT-07, TS-U-11
func TestScenarioILM(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("app")
	h.mem.MakeBucket("copy")
	h.write("lifecycle.xml", `<?xml version="1.0" encoding="UTF-8"?>
<LifecycleConfiguration>
  <Rule><ID>tmp</ID><Filter><Prefix>tmp/</Prefix></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule>
  <Rule><ID>old</ID><Filter><And><Prefix>old/</Prefix><Tag><Key>a</Key><Value>1</Value></Tag></And></Filter><Status>Disabled</Status>
    <NoncurrentVersionExpiration><NoncurrentDays>5</NoncurrentDays></NoncurrentVersionExpiration></Rule>
</LifecycleConfiguration>
`)
	h.write("bad.json", `{"rules": [{"id": "x", "expiration": {"days": "soon"}}]}`)
	h.admin.SetLifecycleApplyResult(nexoadmin.LifecycleApplyResult{Buckets: 1, Versions: 12, Deleted: 3, DeleteMarkers: 1, UploadsAborted: 2, Locked: 1})
	h.admin.SetBucket(nexoadmin.Bucket{Name: "app"})
	s := h.session()
	s.run("ilm", "rule", "ls", "mem/app")
	s.run("ilm", "rule", "add", "--prefix", "logs/", "--expire-days", "30", "mem/app")
	s.run("ilm", "rule", "add", "--id", "versions", "--noncurrent-expire-days", "7", "--newer-noncurrent", "3", "mem/app")
	s.run("ilm", "rule", "add", "--tags", "tier=cold&team=ops", "--size-gt", "1MiB", "--expire-date", "2030-01-02", "mem/app")
	s.run("ilm", "rule", "add", "--abort-mpu-days", "2", "--expired-delete-marker", "--disable", "mem/app")
	s.run("ilm", "rule", "ls", "mem/app")
	s.run("ilm", "rule", "edit", "--id", "rule-1", "--expire-days", "90", "--prefix", "", "mem/app")
	s.run("ilm", "rule", "edit", "--id", "rule-3", "--enable", "--abort-mpu-days", "0", "mem/app")
	s.run("ilm", "rule", "ls", "--json", "mem/app")
	s.run("ilm", "rule", "export", "mem/app")
	s.run("ilm", "rule", "export", "--xml", "mem/app")
	s.run("ilm", "rule", "rm", "--id", "versions", "mem/app")
	s.run("ilm", "rule", "rm", "--id", "ghost", "mem/app")
	s.run("ilm", "rule", "import", "mem/copy", "lifecycle.xml")
	s.run("ilm", "rule", "ls", "mem/copy")
	s.run("ilm", "rule", "rm", "--all", "mem/copy")
	s.run("ilm", "rule", "ls", "mem/copy")
	s.run("ilm", "apply", "mem/app")
	// Usage errors.
	s.run("ilm", "rule", "add", "--prefix", "x/", "mem/app")
	s.run("ilm", "rule", "add", "--id", "rule-1", "--expire-days", "1", "mem/app")
	s.run("ilm", "rule", "add", "--expire-date", "tomorrow", "mem/app")
	s.run("ilm", "rule", "edit", "--expire-days", "1", "mem/app")
	s.run("ilm", "rule", "edit", "--id", "nope", "--expire-days", "1", "mem/app")
	s.run("ilm", "rule", "rm", "mem/app")
	s.run("ilm", "rule", "import", "mem/app", "bad.json")
	s.run("ilm", "rule", "ls", "mem/app/key")
	golden(t, "scenario/ilm", s.b.String())

	// export | import round-trips the configuration.
	r := h.mustRun("ilm", "rule", "export", "mem/app")
	h.stdin = r.stdout
	h.mustRun("ilm", "rule", "import", "mem/copy")
	assert.Equal(t, h.mem.Lifecycle("app"), h.mem.Lifecycle("copy"))
}

// covers: C-OBJ-09, S-BKT-10
func TestScenarioEvents(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("photos")
	const arn = "arn:nexo:sqs:us-east-1:_:webhook"
	h.admin.SetNotifyTargets(
		nexoadmin.NotifyTarget{ID: "_", Type: "webhook", ARN: arn, Enabled: true, Endpoint: "http://hook.test/events", Online: true},
		nexoadmin.NotifyTarget{ID: "audit", Type: "kafka", ARN: "arn:nexo:sqs:us-east-1:audit:kafka", Enabled: true, Endpoint: "kafka:9092",
			PendingEvents: 4, LastError: "dial tcp: connection refused"},
	)
	s := h.session()
	s.run("admin", "notify", "targets", "ls", "mem")
	s.run("admin", "notify", "targets", "test", "mem", arn)
	s.run("admin", "notify", "targets", "test", "mem", "audit:kafka")
	s.run("admin", "notify", "targets", "test", "mem", "ghost:webhook")
	s.run("event", "ls", "mem/photos")
	s.run("event", "add", "mem/photos", arn, "--event", "put", "--suffix", ".jpg")
	s.run("event", "add", "mem/photos", arn, "--event", "put", "--suffix", ".jpg")
	s.run("event", "add", "-p", "mem/photos", arn, "--event", "put", "--suffix", ".jpg")
	s.run("event", "add", "mem/photos", "arn:nexo:sqs:us-east-1:audit:kafka", "--prefix", "raw/")
	s.run("event", "ls", "mem/photos")
	s.run("event", "ls", "--json", "mem/photos", arn)
	s.run("event", "rm", "mem/photos", arn, "--event", "delete")
	s.run("event", "rm", "mem/photos", arn)
	s.run("event", "rm", "--force", "mem/photos")
	s.run("event", "ls", "mem/photos")
	// Usage errors.
	s.run("event", "add", "mem/photos", "not-an-arn")
	s.run("event", "add", "mem/photos", arn, "--event", "teleport")
	s.run("event", "rm", "mem/photos")
	golden(t, "scenario/events", s.b.String())
}

// covers: C-OBJ-09, S-BKT-06
func TestScenarioCORS(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("site")
	h.write("cors.json", `{"rules": [{"id": "web", "allowedOrigins": ["https://app.example.com"], "allowedMethods": ["GET", "PUT"], "maxAgeSeconds": 600}]}`)
	h.write("aws.json", `{"CORSRules": [{"AllowedOrigins": ["*"], "AllowedMethods": ["GET"], "ExposeHeaders": ["ETag"]}]}`)
	h.write("cors.xml", `<CORSConfiguration><CORSRule><AllowedOrigin>https://*.example.com</AllowedOrigin><AllowedMethod>HEAD</AllowedMethod><AllowedHeader>*</AllowedHeader></CORSRule></CORSConfiguration>`)
	h.write("bad.json", `{"rules": [{"allowedOrigins": ["*"], "allowedMethods": ["PATCH"]}]}`)
	s := h.session()
	s.run("cors", "get", "mem/site")
	s.run("cors", "set", "mem/site", "cors.json")
	s.run("cors", "get", "mem/site")
	s.run("cors", "get", "--xml", "mem/site")
	s.run("cors", "set", "mem/site", "aws.json")
	s.run("cors", "get", "--json", "mem/site")
	s.run("cors", "set", "mem/site", "cors.xml")
	s.run("cors", "get", "mem/site")
	s.run("cors", "set", "mem/site", "bad.json")
	s.run("cors", "set", "mem/site", "missing.json")
	s.run("cors", "rm", "mem/site")
	s.run("cors", "get", "mem/site")
	golden(t, "scenario/cors", s.b.String())
}

// covers: C-OBJ-09, S-OBJ-09, S-OBJ-13, S-BKT-11, TS-U-13
func TestScenarioEncryptionAndChecksums(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("enc")
	h.write("secret.txt", "top secret\n")
	h.write("plain.txt", "plain\n")
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	other := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("o", 32)))
	s := h.session()
	s.run("cp", "--enc-s3", "mem/enc/s3/", "plain.txt", "mem/enc/s3/plain.txt")
	s.run("cp", "--enc-kms", "mem/enc/kms=app-key", "plain.txt", "mem/enc/kms/plain.txt")
	s.run("put", "--enc-c", "mem/enc/c="+key, "secret.txt", "mem/enc/c/secret.txt")
	s.run("stat", "mem/enc/s3/plain.txt")
	s.run("stat", "mem/enc/kms/plain.txt")
	s.run("stat", "mem/enc/c/secret.txt")
	s.run("cat", "mem/enc/c/secret.txt")
	s.run("cat", "--enc-c", "mem/enc/c="+other, "mem/enc/c/secret.txt")
	s.run("cat", "--enc-c", "mem/enc/c="+key, "mem/enc/c/secret.txt")
	s.run("stat", "--json", "--enc-c", "mem/enc/c="+key, "mem/enc/c/secret.txt")
	s.run("cp", "--enc-c", "mem/enc/c="+key, "mem/enc/c/secret.txt", "out/secret.txt")
	s.run("cp", "--enc-c", "mem/enc/c="+key, "--enc-s3", "mem/enc/copy", "mem/enc/c/secret.txt", "mem/enc/copy/secret.txt")
	s.run("cat", "mem/enc/copy/secret.txt")
	s.run("cp", "--checksum", "sha256", "plain.txt", "mem/enc/sum/plain.txt")
	s.run("put", "--checksum", "crc32c", "--json", "plain.txt", "mem/enc/sum/crc.txt")
	s.run("stat", "--checksum", "mem/enc/sum/plain.txt")
	// Usage errors.
	s.run("cp", "--enc-c", "mem/enc/c=short", "plain.txt", "mem/enc/c/x.txt")
	s.run("cp", "--enc-c", "mem/enc/c", "plain.txt", "mem/enc/c/x.txt")
	s.run("cp", "--enc-s3", "mem/enc/c=key", "plain.txt", "mem/enc/c/x.txt")
	s.run("cp", "--enc-s3", "mem/enc/c", "--enc-c", "mem/enc/c="+key, "plain.txt", "mem/enc/c/x.txt")
	s.run("cp", "--enc-s3", "ghost/enc", "plain.txt", "mem/enc/c/x.txt")
	s.run("cp", "--checksum", "md5", "plain.txt", "mem/enc/c/x.txt")
	s.run("put", "mem/enc/s3/plain.txt", "mem/enc/other.txt")
	s.run("put", "plain.txt", "local-copy.txt")
	golden(t, "scenario/encryption", s.b.String())
	assert.Equal(t, "top secret\n", h.read("out/secret.txt"))
	state, ok := h.mem.State("enc", "copy/secret.txt", "")
	require.True(t, ok)
	assert.Equal(t, domain.SSES3, state.Encryption.Type)
}

// covers: C-OBJ-09, C-ADM-* (admin kms), S-BKT-09, S-BKT-11
func TestScenarioKMSEncryptAndQuota(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("docs")
	h.admin.SetBucket(nexoadmin.Bucket{Name: "docs", Size: 768 << 10, Objects: 3})
	s := h.session()
	s.run("admin", "kms", "status", "mem")
	s.run("admin", "kms", "key", "ls", "mem")
	s.run("admin", "kms", "key", "create", "mem", "app-key")
	s.run("admin", "kms", "key", "create", "mem", "app-key")
	s.run("admin", "kms", "key", "ls", "--json", "mem", "app")
	s.run("encrypt", "set", "sse-kms", "app-key", "mem/docs")
	s.run("encrypt", "info", "mem/docs")
	s.run("encrypt", "set", "sse-s3", "app-key", "mem/docs")
	s.run("encrypt", "set", "sse-kms", "app-key", "--kms-key-id", "other", "mem/docs")
	s.run("quota", "set", "mem/docs", "--size", "1MiB")
	s.run("quota", "info", "mem/docs")
	s.run("quota", "info", "--json", "mem/docs")
	h.admin.SetKMS(nil)
	s.run("admin", "kms", "status", "mem")
	h.admin.SetKMS(&nexoadmin.KMSStatus{Configured: true, Backend: nexoadmin.KMSBackendKES, DefaultKeyID: "kes-key",
		Endpoints: []nexoadmin.KMSEndpoint{{URL: "https://kes.test:7373", Online: false}}, Error: "connection refused"})
	s.run("admin", "kms", "status", "mem")
	golden(t, "scenario/kms_quota", s.b.String())
}

// covers: C-OBJ-01, C-OBJ-05, S-OBJ-10 (stat --versions)
func TestScenarioStatVersions(t *testing.T) {
	h := newHarness(t)
	s := h.session()
	s.run("mb", "--with-versioning", "mem/ver")
	h.mem.PutObject("ver", "doc.txt", "one", clitest.ObjectSpec{ContentType: "text/plain"})
	h.clock.Advance(time.Minute)
	h.mem.PutObject("ver", "doc.txt", "two!", clitest.ObjectSpec{ContentType: "text/plain", Tags: map[string]string{"v": "2"}})
	h.clock.Advance(time.Minute)
	s.run("rm", "mem/ver/doc.txt")
	s.run("stat", "--versions", "mem/ver/doc.txt")
	s.run("stat", "--versions", "--json", "mem/ver/doc.txt")
	s.run("stat", "--versions", "mem/ver/missing.txt")
	s.run("stat", "--versions", "--version-id", "v0001", "mem/ver/doc.txt")
	golden(t, "scenario/stat_versions", s.b.String())
}

func TestEncryptionFlagsReachTheClient(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("b")
	h.write("f.txt", "data")
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	tests := []struct {
		name string
		args []string
		key  string
		want domain.SSEType
	}{
		{name: "sse-s3 on the bucket", args: []string{"--enc-s3", "mem/b"}, key: "s3.txt", want: domain.SSES3},
		{name: "longest prefix wins", args: []string{"--enc-s3", "mem/b", "--enc-kms", "mem/b/kms/"}, key: "kms/f.txt", want: domain.SSEKMS},
		{name: "comma-separated", args: []string{"--enc-s3", "mem/b/x/,mem/b/y/"}, key: "y/f.txt", want: domain.SSES3},
		{name: "sse-c", args: []string{"--enc-c", "mem/b/c/=" + key}, key: "c/f.txt", want: domain.SSEC},
		{name: "outside the prefix", args: []string{"--enc-s3", "mem/b/only/"}, key: "plain.txt", want: domain.SSENone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h.mustRun(append(append([]string{"cp"}, tt.args...), "f.txt", "mem/b/"+tt.key)...)
			state, ok := h.mem.State("b", tt.key, "")
			require.True(t, ok)
			assert.Equal(t, tt.want, state.Encryption.Type)
		})
	}
	_ = cli.EncryptionRules{}
}
