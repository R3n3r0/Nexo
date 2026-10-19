package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"slices"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// ParseImageFormat returns the format an ImageRequest names: a
// domain.MediaImageFormats name ("jpg" is jpeg, the case is ignored) or
// ImageFormatAuto ("" too); domain.ErrInvalidArgument otherwise.
func ParseImageFormat(format string) (string, error) {
	f := strings.ToLower(strings.TrimSpace(format))
	switch {
	case f == "" || f == ImageFormatAuto:
		return ImageFormatAuto, nil
	case f == "jpg":
		return "jpeg", nil
	case slices.Contains(domain.MediaImageFormats(), f):
		return f, nil
	}
	return "", fmt.Errorf("%w: unknown image format %q (auto or one of %s)", domain.ErrInvalidArgument, format,
		strings.Join(domain.MediaImageFormats(), ", "))
}

// ParseImageWidth parses the width of an ImageRequest: "" is 0 (the image
// at its own size), otherwise a non-negative integer.
func ParseImageWidth(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	w, err := strconv.Atoi(s)
	if err != nil || w < 0 {
		return 0, fmt.Errorf("%w: w must be a width in pixels (a non-negative integer)", domain.ErrInvalidArgument)
	}
	return w, nil
}

// ImageAccepts reports whether a client sending the Accept header accept
// takes images of format (a domain.MediaImageFormats name). The most
// specific media range of the header decides, and q=0 refuses. AVIF and
// WebP are taken only when the header names them (image/avif,
// image/webp): "*/*" or "image/*" do not promise a decoder for them, as
// every browser that has one names them. JPEG and PNG are also taken by
// "image/*", "*/*" and a request without Accept.
func ImageAccepts(accept, format string) bool {
	explicitOnly := format == "avif" || format == "webp"
	if strings.TrimSpace(accept) == "" {
		return !explicitOnly
	}
	// Specificity: 3 the format itself, 2 image/*, 1 */*.
	best, q := 0, 0.0
	for _, part := range strings.Split(accept, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		spec := 0
		switch mt {
		case domain.MediaImageContentType(format):
			spec = 3
		case "image/jpg", "image/pjpeg":
			if format == "jpeg" {
				spec = 3
			}
		case "image/*":
			spec = 2
		case "*/*":
			spec = 1
		}
		if spec == 0 || spec < best || (explicitOnly && spec < 3) {
			continue
		}
		weight := 1.0
		if v, ok := params["q"]; ok {
			if weight, err = strconv.ParseFloat(v, 64); err != nil {
				continue
			}
		}
		if spec > best {
			best, q = spec, weight
		} else {
			q = max(q, weight)
		}
	}
	return best > 0 && q > 0
}

// imageOrder is the order in which format auto tries the formats: the most
// compact first, PNG before JPEG for an image with transparency (its JPEG
// variants are flattened).
func imageOrder(alpha bool) []string {
	if alpha {
		return []string{"avif", "webp", "png", "jpeg"}
	}
	return domain.MediaImageFormats()
}

// imageFallback is the order of the formats served to a client that
// accepts none of the variants: the most widely decoded first, PNG first
// for an image with transparency.
func imageFallback(alpha bool) []string {
	if alpha {
		return []string{"png", "jpeg", "webp", "avif"}
	}
	return []string{"jpeg", "png", "webp", "avif"}
}

// SelectImage selects the variant of an image a request asks for among
// the variants the worker produced:
//
//   - the format: the one req names, or with ImageFormatAuto the first
//     format of domain.MediaImageFormats (the most compact first: AVIF,
//     WebP, JPEG, PNG) produced and accepted by the client (ImageAccepts).
//     PNG comes after JPEG, so it is served only when the client asks for
//     it, does not take JPEG, or the image has no JPEG variant; for an
//     image with transparency (alpha) PNG comes before JPEG, whose
//     variants are flattened. A client that takes none of the formats
//     produced gets the most widely decoded one;
//   - the width, among the variants of that format: the image at its own
//     size when req.Width is 0, otherwise the narrowest variant at least
//     req.Width pixels wide, or the widest when none is.
//
// It fails with domain.ErrNotFound when no variant is in the format req
// names (or there is no variant at all), domain.ErrInvalidArgument for an
// unknown format or a negative width.
func SelectImage(variants []domain.MediaImageOutput, alpha bool, req ImageRequest) (domain.MediaImageOutput, error) {
	format, err := ParseImageFormat(req.Format)
	if err != nil {
		return domain.MediaImageOutput{}, err
	}
	if req.Width < 0 {
		return domain.MediaImageOutput{}, fmt.Errorf("%w: the width must not be negative", domain.ErrInvalidArgument)
	}
	has := func(f string) bool {
		return slices.ContainsFunc(variants, func(v domain.MediaImageOutput) bool { return v.Format == f })
	}
	if format == ImageFormatAuto {
		format = ""
		for _, f := range imageOrder(alpha) {
			if has(f) && ImageAccepts(req.Accept, f) {
				format = f
				break
			}
		}
		if format == "" {
			for _, f := range imageFallback(alpha) {
				if has(f) {
					format = f
					break
				}
			}
		}
	}
	if format == "" || !has(format) {
		if format == "" {
			return domain.MediaImageOutput{}, fmt.Errorf("%w: no image variant", domain.ErrNotFound)
		}
		return domain.MediaImageOutput{}, fmt.Errorf("%w: no %s variant of the image", domain.ErrNotFound, format)
	}
	px := func(v domain.MediaImageOutput) int {
		if v.PixelWidth > 0 {
			return v.PixelWidth
		}
		return v.Width
	}
	var (
		chosen       domain.MediaImageOutput
		found, wider bool
	)
	for _, v := range variants {
		if v.Format != format {
			continue
		}
		switch {
		case !found:
			chosen, found, wider = v, true, req.Width > 0 && px(v) >= req.Width
		case req.Width == 0:
			// The image at its own size, else the widest.
			if chosen.Width != 0 && (v.Width == 0 || px(v) > px(chosen)) {
				chosen = v
			}
		case px(v) >= req.Width:
			// The narrowest wide enough.
			if !wider || px(v) < px(chosen) {
				chosen, wider = v, true
			}
		case !wider && px(v) > px(chosen):
			// None wide enough yet: the widest.
			chosen = v
		}
	}
	return chosen, nil
}

// Image implements Service.
func (s *service) Image(ctx context.Context, src Source, req ImageRequest) (*Image, error) {
	if _, err := ParseImageFormat(req.Format); err != nil {
		return nil, err
	}
	st, err := s.current(ctx, src)
	if err != nil {
		return nil, err
	}
	if len(st.Images) == 0 {
		return nil, fmt.Errorf("%w: no image variant for %s/%s", domain.ErrNotFound, src.Bucket, src.Key)
	}
	v, err := SelectImage(st.Images, st.Alpha, req)
	if err != nil {
		return nil, fmt.Errorf("%s/%s: %w", src.Bucket, src.Key, err)
	}
	key := domain.MediaSidecarDir(src.Key, st.VersionID) + domain.MediaImageFile(v.Width, v.Format)
	info, err := s.deps.Objects.GetObjectInfo(ctx, src.Bucket, key, object.GetObjectOptions{})
	if errors.Is(err, domain.ErrObjectNotFound) {
		return nil, fmt.Errorf("%w: the %s variant of %s/%s is missing", domain.ErrNotFound, domain.MediaImageFile(v.Width, v.Format), src.Bucket, src.Key)
	}
	if err != nil {
		return nil, err
	}
	if info.IsDeleteMarker {
		return nil, fmt.Errorf("%w: the %s variant of %s/%s is deleted", domain.ErrNotFound, domain.MediaImageFile(v.Width, v.Format), src.Bucket, src.Key)
	}
	v.Size = info.Size
	return &Image{
		MediaImageOutput: v,
		VersionID:        st.VersionID,
		ContentType:      domain.MediaImageContentType(v.Format),
		ETag:             fmt.Sprintf(`"%s-%s-%d"`, strings.Trim(st.ETag, `"`), v.Format, v.Width),
		Modified:         st.Updated,
		Content:          &objectContent{ctx: ctx, objects: s.deps.Objects, bucket: src.Bucket, key: key, versionID: info.VersionID, size: info.Size},
	}, nil
}

// objectContent reads an object as an io.ReadSeeker: every read after a
// seek opens the object at the new offset, so that the routes serve
// ranges and conditional requests (http.ServeContent) without holding the
// object in memory.
type objectContent struct {
	ctx         context.Context
	objects     object.Service
	bucket, key string
	versionID   string
	size        int64
	pos         int64
	body        io.ReadCloser
	bodyPos     int64
	closed      bool
}

var _ io.ReadSeekCloser = (*objectContent)(nil)

// Read implements io.Reader.
func (c *objectContent) Read(p []byte) (int, error) {
	if c.closed {
		return 0, fmt.Errorf("media: read of a closed image")
	}
	if c.body != nil && c.bodyPos != c.pos {
		_ = c.body.Close()
		c.body = nil
	}
	if c.pos >= c.size {
		return 0, io.EOF
	}
	if c.body == nil {
		opts := object.GetObjectOptions{VersionID: c.versionID}
		if c.pos > 0 {
			opts.Range = &domain.Range{Start: c.pos, End: -1}
		}
		r, err := c.objects.GetObject(c.ctx, c.bucket, c.key, opts)
		if err != nil {
			return 0, err
		}
		c.body, c.bodyPos = r, c.pos
	}
	n, err := c.body.Read(p)
	c.pos += int64(n)
	c.bodyPos += int64(n)
	return n, err
}

// Seek implements io.Seeker.
func (c *objectContent) Seek(offset int64, whence int) (int64, error) {
	pos := offset
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		pos += c.pos
	case io.SeekEnd:
		pos += c.size
	default:
		return 0, fmt.Errorf("media: seek: invalid whence %d", whence)
	}
	if pos < 0 {
		return 0, fmt.Errorf("media: seek: negative position %d", pos)
	}
	c.pos = pos
	return pos, nil
}

// Close implements io.Closer.
func (c *objectContent) Close() error {
	c.closed = true
	if c.body == nil {
		return nil
	}
	err := c.body.Close()
	c.body = nil
	return err
}
