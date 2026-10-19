package s3

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
)

// Query parameters of GetObject and HeadObject asking for a variant of an
// image the media worker derived (a Nexo extension: a request without
// them is plain S3). Being part of the query, they are covered by the
// SigV4 signature of a presigned URL.
const (
	// QueryImageFormat is auto (the most compact format of the Accept
	// header), avif, webp, jpeg (jpg), png, or original (the object
	// itself).
	QueryImageFormat = "x-nexo-image-format"
	// QueryImageWidth is the width the client displays, in pixels: the
	// narrowest variant at least that wide.
	QueryImageWidth = "x-nexo-image-width"
)

// imageFormatOriginal asks for the object itself.
const imageFormatOriginal = "original"

// HeaderImageVariant names the variant of an image served by GetObject
// with QueryImageFormat or QueryImageWidth ("webp 640", "avif full").
const HeaderImageVariant = "X-Nexo-Image-Variant"

// imageQueried reports a GetObject or HeadObject asking for a variant.
func imageQueried(q url.Values) bool { return q.Has(QueryImageFormat) || q.Has(QueryImageWidth) }

// objectImage answers a GetObject or HeadObject carrying QueryImageFormat
// or QueryImageWidth (already authorized as the read of the object) with
// a variant of the image, as the media route does (media.SelectImage,
// conditional requests and ranges on the variant, Vary: Accept when the
// format is negotiated). It returns false when the object itself is to be
// served: x-nexo-image-format=original, or no variant derived from the
// current content of the object (not an image, not processed yet, not in
// the format asked for). A malformed parameter is an InvalidArgument.
func (h *objectRoutes) objectImage(w http.ResponseWriter, r *http.Request, bucketName, key string) bool {
	q := r.URL.Query()
	format := strings.TrimSpace(q.Get(QueryImageFormat))
	if strings.EqualFold(format, imageFormatOriginal) {
		return false
	}
	req, err := objectImageRequest(r, format)
	if err == nil && q.Has("partNumber") {
		err = fmt.Errorf("%w: partNumber cannot be combined with %s or %s", domain.ErrInvalidArgument, QueryImageFormat, QueryImageWidth)
	}
	if err == nil && h.deps.Media == nil {
		err = fmt.Errorf("%w: the image variants are not served by this server", domain.ErrNotImplemented)
	}
	if err != nil {
		h.fail(w, r, err)
		return true
	}
	img, err := h.deps.Media.Image(r.Context(), media.Source{Bucket: bucketName, Key: key, VersionID: q.Get("versionId")}, req)
	switch {
	case err == nil:
		w.Header().Set(HeaderImageVariant, imageVariantName(img.MediaImageOutput))
		serveImage(w, r, img, req)
		return true
	case errors.Is(err, domain.ErrInvalidArgument):
		h.fail(w, r, err)
		return true
	}
	// No variant (or the object cannot be read: GetObject answers the
	// error of the object itself).
	return false
}

// objectImageRequest reads the variant a GetObject asks for.
func objectImageRequest(r *http.Request, format string) (media.ImageRequest, error) {
	width, err := media.ParseImageWidth(r.URL.Query().Get(QueryImageWidth))
	if err != nil {
		return media.ImageRequest{}, fmt.Errorf("%w: %s must be a width in pixels", domain.ErrInvalidArgument, QueryImageWidth)
	}
	f, err := media.ParseImageFormat(format)
	if err != nil {
		return media.ImageRequest{}, err
	}
	return media.ImageRequest{Width: width, Format: f, Accept: r.Header.Get("Accept")}, nil
}

// imageVariantName names a variant in HeaderImageVariant.
func imageVariantName(v domain.MediaImageOutput) string {
	if v.Width == 0 {
		return v.Format + " full"
	}
	return v.Format + " " + strconv.Itoa(v.Width)
}
