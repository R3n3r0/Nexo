package commands

import (
	"time"

	"github.com/r3n3r0/nexo/internal/cli"
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
	// Bucket settings (stat of a bucket).
	Versioning string `json:"versioning,omitempty"`
	Anonymous  string `json:"anonymous,omitempty"`
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
	Error                 string `json:"error,omitempty"`
}

// bucketRecord is a created or removed bucket.
type bucketRecord struct {
	Target     string `json:"target"`
	Bucket     string `json:"bucket"`
	Created    bool   `json:"created,omitempty"`
	Removed    bool   `json:"removed,omitempty"`
	Versioning string `json:"versioning,omitempty"`
	Objects    int    `json:"removedObjects,omitempty"`
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
