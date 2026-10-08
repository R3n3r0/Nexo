package s3

import "encoding/xml"

// Owner is the owner element of listings. Nexo has a single owner model.
type Owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

// ListAllMyBucketsResult is the body of ListBuckets.
type ListAllMyBucketsResult struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListAllMyBucketsResult"`
	Owner   Owner    `xml:"Owner"`
	Buckets struct {
		Bucket []BucketEntry `xml:"Bucket"`
	} `xml:"Buckets"`
}

// BucketEntry is one bucket of ListBuckets.
type BucketEntry struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
	BucketRegion string `xml:"BucketRegion,omitempty"`
}

// CreateBucketConfiguration is the optional body of CreateBucket.
type CreateBucketConfiguration struct {
	XMLName            xml.Name `xml:"CreateBucketConfiguration"`
	LocationConstraint string   `xml:"LocationConstraint"`
}

// LocationConstraint is the body of GetBucketLocation.
type LocationConstraint struct {
	XMLName  xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ LocationConstraint"`
	Location string   `xml:",chardata"`
}

// ObjectEntry is one object of a listing.
type ObjectEntry struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
	Owner        *Owner `xml:"Owner,omitempty"`
}

// CommonPrefix is one common prefix of a listing.
type CommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// ListBucketResultV1 is the body of ListObjects.
type ListBucketResultV1 struct {
	XMLName        xml.Name       `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
	Name           string         `xml:"Name"`
	Prefix         string         `xml:"Prefix"`
	Marker         string         `xml:"Marker"`
	NextMarker     string         `xml:"NextMarker,omitempty"`
	MaxKeys        int            `xml:"MaxKeys"`
	Delimiter      string         `xml:"Delimiter,omitempty"`
	EncodingType   string         `xml:"EncodingType,omitempty"`
	IsTruncated    bool           `xml:"IsTruncated"`
	Contents       []ObjectEntry  `xml:"Contents"`
	CommonPrefixes []CommonPrefix `xml:"CommonPrefixes"`
}

// ListBucketResultV2 is the body of ListObjectsV2.
type ListBucketResultV2 struct {
	XMLName               xml.Name       `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	KeyCount              int            `xml:"KeyCount"`
	MaxKeys               int            `xml:"MaxKeys"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	EncodingType          string         `xml:"EncodingType,omitempty"`
	IsTruncated           bool           `xml:"IsTruncated"`
	Contents              []ObjectEntry  `xml:"Contents"`
	CommonPrefixes        []CommonPrefix `xml:"CommonPrefixes"`
}

// VersionEntry is one version of ListObjectVersions.
type VersionEntry struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag,omitempty"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass,omitempty"`
	Owner        Owner  `xml:"Owner"`
}

// DeleteMarkerEntry is one delete marker of ListObjectVersions.
type DeleteMarkerEntry struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	Owner        Owner  `xml:"Owner"`
}

// ListVersionsResult is the body of ListObjectVersions.
type ListVersionsResult struct {
	XMLName             xml.Name            `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListVersionsResult"`
	Name                string              `xml:"Name"`
	Prefix              string              `xml:"Prefix"`
	KeyMarker           string              `xml:"KeyMarker"`
	VersionIDMarker     string              `xml:"VersionIdMarker"`
	NextKeyMarker       string              `xml:"NextKeyMarker,omitempty"`
	NextVersionIDMarker string              `xml:"NextVersionIdMarker,omitempty"`
	MaxKeys             int                 `xml:"MaxKeys"`
	Delimiter           string              `xml:"Delimiter,omitempty"`
	IsTruncated         bool                `xml:"IsTruncated"`
	Versions            []VersionEntry      `xml:"Version"`
	DeleteMarkers       []DeleteMarkerEntry `xml:"DeleteMarker"`
	CommonPrefixes      []CommonPrefix      `xml:"CommonPrefixes"`
}

// VersioningConfiguration is the body of GetBucketVersioning.
type VersioningConfiguration struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ VersioningConfiguration"`
	Status  string   `xml:"Status,omitempty"`
}

// versioningRequest is VersioningConfiguration as accepted in request bodies.
// Request documents carry no namespace in the struct tag because clients may
// omit xmlns, and encoding/xml requires a declared namespace to match exactly.
type versioningRequest struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Status  string   `xml:"Status,omitempty"`
}

// Tag is one tag of a Tagging document.
type Tag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// TagSet is the list of tags of a Tagging document.
type TagSet struct {
	Tags []Tag `xml:"Tag"`
}

// Tagging is the body of the tagging getters.
type Tagging struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ Tagging"`
	TagSet  TagSet   `xml:"TagSet"`
}

// taggingRequest is Tagging as accepted in request bodies (see versioningRequest).
type taggingRequest struct {
	XMLName xml.Name `xml:"Tagging"`
	TagSet  TagSet   `xml:"TagSet"`
}

// PolicyStatus is the body of GetBucketPolicyStatus.
type PolicyStatus struct {
	XMLName  xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ PolicyStatus"`
	IsPublic bool     `xml:"IsPublic"`
}

// DefaultRetention is the default retention of an object lock rule.
type DefaultRetention struct {
	Mode  string `xml:"Mode,omitempty"`
	Days  int    `xml:"Days,omitempty"`
	Years int    `xml:"Years,omitempty"`
}

// ObjectLockRule is the rule of an ObjectLockConfiguration.
type ObjectLockRule struct {
	DefaultRetention DefaultRetention `xml:"DefaultRetention"`
}

// ObjectLockConfiguration is the body of GetObjectLockConfiguration.
type ObjectLockConfiguration struct {
	XMLName           xml.Name        `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ObjectLockConfiguration"`
	ObjectLockEnabled string          `xml:"ObjectLockEnabled,omitempty"`
	Rule              *ObjectLockRule `xml:"Rule,omitempty"`
}

// objectLockRequest is ObjectLockConfiguration as accepted in request bodies.
type objectLockRequest struct {
	XMLName           xml.Name        `xml:"ObjectLockConfiguration"`
	ObjectLockEnabled string          `xml:"ObjectLockEnabled,omitempty"`
	Rule              *ObjectLockRule `xml:"Rule,omitempty"`
}

// EncryptionByDefault is the default encryption of an encryption rule.
type EncryptionByDefault struct {
	SSEAlgorithm   string `xml:"SSEAlgorithm"`
	KMSMasterKeyID string `xml:"KMSMasterKeyID,omitempty"`
}

// EncryptionRule is one rule of a ServerSideEncryptionConfiguration.
type EncryptionRule struct {
	ApplyServerSideEncryptionByDefault EncryptionByDefault `xml:"ApplyServerSideEncryptionByDefault"`
}

// ServerSideEncryptionConfiguration is the body of GetBucketEncryption.
type ServerSideEncryptionConfiguration struct {
	XMLName xml.Name         `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ServerSideEncryptionConfiguration"`
	Rules   []EncryptionRule `xml:"Rule"`
}

// encryptionRequest is ServerSideEncryptionConfiguration as accepted in
// request bodies.
type encryptionRequest struct {
	XMLName xml.Name         `xml:"ServerSideEncryptionConfiguration"`
	Rules   []EncryptionRule `xml:"Rule"`
}

// AccessControlPolicy is the canned private ACL returned by the ACL getters.
type AccessControlPolicy struct {
	XMLName           xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ AccessControlPolicy"`
	Owner             Owner    `xml:"Owner"`
	AccessControlList struct {
		Grants []Grant `xml:"Grant"`
	} `xml:"AccessControlList"`
}

// Grant is one ACL grant.
type Grant struct {
	Grantee struct {
		XMLNS       string `xml:"xmlns:xsi,attr"`
		Type        string `xml:"xsi:type,attr"`
		ID          string `xml:"ID"`
		DisplayName string `xml:"DisplayName"`
	} `xml:"Grantee"`
	Permission string `xml:"Permission"`
}

// CopyObjectResult is the body of CopyObject.
type CopyObjectResult struct {
	XMLName      xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CopyObjectResult"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

// CopyPartResult is the body of UploadPartCopy.
type CopyPartResult struct {
	XMLName      xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CopyPartResult"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

// DeleteRequest is the body of DeleteObjects.
type DeleteRequest struct {
	XMLName xml.Name `xml:"Delete"`
	Quiet   bool     `xml:"Quiet"`
	Objects []struct {
		Key       string `xml:"Key"`
		VersionID string `xml:"VersionId"`
	} `xml:"Object"`
}

// DeleteResult is the body of DeleteObjects.
type DeleteResult struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ DeleteResult"`
	Deleted []struct {
		Key                   string `xml:"Key"`
		VersionID             string `xml:"VersionId,omitempty"`
		DeleteMarker          bool   `xml:"DeleteMarker,omitempty"`
		DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId,omitempty"`
	} `xml:"Deleted"`
	Errors []struct {
		Key       string `xml:"Key"`
		VersionID string `xml:"VersionId,omitempty"`
		Code      string `xml:"Code"`
		Message   string `xml:"Message"`
	} `xml:"Error"`
}

// InitiateMultipartUploadResult is the body of CreateMultipartUpload.
type InitiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

// CompleteMultipartUploadRequest is the body of CompleteMultipartUpload.
type CompleteMultipartUploadRequest struct {
	XMLName xml.Name `xml:"CompleteMultipartUpload"`
	Parts   []struct {
		PartNumber     int    `xml:"PartNumber"`
		ETag           string `xml:"ETag"`
		ChecksumCRC32  string `xml:"ChecksumCRC32"`
		ChecksumCRC32C string `xml:"ChecksumCRC32C"`
		ChecksumSHA1   string `xml:"ChecksumSHA1"`
		ChecksumSHA256 string `xml:"ChecksumSHA256"`
	} `xml:"Part"`
}

// CompleteMultipartUploadResult is the body of CompleteMultipartUpload.
type CompleteMultipartUploadResult struct {
	XMLName  xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CompleteMultipartUploadResult"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

// PartEntry is one part of ListParts.
type PartEntry struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

// ListPartsResult is the body of ListParts.
type ListPartsResult struct {
	XMLName              xml.Name    `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListPartsResult"`
	Bucket               string      `xml:"Bucket"`
	Key                  string      `xml:"Key"`
	UploadID             string      `xml:"UploadId"`
	Initiator            Owner       `xml:"Initiator"`
	Owner                Owner       `xml:"Owner"`
	StorageClass         string      `xml:"StorageClass"`
	PartNumberMarker     int         `xml:"PartNumberMarker"`
	NextPartNumberMarker int         `xml:"NextPartNumberMarker"`
	MaxParts             int         `xml:"MaxParts"`
	IsTruncated          bool        `xml:"IsTruncated"`
	Parts                []PartEntry `xml:"Part"`
}

// UploadEntry is one upload of ListMultipartUploads.
type UploadEntry struct {
	Key          string `xml:"Key"`
	UploadID     string `xml:"UploadId"`
	Initiator    Owner  `xml:"Initiator"`
	Owner        Owner  `xml:"Owner"`
	StorageClass string `xml:"StorageClass"`
	Initiated    string `xml:"Initiated"`
}

// ListMultipartUploadsResult is the body of ListMultipartUploads.
type ListMultipartUploadsResult struct {
	XMLName            xml.Name       `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListMultipartUploadsResult"`
	Bucket             string         `xml:"Bucket"`
	KeyMarker          string         `xml:"KeyMarker"`
	UploadIDMarker     string         `xml:"UploadIdMarker"`
	NextKeyMarker      string         `xml:"NextKeyMarker,omitempty"`
	NextUploadIDMarker string         `xml:"NextUploadIdMarker,omitempty"`
	Prefix             string         `xml:"Prefix"`
	Delimiter          string         `xml:"Delimiter,omitempty"`
	MaxUploads         int            `xml:"MaxUploads"`
	IsTruncated        bool           `xml:"IsTruncated"`
	Uploads            []UploadEntry  `xml:"Upload"`
	CommonPrefixes     []CommonPrefix `xml:"CommonPrefixes"`
}

// PostResponse is the body of a browser POST upload answered with 201.
type PostResponse struct {
	XMLName  xml.Name `xml:"PostResponse"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}
