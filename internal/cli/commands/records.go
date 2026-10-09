package commands

import (
	"time"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The JSON records of the commands (--json): one per line, each preceded
// by "status". Their members are part of the CLI contract (TCL-E-18): new
// members may be added, existing ones are never renamed or removed.

// Record types (the "type" member of listings).
const (
	typeBucket = "bucket"
	typeFile   = "file"
	typeFolder = "folder"
	typeUpload = "upload"
	typeAlias  = "alias"
)

// entryRecord is a bucket, an object, an object version, a folder, a local
// file or an incomplete upload (ls, stat, find).
type entryRecord struct {
	Type         string            `json:"type"`
	Target       string            `json:"target"`
	Key          string            `json:"key,omitempty"`
	Name         string            `json:"name"`
	Size         int64             `json:"size"`
	LastModified *time.Time        `json:"lastModified,omitempty"`
	ETag         string            `json:"etag,omitempty"`
	StorageClass string            `json:"storageClass,omitempty"`
	ContentType  string            `json:"contentType,omitempty"`
	VersionID    string            `json:"versionId,omitempty"`
	IsLatest     *bool             `json:"isLatest,omitempty"`
	DeleteMarker bool              `json:"deleteMarker,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Tags         map[string]string `json:"tags,omitempty"`
	UploadID     string            `json:"uploadId,omitempty"`
	// Object Lock state, checksum and encryption of an object version
	// (stat).
	Retention  *retentionInfo  `json:"retention,omitempty"`
	LegalHold  string          `json:"legalHold,omitempty"`
	Checksum   *checksumInfo   `json:"checksum,omitempty"`
	Encryption *encryptionInfo `json:"encryption,omitempty"`
	// Versions is the number of versions and delete markers of the object
	// (stat of an object of a versioned bucket).
	Versions int `json:"versions,omitempty"`
	// Bucket settings (stat of a bucket).
	Versioning string          `json:"versioning,omitempty"`
	Anonymous  string          `json:"anonymous,omitempty"`
	ObjectLock *objectLockInfo `json:"objectLock,omitempty"`
}

// transferRecord is a copy, a move, an upload or a mirror action.
type transferRecord struct {
	Action    string `json:"action"`
	Source    string `json:"source"`
	Target    string `json:"target"`
	Size      int64  `json:"size"`
	ETag      string `json:"etag,omitempty"`
	VersionID string `json:"versionId,omitempty"`
	Parts     int    `json:"parts,omitempty"`
	DryRun    bool   `json:"dryRun,omitempty"`
	// Checksum is the additional checksum of the uploaded object
	// (--checksum).
	Checksum *checksumInfo `json:"checksum,omitempty"`
	// Error is set on the "error" lines of the transfers not attempted.
	Error string `json:"error,omitempty"`
}

// removeRecord is a removed object, version or incomplete upload.
type removeRecord struct {
	Target                string `json:"target"`
	Key                   string `json:"key"`
	VersionID             string `json:"versionId,omitempty"`
	DeleteMarker          bool   `json:"deleteMarker,omitempty"`
	DeleteMarkerVersionID string `json:"deleteMarkerVersionId,omitempty"`
	UploadID              string `json:"uploadId,omitempty"`
	DryRun                bool   `json:"dryRun,omitempty"`
	// BypassGovernance reports a removal with rm --bypass.
	BypassGovernance bool   `json:"bypassGovernance,omitempty"`
	Error            string `json:"error,omitempty"`
}

// bucketRecord is a created or removed bucket.
type bucketRecord struct {
	Target     string `json:"target"`
	Bucket     string `json:"bucket"`
	Created    bool   `json:"created,omitempty"`
	Removed    bool   `json:"removed,omitempty"`
	Versioning string `json:"versioning,omitempty"`
	Objects    int    `json:"removedObjects,omitempty"`
	// ObjectLock reports a bucket created with Object Lock (mb --with-lock).
	ObjectLock bool `json:"objectLock,omitempty"`
}

// shareRecord is a presigned URL (share download/upload, presign).
type shareRecord struct {
	Target    string            `json:"target"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers,omitempty"`
	VersionID string            `json:"versionId,omitempty"`
	Expire    string            `json:"expire"`
	ExpiresAt time.Time         `json:"expiresAt"`
	Command   string            `json:"command,omitempty"`
}

// postPolicyRecord is a POST policy (share upload --post).
type postPolicyRecord struct {
	Target    string            `json:"target"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Fields    map[string]string `json:"fields"`
	Expire    string            `json:"expire"`
	ExpiresAt time.Time         `json:"expiresAt"`
	Command   string            `json:"command"`
	HTML      string            `json:"html"`
}

// accessRecord is the anonymous access of a bucket or prefix.
type accessRecord struct {
	Target string `json:"target"`
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix"`
	Access string `json:"access"`
}

// policyRecord is a bucket policy document (anonymous get-json/set-json).
type policyRecord struct {
	Target string `json:"target"`
	Bucket string `json:"bucket"`
	Policy any    `json:"policy"`
}

// versioningRecord is the versioning state of a bucket.
type versioningRecord struct {
	Target     string `json:"target"`
	Bucket     string `json:"bucket"`
	Versioning string `json:"versioning"`
}

// tagsRecord is the tags of a bucket or object.
type tagsRecord struct {
	Target    string            `json:"target"`
	VersionID string            `json:"versionId,omitempty"`
	Tags      map[string]string `json:"tags"`
}

// usageRecord is a row of du.
type usageRecord struct {
	Target  string `json:"target"`
	Prefix  string `json:"prefix"`
	Size    int64  `json:"size"`
	Objects int64  `json:"objects"`
}

// treeRecord is a node of tree.
type treeRecord struct {
	Target string `json:"target"`
	Path   string `json:"path"`
	Type   string `json:"type"`
	Depth  int    `json:"depth"`
}

// diffRecord is a difference of diff.
type diffRecord struct {
	Diff   string `json:"diff"`
	Path   string `json:"path"`
	First  string `json:"first,omitempty"`
	Second string `json:"second,omitempty"`
}

// contentRecord is the content printed by cat and head with --json.
type contentRecord struct {
	Target        string `json:"target"`
	Size          int64  `json:"size"`
	Content       string `json:"content,omitempty"`
	ContentBase64 string `json:"contentBase64,omitempty"`
}

// aliasRecord is an alias (the secret key is never printed).
type aliasRecord struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	URL       string `json:"url,omitempty"`
	AccessKey string `json:"accessKey,omitempty"`
	SecretKey string `json:"secretKey,omitempty"`
	API       string `json:"api,omitempty"`
	Path      string `json:"path,omitempty"`
	Region    string `json:"region,omitempty"`
	AdminURL  string `json:"adminUrl,omitempty"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func boolPtr(b bool) *bool { return &b }

// objectEntry converts an object of a listing.
func objectEntry(t cli.Target, o cli.ObjectInfo, name string, versions bool) entryRecord {
	r := entryRecord{
		Type: typeFile, Target: t.Child(o.Key).String(), Key: o.Key, Name: name, Size: o.Size,
		LastModified: timePtr(o.LastModified), ETag: o.ETag, StorageClass: o.StorageClass,
		ContentType: o.ContentType, Metadata: o.Metadata,
	}
	if o.Prefix {
		r.Type = typeFolder
		r.LastModified, r.ETag, r.StorageClass = nil, "", ""
	}
	if versions || o.VersionID != "" {
		r.VersionID = o.VersionID
	}
	if versions {
		r.IsLatest = boolPtr(o.IsLatest)
		r.DeleteMarker = o.DeleteMarker
	}
	return r
}

// retentionInfo is the retention of an object version.
type retentionInfo struct {
	Mode        string     `json:"mode"`
	RetainUntil *time.Time `json:"retainUntil,omitempty"`
}

// checksumInfo is the additional checksum of an object.
type checksumInfo struct {
	Algorithm string `json:"algorithm"`
	Type      string `json:"type,omitempty"`
	Value     string `json:"value"`
}

// encryptionInfo is the server-side encryption of an object version.
type encryptionInfo struct {
	// Type is SSE-S3, SSE-KMS or SSE-C.
	Type     string `json:"type"`
	KMSKeyID string `json:"kmsKeyId,omitempty"`
	// CustomerKeyMD5 is the MD5 of the SSE-C key (base64).
	CustomerKeyMD5 string `json:"customerKeyMd5,omitempty"`
}

// objectLockInfo is the Object Lock configuration of a bucket.
type objectLockInfo struct {
	Enabled bool `json:"enabled"`
	// Mode and Validity are the default retention ("30d", "1y").
	Mode     string `json:"mode,omitempty"`
	Validity string `json:"validity,omitempty"`
}

// retentionRecord is the retention of an object version, or the default
// retention of a bucket (Default).
type retentionRecord struct {
	Target    string `json:"target"`
	Key       string `json:"key,omitempty"`
	VersionID string `json:"versionId,omitempty"`
	// Default reports the default retention of a bucket (ObjectLock).
	Default     bool            `json:"default,omitempty"`
	ObjectLock  *objectLockInfo `json:"objectLock,omitempty"`
	Mode        string          `json:"mode,omitempty"`
	RetainUntil *time.Time      `json:"retainUntil,omitempty"`
	// Error is set on the "error" lines of the versions that failed.
	Error string `json:"error,omitempty"`
}

// legalHoldRecord is the legal hold of an object version.
type legalHoldRecord struct {
	Target    string `json:"target"`
	Key       string `json:"key"`
	VersionID string `json:"versionId,omitempty"`
	// LegalHold is ON or OFF.
	LegalHold string `json:"legalHold,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ilmRuleRecord is a lifecycle rule of a bucket (ilm rule ls).
type ilmRuleRecord struct {
	Target string `json:"target"`
	Bucket string `json:"bucket"`
	cli.LifecycleRule
}

// ilmConfigRecord is the lifecycle configuration of a bucket (ilm rule
// export --json, import).
type ilmConfigRecord struct {
	Target string              `json:"target"`
	Bucket string              `json:"bucket"`
	Rules  []cli.LifecycleRule `json:"rules"`
}

// ilmApplyRecord is the outcome of ilm apply.
type ilmApplyRecord struct {
	Target string `json:"target"`
	Bucket string `json:"bucket"`
	nexoadmin.LifecycleApplyResult
}

// eventRecord is an event subscription of a bucket.
type eventRecord struct {
	Target string `json:"target"`
	Bucket string `json:"bucket"`
	cli.EventSubscription
	// Removed reports a subscription removed by event rm.
	Removed bool `json:"removed,omitempty"`
}

// corsRecord is the CORS configuration of a bucket.
type corsRecord struct {
	Target string         `json:"target"`
	Bucket string         `json:"bucket"`
	Rules  []cli.CORSRule `json:"rules"`
}

// kmsStatusRecord is the state of the KMS of a server.
type kmsStatusRecord struct {
	Alias string `json:"alias"`
	nexoadmin.KMSStatus
}

// kmsResealRecord is the outcome of admin kms reseal.
type kmsResealRecord struct {
	Alias  string `json:"alias"`
	Bucket string `json:"bucket,omitempty"`
	nexoadmin.KMSResealResult
}

// kmsKeyRecord is a key of the KMS of a server.
type kmsKeyRecord struct {
	Alias string `json:"alias"`
	nexoadmin.KMSKey
}

// notifyTargetRecord is a notification target of a server.
type notifyTargetRecord struct {
	Alias string `json:"alias"`
	nexoadmin.NotifyTarget
}

// notifyTestRecord is the outcome of a test event.
type notifyTestRecord struct {
	Alias string `json:"alias"`
	nexoadmin.NotifyTargetTest
}

// retentionOf converts a retention (nil when none).
func retentionOf(r domain.ObjectRetention) *retentionInfo {
	if r.IsZero() {
		return nil
	}
	return &retentionInfo{Mode: string(r.Mode), RetainUntil: timePtr(r.RetainUntil)}
}

// checksumOf converts a checksum (nil when none).
func checksumOf(c domain.Checksum) *checksumInfo {
	if c.IsZero() {
		return nil
	}
	return &checksumInfo{Algorithm: string(c.Algorithm), Type: string(c.Type), Value: c.Value}
}

// encryptionOf converts an encryption (nil when not encrypted).
func encryptionOf(e domain.EncryptionInfo) *encryptionInfo {
	if e.IsZero() {
		return nil
	}
	return &encryptionInfo{Type: string(e.Type), KMSKeyID: e.KMSKeyID, CustomerKeyMD5: e.CustomerKeyMD5}
}

// objectLockOf converts the Object Lock configuration of a bucket.
func objectLockOf(cfg *cli.ObjectLockConfig) *objectLockInfo {
	if cfg == nil {
		return nil
	}
	out := &objectLockInfo{Enabled: cfg.Enabled}
	if !cfg.Default.IsZero() {
		out.Mode, out.Validity = string(cfg.Default.Mode), cli.Validity{Days: cfg.Default.Days, Years: cfg.Default.Years}.String()
	}
	return out
}
