package s3

import (
	"net/http"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// Actions of the requests of the replication of another site (S-BKT-12),
// required on top of the action of the operation.
const (
	actionReplicateObject policy.Action = "s3:ReplicateObject"
	actionReplicateDelete policy.Action = "s3:ReplicateDelete"
	actionReplicateTags   policy.Action = "s3:ReplicateTags"
)

// replica is what a request of the replication asks (the
// domain.HeaderReplica... headers): the version id, the time and the ETag
// the version has at its source, or a delete marker.
type replica struct {
	set          bool
	versionID    string
	modTime      time.Time
	etag         string
	deleteMarker bool
}

// validReplicaETag reports an ETag of the form of S3: 32 hexadecimal
// digits, followed by -N (the parts of a multipart upload).
func validReplicaETag(etag string) bool {
	md5, parts, multipart := strings.Cut(etag, "-")
	if len(md5) != 32 || (multipart && (parts == "" || len(parts) > 5)) {
		return false
	}
	for _, c := range md5 {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	for _, c := range parts {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// parseReplica reads the replica headers (set false without
// domain.HeaderReplica).
func parseReplica(h http.Header) (replica, error) {
	if h.Get(domain.HeaderReplica) != "true" {
		return replica{}, nil
	}
	rep := replica{set: true, versionID: h.Get(domain.HeaderReplicaVersionID), etag: h.Get(domain.HeaderReplicaETag),
		deleteMarker: h.Get(domain.HeaderReplicaDeleteMarker) == "true"}
	if rep.versionID != "" && (rep.versionID == domain.NullVersionID || domain.ValidateVersionID(rep.versionID) != nil) {
		return replica{}, invalidArgument("Invalid replica version id")
	}
	if raw := h.Get(domain.HeaderReplicaModTime); raw != "" {
		t, err := time.Parse(domain.ReplicaModTimeLayout, raw)
		if err != nil || t.IsZero() {
			return replica{}, invalidArgument("Invalid replica modification time")
		}
		rep.modTime = t.UTC()
	}
	if rep.etag != "" && !validReplicaETag(rep.etag) {
		return replica{}, invalidArgument("Invalid replica ETag")
	}
	return rep, nil
}

// replicaRequest authorizes a request of the replication with action and
// marks its context (object.WithReplica: what it writes or deletes is not
// replicated back). A request without the replica headers is returned
// unchanged. It writes the error response and returns false on failure.
func (a *authorizer) replicaRequest(w http.ResponseWriter, r *http.Request, action policy.Action, bucketName, key string) (*http.Request, replica, bool) {
	rep, err := parseReplica(r.Header)
	if err != nil {
		a.enc.WriteS3Error(w, r, err)
		return r, replica{}, false
	}
	if !rep.set {
		return r, rep, true
	}
	if !a.authorizeAction(w, r, action, bucketName, key) {
		return r, replica{}, false
	}
	return r.WithContext(object.WithReplica(r.Context())), rep, true
}

// writeReplicationStatus sets x-amz-replication-status on an object
// response.
func writeReplicationStatus(w http.ResponseWriter, info domain.ObjectInfo) {
	if info.ReplicationStatus != "" {
		w.Header().Set(headerAmzReplicationStatus, string(info.ReplicationStatus))
	}
}

const headerAmzReplicationStatus = "x-amz-replication-status"
