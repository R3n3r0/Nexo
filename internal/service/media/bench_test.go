package media_test

import (
	"context"
	"testing"

	"github.com/r3n3r0/nexo/internal/service/media"
)

// Benchmarks of the packager: what a player costs the server, per request.
// The index of a video is cached by ETag: BenchmarkDescribe/cold measures
// the first request of a version, the others the requests that follow.

func BenchmarkDescribe(b *testing.B) {
	s, objects := newStore(b, "h264_aac.mp4")
	src := media.Source{Bucket: "videos", Key: "h264_aac.mp4"}
	b.Run("cold", func(b *testing.B) {
		b.SetBytes(int64(len(s.objects[src.Key])))
		for b.Loop() {
			svc := newService(b, objects) // a new service: an empty index cache
			if _, err := svc.Describe(context.Background(), src); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("cached", func(b *testing.B) {
		svc := newService(b, objects)
		for b.Loop() {
			if _, err := svc.Describe(context.Background(), src); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkPlaylists(b *testing.B) {
	_, objects := newStore(b, "h264_aac.mp4")
	svc := newService(b, objects)
	src := media.Source{Bucket: "videos", Key: "h264_aac.mp4"}
	b.Run("master", func(b *testing.B) {
		for b.Loop() {
			if _, err := svc.MasterPlaylist(context.Background(), src, links{}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("media", func(b *testing.B) {
		for b.Loop() {
			if _, err := svc.MediaPlaylist(context.Background(), src, links{}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkSegment(b *testing.B) {
	_, objects := newStore(b, "h264_aac.mp4")
	svc := newService(b, objects)
	src := media.Source{Bucket: "videos", Key: "h264_aac.mp4"}
	seg, err := svc.Segment(context.Background(), src, 1)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("init", func(b *testing.B) {
		for b.Loop() {
			if _, err := svc.Init(context.Background(), src); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("media", func(b *testing.B) {
		b.SetBytes(int64(len(seg)))
		for b.Loop() {
			if _, err := svc.Segment(context.Background(), src, 1); err != nil {
				b.Fatal(err)
			}
		}
	})
}
