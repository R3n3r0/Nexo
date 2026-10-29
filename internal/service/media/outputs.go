package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// maxSidecar bounds the size of a sidecar file read into memory (the
// status, the poster, the sprite and the WebVTT map of the thumbnails).
const maxSidecar = 32 << 20

// sameETag compares two ETags, quoted or not.
func sameETag(a, b string) bool { return strings.Trim(a, `"`) == strings.Trim(b, `"`) }

// readSmall reads a whole object of at most maxSidecar bytes.
func (s *service) readSmall(ctx context.Context, bucket, key string) ([]byte, error) {
	r, err := s.deps.Objects.GetObject(ctx, bucket, key, object.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(io.LimitReader(r, maxSidecar+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSidecar {
		return nil, fmt.Errorf("%w: %s/%s is larger than %d bytes", domain.ErrInvalidArgument, bucket, key, maxSidecar)
	}
	return data, nil
}

// outputs reads the status of the worker for a version of a video whose
// content has the ETag etag: nil when the worker never processed it.
func (s *service) outputs(ctx context.Context, bucket, key, versionID, etag string) (*OutputStatus, error) {
	data, err := s.readSmall(ctx, bucket, domain.MediaSidecarDir(key, versionID)+domain.MediaStatusFile)
	if errors.Is(err, domain.ErrObjectNotFound) || errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := &OutputStatus{VersionID: versionID}
	if err := json.Unmarshal(data, &out.MediaStatus); err != nil {
		return nil, fmt.Errorf("media: status of %s/%s: %w", bucket, key, err)
	}
	out.Current = out.State == domain.MediaStateDone && sameETag(out.ETag, etag)
	return out, nil
}

// Status implements Service.
func (s *service) Status(ctx context.Context, src Source) (*OutputStatus, error) {
	info, err := s.deps.Objects.GetObjectInfo(ctx, src.Bucket, src.Key, object.GetObjectOptions{VersionID: src.VersionID})
	if err != nil {
		return nil, err
	}
	if info.IsDeleteMarker {
		return nil, fmt.Errorf("%w: %s/%s is a delete marker", domain.ErrNotFound, src.Bucket, src.Key)
	}
	return s.outputs(ctx, src.Bucket, src.Key, info.VersionID, info.ETag)
}

// current returns the status of the outputs of the version of src when
// they derive from its current content, domain.ErrNotFound otherwise.
func (s *service) current(ctx context.Context, src Source) (*OutputStatus, error) {
	st, err := s.Status(ctx, src)
	if err != nil {
		return nil, err
	}
	if st == nil || !st.Current {
		return nil, fmt.Errorf("%w: no media output for %s/%s", domain.ErrNotFound, src.Bucket, src.Key)
	}
	return st, nil
}

// Sidecar implements Service.
func (s *service) Sidecar(ctx context.Context, src Source, file string) ([]byte, error) {
	st, err := s.current(ctx, src)
	if err != nil {
		return nil, err
	}
	switch file {
	case domain.MediaPosterFile:
		if !st.Poster {
			return nil, fmt.Errorf("%w: no poster for %s/%s", domain.ErrNotFound, src.Bucket, src.Key)
		}
	case domain.MediaSpriteFile, domain.MediaThumbnailsFile:
		if st.Thumbnails == nil {
			return nil, fmt.Errorf("%w: no thumbnails for %s/%s", domain.ErrNotFound, src.Bucket, src.Key)
		}
	default:
		return nil, fmt.Errorf("%w: unknown media file %q", domain.ErrInvalidArgument, file)
	}
	return s.readSmall(ctx, src.Bucket, domain.MediaSidecarDir(src.Key, st.VersionID)+file)
}

// resolve returns the source of a rendition request: the rendition file
// of the worker, when it derives from the current content of the video.
func (s *service) resolve(ctx context.Context, src Source) (Source, error) {
	if src.Rendition == "" {
		return src, nil
	}
	if _, ok := domain.MediaRenditionPreset(src.Rendition); !ok {
		return Source{}, fmt.Errorf("%w: unknown rendition %q", domain.ErrInvalidArgument, src.Rendition)
	}
	st, err := s.current(ctx, Source{Bucket: src.Bucket, Key: src.Key, VersionID: src.VersionID})
	if err != nil {
		return Source{}, err
	}
	i := slices.IndexFunc(st.Renditions, func(r domain.MediaRenditionOutput) bool { return r.Name == src.Rendition })
	if i < 0 {
		return Source{}, fmt.Errorf("%w: no rendition %s for %s/%s", domain.ErrNotFound, src.Rendition, src.Bucket, src.Key)
	}
	out := Source{Bucket: src.Bucket, Key: renditionKey(src.Key, st.VersionID, src.Rendition, false), Revision: src.Revision,
		Session: src.Session, Viewer: src.Viewer}
	if st.Renditions[i].Forensic {
		out.variantB = renditionKey(src.Key, st.VersionID, src.Rendition, true)
	}
	return out, nil
}

// renditionKey is the key of the file of a rendition (variantB: of its B
// variant).
func renditionKey(key, versionID, name string, variantB bool) string {
	return domain.MediaSidecarDir(key, versionID) + domain.MediaRenditionsDir + domain.MediaRenditionFile(name, variantB)
}

// RewriteSprite returns a WebVTT map of thumbnails with its references to
// the sprite (the file name the worker wrote, before "#xywh=") replaced by
// uri: the routes serve the sprite at their own URI.
func RewriteSprite(vtt []byte, uri string) []byte {
	lines := strings.Split(string(vtt), "\n")
	for i, l := range lines {
		if before, after, ok := strings.Cut(l, "#xywh="); ok && !strings.Contains(before, "-->") {
			lines[i] = uri + "#xywh=" + after
		}
	}
	return []byte(strings.Join(lines, "\n"))
}
