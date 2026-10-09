package admin

import (
	"net/http"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
)

// RetentionResponse is the Object Lock retention of a version: the body
// of GET /buckets/{bucket}/objects/retention (no mode when the version has
// none) and of the answer to PUT.
type RetentionResponse struct {
	VersionID string `json:"versionId,omitempty"`
	// Mode is GOVERNANCE or COMPLIANCE.
	Mode        string     `json:"mode,omitempty"`
	RetainUntil *time.Time `json:"retainUntil,omitempty"`
}

// RetentionRequest is the JSON body of PUT
// /buckets/{bucket}/objects/retention: a mode with a future retain-until
// date, or neither to remove the retention.
type RetentionRequest struct {
	Mode        string     `json:"mode,omitempty"`
	RetainUntil *time.Time `json:"retainUntil,omitempty"`
	// BypassGovernance shortens or removes a GOVERNANCE retention
	// (s3:BypassGovernanceRetention).
	BypassGovernance bool `json:"bypassGovernance,omitempty"`
}

// LegalHoldResponse is the legal hold of a version (ON or OFF), the body
// of GET and PUT /buckets/{bucket}/objects/legal-hold.
type LegalHoldResponse struct {
	VersionID string `json:"versionId,omitempty"`
	Status    string `json:"status"`
}

// ChecksumResponse is the additional checksum of an object or a part.
type ChecksumResponse struct {
	// Algorithm is CRC32, CRC32C, CRC64NVME, SHA1 or SHA256.
	Algorithm string `json:"algorithm"`
	// Type is FULL_OBJECT or COMPOSITE (multipart objects).
	Type string `json:"type,omitempty"`
	// Value is base64-encoded.
	Value string `json:"value"`
}

// ObjectPartResponse is one part of a multipart object.
type ObjectPartResponse struct {
	PartNumber int               `json:"partNumber"`
	Size       int64             `json:"size"`
	Checksum   *ChecksumResponse `json:"checksum,omitempty"`
}

// ObjectAttributesResponse is the body of GET
// /buckets/{bucket}/objects/attributes.
type ObjectAttributesResponse struct {
	Key          string            `json:"key"`
	VersionID    string            `json:"versionId,omitempty"`
	ETag         string            `json:"etag"`
	Size         int64             `json:"size"`
	LastModified time.Time         `json:"lastModified"`
	StorageClass string            `json:"storageClass,omitempty"`
	Checksum     *ChecksumResponse `json:"checksum,omitempty"`
	// PartsCount is the number of parts of a multipart object (0 for an
	// object written in one request); Parts is a page of them.
	PartsCount           int                  `json:"partsCount"`
	Parts                []ObjectPartResponse `json:"parts"`
	IsTruncated          bool                 `json:"isTruncated"`
	NextPartNumberMarker int                  `json:"nextPartNumberMarker,omitempty"`
}

// versionRefOf names the version of an object route (?key=&versionId=).
func versionRefOf(r *http.Request) adminsvc.VersionRef {
	q := r.URL.Query()
	return adminsvc.VersionRef{Bucket: bucketName(r), Key: q.Get("key"), VersionID: q.Get("versionId")}
}

func newRetentionResponse(versionID string, ret domain.ObjectRetention) RetentionResponse {
	return RetentionResponse{VersionID: versionID, Mode: string(ret.Mode), RetainUntil: optTime(ret.RetainUntil)}
}

func newChecksumResponse(c domain.Checksum) *ChecksumResponse {
	if c.IsZero() {
		return nil
	}
	return &ChecksumResponse{Algorithm: string(c.Algorithm), Type: string(c.Type), Value: c.Value}
}

func (h *objectRoutes) getRetention(w http.ResponseWriter, r *http.Request) {
	ref := versionRefOf(r)
	ret, err := h.deps.Browser.Retention(r.Context(), ref)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newRetentionResponse(ref.VersionID, ret))
}

func (h *objectRoutes) putRetention(w http.ResponseWriter, r *http.Request) {
	var in RetentionRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	ret := domain.ObjectRetention{Mode: domain.RetentionMode(in.Mode)}
	if in.RetainUntil != nil {
		ret.RetainUntil = *in.RetainUntil
	}
	info, err := h.deps.Browser.SetRetention(r.Context(), adminsvc.SetRetentionInput{
		Requester: requesterOf(r), Object: versionRefOf(r), Retention: ret, BypassGovernance: in.BypassGovernance,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newRetentionResponse(info.VersionID, info.Retention))
}

func (h *objectRoutes) getLegalHold(w http.ResponseWriter, r *http.Request) {
	ref := versionRefOf(r)
	hold, err := h.deps.Browser.LegalHold(r.Context(), ref)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, LegalHoldResponse{VersionID: ref.VersionID, Status: string(hold)})
}

func (h *objectRoutes) putLegalHold(w http.ResponseWriter, r *http.Request) {
	var in LegalHoldResponse
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	info, err := h.deps.Browser.SetLegalHold(r.Context(), adminsvc.SetLegalHoldInput{Object: versionRefOf(r), Hold: domain.LegalHoldStatus(in.Status)})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, LegalHoldResponse{VersionID: info.VersionID, Status: string(info.LegalHold)})
}

func (h *objectRoutes) attributes(w http.ResponseWriter, r *http.Request) {
	maxParts, err := queryInt(r, "maxParts")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	marker, err := queryInt(r, "partNumberMarker")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	attrs, err := h.deps.Browser.Attributes(r.Context(), adminsvc.AttributesInput{Object: versionRefOf(r), MaxParts: maxParts, PartNumberMarker: marker})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	info := attrs.Info
	out := ObjectAttributesResponse{
		Key: info.Key, VersionID: info.VersionID, ETag: info.ETag, Size: info.Size, LastModified: info.ModTime.UTC(),
		StorageClass: info.StorageClass, Checksum: newChecksumResponse(info.Checksum), PartsCount: attrs.PartsCount,
		Parts: make([]ObjectPartResponse, 0, len(attrs.Parts)), IsTruncated: attrs.IsTruncated, NextPartNumberMarker: attrs.NextPartNumberMarker,
	}
	for _, p := range attrs.Parts {
		out.Parts = append(out.Parts, ObjectPartResponse{PartNumber: p.Number, Size: p.Size, Checksum: newChecksumResponse(p.Checksum)})
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}
