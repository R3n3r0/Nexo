package admin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// UploadManagerDeps are the dependencies of the upload manager.
type UploadManagerDeps struct {
	// Objects is the object layer (required).
	Objects object.Service
	Logger  *slog.Logger
}

// UploadManagerOption configures the upload manager.
type UploadManagerOption func(*uploadManager)

// WithMaxUploadSize sets the limit of an object stored in one request
// (default DefaultMaxUploadSize).
func WithMaxUploadSize(n int64) UploadManagerOption {
	return func(m *uploadManager) {
		if n > 0 {
			m.maxUpload = n
		}
	}
}

// WithMaxPartSize sets the limit of a proxied part (default
// DefaultMaxPartSize, at least MinPartSize).
func WithMaxPartSize(n int64) UploadManagerOption {
	return func(m *uploadManager) {
		if n >= MinPartSize {
			m.maxPart = n
		}
	}
}

type uploadManager struct {
	deps      UploadManagerDeps
	maxUpload int64
	maxPart   int64
}

var _ UploadManager = (*uploadManager)(nil)

// NewUploadManager returns the upload manager. A missing object layer is
// a wiring error (domain.ErrInvalidArgument).
func NewUploadManager(deps UploadManagerDeps, opts ...UploadManagerOption) (UploadManager, error) {
	if deps.Objects == nil {
		return nil, fmt.Errorf("%w: upload manager requires the object layer", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &uploadManager{deps: deps, maxUpload: DefaultMaxUploadSize, maxPart: DefaultMaxPartSize}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// limits returns the limits of the uploads.
func (m *uploadManager) limits() UploadLimits {
	return UploadLimits{
		MaxUploadSize: m.maxUpload,
		PartSize:      min(RecommendedPartSize, m.maxPart),
		MinPartSize:   MinPartSize,
		MaxPartSize:   m.maxPart,
		MaxParts:      MaxUploadParts,
	}
}

// limitedBody streams a body and fails, without buffering, once it
// exceeds its limit: the object layer then commits nothing.
type limitedBody struct {
	r        io.Reader
	left     int64
	exceeded bool
}

func newLimitedBody(r io.Reader, limit int64) *limitedBody { return &limitedBody{r: r, left: limit} }

// Read implements io.Reader.
func (l *limitedBody) Read(p []byte) (int, error) {
	if l.left <= 0 {
		var probe [1]byte
		n, err := l.r.Read(probe[:])
		if n > 0 {
			l.exceeded = true
			return 0, domain.ErrEntityTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}

// tooLarge is the error of a body over limit bytes.
func tooLarge(limit int64) error {
	return fmt.Errorf("%w: the body exceeds %d bytes", domain.ErrEntityTooLarge, limit)
}

// Upload implements UploadManager.
func (m *uploadManager) Upload(ctx context.Context, in object.PutObjectInput) (domain.ObjectInfo, error) {
	if in.Size > m.maxUpload {
		return domain.ObjectInfo{}, tooLarge(m.maxUpload)
	}
	if in.Reader == nil {
		in.Reader = bytes.NewReader(nil)
	}
	if in.ContentType == "" {
		in.ContentType = DefaultContentType
	}
	body := newLimitedBody(in.Reader, m.maxUpload)
	in.Reader = body
	info, err := m.deps.Objects.PutObject(ctx, in)
	if body.exceeded {
		return domain.ObjectInfo{}, tooLarge(m.maxUpload)
	}
	return info, err
}

// Create implements UploadManager.
func (m *uploadManager) Create(ctx context.Context, in object.NewMultipartInput) (*NewUpload, error) {
	if in.ContentType == "" {
		in.ContentType = DefaultContentType
	}
	up, err := m.deps.Objects.NewMultipartUpload(ctx, in)
	if err != nil {
		return nil, err
	}
	return &NewUpload{Upload: up, Limits: m.limits()}, nil
}

// uploadCursor is the position of an upload listing.
type uploadCursor struct {
	Key      string `json:"k"`
	UploadID string `json:"u"`
}

// List implements UploadManager.
func (m *uploadManager) List(ctx context.Context, in ListUploadsInput) (*Paged[domain.MultipartUpload], error) {
	limit, err := pageLimit(in.Page)
	if err != nil {
		return nil, err
	}
	var cur uploadCursor
	if in.Page.Cursor != "" {
		if err := decodeJSONCursor(in.Page.Cursor, &cur); err != nil {
			return nil, err
		}
	}
	res, err := m.deps.Objects.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{
		Bucket: in.Bucket, Prefix: in.Prefix, KeyMarker: cur.Key, UploadIDMarker: cur.UploadID, MaxUploads: limit,
	})
	if err != nil {
		return nil, err
	}
	out := &Paged[domain.MultipartUpload]{Items: res.Uploads}
	if out.Items == nil {
		out.Items = []domain.MultipartUpload{}
	}
	if res.IsTruncated {
		out.NextCursor = encodeJSONCursor(uploadCursor{Key: res.NextKeyMarker, UploadID: res.NextUploadIDMarker})
	}
	return out, nil
}

// Parts implements UploadManager.
func (m *uploadManager) Parts(ctx context.Context, in ListPartsInput) (*Paged[domain.Part], error) {
	limit, err := pageLimit(in.Page)
	if err != nil {
		return nil, err
	}
	marker := 0
	if in.Page.Cursor != "" {
		if err := decodeJSONCursor(in.Page.Cursor, &marker); err != nil {
			return nil, err
		}
		if marker < 0 {
			return nil, fmt.Errorf("%w: invalid cursor", domain.ErrInvalidArgument)
		}
	}
	res, err := m.deps.Objects.ListObjectParts(ctx, domain.ListPartsInput{
		Bucket: in.Upload.Bucket, Key: in.Upload.Key, UploadID: in.Upload.UploadID, PartNumberMarker: marker, MaxParts: limit,
	})
	if err != nil {
		return nil, err
	}
	out := &Paged[domain.Part]{Items: res.Parts}
	if out.Items == nil {
		out.Items = []domain.Part{}
	}
	if res.IsTruncated {
		out.NextCursor = encodeJSONCursor(res.NextPartNumberMarker)
	}
	return out, nil
}

// UploadPart implements UploadManager.
func (m *uploadManager) UploadPart(ctx context.Context, in object.PutObjectPartInput) (domain.Part, error) {
	if in.PartNumber < 1 || in.PartNumber > MaxUploadParts {
		return domain.Part{}, fmt.Errorf("%w: the part number must be between 1 and %d", domain.ErrInvalidArgument, MaxUploadParts)
	}
	if in.Size < 0 {
		return domain.Part{}, fmt.Errorf("%w: the size of a part is required (Content-Length)", domain.ErrInvalidArgument)
	}
	if in.Size > m.maxPart {
		return domain.Part{}, tooLarge(m.maxPart)
	}
	if in.Reader == nil {
		in.Reader = bytes.NewReader(nil)
	}
	body := newLimitedBody(in.Reader, m.maxPart)
	in.Reader = body
	part, err := m.deps.Objects.PutObjectPart(ctx, in)
	if body.exceeded {
		return domain.Part{}, tooLarge(m.maxPart)
	}
	return part, err
}

// Complete implements UploadManager.
func (m *uploadManager) Complete(ctx context.Context, in object.CompleteMultipartInput) (domain.ObjectInfo, error) {
	return m.deps.Objects.CompleteMultipartUpload(ctx, in)
}

// Abort implements UploadManager.
func (m *uploadManager) Abort(ctx context.Context, in UploadRef) error {
	return m.deps.Objects.AbortMultipartUpload(ctx, in.Bucket, in.Key, in.UploadID)
}
