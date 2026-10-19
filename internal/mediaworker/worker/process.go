package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/mediaworker/transcode"
)

// Output geometry.
const (
	posterMaxWidth = 1280
	tileWidth      = 160
	spriteColumns  = 10
	spriteName     = "sprite.jpg"
)

// sameETag compares two ETags, quoted or not.
func sameETag(a, b string) bool { return strings.Trim(a, `"`) == strings.Trim(b, `"`) }

// job is a process job being run.
type job struct {
	w       *Worker
	job     domain.MediaJob
	sidecar string
	status  domain.MediaStatus
}

// process derives the outputs of the rule from a version of a video.
func (w *Worker) process(ctx context.Context, mj domain.MediaJob) error {
	info, err := w.store.Head(ctx, mj.Bucket, mj.Key, mj.VersionID)
	if errors.Is(err, ErrNotFound) {
		return nil // removed meanwhile: its delete job cleans up
	}
	if err != nil {
		return err
	}
	if mj.ETag != "" && !sameETag(mj.ETag, info.ETag) {
		return nil // overwritten meanwhile: the job of the new content follows
	}
	version := mj.VersionID
	if version == "" {
		version = info.VersionID
	}
	j := &job{w: w, job: mj, sidecar: domain.MediaSidecarDir(mj.Key, version), status: domain.MediaStatus{
		State: domain.MediaStateProcessing, Rule: mj.Rule.ID, ETag: info.ETag, Worker: w.cfg.Name, Started: time.Now().UTC(),
	}}
	err = j.run(ctx, version)
	if err == nil || ctx.Err() != nil {
		return err
	}
	j.status.State, j.status.Error = domain.MediaStateFailed, err.Error()
	if serr := j.save(ctx); serr != nil {
		w.logger.Warn("worker: cannot write the failed status", "bucket", mj.Bucket, "key", mj.Key, "err", serr)
	}
	return err
}

func (j *job) step(ctx context.Context, step string) error {
	j.status.Step = step
	return j.save(ctx)
}

// save writes status.json.
func (j *job) save(ctx context.Context) error {
	j.status.Updated = time.Now().UTC()
	data, err := json.MarshalIndent(j.status, "", "  ")
	if err != nil {
		return err
	}
	return j.w.store.Put(ctx, j.job.Bucket, j.sidecar+domain.MediaStatusFile, data, "application/json")
}

func (j *job) put(ctx context.Context, name string, data []byte, contentType string) error {
	return j.w.store.Put(ctx, j.job.Bucket, j.sidecar+name, data, contentType)
}

func (j *job) run(ctx context.Context, version string) error {
	w, mj, rule := j.w, j.job, j.job.Rule
	if err := j.step(ctx, "download"); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(w.cfg.WorkDir, "job-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	srcPath := filepath.Join(dir, "source")
	if err := w.store.Download(ctx, mj.Bucket, mj.Key, version, srcPath); err != nil {
		return err
	}
	src, err := transcode.Open(srcPath)
	if err != nil {
		if errors.Is(err, transcode.ErrUnsupported) {
			return fmt.Errorf("%w: %w", errPermanent, err)
		}
		return err
	}
	defer func() { _ = src.Close() }()
	j.status.Duration, j.status.Width, j.status.Height = src.Seconds(), src.Width, src.Height
	produced := map[string]bool{domain.MediaStatusFile: true}

	// Renditions: those taller than the source are skipped.
	var specs []transcode.RenditionSpec
	for _, name := range rule.Renditions {
		p, ok := domain.MediaRenditionPreset(name)
		if ok && p.Height <= src.Height {
			specs = append(specs, transcode.RenditionSpec{Name: name, Height: p.Height, Bitrate: p.VideoBitrate})
		}
	}
	if len(specs) > 0 {
		if err := j.step(ctx, "renditions"); err != nil {
			return err
		}
		results, err := transcode.Renditions(ctx, w.lib, src, specs, dir, nil)
		if err != nil {
			return wrapTranscode(err)
		}
		for _, r := range results {
			key := domain.MediaRenditionsDir + r.Name + ".mp4"
			if err := w.store.Upload(ctx, mj.Bucket, j.sidecar+key, r.Path, "video/mp4"); err != nil {
				return err
			}
			_ = os.Remove(r.Path)
			produced[key] = true
			j.status.Renditions = append(j.status.Renditions, domain.MediaRenditionOutput{Name: r.Name, Width: r.Width, Height: r.Height, Bandwidth: r.Bandwidth})
		}
	}
	if rule.Poster {
		if err := j.step(ctx, "poster"); err != nil {
			return err
		}
		var b bytes.Buffer
		if _, _, err := transcode.Poster(ctx, w.lib, src, posterMaxWidth, &b); err != nil {
			return wrapTranscode(err)
		}
		if err := j.put(ctx, domain.MediaPosterFile, b.Bytes(), "image/jpeg"); err != nil {
			return err
		}
		produced[domain.MediaPosterFile] = true
		j.status.Poster = true
	}
	if rule.Thumbnails != nil {
		if err := j.step(ctx, "thumbnails"); err != nil {
			return err
		}
		var sprite, vtt bytes.Buffer
		res, err := transcode.Sprite(ctx, w.lib, src, rule.Thumbnails.IntervalDuration(), tileWidth, spriteColumns, spriteName, &sprite, &vtt, nil)
		if err != nil {
			return wrapTranscode(err)
		}
		if err := j.put(ctx, domain.MediaSpriteFile, sprite.Bytes(), "image/jpeg"); err != nil {
			return err
		}
		if err := j.put(ctx, domain.MediaThumbnailsFile, vtt.Bytes(), "text/vtt"); err != nil {
			return err
		}
		produced[domain.MediaSpriteFile], produced[domain.MediaThumbnailsFile] = true, true
		j.status.Thumbnails = &domain.MediaThumbnailsOutput{Interval: res.Interval, Width: res.Width, Height: res.Height, Columns: res.Columns, Count: res.Count}
	}
	j.status.State, j.status.Step = domain.MediaStateDone, ""
	if err := j.save(ctx); err != nil {
		return err
	}
	j.prune(ctx, produced)
	return nil
}

// prune removes the files of earlier runs the rule no longer produces and,
// in a versioned bucket, the versions the run replaced.
func (j *job) prune(ctx context.Context, produced map[string]bool) {
	versions, err := j.w.store.List(ctx, j.job.Bucket, j.sidecar)
	if err != nil {
		return
	}
	var stale []ObjectVersion
	for _, v := range versions {
		if !v.Latest || !produced[strings.TrimPrefix(v.Key, j.sidecar)] {
			stale = append(stale, v)
		}
	}
	slices.SortFunc(stale, func(a, b ObjectVersion) int {
		return strings.Compare(a.Key+"\x00"+a.VersionID, b.Key+"\x00"+b.VersionID)
	})
	if len(stale) > 0 {
		if err := j.w.store.Delete(ctx, j.job.Bucket, stale); err != nil {
			j.w.logger.Warn("worker: stale outputs not removed", "bucket", j.job.Bucket, "key", j.job.Key, "err", err)
		}
	}
}

// wrapTranscode marks the failures of an unsupported source permanent.
func wrapTranscode(err error) error {
	if errors.Is(err, transcode.ErrUnsupported) {
		return fmt.Errorf("%w: %w", errPermanent, err)
	}
	return err
}
