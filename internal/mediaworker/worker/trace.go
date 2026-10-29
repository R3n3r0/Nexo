package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/mediaworker/openh264"
	"github.com/r3n3r0/nexo/internal/mediaworker/transcode"
)

// TraceRequest asks which variants of the forensic watermark a leaked
// copy of a video carries.
type TraceRequest struct {
	Bucket, Key, VersionID string
	// Leak is the path of the copy: an MP4 file with H.264 video.
	Leak string
	// WorkDir receives the variants of the renditions (removed after).
	WorkDir string
	Options transcode.TraceOptions
}

// TraceResult are the votes of the pictures of the copy, for nexoctl
// media trace (the server reads the code from them).
type TraceResult struct {
	Bucket     string                `json:"bucket"`
	Key        string                `json:"key"`
	VersionID  string                `json:"versionId,omitempty"`
	Renditions []string              `json:"renditions"`
	Votes      []transcode.TraceVote `json:"votes"`
}

// statusLimit bounds the status document read.
const statusLimit = 4 << 20

// TraceLeak compares a leaked copy with the two variants of every
// rendition of the video marked by the forensic watermark.
func TraceLeak(ctx context.Context, store Store, lib *openh264.Library, req TraceRequest) (*TraceResult, error) {
	info, err := store.Head(ctx, req.Bucket, req.Key, req.VersionID)
	if err != nil {
		return nil, fmt.Errorf("%s/%s: %w", req.Bucket, req.Key, err)
	}
	sidecar := domain.MediaSidecarDir(req.Key, info.VersionID)
	data, _, err := store.Get(ctx, req.Bucket, sidecar+domain.MediaStatusFile, statusLimit)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%s/%s: the media worker never processed it", req.Bucket, req.Key)
	}
	if err != nil {
		return nil, err
	}
	var st domain.MediaStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s/%s: status: %w", req.Bucket, req.Key, err)
	}
	if st.State != domain.MediaStateDone || !sameETag(st.ETag, info.ETag) {
		return nil, fmt.Errorf("%s/%s: its outputs are not those of its current content", req.Bucket, req.Key)
	}
	dir, err := os.MkdirTemp(req.WorkDir, "trace-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	out := &TraceResult{Bucket: req.Bucket, Key: req.Key, VersionID: info.VersionID}
	var renditions []transcode.TraceRendition
	defer func() {
		for _, r := range renditions {
			_ = r.A.Close()
			_ = r.B.Close()
		}
	}()
	for _, r := range st.Renditions {
		if !r.Forensic {
			continue
		}
		var variants [2]*transcode.Source
		for i, variantB := range []bool{false, true} {
			name := domain.MediaRenditionFile(r.Name, variantB)
			path := filepath.Join(dir, name)
			if err := store.Download(ctx, req.Bucket, sidecar+domain.MediaRenditionsDir+name, "", path); err != nil {
				return nil, err
			}
			if variants[i], err = transcode.Open(path); err != nil {
				return nil, fmt.Errorf("rendition %s: %w", name, err)
			}
		}
		renditions = append(renditions, transcode.TraceRendition{Name: r.Name, A: variants[0], B: variants[1]})
		out.Renditions = append(out.Renditions, r.Name)
	}
	if len(renditions) == 0 {
		return nil, fmt.Errorf("%s/%s: no rendition with the forensic watermark (protection with forensic on, then reprocess)", req.Bucket, req.Key)
	}
	leak, err := transcode.Open(req.Leak)
	if err != nil {
		return nil, fmt.Errorf("%s: %w (an MP4 with H.264 video is expected)", req.Leak, err)
	}
	defer func() { _ = leak.Close() }()
	if out.Votes, err = transcode.Trace(ctx, lib, leak, renditions, req.Options); err != nil {
		return nil, err
	}
	return out, nil
}
