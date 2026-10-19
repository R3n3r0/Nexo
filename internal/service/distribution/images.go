package distribution

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// OutcomeImage is the outcome of an answer serving a variant of an image
// (Images).
const OutcomeImage = "image"

// HeaderImageVariant names the variant served at the URL of an image
// ("webp 640", "avif full"): absent when the original is served.
const HeaderImageVariant = "X-Nexo-Image-Variant"

// maxImageStatus bounds the status.json of an image read on a request.
const maxImageStatus = 1 << 20

// negotiatesImages reports a behavior serving the variants of the images
// at their own URL.
func negotiatesImages(b Behavior) bool { return b.Images != nil && b.Images.Negotiate }

// ImageQuery is the variant a URL of an image asks for (Images): ok is
// false for ?format=original. A malformed ?w= or ?format= fails with
// domain.ErrInvalidArgument.
func ImageQuery(rawQuery string) (req media.ImageRequest, ok bool, err error) {
	q, _ := url.ParseQuery(rawQuery)
	format := q.Get(ImageParamFormat)
	if strings.EqualFold(strings.TrimSpace(format), ImageFormatOriginal) {
		return media.ImageRequest{}, false, nil
	}
	if req.Format, err = media.ParseImageFormat(format); err != nil {
		return media.ImageRequest{}, false, err
	}
	if req.Width, err = media.ParseImageWidth(q.Get(ImageParamWidth)); err != nil {
		return media.ImageRequest{}, false, err
	}
	return req, true, nil
}

// imageStatus reads the status of the media worker for the version info
// of key: nil when the worker derived no image variant from its current
// content (never processed, processing, failed, another content).
func (w *website) imageStatus(ctx context.Context, bucket, key string, info domain.ObjectInfo) *domain.MediaStatus {
	rd, err := w.deps.Objects.GetObject(ctx, bucket, domain.MediaSidecarDir(key, info.VersionID)+domain.MediaStatusFile, object.GetObjectOptions{})
	if err != nil {
		return nil
	}
	defer func() { _ = rd.Close() }()
	data, err := io.ReadAll(io.LimitReader(rd, maxImageStatus+1))
	if err != nil || len(data) > maxImageStatus {
		return nil
	}
	var st domain.MediaStatus
	if json.Unmarshal(data, &st) != nil {
		return nil
	}
	if st.State != domain.MediaStateDone || len(st.Images) == 0 || strings.Trim(st.ETag, `"`) != strings.Trim(info.ETag, `"`) {
		return nil
	}
	return &st
}

// serveImage answers a GET or HEAD of key with a variant of the image
// (Images); ok is false when the original is to be served: ?format=
// original, not an image, no variant from the current content of the
// object, none in the format the request names, a variant missing.
//
// The variant is read through the object layer of the website (the
// cache of the distribution), as the object of the sidecar directory it
// is: the conditional requests, ranges and validators are those of that
// object, its entries are keyed by the format and the width the variant
// file names. The Cache-Control of the behavior, else of the image,
// applies to its variants.
func (w *website) serveImage(ctx context.Context, c *compiledDistribution, req WebRequest, b Behavior, key string) (*WebResponse, bool) {
	ireq, ok, qerr := ImageQuery(req.RawQuery)
	if !ok && qerr == nil {
		return nil, false
	}
	bucket := c.d.Origin.Bucket
	info, err := w.deps.Objects.GetObjectInfo(ctx, bucket, key, object.GetObjectOptions{})
	if err != nil || info.IsDeleteMarker {
		return nil, false
	}
	st := w.imageStatus(ctx, bucket, key, info)
	if st == nil {
		// Not an image with variants: its query is none of our business.
		return nil, false
	}
	if qerr != nil {
		return w.plain(http.StatusBadRequest, qerr.Error(), OutcomeError), true
	}
	ireq.Accept = req.Header.Get("Accept")
	v, err := media.SelectImage(st.Images, st.Alpha, ireq)
	if err != nil {
		return nil, false
	}
	if (b.Cache == nil || b.Cache.CacheControl == "") && info.CacheControl != "" {
		b.Cache = &Cache{CacheControl: info.CacheControl}
	}
	variant := domain.MediaSidecarDir(key, info.VersionID) + domain.MediaImageFile(v.Width, v.Format)
	resp, missing := w.readObject(ctx, c, req, b, variant, OutcomeImage)
	if missing {
		return nil, false
	}
	if ireq.Format == media.ImageFormatAuto {
		resp.Header.Add("Vary", "Accept")
	}
	switch resp.Status {
	case http.StatusOK, http.StatusPartialContent, http.StatusNotModified:
		if resp.Status != http.StatusNotModified {
			resp.Header.Set("Content-Type", domain.MediaImageContentType(v.Format))
		}
		resp.Header.Set("X-Content-Type-Options", "nosniff")
		resp.Header.Set(HeaderImageVariant, imageVariantName(v))
	}
	resp.Key = key
	return resp, true
}

// imageVariantName names a variant in HeaderImageVariant.
func imageVariantName(v domain.MediaImageOutput) string {
	if v.Width == 0 {
		return v.Format + " full"
	}
	return v.Format + " " + strconv.Itoa(v.Width)
}
