package event

import (
	"strings"
	"time"
)

// Name is an S3 event name such as "s3:ObjectCreated:Put".
type Name string

// Standard S3 event names.
const (
	ObjectCreatedAll                     Name = "s3:ObjectCreated:*"
	ObjectCreatedPut                     Name = "s3:ObjectCreated:Put"
	ObjectCreatedPost                    Name = "s3:ObjectCreated:Post"
	ObjectCreatedCopy                    Name = "s3:ObjectCreated:Copy"
	ObjectCreatedCompleteMultipartUpload Name = "s3:ObjectCreated:CompleteMultipartUpload"
	ObjectCreatedPutTagging              Name = "s3:ObjectCreated:PutTagging"
	ObjectCreatedDeleteTagging           Name = "s3:ObjectCreated:DeleteTagging"
	ObjectRemovedAll                     Name = "s3:ObjectRemoved:*"
	ObjectRemovedDelete                  Name = "s3:ObjectRemoved:Delete"
	ObjectRemovedDeleteMarkerCreated     Name = "s3:ObjectRemoved:DeleteMarkerCreated"
	ObjectAccessedAll                    Name = "s3:ObjectAccessed:*"
	ObjectAccessedGet                    Name = "s3:ObjectAccessed:Get"
	ObjectAccessedHead                   Name = "s3:ObjectAccessed:Head"
	BucketCreated                        Name = "s3:BucketCreated:*"
	BucketRemoved                        Name = "s3:BucketRemoved:*"
)

// Match reports whether the event name matches pattern, where pattern may
// end with ":*" to match a whole family (e.g. "s3:ObjectCreated:*").
func (n Name) Match(pattern Name) bool {
	if n == pattern || pattern == "*" {
		return true
	}
	if strings.HasSuffix(string(pattern), ":*") {
		return strings.HasPrefix(string(n), strings.TrimSuffix(string(pattern), "*"))
	}
	return false
}

// Event is one S3 notification record (eventVersion 2.1).
type Event struct {
	EventVersion      string            `json:"eventVersion"`
	EventSource       string            `json:"eventSource"`
	AwsRegion         string            `json:"awsRegion"`
	EventTime         string            `json:"eventTime"`
	EventName         Name              `json:"eventName"`
	UserIdentity      Identity          `json:"userIdentity"`
	RequestParameters map[string]string `json:"requestParameters"`
	ResponseElements  map[string]string `json:"responseElements"`
	S3                Metadata          `json:"s3"`
	Source            Source            `json:"source"`
}

// Identity is the principal in an event.
type Identity struct {
	PrincipalID string `json:"principalId"`
}

// Metadata is the "s3" element of an event.
type Metadata struct {
	SchemaVersion   string `json:"s3SchemaVersion"`
	ConfigurationID string `json:"configurationId"`
	Bucket          Bucket `json:"bucket"`
	Object          Object `json:"object"`
}

// Bucket is the bucket element of an event.
type Bucket struct {
	Name          string   `json:"name"`
	OwnerIdentity Identity `json:"ownerIdentity"`
	ARN           string   `json:"arn"`
}

// Object is the object element of an event.
type Object struct {
	Key          string            `json:"key"`
	Size         int64             `json:"size,omitempty"`
	ETag         string            `json:"eTag,omitempty"`
	ContentType  string            `json:"contentType,omitempty"`
	UserMetadata map[string]string `json:"userMetadata,omitempty"`
	VersionID    string            `json:"versionId,omitempty"`
	Sequencer    string            `json:"sequencer"`
}

// Source describes the client that caused the event.
type Source struct {
	Host      string `json:"host"`
	Port      string `json:"port"`
	UserAgent string `json:"userAgent"`
}

// Defaults for new events.
const (
	Version       = "2.1"
	SourceName    = "nexo:s3"
	SchemaVersion = "1.0"
)

// NewEvent builds an event with the standard constant fields filled in. It
// is a model constructor, not the service constructor (see NewDispatcher).
func NewEvent(name Name, region string, at time.Time) Event {
	return Event{
		EventVersion:      Version,
		EventSource:       SourceName,
		AwsRegion:         region,
		EventTime:         at.UTC().Format("2006-01-02T15:04:05.000Z"),
		EventName:         name,
		RequestParameters: map[string]string{},
		ResponseElements:  map[string]string{},
		S3:                Metadata{SchemaVersion: SchemaVersion},
	}
}

// Matches reports whether the event passes the filter.
func (f Filter) Matches(e Event) bool {
	if f.Bucket != "" && f.Bucket != e.S3.Bucket.Name {
		return false
	}
	if f.Prefix != "" && !strings.HasPrefix(e.S3.Object.Key, f.Prefix) {
		return false
	}
	if f.Suffix != "" && !strings.HasSuffix(e.S3.Object.Key, f.Suffix) {
		return false
	}
	if len(f.Events) == 0 {
		return true
	}
	for _, p := range f.Events {
		if e.EventName.Match(p) {
			return true
		}
	}
	return false
}
