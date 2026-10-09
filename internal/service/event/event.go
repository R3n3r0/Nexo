package event

import (
	"context"
	"net/url"
	"strings"
	"time"
)

// Name is an S3 event name such as "s3:ObjectCreated:Put".
type Name string

// Standard S3 event names.
const (
	ObjectCreatedAll                       Name = "s3:ObjectCreated:*"
	ObjectCreatedPut                       Name = "s3:ObjectCreated:Put"
	ObjectCreatedPost                      Name = "s3:ObjectCreated:Post"
	ObjectCreatedCopy                      Name = "s3:ObjectCreated:Copy"
	ObjectCreatedCompleteMultipartUpload   Name = "s3:ObjectCreated:CompleteMultipartUpload"
	ObjectRemovedAll                       Name = "s3:ObjectRemoved:*"
	ObjectRemovedDelete                    Name = "s3:ObjectRemoved:Delete"
	ObjectRemovedDeleteMarkerCreated       Name = "s3:ObjectRemoved:DeleteMarkerCreated"
	ObjectAccessedAll                      Name = "s3:ObjectAccessed:*"
	ObjectAccessedGet                      Name = "s3:ObjectAccessed:Get"
	ObjectAccessedHead                     Name = "s3:ObjectAccessed:Head"
	ObjectTaggingAll                       Name = "s3:ObjectTagging:*"
	ObjectTaggingPut                       Name = "s3:ObjectTagging:Put"
	ObjectTaggingDelete                    Name = "s3:ObjectTagging:Delete"
	LifecycleExpirationAll                 Name = "s3:LifecycleExpiration:*"
	LifecycleExpirationDelete              Name = "s3:LifecycleExpiration:Delete"
	LifecycleExpirationDeleteMarkerCreated Name = "s3:LifecycleExpiration:DeleteMarkerCreated"
	BucketCreated                          Name = "s3:BucketCreated:*"
	BucketRemoved                          Name = "s3:BucketRemoved:*"
	// TestEvent is the event Service.TestTarget delivers to check a
	// target; no configuration subscribes to it.
	TestEvent Name = "s3:TestEvent"

	// ObjectCreatedPutTagging and ObjectCreatedDeleteTagging are the names
	// the object layer publishes for its tagging operations: the S3
	// s3:ObjectTagging events.
	ObjectCreatedPutTagging    = ObjectTaggingPut
	ObjectCreatedDeleteTagging = ObjectTaggingDelete
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

// notifiableNames are the event names (and families) a bucket notification
// configuration may subscribe to.
func notifiableNames() []Name {
	return []Name{
		ObjectCreatedAll, ObjectCreatedPut, ObjectCreatedPost, ObjectCreatedCopy, ObjectCreatedCompleteMultipartUpload,
		ObjectRemovedAll, ObjectRemovedDelete, ObjectRemovedDeleteMarkerCreated,
		ObjectAccessedAll, ObjectAccessedGet, ObjectAccessedHead,
		ObjectTaggingAll, ObjectTaggingPut, ObjectTaggingDelete,
		LifecycleExpirationAll, LifecycleExpirationDelete, LifecycleExpirationDeleteMarkerCreated,
	}
}

// IsNotifiable reports whether a bucket notification configuration may
// subscribe to n.
func (n Name) IsNotifiable() bool {
	for _, known := range notifiableNames() {
		if n == known {
			return true
		}
	}
	return false
}

// isRemoval reports the events that remove an object: the namespace
// formats delete its entry.
func (n Name) isRemoval() bool {
	return n.Match(ObjectRemovedAll) || n.Match(LifecycleExpirationAll)
}

// Event is one S3 notification record (eventVersion 2.0). Between the
// publisher and the router the object key is the raw key; the records
// queued for the targets carry it URL-encoded as S3 does (spaces as "+",
// slashes kept).
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
	Version       = "2.0"
	SourceName    = "nexo:s3"
	SchemaVersion = "1.0"
	// ConfigID is the configurationId of the records routed by the filter
	// of a target rather than by a bucket configuration.
	ConfigID = "Config"
	// LifecyclePrincipal is the principal of the lifecycle expirations.
	LifecyclePrincipal = "nexo:lifecycle"
)

// Response elements and request parameters of the records.
const (
	paramSourceIP    = "sourceIPAddress"
	elemRequestID    = "x-amz-request-id"
	elemHostID       = "x-amz-id-2"
	eventTimeLayout  = "2006-01-02T15:04:05.000Z"
	bucketARNPrefix  = "arn:aws:s3:::"
	postObjectOpName = "PostObject"
)

// NewEvent builds an event with the standard constant fields filled in. It
// is a model constructor, not the service constructor (see NewDispatcher).
func NewEvent(name Name, region string, at time.Time) Event {
	return Event{
		EventVersion:      Version,
		EventSource:       SourceName,
		AwsRegion:         region,
		EventTime:         at.UTC().Format(eventTimeLayout),
		EventName:         name,
		RequestParameters: map[string]string{},
		ResponseElements:  map[string]string{},
		S3:                Metadata{SchemaVersion: SchemaVersion},
	}
}

// IsZero reports whether the filter selects nothing (no criterion set).
func (f Filter) IsZero() bool {
	return len(f.Events) == 0 && f.Bucket == "" && f.Prefix == "" && f.Suffix == ""
}

// Matches reports whether the event passes the filter. Empty fields match
// all (the zero Filter matches every event; the dispatcher never routes by
// a zero Filter).
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

// TargetID returns the identifier of the configured target.
func (c TargetConfig) TargetID() TargetID { return TargetID{ID: c.ID, Type: c.Type} }

// String returns "<id>:<type>".
func (t TargetID) String() string { return t.ID + ":" + t.Type }

// ARN returns the ARN of the target in region:
// arn:nexo:sqs:<region>:<id>:<type>.
func (t TargetID) ARN(region string) string {
	return ARN{Region: region, ID: t.ID, Type: t.Type}.String()
}

// Payload is the JSON document the targets deliver: the event name, the
// "<bucket>/<key>" it concerns and the records (one).
type Payload struct {
	EventName Name    `json:"EventName"`
	Key       string  `json:"Key"`
	Records   []Event `json:"Records"`
}

// record returns the copy of e delivered for a configuration: the
// configuration id set and the object key URL-encoded.
func (e Event) record(configID string) Event {
	rec := e
	rec.S3.ConfigurationID = configID
	rec.S3.Object.Key = encodeKey(e.S3.Object.Key)
	return rec
}

// encodeKey URL-encodes an object key as S3 does in the records: spaces
// become "+", the slashes are kept.
func encodeKey(key string) string {
	return strings.ReplaceAll(url.QueryEscape(key), "%2F", "/")
}

// rawKey returns the object key of a record, decoded.
func (e Event) rawKey() string {
	if k, err := url.QueryUnescape(e.S3.Object.Key); err == nil {
		return k
	}
	return e.S3.Object.Key
}

// payload returns the JSON payload of a record.
func (e Event) payload() Payload {
	return Payload{EventName: e.EventName, Key: e.S3.Bucket.Name + "/" + e.rawKey(), Records: []Event{e}}
}

// --- request context ----------------------------------------------------------

type requestKey struct{}

// WithRequest returns a context carrying the request that will cause the
// events published with it.
func WithRequest(ctx context.Context, r Request) context.Context {
	return context.WithValue(ctx, requestKey{}, r)
}

// RequestFrom returns the request carried by ctx.
func RequestFrom(ctx context.Context) (Request, bool) {
	r, ok := ctx.Value(requestKey{}).(Request)
	return r, ok
}

// complete fills in what the request of ctx tells about an event: the
// identity, the client and the request ids, the POST upload and the
// lifecycle expirations.
func complete(ctx context.Context, e Event, owner string) Event {
	// The maps are copied: the publisher keeps its event.
	e.RequestParameters = cloneMap(e.RequestParameters)
	e.ResponseElements = cloneMap(e.ResponseElements)
	if e.EventVersion == "" {
		e.EventVersion = Version
	}
	if e.EventSource == "" {
		e.EventSource = SourceName
	}
	if e.S3.SchemaVersion == "" {
		e.S3.SchemaVersion = SchemaVersion
	}
	if e.S3.Bucket.ARN == "" && e.S3.Bucket.Name != "" {
		e.S3.Bucket.ARN = bucketARNPrefix + e.S3.Bucket.Name
	}
	if e.S3.Bucket.OwnerIdentity.PrincipalID == "" {
		e.S3.Bucket.OwnerIdentity.PrincipalID = owner
	}
	req, ok := RequestFrom(ctx)
	if !ok {
		return e
	}
	if req.Lifecycle {
		switch e.EventName {
		case ObjectRemovedDelete:
			e.EventName = LifecycleExpirationDelete
		case ObjectRemovedDeleteMarkerCreated:
			e.EventName = LifecycleExpirationDeleteMarkerCreated
		}
		if req.PrincipalID == "" {
			req.PrincipalID = LifecyclePrincipal
		}
	}
	if e.EventName == ObjectCreatedPut && req.Operation != nil && req.Operation() == postObjectOpName {
		e.EventName = ObjectCreatedPost
	}
	if req.PrincipalID != "" {
		e.UserIdentity.PrincipalID = req.PrincipalID
	}
	if req.SourceIP != "" {
		e.RequestParameters[paramSourceIP] = req.SourceIP
		e.Source.Host = req.SourceIP
		e.Source.Port = req.Port
	}
	if req.UserAgent != "" {
		e.Source.UserAgent = req.UserAgent
	}
	if req.RequestID != "" {
		e.ResponseElements[elemRequestID] = req.RequestID
	}
	if req.HostID != "" {
		e.ResponseElements[elemHostID] = req.HostID
	}
	return e
}

// cloneMap returns a copy of m (an empty map for nil).
func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
