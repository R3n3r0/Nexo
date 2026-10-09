package s3

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// Request headers of GetObjectAttributes.
const (
	headerAmzObjectAttributes = "x-amz-object-attributes"
	headerAmzMaxParts         = "x-amz-max-parts"
	headerAmzPartNumberMarker = "x-amz-part-number-marker"
)

// Attributes GetObjectAttributes can return (x-amz-object-attributes).
const (
	attributeETag         = "ETag"
	attributeChecksum     = "Checksum"
	attributeObjectParts  = "ObjectParts"
	attributeStorageClass = "StorageClass"
	attributeObjectSize   = "ObjectSize"
)

// GetObjectAttributesResponse is the body of GetObjectAttributes: the
// requested attributes only.
type GetObjectAttributesResponse struct {
	XMLName      xml.Name            `xml:"http://s3.amazonaws.com/doc/2006-03-01/ GetObjectAttributesResponse"`
	ETag         string              `xml:"ETag,omitempty"`
	Checksum     *AttributesChecksum `xml:"Checksum,omitempty"`
	ObjectParts  *AttributesParts    `xml:"ObjectParts,omitempty"`
	StorageClass string              `xml:"StorageClass,omitempty"`
	ObjectSize   *int64              `xml:"ObjectSize,omitempty"`
}

// AttributesChecksum is the Checksum of GetObjectAttributes.
type AttributesChecksum struct {
	ChecksumFields
	ChecksumType string `xml:"ChecksumType,omitempty"`
}

// AttributesParts is the ObjectParts of GetObjectAttributes.
type AttributesParts struct {
	IsTruncated          bool             `xml:"IsTruncated"`
	MaxParts             int              `xml:"MaxParts"`
	NextPartNumberMarker int              `xml:"NextPartNumberMarker"`
	PartNumberMarker     int              `xml:"PartNumberMarker"`
	Parts                []AttributesPart `xml:"Part"`
	PartsCount           int              `xml:"PartsCount"`
}

// AttributesPart is one part of ObjectParts.
type AttributesPart struct {
	ChecksumFields
	PartNumber int   `xml:"PartNumber"`
	Size       int64 `xml:"Size"`
}

// objectAttributesRequest reads the attributes a GetObjectAttributes
// asks for.
func objectAttributesRequest(h http.Header) (map[string]bool, error) {
	want := map[string]bool{}
	for _, v := range h.Values(headerAmzObjectAttributes) {
		for _, name := range strings.Split(v, ",") {
			name = strings.TrimSpace(name)
			switch name {
			case "":
			case attributeETag, attributeChecksum, attributeObjectParts, attributeStorageClass, attributeObjectSize:
				want[name] = true
			default:
				return nil, invalidArgument("Invalid attribute name specified.")
			}
		}
	}
	if len(want) == 0 {
		return nil, invalidRequest("The x-amz-object-attributes header specifying the attributes to be retrieved is either missing or empty")
	}
	return want, nil
}

// headerInt reads a non-negative integer header (def when absent).
func headerInt(h http.Header, name string, def int) (int, error) {
	v := strings.TrimSpace(h.Get(name))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, invalidArgument("Header " + name + " must be a non-negative integer")
	}
	return n, nil
}

// getAttributes serves GetObjectAttributes. It needs s3:GetObject (or
// s3:GetObjectVersion) on top of s3:GetObjectAttributes (or
// s3:GetObjectVersionAttributes), as S3 does.
func (h *objectRoutes) getAttributes(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	versionID := r.URL.Query().Get("versionId")
	if !h.authz.authorize(w, r, bucketName, key) ||
		!h.authz.authorizeAction(w, r, ActionOf("GetObject", versionID != ""), bucketName, key) {
		return
	}
	want, err := objectAttributesRequest(r.Header)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	maxParts, err := headerInt(r.Header, headerAmzMaxParts, maxListKeys)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	marker, err := headerInt(r.Header, headerAmzPartNumberMarker, 0)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ckey, err := customerKeyFromHeaders(r, headerSSECustomerPrefix, h.opts.insecureSSEC)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	maxParts = min(maxParts, maxListKeys)
	opts := object.GetObjectOptions{VersionID: versionID, SSECustomerKey: ckey}
	attrs, err := h.deps.Objects.GetObjectAttributes(r.Context(), bucketName, key, object.AttributesOptions{
		VersionID: versionID, MaxParts: serviceLimit(maxParts), PartNumberMarker: marker, SSECustomerKey: ckey,
	})
	if err != nil {
		h.writeReadError(w, r, err, bucketName, key, opts)
		return
	}
	info := attrs.Info
	out := GetObjectAttributesResponse{}
	if want[attributeETag] {
		out.ETag = trimETag(info.ETag)
	}
	if want[attributeChecksum] && !info.Checksum.IsZero() {
		out.Checksum = &AttributesChecksum{ChecksumFields: checksumFields(info.Checksum), ChecksumType: string(info.Checksum.Type)}
	}
	if want[attributeObjectParts] && attrs.PartsCount > 0 {
		parts := &AttributesParts{
			IsTruncated: attrs.IsTruncated, MaxParts: maxParts, NextPartNumberMarker: attrs.NextPartNumberMarker,
			PartNumberMarker: marker, PartsCount: attrs.PartsCount,
		}
		for _, p := range attrs.Parts {
			parts.Parts = append(parts.Parts, AttributesPart{ChecksumFields: checksumFields(p.Checksum), PartNumber: p.Number, Size: p.Size})
		}
		out.ObjectParts = parts
	}
	if want[attributeStorageClass] {
		out.StorageClass = storageClassOrStandard(info.StorageClass)
	}
	if want[attributeObjectSize] {
		size := info.Size
		out.ObjectSize = &size
	}
	if !info.ModTime.IsZero() {
		w.Header().Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	}
	if info.VersionID != "" {
		w.Header().Set(httpx.HeaderAmzVersionID, info.VersionID)
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}
