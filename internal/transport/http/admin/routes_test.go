package admin_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/heal"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/service/share"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
)

func httptestBody(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }

func TestInfoRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	f.adm.EXPECT().ServerInfo(mock.Anything).Return(&domain.ServerInfo{
		Version: "v-test", Mode: domain.ModeSNSD, Region: region, Uptime: 90 * time.Second,
		Pools: []domain.PoolInfo{{Index: 0, Sets: 1, DrivesPerSet: 1}},
		Nodes: []domain.NodeInfo{{Endpoint: "local", State: domain.StateOnline, Disks: []domain.DiskInfo{{Path: "/data", State: domain.StateOnline}}}},
		TLS: &domain.TLSInfo{Enabled: true, MinVersion: "1.2", Certificates: []domain.TLSCertificate{
			{Domains: []string{"nexo.test"}, Subject: "CN=nexo.test", Issuer: "CN=ca", NotAfter: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), Fingerprint: "ab"},
		}},
		Telemetry: &domain.TelemetryInfo{Enabled: true, Endpoint: "http://otel:4318", Protocol: "http/protobuf", SampleRatio: 1},
	}, nil)
	rec := c.do(http.MethodGet, "/info", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	info := decode[admin.ServerInfoResponse](t, rec)
	assert.Equal(t, "snsd", info.Mode)
	assert.Equal(t, int64(90), info.UptimeSec)
	assert.Contains(t, rec.Body.String(), `"drivesPerSet":1`, "camelCase wire format")
	require.NotNil(t, info.TLS)
	assert.Equal(t, []admin.TLSCertificateResponse{{Domains: []string{"nexo.test"}, Subject: "CN=nexo.test", Issuer: "CN=ca",
		NotAfter: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), Fingerprint: "ab"}}, info.TLS.Certificates)
	assert.Equal(t, &admin.TelemetryInfoResponse{Enabled: true, Endpoint: "http://otel:4318", Protocol: "http/protobuf", SampleRatio: 1}, info.Telemetry)

	f.adm.EXPECT().DataUsage(mock.Anything).Return(&adminsvc.DataUsage{BucketsCount: 1, Buckets: map[string]adminsvc.BucketUsage{"a": {Size: 3}}}, nil)
	rec = c.do(http.MethodGet, "/usage", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, uint64(3), decode[adminsvc.DataUsage](t, rec).Buckets["a"].Size)

	f.adm.EXPECT().StartHeal(mock.Anything, adminsvc.HealInput{Bucket: "photos", Recursive: true}).Return(heal.Job{}, fmt.Errorf("heal: %w", domain.ErrNotImplemented)).Once()
	rec = c.do(http.MethodPost, "/heal", `{"bucket":"photos","recursive":true}`)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	assert.Equal(t, "not_implemented", problemOf(t, rec).Code)

	started := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	job := heal.Job{
		ID: "h1", State: heal.JobDone, Node: "local", StartedAt: started, UpdatedAt: started, FinishedAt: started.Add(time.Second),
		Request: heal.JobRequest{Bucket: "photos", Recursive: true, Options: heal.Options{DryRun: true, Deep: true}},
		Scanned: 3, Healed: 1,
		Drives: []heal.JobDrive{{Endpoint: "/d1", Disk: 0, State: "online", Missing: 1}},
		Items: []heal.JobItem{{Kind: heal.ItemObject, Bucket: "photos", Object: "a.jpg", Healed: 1,
			Drives: []object.HealDrive{{Endpoint: "/d1", Before: object.HealDriveMissing, After: object.HealDriveMissing}}}},
	}
	f.adm.EXPECT().StartHeal(mock.Anything, adminsvc.HealInput{Bucket: "photos", Prefix: "2026/", Recursive: true, DryRun: true, Deep: true, Remove: true}).
		Return(job, nil).Once()
	rec = c.do(http.MethodPost, "/heal", `{"bucket":"photos","prefix":"2026/","recursive":true,"dryRun":true,"deep":true,"remove":true}`)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	got := decode[admin.HealJobResponse](t, rec)
	assert.Equal(t, "done", got.State)
	assert.True(t, got.Finished)
	assert.True(t, got.Request.Deep)
	require.Len(t, got.Items, 1)
	assert.Equal(t, "missing", got.Items[0].Drives[0].Before)
	require.Len(t, got.Drives, 1)
	assert.Equal(t, uint64(1), got.Drives[0].Missing)

	f.adm.EXPECT().ListHeals(mock.Anything).Return([]heal.Job{job, {ID: "h2", State: heal.JobRunning, StartedAt: started}}, nil).Once()
	rec = c.do(http.MethodGet, "/heal", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	list := decode[admin.HealJobListResponse](t, rec)
	require.Len(t, list.Items, 2)
	assert.False(t, list.Items[1].Finished)
	assert.Nil(t, list.Items[1].FinishedAt)
	assert.NotNil(t, list.Items[1].Drives, "empty lists, not null")

	f.adm.EXPECT().HealStatus(mock.Anything, "nope").Return(heal.Job{}, fmt.Errorf("heal: %w", domain.ErrNotFound)).Once()
	rec = c.do(http.MethodGet, "/heal/nope", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	f.adm.EXPECT().ServiceAction(mock.Anything, adminsvc.ActionRestart).Return(nil)
	rec = c.do(http.MethodPost, "/service/restart", nil)
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "restart", decode[admin.ServiceActionResponse](t, rec).Action)
	f.adm.EXPECT().ServiceAction(mock.Anything, adminsvc.ActionStop).Return(fmt.Errorf("x: %w", domain.ErrNotImplemented))
	rec = c.do(http.MethodPost, "/service/stop", nil)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	rec = c.do(http.MethodPost, "/service/reboot", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestMyPermissions(t *testing.T) {
	f := newFixture(t)
	f.authz.EXPECT().Permissions(mock.Anything, mock.MatchedBy(func(in adminsvc.PermissionsInput) bool {
		return in.Requester.Identity.AccessKey == bobKey && in.Bucket == "photos" && in.Prefix == "docs/"
	})).Return(&adminsvc.Permissions{User: bobKey, Bucket: "photos", Prefix: "docs/",
		Allowed: []policy.Action{policy.ActionGetObject}, Denied: []policy.Action{policy.ActionPutObject}}, nil)
	rec := f.session(t, bobKey, bobSecret).do(http.MethodGet, "/me/permissions?bucket=photos&prefix=docs/", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	p := decode[admin.PermissionsResponse](t, rec)
	assert.Equal(t, []string{"s3:GetObject"}, p.Allowed)
	assert.Equal(t, []string{"s3:PutObject"}, p.Denied)
}

func bucketSummary(name string) adminsvc.BucketSummary {
	return adminsvc.BucketSummary{Info: domain.BucketInfo{Bucket: domain.Bucket{Name: name, Created: time.Unix(0, 0).UTC()}, Versioning: domain.VersioningEnabled, Objects: 2}, Public: true}
}

func TestBucketRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)

	// The service selects the page: the route carries cursor and limit.
	f.buckets.EXPECT().List(mock.Anything, mock.MatchedBy(func(in adminsvc.ListBucketsInput) bool {
		return in.Prefix == "" && in.Page == adminsvc.Page{Limit: 2}
	})).Return(&adminsvc.Paged[adminsvc.BucketSummary]{Items: []adminsvc.BucketSummary{bucketSummary("a"), bucketSummary("b")}, NextCursor: "next-1"}, nil)
	rec := c.do(http.MethodGet, "/buckets?limit=2", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	page := decode[admin.BucketListResponse](t, rec)
	require.Len(t, page.Items, 2)
	assert.Equal(t, "a", page.Items[0].Name)
	assert.Equal(t, "enabled", page.Items[0].Versioning)
	assert.True(t, page.Items[0].Public)
	assert.Equal(t, "next-1", page.NextCursor)
	f.buckets.EXPECT().List(mock.Anything, mock.MatchedBy(func(in adminsvc.ListBucketsInput) bool {
		return in.Page == adminsvc.Page{Cursor: "next-1", Limit: 2}
	})).Return(&adminsvc.Paged[adminsvc.BucketSummary]{Items: []adminsvc.BucketSummary{bucketSummary("c")}}, nil)
	rec = c.do(http.MethodGet, "/buckets?limit=2&cursor=next-1", nil)
	page = decode[admin.BucketListResponse](t, rec)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "c", page.Items[0].Name)
	assert.Empty(t, page.NextCursor)
	rec = c.do(http.MethodGet, "/buckets?limit=0", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	f.buckets.EXPECT().List(mock.Anything, mock.MatchedBy(func(in adminsvc.ListBucketsInput) bool { return in.Page.Cursor == "***" })).
		Return(nil, fmt.Errorf("x: %w", domain.ErrInvalidArgument))
	rec = c.do(http.MethodGet, "/buckets?cursor=***", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	b := bucketSummary("new")
	f.buckets.EXPECT().Create(mock.Anything, mock.MatchedBy(func(in adminsvc.CreateBucketInput) bool {
		return in.Name == "new" && in.Versioning && in.QuotaBytes == 1024 && in.Requester.Identity.IsRoot
	})).Return(&b, nil)
	rec = c.do(http.MethodPost, "/buckets", `{"name":"new","versioning":true,"quotaBytes":1024}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	f.buckets.EXPECT().Create(mock.Anything, mock.Anything).Return(nil, fmt.Errorf("x: %w", domain.ErrBucketAlreadyExists))
	rec = c.do(http.MethodPost, "/buckets", `{"name":"new"}`)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "bucket_already_exists", problemOf(t, rec).Code)

	f.buckets.EXPECT().Get(mock.Anything, requester(rootKey), "missing").Return(nil, fmt.Errorf("x: %w", domain.ErrBucketNotFound))
	rec = c.do(http.MethodGet, "/buckets/missing", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "bucket_not_found", problemOf(t, rec).Code)
	// A summary restricted by the service says so.
	f.buckets.EXPECT().Get(mock.Anything, requester(rootKey), "home").
		Return(&adminsvc.BucketSummary{Info: domain.BucketInfo{Bucket: domain.Bucket{Name: "home"}}, Partial: true}, nil)
	rec = c.do(http.MethodGet, "/buckets/home", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, decode[admin.BucketResponse](t, rec).Partial)

	f.buckets.EXPECT().Delete(mock.Anything, adminsvc.DeleteBucketInput{Name: "full"}).Return(fmt.Errorf("x: %w", domain.ErrBucketNotEmpty))
	rec = c.do(http.MethodDelete, "/buckets/full", nil)
	assert.Equal(t, http.StatusConflict, rec.Code)
	rec = c.do(http.MethodDelete, "/buckets/full?force=perhaps", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	f.bucketSvc.EXPECT().SetVersioning(mock.Anything, "b", domain.VersioningSuspended).Return(nil)
	rec = c.do(http.MethodPut, "/buckets/b/versioning", `{"status":"suspended"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	rec = c.do(http.MethodPut, "/buckets/b/versioning", `{"status":"maybe"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	f.bucketSvc.EXPECT().SetQuota(mock.Anything, "b", bucket.Quota{Size: 10}).Return(bucket.Quota{Type: bucket.QuotaHard, Size: 10}, nil)
	rec = c.do(http.MethodPut, "/buckets/b/quota", `{"type":"","size":10}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "hard", decode[admin.QuotaResponse](t, rec).Type)

	f.bucketSvc.EXPECT().SetTags(mock.Anything, "b", map[string]string(nil)).Return(nil)
	rec = c.do(http.MethodPut, "/buckets/b/tags", `{"tags":null}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"tags":{}}`, rec.Body.String())

	// The tags and the object lock configuration come from the bucket
	// manager (which represents the missing ones as empty values); a
	// missing bucket stays an error.
	f.buckets.EXPECT().Tags(mock.Anything, "b").Return(map[string]string{}, nil)
	rec = c.do(http.MethodGet, "/buckets/b/tags", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"tags":{}}`, rec.Body.String())
	f.buckets.EXPECT().ObjectLock(mock.Anything, "b").Return(bucket.ObjectLockConfig{Enabled: true, Mode: bucket.RetentionMode("COMPLIANCE"), Days: 3}, nil)
	rec = c.do(http.MethodGet, "/buckets/b/objectlock", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"enabled":true,"mode":"COMPLIANCE","days":3}`, rec.Body.String())
	f.buckets.EXPECT().Tags(mock.Anything, "gone").Return(nil, fmt.Errorf("x: %w", domain.ErrBucketNotFound))
	rec = c.do(http.MethodGet, "/buckets/gone/tags", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	f.bucketSvc.EXPECT().GetPolicy(mock.Anything, "b").Return(json.RawMessage(`{"Version":"2012-10-17","Statement":[]}`), nil)
	rec = c.do(http.MethodGet, "/buckets/b/policy", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"Version":"2012-10-17","Statement":[]}`, rec.Body.String())

	// A malformed policy is located by line and column.
	text := "{\n  \"Version\": \"2012-10-17\"\n  \"Statement\": []\n}"
	f.bucketSvc.EXPECT().SetPolicy(mock.Anything, "b", json.RawMessage(text)).
		Return(fmt.Errorf("bucket: %w", &policy.ValidationError{Line: 3, Column: 3, Msg: "invalid character"}))
	rec = c.do(http.MethodPut, "/buckets/b/policy", text)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	p := problemOf(t, rec)
	assert.Equal(t, "malformed_policy", p.Code)
	require.Len(t, p.Errors, 1)
	assert.Equal(t, 3, p.Errors[0].Line)
	assert.NotEmpty(t, p.RequestID+p.Instance)

	f.buckets.EXPECT().SetAccess(mock.Anything, adminsvc.SetAccessInput{Bucket: "b", Prefix: "pub/", Preset: policy.PresetDownload}).
		Return(&adminsvc.BucketAccess{Bucket: "b", Rules: []policy.PublicAccessRule{{Prefix: "pub/", Preset: policy.PresetDownload}}, Public: true, HasPolicy: true}, nil)
	rec = c.do(http.MethodPut, "/buckets/b/access", `{"prefix":"pub/","preset":"download"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"bucket":"b","rules":[{"prefix":"pub/","preset":"download"}],"public":true,"hasPolicy":true}`, rec.Body.String())
	f.buckets.EXPECT().SetAccess(mock.Anything, adminsvc.SetAccessInput{Bucket: "b", Prefix: "pub/", Preset: policy.PresetPrivate}).
		Return(&adminsvc.BucketAccess{Bucket: "b", Rules: []policy.PublicAccessRule{}}, nil)
	rec = c.do(http.MethodDelete, "/buckets/b/access?prefix=pub/", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	// Bucket replication is P2.
	rec = c.do(http.MethodGet, "/buckets/b/replication", nil)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// TestListDecodesOnly: the listing handler only decodes: an absent
// delimiter stays absent (the service applies the folder view and
// authorizes with the effective conditions), the requester is passed on.
func TestListDecodesOnly(t *testing.T) {
	tests := []struct {
		name, query string
		delimiter   *string
	}{
		{name: "absent delimiter", query: "prefix=docs/&limit=10"},
		{name: "empty delimiter", query: "prefix=docs/&limit=10&delimiter=", delimiter: new(string)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			c := f.session(t, bobKey, bobSecret)
			f.browser.EXPECT().List(mock.Anything, mock.MatchedBy(func(in adminsvc.ListObjectsInput) bool {
				return in.Requester.Identity.AccessKey == bobKey && in.Bucket == "photos" && in.Prefix == "docs/" &&
					assert.ObjectsAreEqual(tt.delimiter, in.Delimiter) && in.Limit == 10
			})).Return(&adminsvc.ObjectListing{}, nil)
			f.resetChecks()
			rec := c.do(http.MethodGet, "/buckets/photos/objects?"+tt.query, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			f.mu.Lock()
			defer f.mu.Unlock()
			assert.Empty(t, f.checks, "the service authorizes the listing")
		})
	}
}

func TestObjectRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	mod := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	f.browser.EXPECT().List(mock.Anything, mock.MatchedBy(func(in adminsvc.ListObjectsInput) bool {
		return in.Requester.Identity.AccessKey == rootKey && in.Bucket == "b" && in.Prefix == "a/" && in.Delimiter != nil && *in.Delimiter == "" &&
			in.Cursor == "tok" && in.Limit == 5
	})).Return(&adminsvc.ObjectListing{Objects: []domain.ObjectInfo{{Key: "a/x", Size: 1, ModTime: mod}}, Prefixes: []string{}, NextCursor: "next"}, nil)
	rec := c.do(http.MethodGet, "/buckets/b/objects?prefix=a/&delimiter=&cursor=tok&limit=5", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	list := decode[admin.ObjectListResponse](t, rec)
	assert.True(t, list.IsTruncated)
	assert.Equal(t, "next", list.NextCursor)
	assert.NotNil(t, list.Prefixes)

	f.browser.EXPECT().Delete(mock.Anything, mock.MatchedBy(func(in adminsvc.DeleteObjectsInput) bool {
		return in.Bucket == "b" && len(in.Objects) == 2 && in.Objects[1].VersionID == "v1" && in.AllVersions
	})).Return(&adminsvc.DeleteObjectsResult{
		Deleted: []adminsvc.DeletedObject{{Key: "k1"}},
		Errors:  []adminsvc.ObjectError{{Key: "k2", VersionID: "v1", Err: fmt.Errorf("x: %w", domain.ErrObjectLocked)}},
	}, nil)
	rec = c.do(http.MethodDelete, "/buckets/b/objects", `{"objects":[{"key":"k1"},{"key":"k2","versionId":"v1"}],"allVersions":true}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	del := decode[admin.DeleteObjectsResponse](t, rec)
	require.Len(t, del.Errors, 1)
	assert.Equal(t, "object_locked", del.Errors[0].Code)

	f.browser.EXPECT().Rename(mock.Anything, mock.MatchedBy(func(in adminsvc.RenameInput) bool { return in.Source == "a/" && in.Destination == "b/" && in.Prefix })).
		Return(&adminsvc.RenameResult{Renamed: []adminsvc.RenamedObject{{Source: "a/x", Destination: "b/x"}},
			Errors: []adminsvc.RenameError{{Source: "a/y", Destination: "b/y", Err: fmt.Errorf("x: %w", domain.ErrConflict)}}}, nil)
	rec = c.do(http.MethodPost, "/buckets/b/objects/rename", `{"source":"a/","destination":"b/","prefix":true}`)
	require.Equal(t, http.StatusOK, rec.Code)
	ren := decode[admin.RenameResponse](t, rec)
	assert.Equal(t, "conflict", ren.Errors[0].Code)

	f.browser.EXPECT().Stat(mock.Anything, mock.Anything).Return(&adminsvc.ObjectDetails{
		Info: domain.ObjectInfo{Key: "k", Size: 3, ModTime: mod, UserMetadata: map[string]string{"a": "b"}},
		Tags: map[string]string{}, Versions: nil,
	}, nil).Once()
	rec = c.do(http.MethodGet, "/buckets/b/objects/stat?key=k", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"tags":{}`, "readable empty tags are present")
	assert.NotContains(t, rec.Body.String(), `"versions"`, "versions the caller may not list are absent")
	assert.NotContains(t, rec.Body.String(), `"encryption"`, "an unencrypted version has no encryption")

	f.browser.EXPECT().Stat(mock.Anything, mock.MatchedBy(func(in adminsvc.StatInput) bool { return in.Key == "sealed" })).Return(&adminsvc.ObjectDetails{
		Info: domain.ObjectInfo{Key: "sealed", ModTime: mod, Encryption: domain.EncryptionInfo{Type: domain.SSEKMS, KMSKeyID: "app-key"}},
	}, nil)
	rec = c.do(http.MethodGet, "/buckets/b/objects/stat?key=sealed", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, &admin.ObjectEncryptionResponse{Type: "SSE-KMS", KMSKeyID: "app-key"}, decode[admin.ObjectDetailsResponse](t, rec).Encryption)

	f.browser.EXPECT().SetTags(mock.Anything, adminsvc.SetTagsInput{Bucket: "b", Key: "k", VersionID: "v2", Tags: map[string]string{"x": "y"}}).Return(map[string]string{"x": "y"}, nil)
	rec = c.do(http.MethodPut, "/buckets/b/objects/tags?key=k&versionId=v2", `{"tags":{"x":"y"}}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, policy.ActionPutObjectVersionTagging, f.lastCheck().Actions[0])

	f.browser.EXPECT().CreateFolder(mock.Anything, mock.MatchedBy(func(in adminsvc.FolderInput) bool { return in.Prefix == "docs" })).
		Return(domain.ObjectInfo{Key: "docs/", ModTime: mod}, nil)
	rec = c.do(http.MethodPost, "/buckets/b/objects/folder", `{"prefix":"docs"}`)
	require.Equal(t, http.StatusCreated, rec.Code)

	f.browser.EXPECT().Versions(mock.Anything, adminsvc.VersionsInput{Bucket: "b", Key: "k"}).Return(&adminsvc.VersionList{Versions: []domain.ObjectInfo{{Key: "k", VersionID: "v1"}}}, nil)
	rec = c.do(http.MethodGet, "/buckets/b/objects/versions?key=k", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"k"}, f.lastCheck().Conditions["s3:prefix"])
}

// download is an opened object for the mocked browser.
func download(body, contentType, disposition string, sandbox bool) *adminsvc.Download {
	return &adminsvc.Download{
		Reader:      &object.Reader{ReadCloser: io.NopCloser(strings.NewReader(body)), Info: domain.ObjectInfo{Key: "k", ETag: "abc", Size: int64(len(body))}, Length: int64(len(body))},
		ContentType: contentType, Disposition: disposition, FileName: "k.txt", Sandbox: sandbox,
	}
}

func TestDownloadAndPreview(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	f.browser.EXPECT().Open(mock.Anything, mock.MatchedBy(func(in adminsvc.OpenInput) bool { return !in.Preview && in.Range != nil && in.Range.Start == 2 })).
		RunAndReturn(func(context.Context, adminsvc.OpenInput) (*adminsvc.Download, error) {
			d := download("cd", "text/plain", adminsvc.DispositionAttachment, false)
			d.Partial, d.Reader.Offset, d.Reader.Info.Size = true, 2, 4
			return d, nil
		})
	req := c.newRequest(http.MethodGet, "/buckets/b/objects/download?key=k", nil)
	req.Header.Set("Range", "bytes=2-3")
	rec := c.serve(req)
	require.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Equal(t, "bytes 2-3/4", rec.Header().Get("Content-Range"))
	assert.Equal(t, `attachment; filename=k.txt`, rec.Header().Get("Content-Disposition"))
	assert.Equal(t, "cd", rec.Body.String())

	f.browser.EXPECT().Open(mock.Anything, mock.MatchedBy(func(in adminsvc.OpenInput) bool { return in.Preview })).
		Return(download("hello", "text/plain; charset=utf-8", adminsvc.DispositionInline, true), nil)
	rec = c.do(http.MethodGet, "/buckets/b/objects/preview?key=k", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, strings.HasPrefix(rec.Header().Get("Content-Disposition"), "inline"))
	assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "sandbox")
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))

	f.browser.EXPECT().Open(mock.Anything, mock.Anything).Return(nil, fmt.Errorf("x: %w", domain.ErrNotModified)).Once()
	rec = c.do(http.MethodGet, "/buckets/b/objects/download?key=k", nil)
	assert.Equal(t, http.StatusNotModified, rec.Code)
	assert.Empty(t, rec.Body.String())

	req = c.newRequest(http.MethodGet, "/buckets/b/objects/download?key=k", nil)
	req.Header.Set("Range", "lines=1-2")
	f.browser.ExpectedCalls = nil
	rec = c.serve(req)
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, rec.Code)
}

func TestZipRoute(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	// Validation errors come before the response starts: a problem.
	f.browser.EXPECT().Zip(mock.Anything, mock.MatchedBy(func(in adminsvc.ZipInput) bool { return len(in.Keys) == 1 }), mock.Anything).
		Return(nil, fmt.Errorf("x: %w", domain.ErrObjectNotFound)).Once()
	rec := c.do(http.MethodPost, "/buckets/b/objects/download-zip", `{"keys":["missing"]}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "object_not_found", problemOf(t, rec).Code)

	f.browser.EXPECT().Zip(mock.Anything, mock.MatchedBy(func(in adminsvc.ZipInput) bool { return in.Base == "logs/" }), mock.Anything).
		RunAndReturn(func(_ context.Context, _ adminsvc.ZipInput, open adminsvc.ZipOpener) (*adminsvc.ZipResult, error) {
			w, err := open("logs.zip")
			if err != nil {
				return nil, err
			}
			zw := zip.NewWriter(w)
			e, _ := zw.Create("a.txt")
			_, _ = io.WriteString(e, "A")
			return &adminsvc.ZipResult{Files: 1}, zw.Close()
		})
	rec = c.do(http.MethodPost, "/buckets/b/objects/download-zip", `{"base":"logs/","prefixes":["logs/"]}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/zip", rec.Header().Get("Content-Type"))
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	require.NoError(t, err)
	assert.Equal(t, "a.txt", zr.File[0].Name)
}

// TestZipForm: the console submits the zip request as a form, with the
// CSRF token in a field, so that the browser streams the archive to disk
// (regression: the console buffered the whole archive in memory). The
// token stays mandatory and is checked like the header.
func TestZipForm(t *testing.T) {
	form := func(csrf, request string) url.Values {
		v := url.Values{admin.ZipFormField: {request}}
		if csrf != "" {
			v.Set(session.CSRFFormField, csrf)
		}
		return v
	}
	post := func(c *client, values url.Values, contentType string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, admin.Prefix+"/buckets/b/objects/download-zip", strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", contentType)
		req.AddCookie(c.cookie)
		return c.serve(req)
	}
	t.Run("streams the archive", func(t *testing.T) {
		f := newFixture(t)
		c := f.root(t)
		f.browser.EXPECT().Zip(mock.Anything, mock.MatchedBy(func(in adminsvc.ZipInput) bool {
			return in.Bucket == "b" && in.Base == "logs/" && len(in.Keys) == 1 && in.Keys[0] == "logs/a.txt" && in.Requester.Identity.AccessKey == rootKey
		}), mock.Anything).RunAndReturn(func(_ context.Context, _ adminsvc.ZipInput, open adminsvc.ZipOpener) (*adminsvc.ZipResult, error) {
			w, err := open("logs.zip")
			if err != nil {
				return nil, err
			}
			_, err = io.WriteString(w, "PK")
			return &adminsvc.ZipResult{Files: 1}, err
		})
		values := form(c.csrf, `{"base":"logs/","keys":["logs/a.txt"]}`)
		values.Set(admin.DownloadTokenField, "abcdef0123456789")
		rec := post(c, values, "application/x-www-form-urlencoded")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "application/zip", rec.Header().Get("Content-Type"))
		assert.Contains(t, rec.Header().Get("Content-Disposition"), "attachment")
		started := namedCookie(t, rec, admin.DownloadCookiePrefix+"abcdef0123456789")
		assert.Equal(t, "1", started.Value, "the console learns that the archive started")
		assert.False(t, started.HttpOnly, "the console scripts read it")
		assert.Equal(t, http.SameSiteStrictMode, started.SameSite)
	})
	t.Run("an invalid download token sets no cookie", func(t *testing.T) {
		f := newFixture(t)
		c := f.root(t)
		f.browser.EXPECT().Zip(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, _ adminsvc.ZipInput, open adminsvc.ZipOpener) (*adminsvc.ZipResult, error) {
				_, err := open("a.zip")
				return &adminsvc.ZipResult{}, err
			}).Times(3)
		for _, token := range []string{"short", "has;semicolon=and-more-chars", strings.Repeat("a", 65)} {
			values := form(c.csrf, `{"keys":["k"]}`)
			values.Set(admin.DownloadTokenField, token)
			rec := post(c, values, "application/x-www-form-urlencoded")
			require.Equal(t, http.StatusOK, rec.Code)
			assert.Empty(t, rec.Result().Cookies(), "token %q", token)
		}
	})
	tests := []struct {
		name, csrf, request, contentType string
		status                           int
		code                             string
	}{
		{name: "no token", request: `{"keys":["k"]}`, contentType: "application/x-www-form-urlencoded", status: http.StatusForbidden, code: "invalid_csrf_token"},
		{name: "wrong token", csrf: "forged", request: `{"keys":["k"]}`, contentType: "application/x-www-form-urlencoded", status: http.StatusForbidden, code: "invalid_csrf_token"},
		{name: "text/plain form", csrf: "valid", request: `{"keys":["k"]}`, contentType: "text/plain", status: http.StatusForbidden, code: "invalid_csrf_token"},
		{name: "invalid request field", csrf: "valid", request: `{"keys":`, contentType: "application/x-www-form-urlencoded", status: http.StatusBadRequest, code: "invalid_argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			c := f.root(t)
			csrf := tt.csrf
			if csrf == "valid" {
				csrf = c.csrf
			}
			// The browser mock fails the test if the archive is requested.
			rec := post(c, form(csrf, tt.request), tt.contentType)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			assert.Equal(t, tt.code, problemOf(t, rec).Code)
		})
	}
	t.Run("the field is required", func(t *testing.T) {
		f := newFixture(t)
		c := f.root(t)
		rec := post(c, url.Values{session.CSRFFormField: {c.csrf}}, "application/x-www-form-urlencoded")
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})
}

func TestUploadRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	mod := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	// The body is streamed to the upload manager, which applies the limits.
	f.uploads.EXPECT().Upload(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool {
		return in.Bucket == "b" && in.Key == "dir/a.txt" && in.ContentType == "text/plain" && in.Size == 5 && in.UserMetadata["owner"] == "me"
	})).RunAndReturn(func(_ context.Context, in object.PutObjectInput) (domain.ObjectInfo, error) {
		data, err := io.ReadAll(in.Reader)
		if err != nil {
			return domain.ObjectInfo{}, err
		}
		return domain.ObjectInfo{Key: in.Key, Size: int64(len(data)), ContentType: in.ContentType, ModTime: mod}, nil
	}).Once()
	req := c.newRequest(http.MethodPost, "/buckets/b/objects/upload?key=dir/a.txt", strings.NewReader("hello"))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Amz-Meta-Owner", "me")
	rec := c.serve(req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, int64(5), decode[admin.ObjectResponse](t, rec).Size)

	// The tags of x-amz-tagging are forwarded and authorized as the S3
	// PutObject would (s3:RequestObjectTag/<key>).
	f.uploads.EXPECT().Upload(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool {
		return in.Key == "tagged.txt" && assert.ObjectsAreEqual(map[string]string{"class": "secret"}, in.Tags)
	})).Return(domain.ObjectInfo{Key: "tagged.txt", ModTime: mod}, nil).Once()
	f.resetChecks()
	req = c.newRequest(http.MethodPost, "/buckets/b/objects/upload?key=tagged.txt", strings.NewReader("hi"))
	req.Header.Set("X-Amz-Tagging", "class=secret")
	rec = c.serve(req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, map[string][]string{
		"s3:x-amz-tagging": {"class=secret"}, "s3:RequestObjectTag/class": {"secret"}, "s3:RequestObjectTagKeys": {"class"},
	}, f.lastCheck().Conditions)
	req = c.newRequest(http.MethodPost, "/buckets/b/objects/upload?key=bad.txt", strings.NewReader("hi"))
	req.Header.Set("X-Amz-Tagging", "a=1&a=2")
	rec = c.serve(req)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a malformed tag set is refused before storing anything")

	// The server-side encryption of the S3 headers is forwarded and
	// authorized as the S3 PutObject would (s3:x-amz-server-side-encryption*).
	f.uploads.EXPECT().Upload(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool {
		return in.Key == "sealed.txt" && in.SSE.Type == domain.SSEKMS && in.SSE.KMSKeyID == "app-key"
	})).Return(domain.ObjectInfo{Key: "sealed.txt", ModTime: mod}, nil).Once()
	f.resetChecks()
	req = c.newRequest(http.MethodPost, "/buckets/b/objects/upload?key=sealed.txt", strings.NewReader("hi"))
	req.Header.Set("X-Amz-Server-Side-Encryption", "aws:kms")
	req.Header.Set("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id", "app-key")
	rec = c.serve(req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, map[string][]string{
		"s3:x-amz-server-side-encryption": {"aws:kms"}, "s3:x-amz-server-side-encryption-aws-kms-key-id": {"app-key"},
	}, f.lastCheck().Conditions)
	for name, headers := range map[string]map[string]string{
		"unknown algorithm": {"X-Amz-Server-Side-Encryption": "AES128"},
		"key without kms":   {"X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id": "app-key"},
		"key with sse-s3":   {"X-Amz-Server-Side-Encryption": "AES256", "X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id": "app-key"},
	} {
		req = c.newRequest(http.MethodPost, "/buckets/b/objects/upload?key=bad.txt", strings.NewReader("hi"))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec = c.serve(req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, name)
	}

	// The limits are the service's; the transport maps the error.
	f.uploads.EXPECT().Upload(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool { return in.Key == "big" && in.Size == -1 })).
		Return(domain.ObjectInfo{}, fmt.Errorf("x: %w", domain.ErrEntityTooLarge)).Once()
	req = c.newRequest(http.MethodPost, "/buckets/b/objects/upload?key=big", strings.NewReader("123456789"))
	req.ContentLength = -1
	rec = c.serve(req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Equal(t, "entity_too_large", problemOf(t, rec).Code)

	// The default content type is the service's: the handler passes none.
	f.uploads.EXPECT().Create(mock.Anything, object.NewMultipartInput{Bucket: "b", Key: "big.bin"}).
		Return(&adminsvc.NewUpload{
			Upload: domain.MultipartUpload{Bucket: "b", Key: "big.bin", UploadID: "u1", Initiated: mod},
			Limits: adminsvc.UploadLimits{MaxUploadSize: 8, PartSize: 6 << 20, MinPartSize: 5 << 20, MaxPartSize: 6 << 20, MaxParts: 10000},
		}, nil).Once()
	rec = c.do(http.MethodPost, "/buckets/b/uploads?key=big.bin", nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, admin.UploadResponse{Bucket: "b", Key: "big.bin", UploadID: "u1", Initiated: mod, PartSize: 6 << 20, MinPartSize: 5 << 20, MaxPartSize: 6 << 20, MaxParts: 10000},
		decode[admin.UploadResponse](t, rec))

	// A multipart upload asks for its encryption when it starts.
	f.uploads.EXPECT().Create(mock.Anything, object.NewMultipartInput{Bucket: "b", Key: "sealed.bin", SSE: domain.SSERequest{Type: domain.SSES3}}).
		Return(&adminsvc.NewUpload{Upload: domain.MultipartUpload{Bucket: "b", Key: "sealed.bin", UploadID: "u2", Initiated: mod}}, nil).Once()
	req = c.newRequest(http.MethodPost, "/buckets/b/uploads?key=sealed.bin", nil)
	req.Header.Set("X-Amz-Server-Side-Encryption", "AES256")
	rec = c.serve(req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	f.uploads.EXPECT().UploadPart(mock.Anything, mock.MatchedBy(func(in object.PutObjectPartInput) bool {
		return in.UploadID == "u1" && in.PartNumber == 2 && in.Key == "big.bin" && in.Size == 4
	})).Return(domain.Part{Number: 2, ETag: "e2", Size: 4}, nil).Once()
	rec = c.do(http.MethodPut, "/buckets/b/uploads/u1/parts/2?key=big.bin", strings.NewReader("data"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "e2", decode[admin.PartResponse](t, rec).ETag)
	rec = c.do(http.MethodPut, "/buckets/b/uploads/u1/parts/two?key=big.bin", strings.NewReader("data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "the part number is an integer")
	f.uploads.EXPECT().UploadPart(mock.Anything, mock.MatchedBy(func(in object.PutObjectPartInput) bool { return in.PartNumber == 0 })).
		Return(domain.Part{}, fmt.Errorf("x: %w", domain.ErrInvalidArgument)).Once()
	rec = c.do(http.MethodPut, "/buckets/b/uploads/u1/parts/0?key=big.bin", strings.NewReader("data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "the service validates the range")

	f.uploads.EXPECT().Complete(mock.Anything, object.CompleteMultipartInput{Bucket: "b", Key: "big.bin", UploadID: "u1",
		Parts: []domain.CompletePart{{Number: 1, ETag: "e1"}, {Number: 2, ETag: "e2"}}}).Return(domain.ObjectInfo{Key: "big.bin", Size: 9, ModTime: mod}, nil).Once()
	rec = c.do(http.MethodPost, "/buckets/b/uploads/u1/complete?key=big.bin", `{"parts":[{"partNumber":1,"etag":"e1"},{"partNumber":2,"etag":"e2"}]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	f.uploads.EXPECT().Abort(mock.Anything, adminsvc.UploadRef{Bucket: "b", Key: "big.bin", UploadID: "u9"}).Return(fmt.Errorf("x: %w", domain.ErrNoSuchUpload)).Once()
	rec = c.do(http.MethodDelete, "/buckets/b/uploads/u9?key=big.bin", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "upload_not_found", problemOf(t, rec).Code)

	// The cursor and the limit are carried to the service, which pages.
	f.uploads.EXPECT().List(mock.Anything, adminsvc.ListUploadsInput{Bucket: "b", Prefix: "x/", Page: adminsvc.Page{Limit: 1}}).
		Return(&adminsvc.Paged[domain.MultipartUpload]{Items: []domain.MultipartUpload{{Key: "x/1", UploadID: "u1", Initiated: mod}}, NextCursor: "c1"}, nil).Once()
	rec = c.do(http.MethodGet, "/buckets/b/uploads?prefix=x/&limit=1", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	ul := decode[admin.UploadListResponse](t, rec)
	assert.Equal(t, "c1", ul.NextCursor)
	require.Len(t, ul.Items, 1)
	f.uploads.EXPECT().List(mock.Anything, adminsvc.ListUploadsInput{Bucket: "b", Page: adminsvc.Page{Cursor: "c1"}}).
		Return(&adminsvc.Paged[domain.MultipartUpload]{}, nil).Once()
	rec = c.do(http.MethodGet, "/buckets/b/uploads?cursor=c1", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"items":[]}`, rec.Body.String())
	rec = c.do(http.MethodGet, "/buckets/b/uploads?limit=0", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	f.uploads.EXPECT().Parts(mock.Anything, adminsvc.ListPartsInput{Upload: adminsvc.UploadRef{Bucket: "b", Key: "big.bin", UploadID: "u1"}, Page: adminsvc.Page{Cursor: "p2", Limit: 2}}).
		Return(&adminsvc.Paged[domain.Part]{Items: []domain.Part{{Number: 3, ETag: "e3", Size: 1}}}, nil).Once()
	rec = c.do(http.MethodGet, "/buckets/b/uploads/u1/parts?key=big.bin&cursor=p2&limit=2", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"items":[{"partNumber":3,"etag":"e3","size":1}]}`, rec.Body.String())
}

func TestShareRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.session(t, bobKey, bobSecret)
	exp := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	sh := &share.Share{ID: "abc", Owner: bobKey, Bucket: "b", Key: "k", Permission: share.PermissionDownload, Expires: exp, CreatedAt: exp, URL: "http://s3/b/k?sig", AccessKey: "share-abc"}

	f.shares.EXPECT().Create(mock.Anything, mock.MatchedBy(func(in adminsvc.CreateShareInput) bool {
		// The default permission is the service's: the handler passes none.
		return in.Bucket == "b" && in.Key == "k" && in.Permission == "" && in.Expires == time.Hour && in.Requester.Identity.AccessKey == bobKey
	})).Return(sh, nil).Twice()
	rec := c.do(http.MethodPost, "/shares", `{"bucket":"b","key":"k","expiresIn":3600}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, "http://s3/b/k?sig", decode[admin.ShareResponse](t, rec).URL)
	rec = c.do(http.MethodPost, "/buckets/b/objects/share", `{"key":"k","expiresIn":3600}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = c.do(http.MethodPost, "/buckets/b/objects/share", `{"bucket":"other","key":"k"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	f.shares.EXPECT().List(mock.Anything, adminsvc.ListSharesInput{Requester: f.lastRequester(t, c), Owner: "alice", Bucket: "b"}).
		Return(nil, fmt.Errorf("x: %w", domain.ErrAccessDenied))
	rec = c.do(http.MethodGet, "/shares?owner=alice&bucket=b", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	f.shares.EXPECT().RevokeAll(mock.Anything, requester(bobKey), "").Return(3, nil)
	rec = c.do(http.MethodDelete, "/shares", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"revoked":3}`, rec.Body.String())
	f.shares.EXPECT().Revoke(mock.Anything, requester(bobKey), "zzz").Return(fmt.Errorf("x: %w", domain.ErrShareNotFound))
	rec = c.do(http.MethodDelete, "/shares/zzz", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "share_not_found", problemOf(t, rec).Code)

	f.shares.EXPECT().Presign(mock.Anything, mock.MatchedBy(func(in adminsvc.PresignInput) bool { return in.Method == "PUT" && in.Expires == time.Minute })).
		Return(&adminsvc.SignedURL{URL: "http://s3/b/k?X-Amz-Signature=x", Method: "PUT", Expires: exp}, nil)
	rec = c.do(http.MethodPost, "/presign", `{"method":"PUT","bucket":"b","key":"k","expiresIn":60}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"url":"http://s3/b/k?X-Amz-Signature=x","method":"PUT","expiresAt":"2026-05-01T00:00:00Z","headers":{}}`, rec.Body.String())

	f.shares.EXPECT().PostPolicy(mock.Anything, mock.MatchedBy(func(in adminsvc.PostPolicyInput) bool { return in.KeyPrefix == "up/" && in.MaxSize == 10 })).
		Return(&adminsvc.PostPolicyForm{URL: "http://s3/b", Fields: map[string]string{"key": "up/${filename}"}, Expiration: exp,
			Conditions: []adminsvc.PostCondition{{Operator: "content-length-range", Max: 10}}}, nil)
	rec = c.do(http.MethodPost, "/post-policy", `{"bucket":"b","keyPrefix":"up/","maxSize":10}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"operator":"content-length-range"`)
}

// TestShareExpiresInRange: the validity in seconds is checked before its
// conversion to a duration, which overflows for huge values and could
// land in the valid range with another validity (regression).
func TestShareExpiresInRange(t *testing.T) {
	tests := []struct {
		name, path, body string
	}{
		{"share overflow", "/shares", `{"bucket":"b","key":"k","expiresIn":9223372037}`},
		{"share wrapping into range", "/shares", `{"bucket":"b","key":"k","expiresIn":18446744074}`},
		{"share beyond 7 days", "/shares", `{"bucket":"b","key":"k","expiresIn":604801}`},
		{"share negative", "/shares", `{"bucket":"b","key":"k","expiresIn":-1}`},
		{"presign overflow", "/presign", `{"method":"GET","bucket":"b","key":"k","expiresIn":9223372037}`},
		{"post policy overflow", "/post-policy", `{"bucket":"b","keyPrefix":"up/","expiresIn":9223372037}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			// The share manager mock fails the test if it is called.
			rec := f.session(t, bobKey, bobSecret).do(http.MethodPost, tt.path, tt.body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, "invalid_argument", problemOf(t, rec).Code)
		})
	}
	t.Run("the bounds are accepted", func(t *testing.T) {
		f := newFixture(t)
		f.shares.EXPECT().Presign(mock.Anything, mock.MatchedBy(func(in adminsvc.PresignInput) bool { return in.Expires == 7*24*time.Hour })).
			Return(&adminsvc.SignedURL{URL: "http://s3/b/k", Method: "GET"}, nil)
		rec := f.session(t, bobKey, bobSecret).do(http.MethodPost, "/presign", `{"method":"GET","bucket":"b","key":"k","expiresIn":604800}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}

// lastRequester returns the requester the fixture sees for a client (the
// conditions included), to match exact inputs.
func (f *fixture) lastRequester(t *testing.T, c *client) adminsvc.Requester {
	t.Helper()
	f.adm.EXPECT().ServerInfo(mock.Anything).Return(&domain.ServerInfo{}, nil).Once()
	f.resetChecks()
	rec := c.do(http.MethodGet, "/info", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	return f.lastCheck().Requester
}

func TestUserGroupAccessKeyRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	f.identities.EXPECT().ListUsers(mock.Anything, adminsvc.Page{}).Return(&adminsvc.Paged[iam.User]{
		Items: []iam.User{{AccessKey: "a", Status: domain.StatusDisabled}, {AccessKey: "b", Status: domain.StatusEnabled}},
	}, nil)
	rec := c.do(http.MethodGet, "/users", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	users := decode[admin.UserListResponse](t, rec)
	require.Len(t, users.Items, 2)
	assert.Equal(t, "a", users.Items[0].AccessKey)
	assert.NotNil(t, users.Items[0].Groups)

	rec = c.do(http.MethodPost, "/users", `{"accessKey":"alice","secretKey":"alice-secret","status":"sleeping"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	f.identities.EXPECT().CreateUser(mock.Anything, mock.MatchedBy(func(in adminsvc.CreateUserInput) bool {
		return in.AccessKey == "alice" && in.Groups[0] == "staff" && in.Status == ""
	})).Return(&iam.User{AccessKey: "alice", Status: domain.StatusEnabled, Groups: []string{"staff"}, CreatedAt: now, UpdatedAt: now}, nil)
	rec = c.do(http.MethodPost, "/users", `{"accessKey":"alice","secretKey":"alice-secret","groups":["staff"]}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	f.identities.EXPECT().UpdateUser(mock.Anything, mock.MatchedBy(func(in adminsvc.UpdateUserInput) bool {
		return in.AccessKey == "alice" && in.Status != nil && *in.Status == domain.StatusDisabled && in.Groups != nil && len(*in.Groups) == 0
	})).Return(&iam.User{AccessKey: "alice", Status: domain.StatusDisabled}, nil)
	rec = c.do(http.MethodPut, "/users/alice", `{"status":"disabled","groups":[]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	f.iam.EXPECT().GetUser(mock.Anything, "ghost").Return(nil, fmt.Errorf("x: %w", domain.ErrUserNotFound))
	rec = c.do(http.MethodGet, "/users/ghost", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "user_not_found", problemOf(t, rec).Code)
	// The deletion goes through the identity manager, which also removes
	// the share links of the user.
	f.identities.EXPECT().DeleteUser(mock.Anything, requester(rootKey), "alice").Return(nil)
	rec = c.do(http.MethodDelete, "/users/alice", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	f.identities.EXPECT().CreateGroup(mock.Anything, mock.MatchedBy(func(in adminsvc.CreateGroupInput) bool { return in.Name == "g" })).
		Return(nil, fmt.Errorf("x: %w", domain.ErrGroupAlreadyExists))
	rec = c.do(http.MethodPost, "/groups", `{"name":"g"}`)
	assert.Equal(t, http.StatusConflict, rec.Code)
	f.identities.EXPECT().UpdateGroup(mock.Anything, mock.MatchedBy(func(in adminsvc.UpdateGroupInput) bool {
		return in.Name == "g" && len(in.AddMembers) == 1 && in.Status == nil
	})).Return(&iam.Group{Name: "g", Status: domain.StatusEnabled, Members: []string{"x"}}, nil)
	rec = c.do(http.MethodPut, "/groups/g", `{"addMembers":["x"]}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"x"}, decode[admin.GroupResponse](t, rec).Members)
	f.identities.EXPECT().ListGroups(mock.Anything, adminsvc.Page{}).Return(&adminsvc.Paged[iam.Group]{}, nil)
	rec = c.do(http.MethodGet, "/groups", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"items":[]}`, rec.Body.String())

	f.accessKeys.EXPECT().Create(mock.Anything, requester(rootKey), mock.MatchedBy(func(in iam.CreateAccessKeyInput) bool {
		return in.ParentUser == "alice" && in.Name == "ci" && !in.Expiration.IsZero()
	})).Return(&iam.AccessKey{AccessKey: "AK1", SecretKey: "SK1", ParentUser: "alice", Status: domain.StatusEnabled, CreatedAt: now}, nil)
	rec = c.do(http.MethodPost, "/access-keys", `{"parentUser":"alice","name":"ci","expiration":"2030-01-01T00:00:00Z"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, "SK1", decode[admin.AccessKeyResponse](t, rec).SecretKey, "the secret is returned once")

	f.accessKeys.EXPECT().Get(mock.Anything, requester(rootKey), "AK1").Return(&iam.AccessKey{AccessKey: "AK1", SecretKey: "leak?", ParentUser: "alice"}, nil)
	rec = c.do(http.MethodGet, "/access-keys/AK1", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "leak?", "never the secret again")

	f.accessKeys.EXPECT().List(mock.Anything, requester(rootKey), "alice", adminsvc.Page{Limit: 1}).
		Return(&adminsvc.Paged[iam.AccessKey]{Items: []iam.AccessKey{{AccessKey: "AK1", SecretKey: "leak?"}}, NextCursor: "c1"}, nil)
	rec = c.do(http.MethodGet, "/access-keys?user=alice&limit=1", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	keys := decode[admin.AccessKeyListResponse](t, rec)
	require.Len(t, keys.Items, 1)
	assert.Equal(t, "AK1", keys.Items[0].AccessKey)
	assert.Equal(t, "c1", keys.NextCursor)
	assert.NotContains(t, rec.Body.String(), "leak?", "a listing never carries secrets")

	f.accessKeys.EXPECT().Update(mock.Anything, requester(rootKey), "AK1", mock.MatchedBy(func(in iam.UpdateAccessKeyInput) bool {
		return in.Expiration != nil && in.Expiration.IsZero() && in.Status != nil && *in.Status == domain.StatusDisabled
	})).Return(&iam.AccessKey{AccessKey: "AK1", Status: domain.StatusDisabled}, nil)
	rec = c.do(http.MethodPut, "/access-keys/AK1", `{"clearExpiration":true,"status":"disabled"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	f.accessKeys.EXPECT().Delete(mock.Anything, requester(rootKey), "AK1").Return(nil)
	rec = c.do(http.MethodDelete, "/access-keys/AK1", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestPolicyRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	doc := &domain.Policy{Version: domain.PolicyVersion, Statement: domain.Statements{{Effect: domain.Allow, Action: domain.StringSet{"s3:*"}}}}

	f.policies.EXPECT().List(mock.Anything, adminsvc.Page{}).
		Return(&adminsvc.Paged[policy.Named]{Items: []policy.Named{{Name: "custom", Policy: doc}, {Name: "readonly", Policy: doc, BuiltIn: true}}}, nil)
	rec := c.do(http.MethodGet, "/policies", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, decode[admin.PolicyListResponse](t, rec).Items, 2)

	// A document given as text is passed on verbatim (positions).
	text := "{\"Version\":\"2012-10-17\",\n\"Statement\":[]}"
	body, err := json.Marshal(map[string]string{"name": "p1", "document": text})
	require.NoError(t, err)
	f.policies.EXPECT().Save(mock.Anything, adminsvc.SavePolicyInput{Name: "p1", Document: json.RawMessage(text), Create: true}).
		Return(nil, fmt.Errorf("x: %w", domain.ErrConflict))
	rec = c.do(http.MethodPost, "/policies", string(body))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "conflict", problemOf(t, rec).Code)
	rec = c.do(http.MethodPost, "/policies", `{"name":"p1"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "document required")

	f.policies.EXPECT().Save(mock.Anything, mock.MatchedBy(func(in adminsvc.SavePolicyInput) bool { return in.Name == "p2" && !in.Create })).
		Return(&policy.Named{Name: "p2", Policy: doc}, nil)
	rec = c.do(http.MethodPut, "/policies/p2", `{"document":{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*"}]}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	f.policies.EXPECT().Validate(mock.Anything, adminsvc.ValidatePolicyInput{Document: json.RawMessage(`{"x":1}`), Bucket: "b"}).
		Return(&adminsvc.ValidationReport{Problems: []adminsvc.PolicyProblem{{Line: 1, Column: 2, Message: "bad"}}}, nil)
	rec = c.do(http.MethodPost, "/policies/validate", `{"document":"{\"x\":1}","bucket":"b"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"valid":false,"errors":[{"statement":0,"line":1,"column":2,"message":"bad"}]}`, rec.Body.String())

	stmt := &doc.Statement[0]
	// The handler only decodes: the service composes the condition keys.
	f.policies.EXPECT().Simulate(mock.Anything, mock.MatchedBy(func(in adminsvc.SimulateInput) bool {
		return in.User == "alice" && in.Action == policy.ActionGetObject && in.SourceIP == "10.0.0.1" &&
			in.SecureTransport != nil && *in.SecureTransport && in.Now.Equal(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)) &&
			in.Conditions["aws:Referer"][0] == "https://app.example.com/"
	})).Return(&adminsvc.Simulation{User: "alice", Decision: policy.Decision{
		Allowed: true, Reason: "allowed", Source: policy.SourceIAM, PolicyName: "readonly", MatchedStatement: stmt, Index: 0,
		Evaluated: []policy.EvaluatedStatement{{Source: policy.SourceIAM, PolicyName: "readonly", Effect: domain.Allow, Matched: true, Statement: stmt},
			{Source: policy.SourceBucket, Effect: domain.Deny, Mismatch: policy.MismatchCondition}},
	}}, nil)
	rec = c.do(http.MethodPost, "/policies/simulate", `{"user":"alice","action":"s3:GetObject","bucket":"b","object":"k","sourceIp":"10.0.0.1","secureTransport":true,"time":"2026-01-01T10:00:00Z","conditions":{"aws:Referer":["https://app.example.com/"]}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	sim := decode[admin.SimulationResponse](t, rec)
	assert.Equal(t, "allow", sim.Decision)
	require.NotNil(t, sim.Statement)
	assert.Equal(t, "Allow", sim.Statement.Effect)
	require.Len(t, sim.Evaluated, 2)
	assert.Equal(t, "condition", sim.Evaluated[1].Mismatch)

	f.policy.EXPECT().Attach(mock.Anything, policy.AttachInput{PolicyNames: []string{"readonly"}, Group: "g"}).Return(nil)
	rec = c.do(http.MethodPost, "/policies/attach", `{"policyNames":["readonly"],"group":"g"}`)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	f.policy.EXPECT().Detach(mock.Anything, policy.AttachInput{PolicyNames: []string{"readonly"}}).Return(fmt.Errorf("x: %w", domain.ErrInvalidArgument))
	rec = c.do(http.MethodPost, "/policies/detach", `{"policyNames":["readonly"]}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	f.policy.EXPECT().Entities(mock.Anything, "readonly").Return(policy.PolicyEntities{Policy: "readonly"}, nil)
	rec = c.do(http.MethodGet, "/policies/readonly/entities", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"policy":"readonly","users":[],"groups":[]}`, rec.Body.String())
	f.policies.EXPECT().Get(mock.Anything, "nope").Return(nil, fmt.Errorf("x: %w", domain.ErrPolicyNotFound))
	rec = c.do(http.MethodGet, "/policies/nope", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "policy_not_found", problemOf(t, rec).Code)
	f.policy.EXPECT().DeletePolicy(mock.Anything, "readonly").Return(fmt.Errorf("x: %w", domain.ErrInvalidArgument))
	rec = c.do(http.MethodDelete, "/policies/readonly", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "built-ins are read-only")
}

func TestConfigRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	f.config.EXPECT().List(mock.Anything).Return([]adminsvc.Subsystem{{Name: "api", Settings: config.KV{}}}, nil)
	rec := c.do(http.MethodGet, "/config", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[{"name":"api","settings":{}}]`, rec.Body.String())

	f.config.EXPECT().Set(mock.Anything, "notify_webhook", config.KV{"auth_token": "x"}).Return(config.KV{"auth_token": adminsvc.SensitiveMask}, nil)
	rec = c.do(http.MethodPut, "/config/notify_webhook", `{"settings":{"auth_token":"x"}}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), adminsvc.SensitiveMask)

	f.config.EXPECT().Delete(mock.Anything, "api", []string{"a", "b"}).Return(nil)
	rec = c.do(http.MethodDelete, "/config/api?keys=a,%20b", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	f.config.EXPECT().SetAll(mock.Anything, map[string]config.KV{"region": {"name": "x"}}).Return(fmt.Errorf("x: %w", domain.ErrInvalidArgument))
	rec = c.do(http.MethodPut, "/config", `{"region":{"name":"x"}}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	f.config.EXPECT().Schema(mock.Anything).Return([]adminsvc.SubsystemSchema{{Name: "logger", Keys: []adminsvc.KeySchema{{Name: "level", Type: adminsvc.KeyEnum, Values: []string{"info"}}}}})
	rec = c.do(http.MethodGet, "/config/schema", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"type":"enum"`)

	f.config.EXPECT().Export(mock.Anything).Return([]byte(`{"api":{}}`), nil)
	rec = c.do(http.MethodGet, "/config/export", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "nexo-config.json")
	f.config.EXPECT().Import(mock.Anything, []byte(`{"api":{}}`)).Return(nil)
	rec = c.do(http.MethodPost, "/config/import", `{"api":{}}`)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	f.config.EXPECT().Get(mock.Anything, "nope").Return(nil, errors.New("boom"))
	rec = c.do(http.MethodGet, "/config/nope", nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "boom", "unknown errors do not leak")
}

func TestShareListAndLinks(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.shares.EXPECT().List(mock.Anything, mock.MatchedBy(func(in adminsvc.ListSharesInput) bool { return in.Page == adminsvc.Page{Cursor: "c0", Limit: 2} })).
		Return(&adminsvc.Paged[share.Share]{Items: []share.Share{{ID: "new", CreatedAt: base.Add(time.Hour)}, {ID: "mid", CreatedAt: base}}, NextCursor: "c1"}, nil)
	rec := c.do(http.MethodGet, "/shares?limit=2&cursor=c0", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	page := decode[admin.ShareListResponse](t, rec)
	assert.Equal(t, []string{"new", "mid"}, []string{page.Items[0].ID, page.Items[1].ID})
	assert.Equal(t, "c1", page.NextCursor)

	// The per-object links of a prefix share (D-SHR-01).
	sh := &share.Share{ID: "s1", Bucket: "b", Key: "docs/", Prefix: true, Permission: share.PermissionDownload, Expires: base.Add(time.Hour)}
	f.shares.EXPECT().Links(mock.Anything, mock.MatchedBy(func(in adminsvc.ShareLinksInput) bool {
		return in.ID == "s1" && in.Page == adminsvc.Page{Cursor: "k1", Limit: 10} && in.Requester.Identity.IsRoot
	})).Return(&adminsvc.ShareLinks{Share: sh, Items: []adminsvc.ShareLink{{Key: "docs/a.pdf", Size: 3, URL: "http://s3/b/docs/a.pdf?sig"}}, NextCursor: "k2"}, nil)
	rec = c.do(http.MethodGet, "/shares/s1/links?limit=10&cursor=k1", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	links := decode[admin.ShareLinksResponse](t, rec)
	assert.Equal(t, "s1", links.Share.ID)
	assert.Equal(t, []admin.ShareLinkResponse{{Key: "docs/a.pdf", Size: 3, URL: "http://s3/b/docs/a.pdf?sig"}}, links.Items)
	assert.Equal(t, "k2", links.NextCursor)
	f.shares.EXPECT().Links(mock.Anything, mock.MatchedBy(func(in adminsvc.ShareLinksInput) bool { return in.ID == "up" })).
		Return(nil, fmt.Errorf("x: %w", domain.ErrInvalidArgument))
	rec = c.do(http.MethodGet, "/shares/up/links", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDownloadAttachmentIsSandboxed(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	d := download("<script>x</script>", "text/html", adminsvc.DispositionAttachment, false)
	d.FileName = "page\x01.html"
	f.browser.EXPECT().Open(mock.Anything, mock.Anything).Return(d, nil)
	rec := c.do(http.MethodGet, "/buckets/b/objects/download?key=k", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment"), rec.Header().Get("Content-Disposition"))
	assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "sandbox")
}

func TestRestoreVersionRoute(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	f.browser.EXPECT().RestoreVersion(mock.Anything, mock.MatchedBy(func(in adminsvc.RestoreVersionInput) bool {
		return in.Bucket == "b" && in.Key == "dir/k" && in.VersionID == "v1" && in.Requester.Identity.IsRoot
	})).Return(domain.ObjectInfo{Key: "dir/k", VersionID: "v3", IsLatest: true, Size: 4}, nil)
	rec := c.do(http.MethodPost, "/buckets/b/objects/restore?key=dir/k&versionId=v1", nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, "v3", decode[admin.ObjectResponse](t, rec).VersionID)
	f.browser.EXPECT().RestoreVersion(mock.Anything, mock.MatchedBy(func(in adminsvc.RestoreVersionInput) bool { return in.VersionID == "dm" })).
		Return(domain.ObjectInfo{}, fmt.Errorf("x: %w", domain.ErrMethodNotAllowed))
	rec = c.do(http.MethodPost, "/buckets/b/objects/restore?key=k&versionId=dm", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// TestEscapedPathParameters: the path parameters are decoded once, whether
// chi routed on the escaped path (a character Go leaves as is was
// escaped: "@", "+", "/"...) or on the decoded one (regression: the
// console escapes every segment, and users, policies or groups with such
// characters could be listed but not managed).
func TestEscapedPathParameters(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	f.iam.EXPECT().GetUser(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, key string) (*iam.User, error) {
		return &iam.User{AccessKey: key}, nil
	}).Maybe()
	f.iam.EXPECT().GetGroup(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, name string) (*iam.Group, error) {
		return &iam.Group{Name: name}, nil
	}).Maybe()
	f.policies.EXPECT().Get(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, name string) (*policy.Named, error) {
		return &policy.Named{Name: name, Policy: &domain.Policy{Version: domain.PolicyVersion}}, nil
	}).Maybe()
	f.accessKeys.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, _ adminsvc.Requester, key string) (*iam.AccessKey, error) {
		return &iam.AccessKey{AccessKey: key}, nil
	}).Maybe()

	tests := []struct {
		name string
		path string
		// field is the JSON member echoing the decoded parameter.
		field string
		want  string
	}{
		{name: "user with @", path: "/users/alice%40example.com", field: "accessKey", want: "alice@example.com"},
		{name: "user with @ unescaped", path: "/users/alice@example.com", field: "accessKey", want: "alice@example.com"},
		{name: "user with +", path: "/users/a%2Bb", field: "accessKey", want: "a+b"},
		{name: "policy with +", path: "/policies/read%2Bwrite", field: "name", want: "read+write"},
		{name: "policy with , and =", path: "/policies/a%2Cb%3Dc", field: "name", want: "a,b=c"},
		{name: "group with /", path: "/groups/eng%2Fbackend", field: "name", want: "eng/backend"},
		{name: "group with a space", path: "/groups/dev%20team", field: "name", want: "dev team"},
		{name: "group with unicode", path: "/groups/citt%C3%A0", field: "name", want: "città"},
		// Without RawPath chi routed on the decoded path: no second decoding.
		{name: "literal percent", path: "/groups/a%2541", field: "name", want: "a%41"},
		{name: "literal percent with /", path: "/groups/a%2541%2Fb", field: "name", want: "a%41/b"},
		{name: "access key with +", path: "/access-keys/AK%2B1", field: "accessKey", want: "AK+1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := c.do(http.MethodGet, tt.path, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			got := decode[map[string]any](t, rec)
			assert.Equal(t, tt.want, got[tt.field])
		})
	}

	t.Run("mutations", func(t *testing.T) {
		f.identities.EXPECT().UpdateUser(mock.Anything, mock.MatchedBy(func(in adminsvc.UpdateUserInput) bool { return in.AccessKey == "alice@example.com" })).
			Return(&iam.User{AccessKey: "alice@example.com", Status: domain.StatusDisabled}, nil).Once()
		rec := c.do(http.MethodPut, "/users/alice%40example.com", `{"status":"disabled"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		f.identities.EXPECT().DeleteUser(mock.Anything, requester(rootKey), "alice@example.com").Return(nil).Once()
		rec = c.do(http.MethodDelete, "/users/alice%40example.com", nil)
		assert.Equal(t, http.StatusNoContent, rec.Code)

		f.policies.EXPECT().Save(mock.Anything, mock.MatchedBy(func(in adminsvc.SavePolicyInput) bool { return in.Name == "read+write" && !in.Create })).
			Return(&policy.Named{Name: "read+write", Policy: &domain.Policy{Version: domain.PolicyVersion}}, nil).Once()
		rec = c.do(http.MethodPut, "/policies/read%2Bwrite", `{"document":{"Version":"2012-10-17","Statement":[]}}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		f.policy.EXPECT().Entities(mock.Anything, "read+write").Return(policy.PolicyEntities{Policy: "read+write"}, nil).Once()
		rec = c.do(http.MethodGet, "/policies/read%2Bwrite/entities", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		f.policy.EXPECT().DeletePolicy(mock.Anything, "read+write").Return(nil).Once()
		rec = c.do(http.MethodDelete, "/policies/read%2Bwrite", nil)
		assert.Equal(t, http.StatusNoContent, rec.Code)

		f.identities.EXPECT().UpdateGroup(mock.Anything, mock.MatchedBy(func(in adminsvc.UpdateGroupInput) bool { return in.Name == "eng/backend" })).
			Return(&iam.Group{Name: "eng/backend"}, nil).Once()
		rec = c.do(http.MethodPut, "/groups/eng%2Fbackend", `{"addMembers":["x"]}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		f.iam.EXPECT().DeleteGroup(mock.Anything, "eng/backend").Return(nil).Once()
		rec = c.do(http.MethodDelete, "/groups/eng%2Fbackend", nil)
		assert.Equal(t, http.StatusNoContent, rec.Code)

		f.accessKeys.EXPECT().Delete(mock.Anything, requester(rootKey), "AK+1").Return(nil).Once()
		rec = c.do(http.MethodDelete, "/access-keys/AK%2B1", nil)
		assert.Equal(t, http.StatusNoContent, rec.Code)

		f.shares.EXPECT().Revoke(mock.Anything, requester(rootKey), "s@1").Return(nil).Once()
		rec = c.do(http.MethodDelete, "/shares/s%401", nil)
		assert.Equal(t, http.StatusNoContent, rec.Code)

		f.config.EXPECT().Delete(mock.Anything, "a:b", []string(nil)).Return(nil).Once()
		rec = c.do(http.MethodDelete, "/config/a%3Ab", nil)
		assert.Equal(t, http.StatusNoContent, rec.Code)

		f.adm.EXPECT().HealStatus(mock.Anything, "h+1").Return(heal.Job{ID: "h+1"}, nil).Once()
		rec = c.do(http.MethodGet, "/heal/h%2B1", nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		f.uploads.EXPECT().Abort(mock.Anything, adminsvc.UploadRef{Bucket: "b", Key: "k", UploadID: "u+1"}).Return(nil).Once()
		rec = c.do(http.MethodDelete, "/buckets/b/uploads/u%2B1?key=k", nil)
		assert.Equal(t, http.StatusNoContent, rec.Code)
		f.uploads.EXPECT().UploadPart(mock.Anything, mock.MatchedBy(func(in object.PutObjectPartInput) bool { return in.UploadID == "u+1" && in.PartNumber == 3 })).
			Return(domain.Part{Number: 3, ETag: "e3"}, nil).Once()
		rec = c.do(http.MethodPut, "/buckets/b/uploads/u%2B1/parts/%33?key=k", strings.NewReader("data"))
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("a malformed escape is an invalid argument", func(t *testing.T) {
		// net/http refuses such a request line; a router that hands chi a
		// raw path anyway must not reach the services.
		req := c.newRequest(http.MethodGet, "/users/x", nil)
		req.URL.RawPath = admin.Prefix + "/users/a%zz%40b"
		rec := c.serve(req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "invalid_argument", problemOf(t, rec).Code)
	})
}
