package admin_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	adminmock "github.com/r3n3r0/nexo/internal/service/admin/mock"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/iam"
	iammock "github.com/r3n3r0/nexo/internal/service/iam/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
)

// clusterFixture is a cluster manager over generated mocks.
type clusterFixture struct {
	m       admin.ClusterManager
	iam     *iammock.Service
	policy  *policymock.Service
	config  *adminmock.ConfigManager
	buckets *bucketmock.Service
}

func newClusterFixture(t *testing.T) *clusterFixture {
	t.Helper()
	f := &clusterFixture{
		iam: iammock.NewService(t), policy: policymock.NewService(t),
		config: adminmock.NewConfigManager(t), buckets: bucketmock.NewService(t),
	}
	m, err := admin.NewClusterManager(admin.ClusterManagerDeps{
		IAM: f.iam, Policy: f.policy, Config: f.config, Buckets: f.buckets,
		Clock: clock.NewFake(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)),
	}, admin.WithServerVersion("v1.2.3"))
	require.NoError(t, err)
	f.m = m
	return f
}

var testPolicy = []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::photos/*"]}]}`)

// exportFixture records the state of a source deployment on the mocks.
func exportFixture(t *testing.T, f *clusterFixture) {
	t.Helper()
	var doc domain.Policy
	require.NoError(t, json.Unmarshal(testPolicy, &doc))
	f.iam.EXPECT().ListUsers(mock.Anything).Return([]iam.User{{AccessKey: "alice"}, {AccessKey: "bob"}}, nil)
	f.iam.EXPECT().ListGroups(mock.Anything).Return([]iam.Group{{Name: "editors"}}, nil)
	f.iam.EXPECT().Export(mock.Anything).Return([]byte("encrypted-iam"), nil)
	f.policy.EXPECT().ListPolicies(mock.Anything).Return([]policy.Named{
		{Name: "readonly", BuiltIn: true, Policy: &domain.Policy{}},
		{Name: "photos-read", Policy: &doc},
	}, nil)
	f.config.EXPECT().Export(mock.Anything).Return([]byte(`{"region":{"name":"eu-south-1"}}`), nil)
	f.buckets.EXPECT().List(mock.Anything, bucket.ListInput{}).Return([]domain.BucketInfo{{Bucket: domain.Bucket{Name: "photos"}}, {Bucket: domain.Bucket{Name: "plain"}}}, nil)
	noConfig := domain.NewS3Error(domain.ErrPolicyNotFound, "NoSuchConfig", "none")
	// photos: versioned, locked, with every setting.
	f.buckets.EXPECT().GetVersioning(mock.Anything, "photos").Return(domain.VersioningEnabled, nil)
	f.buckets.EXPECT().GetObjectLock(mock.Anything, "photos").Return(bucket.ObjectLockConfig{Enabled: true, Mode: bucket.RetentionGovernance, Days: 3}, nil)
	f.buckets.EXPECT().GetPolicy(mock.Anything, "photos").Return(json.RawMessage(`{"Version":"2012-10-17"}`), nil)
	f.buckets.EXPECT().GetLifecycle(mock.Anything, "photos").Return([]byte("<LifecycleConfiguration/>"), nil)
	f.buckets.EXPECT().GetEncryption(mock.Anything, "photos").Return(bucket.EncryptionConfig{Algorithm: "AES256"}, nil)
	f.buckets.EXPECT().GetQuota(mock.Anything, "photos").Return(bucket.Quota{Type: bucket.QuotaHard, Size: 1 << 30}, nil)
	f.buckets.EXPECT().GetTags(mock.Anything, "photos").Return(map[string]string{"team": "media"}, nil)
	f.buckets.EXPECT().GetCORS(mock.Anything, "photos").Return([]byte("<CORSConfiguration/>"), nil)
	f.buckets.EXPECT().GetNotification(mock.Anything, "photos").Return([]byte("<NotificationConfiguration/>"), nil)
	f.buckets.EXPECT().GetMedia(mock.Anything, "photos").Return(testMediaConfig(), nil)
	// plain: nothing set.
	f.buckets.EXPECT().GetVersioning(mock.Anything, "plain").Return(domain.VersioningOff, nil)
	f.buckets.EXPECT().GetObjectLock(mock.Anything, "plain").Return(bucket.ObjectLockConfig{}, noConfig)
	f.buckets.EXPECT().GetPolicy(mock.Anything, "plain").Return(nil, noConfig)
	f.buckets.EXPECT().GetLifecycle(mock.Anything, "plain").Return(nil, noConfig)
	f.buckets.EXPECT().GetEncryption(mock.Anything, "plain").Return(bucket.EncryptionConfig{}, noConfig)
	f.buckets.EXPECT().GetQuota(mock.Anything, "plain").Return(bucket.Quota{}, nil)
	f.buckets.EXPECT().GetTags(mock.Anything, "plain").Return(nil, noConfig)
	f.buckets.EXPECT().GetCORS(mock.Anything, "plain").Return(nil, noConfig)
	f.buckets.EXPECT().GetNotification(mock.Anything, "plain").Return(nil, noConfig)
	f.buckets.EXPECT().GetMedia(mock.Anything, "plain").Return(domain.MediaConfig{}, noConfig)
}

func testMediaConfig() domain.MediaConfig {
	return domain.MediaConfig{Rules: []domain.MediaRule{{ID: "videos", Suffixes: []string{".mp4"}, Renditions: []string{"720p"}, Poster: true}}}
}

func zipEntries(t *testing.T, archive []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(rc)
		require.NoError(t, err)
		_ = rc.Close()
		out[f.Name] = data
	}
	return out
}

func TestClusterExport(t *testing.T) {
	f := newClusterFixture(t)
	exportFixture(t, f)
	archive, err := f.m.Export(context.Background())
	require.NoError(t, err)
	entries := zipEntries(t, archive)
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	assert.Equal(t, []string{"buckets/photos.json", "buckets/plain.json", "config.json", "iam.bin", "manifest.json", "policies.json"}, names)
	assert.Equal(t, "encrypted-iam", string(entries["iam.bin"]))
	var man map[string]any
	require.NoError(t, json.Unmarshal(entries["manifest.json"], &man))
	assert.Equal(t, admin.ClusterArchiveFormat, man["format"])
	assert.EqualValues(t, admin.ClusterArchiveVersion, man["version"])
	assert.Equal(t, "v1.2.3", man["serverVersion"])
	assert.Equal(t, []any{"alice", "bob"}, man["users"])
	assert.Equal(t, []any{"photos-read"}, man["policies"], "built-in policies are not exported")
	assert.JSONEq(t, `{"name":"plain"}`, string(entries["buckets/plain.json"]))
	var photos map[string]any
	require.NoError(t, json.Unmarshal(entries["buckets/photos.json"], &photos))
	assert.Equal(t, "Enabled", photos["versioning"])
	assert.Equal(t, "<LifecycleConfiguration/>", photos["lifecycle"])
	assert.Equal(t, map[string]any{"rules": []any{map[string]any{"id": "videos", "suffixes": []any{".mp4"}, "renditions": []any{"720p"}, "poster": true}}},
		photos["media"])
}

// archiveOf exports the fixture state with fresh mocks.
func archiveOf(t *testing.T) []byte {
	t.Helper()
	f := newClusterFixture(t)
	exportFixture(t, f)
	archive, err := f.m.Export(context.Background())
	require.NoError(t, err)
	return archive
}

func TestClusterImportDryRun(t *testing.T) {
	archive := archiveOf(t)
	f := newClusterFixture(t)
	f.policy.EXPECT().Parse(mock.Anything, mock.Anything).Return(&domain.Policy{}, nil)
	f.buckets.EXPECT().List(mock.Anything, bucket.ListInput{}).Return([]domain.BucketInfo{{Bucket: domain.Bucket{Name: "plain"}}}, nil)
	// Replace mode: what this deployment has and the archive does not.
	f.iam.EXPECT().ListUsers(mock.Anything).Return([]iam.User{{AccessKey: "alice"}, {AccessKey: "mallory"}}, nil)
	f.iam.EXPECT().ListGroups(mock.Anything).Return(nil, nil)
	f.policy.EXPECT().ListPolicies(mock.Anything).Return([]policy.Named{{Name: "readonly", BuiltIn: true}, {Name: "stale"}}, nil)
	sum, err := f.m.Import(context.Background(), archive, admin.ImportOptions{Mode: admin.ImportReplace, DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, admin.ImportSummary{
		Mode: admin.ImportReplace, DryRun: true, Users: 2, Groups: 1, Policies: 1, ConfigSubsystems: 1, Buckets: 2,
		BucketsCreated: []string{"photos"}, Removed: []string{"user:mallory", "policy:stale"},
	}, sum)
	// Nothing was written: the mocks fail on any unexpected call.
}

func TestClusterImportMerge(t *testing.T) {
	archive := archiveOf(t)
	f := newClusterFixture(t)
	ctx := context.Background()
	var calls []string
	record := func(s string) { calls = append(calls, s) }
	f.policy.EXPECT().Parse(mock.Anything, mock.Anything).Return(&domain.Policy{Version: "2012-10-17"}, nil)
	f.buckets.EXPECT().List(mock.Anything, bucket.ListInput{}).Return([]domain.BucketInfo{{Bucket: domain.Bucket{Name: "plain"}}}, nil)
	f.config.EXPECT().SetAll(mock.Anything, map[string]config.KV{"region": {"name": "eu-south-1"}}).
		Run(func(context.Context, map[string]config.KV) { record("config") }).Return(nil)
	f.policy.EXPECT().SetPolicy(mock.Anything, "photos-read", mock.Anything).
		Run(func(context.Context, string, *domain.Policy) { record("policy") }).Return(nil)
	f.iam.EXPECT().Import(mock.Anything, []byte("encrypted-iam")).Run(func(context.Context, []byte) { record("iam") }).Return(nil)
	f.buckets.EXPECT().Create(mock.Anything, bucket.CreateInput{Name: "photos", ObjectLock: true}).
		Run(func(context.Context, bucket.CreateInput) { record("create photos") }).Return(&domain.Bucket{Name: "photos"}, nil)
	f.buckets.EXPECT().SetVersioning(mock.Anything, "photos", domain.VersioningEnabled).Run(func(context.Context, string, domain.Versioning) { record("versioning") }).Return(nil)
	f.buckets.EXPECT().SetObjectLock(mock.Anything, "photos", bucket.ObjectLockConfig{Enabled: true, Mode: bucket.RetentionGovernance, Days: 3}).Return(nil)
	f.buckets.EXPECT().SetPolicy(mock.Anything, "photos", json.RawMessage(`{"Version":"2012-10-17"}`)).Return(nil)
	f.buckets.EXPECT().SetLifecycle(mock.Anything, "photos", []byte("<LifecycleConfiguration/>")).Return(nil)
	f.buckets.EXPECT().SetEncryption(mock.Anything, "photos", bucket.EncryptionConfig{Algorithm: "AES256"}).Return(nil)
	f.buckets.EXPECT().SetQuota(mock.Anything, "photos", bucket.Quota{Type: bucket.QuotaHard, Size: 1 << 30}).Return(bucket.Quota{}, nil)
	f.buckets.EXPECT().SetTags(mock.Anything, "photos", map[string]string{"team": "media"}).Return(nil)
	f.buckets.EXPECT().SetCORS(mock.Anything, "photos", []byte("<CORSConfiguration/>")).Return(nil)
	f.buckets.EXPECT().SetNotification(mock.Anything, "photos", []byte("<NotificationConfiguration/>")).Return(nil)
	f.buckets.EXPECT().SetMedia(mock.Anything, "photos", testMediaConfig()).Return(nil)
	// plain: nothing set, nothing removed in merge mode.
	sum, err := f.m.Import(ctx, archive, admin.ImportOptions{})
	require.NoError(t, err)
	assert.Equal(t, admin.ImportMerge, sum.Mode)
	assert.Equal(t, []string{"photos"}, sum.BucketsCreated)
	assert.Empty(t, sum.Removed)
	assert.Equal(t, []string{"config", "policy", "iam", "create photos", "versioning"}, calls,
		"the policies exist before the IAM state attaches them; versioning before the object lock")
}

func TestClusterImportReplace(t *testing.T) {
	archive := archiveOf(t)
	f := newClusterFixture(t)
	ctx := context.Background()
	f.policy.EXPECT().Parse(mock.Anything, mock.Anything).Return(&domain.Policy{}, nil)
	f.buckets.EXPECT().List(mock.Anything, bucket.ListInput{}).Return([]domain.BucketInfo{{Bucket: domain.Bucket{Name: "photos"}}, {Bucket: domain.Bucket{Name: "plain"}}}, nil)
	f.iam.EXPECT().ListUsers(mock.Anything).Return([]iam.User{{AccessKey: "alice"}, {AccessKey: "mallory"}}, nil)
	f.iam.EXPECT().ListGroups(mock.Anything).Return([]iam.Group{{Name: "editors"}, {Name: "old"}}, nil)
	f.policy.EXPECT().ListPolicies(mock.Anything).Return([]policy.Named{{Name: "photos-read"}, {Name: "stale"}}, nil)
	f.config.EXPECT().Import(mock.Anything, []byte(`{"region":{"name":"eu-south-1"}}`)).Return(nil)
	f.policy.EXPECT().SetPolicy(mock.Anything, "photos-read", mock.Anything).Return(nil)
	f.iam.EXPECT().Import(mock.Anything, mock.Anything).Return(nil)
	f.iam.EXPECT().DeleteUser(mock.Anything, "mallory").Return(nil)
	f.iam.EXPECT().DeleteGroup(mock.Anything, "old").Return(nil)
	f.policy.EXPECT().DeletePolicy(mock.Anything, "stale").Return(nil)
	f.buckets.EXPECT().SetVersioning(mock.Anything, mock.Anything, mock.Anything).Return(nil)
	f.buckets.EXPECT().SetObjectLock(mock.Anything, mock.Anything, mock.Anything).Return(nil)
	f.buckets.EXPECT().SetPolicy(mock.Anything, mock.Anything, mock.Anything).Return(nil)
	f.buckets.EXPECT().SetLifecycle(mock.Anything, mock.Anything, mock.Anything).Return(nil)
	f.buckets.EXPECT().SetEncryption(mock.Anything, mock.Anything, mock.Anything).Return(nil)
	f.buckets.EXPECT().SetQuota(mock.Anything, mock.Anything, mock.Anything).Return(bucket.Quota{}, nil)
	f.buckets.EXPECT().SetTags(mock.Anything, mock.Anything, mock.Anything).Return(nil)
	f.buckets.EXPECT().SetCORS(mock.Anything, mock.Anything, mock.Anything).Return(nil)
	f.buckets.EXPECT().SetNotification(mock.Anything, mock.Anything, mock.Anything).Return(nil)
	f.buckets.EXPECT().SetMedia(mock.Anything, "photos", testMediaConfig()).Return(nil)
	// The settings "plain" does not have in the archive are removed.
	noConfig := domain.NewS3Error(domain.ErrPolicyNotFound, "NoSuchConfig", "none")
	f.buckets.EXPECT().DeletePolicy(mock.Anything, "plain").Return(noConfig)
	f.buckets.EXPECT().DeleteLifecycle(mock.Anything, "plain").Return(nil)
	f.buckets.EXPECT().DeleteEncryption(mock.Anything, "plain").Return(nil)
	f.buckets.EXPECT().DeleteTags(mock.Anything, "plain").Return(nil)
	f.buckets.EXPECT().DeleteCORS(mock.Anything, "plain").Return(nil)
	f.buckets.EXPECT().DeleteMedia(mock.Anything, "plain").Return(nil)
	sum, err := f.m.Import(ctx, archive, admin.ImportOptions{Mode: admin.ImportReplace})
	require.NoError(t, err)
	assert.Empty(t, sum.BucketsCreated)
	assert.Equal(t, []string{"user:mallory", "group:old", "policy:stale"}, sum.Removed)
}

func TestClusterImportRefusesBadArchives(t *testing.T) {
	ctx := context.Background()
	archive := archiveOf(t)
	rewrite := func(edit func(name string, data []byte) ([]byte, bool)) []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for name, data := range zipEntries(t, archive) {
			out, keep := edit(name, data)
			if !keep {
				continue
			}
			w, err := zw.Create(name)
			require.NoError(t, err)
			_, _ = w.Write(out)
		}
		require.NoError(t, zw.Close())
		return buf.Bytes()
	}
	tests := map[string][]byte{
		"not a zip": []byte("plain text"),
		"newer version": rewrite(func(name string, data []byte) ([]byte, bool) {
			if name == "manifest.json" {
				return bytes.Replace(data, []byte(`"version": 1`), []byte(`"version": 99`), 1), true
			}
			return data, true
		}),
		"no manifest": rewrite(func(name string, data []byte) ([]byte, bool) { return data, name != "manifest.json" }),
		"no IAM":      rewrite(func(name string, data []byte) ([]byte, bool) { return data, name != "iam.bin" }),
		"bad config": rewrite(func(name string, data []byte) ([]byte, bool) {
			if name == "config.json" {
				return []byte(`{"nope":{"x":"y"}}`), true
			}
			return data, true
		}),
		"misnamed bucket": rewrite(func(name string, data []byte) ([]byte, bool) {
			if name == "buckets/plain.json" {
				return []byte(`{"name":"other"}`), true
			}
			return data, true
		}),
		"unexpected entry":    withEntries(t, archive, map[string][]byte{"notes.txt": []byte("x")}),
		"nested bucket entry": withEntries(t, archive, map[string][]byte{"buckets/a/b.json": []byte("{}")}),
		// Many small deflated entries whose sum exceeds the bound once
		// uncompressed, although each is far below it (zip bomb).
		"documents too large uncompressed": withEntries(t, archive, func() map[string][]byte {
			extra := map[string][]byte{}
			for i := range 70 {
				extra[fmt.Sprintf("buckets/bomb-%02d.json", i)] = bytes.Repeat([]byte(" "), 1<<20)
			}
			return extra
		}()),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			f := newClusterFixture(t)
			f.policy.EXPECT().Parse(mock.Anything, mock.Anything).Return(&domain.Policy{}, nil).Maybe()
			_, err := f.m.Import(ctx, data, admin.ImportOptions{})
			assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		})
	}
	f := newClusterFixture(t)
	_, err := f.m.Import(ctx, archive, admin.ImportOptions{Mode: "wipe"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)

	_, err = admin.NewClusterManager(admin.ClusterManagerDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// withEntries returns archive with extra entries added (deflated).
func withEntries(t *testing.T, archive []byte, extra map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name string, data []byte) {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		require.NoError(t, err)
		_, err = w.Write(data)
		require.NoError(t, err)
	}
	for name, data := range zipEntries(t, archive) {
		write(name, data)
	}
	for name, data := range extra {
		write(name, data)
	}
	require.NoError(t, zw.Close())
	require.Less(t, buf.Len(), admin.MaxClusterArchive, "the archive itself is small")
	return buf.Bytes()
}
