package media

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/maypok86/otter/v2"
	"golang.org/x/sync/singleflight"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// Deps are the dependencies of the service.
type Deps struct {
	// Objects reads the objects (required).
	Objects object.Service
	// Protections returns the download protection of the buckets
	// (optional: no video is protected without it).
	Protections Protections
	Logger      *slog.Logger
}

type service struct {
	deps   Deps
	opts   options
	cache  *otter.Cache[string, *cached]
	flight singleflight.Group
}

// cached is a parsed presentation and its init segment, built once.
type cached struct {
	p        *presentation
	initOnce sync.Once
	init     []byte
	initErr  error
}

var _ Service = (*service)(nil)

// New returns the media service.
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.Objects == nil {
		return nil, fmt.Errorf("%w: the media service requires the object layer", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	s := &service{deps: deps, opts: defaultOptions()}
	for _, o := range opts {
		o(&s.opts)
	}
	cache, err := otter.New(&otter.Options[string, *cached]{
		// #nosec G115 -- cacheSize is a positive size; the weigher clamps to the uint32 range
		MaximumWeight: uint64(s.opts.cacheSize),
		Weigher: func(key string, v *cached) uint32 {
			return uint32(min(len(key)+v.p.weight+len(v.init), math.MaxUint32))
		},
	})
	if err != nil {
		return nil, fmt.Errorf("media: cache: %w", err)
	}
	s.cache = cache
	return s, nil
}

// load returns the parsed presentation of src, from the cache when the
// object did not change.
func (s *service) load(ctx context.Context, src Source) (*cached, error) {
	info, err := s.deps.Objects.GetObjectInfo(ctx, src.Bucket, src.Key, object.GetObjectOptions{VersionID: src.VersionID})
	if err != nil {
		return nil, err
	}
	if info.IsDeleteMarker {
		return nil, fmt.Errorf("%w: %s/%s is a delete marker", domain.ErrNotFound, src.Bucket, src.Key)
	}
	if info.Encryption.Type == domain.SSEC {
		return nil, fmt.Errorf("%w: SSE-C objects cannot be streamed", domain.ErrSSECustomerKeyRequired)
	}
	if src.Revision != "" && src.Revision != info.ETag {
		return nil, fmt.Errorf("%w: %s/%s changed (ETag %q, %q expected)", domain.ErrPreconditionFailed, src.Bucket, src.Key, info.ETag, src.Revision)
	}
	key := strings.Join([]string{src.Bucket, src.Key, info.VersionID, info.ETag}, "\x00")
	if c, ok := s.cache.GetIfPresent(key); ok {
		return c, nil
	}
	v, err, _ := s.flight.Do(key, func() (any, error) {
		if c, ok := s.cache.GetIfPresent(key); ok {
			return c, nil
		}
		// The parse must not fail because the first caller went away.
		pctx := context.WithoutCancel(ctx)
		moov, size, err := s.findMoov(pctx, info, src.Bucket, src.Key)
		if err != nil {
			return nil, err
		}
		p, err := newPresentation(info, moov, size, s.opts.targetSegment)
		if err != nil {
			return nil, err
		}
		c := &cached{p: p}
		s.cache.Set(key, c)
		return c, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*cached), nil
}

// Describe implements Service.
func (s *service) Describe(ctx context.Context, src Source) (*Presentation, error) {
	src, err := s.resolve(ctx, src)
	if err != nil {
		return nil, err
	}
	c, err := s.load(ctx, src)
	if err != nil {
		return nil, err
	}
	return c.p.describe(src), nil
}

// describe returns the exported description of p.
func (p *presentation) describe(src Source) *Presentation {
	video := p.tracks[0]
	out := &Presentation{
		Bucket: src.Bucket, Key: src.Key, VersionID: p.info.VersionID, Revision: p.info.ETag,
		Size: p.info.Size, Codecs: append([]string(nil), p.codecs...),
		Width: p.width, Height: p.height, FrameRate: p.frameRate, HasAudio: len(p.tracks) > 1,
		Segments: make([]time.Duration, len(p.segments)),
	}
	var bits, ticks float64
	for i, seg := range p.segments {
		d := seconds(seg.end-seg.start, video.timescale)
		out.Segments[i] = time.Duration(d * float64(time.Second))
		out.Duration += out.Segments[i]
		size := float64(seg.hi - seg.lo)
		if d > 0 {
			out.PeakBandwidth = max(out.PeakBandwidth, int64(math.Ceil(size*8/d)))
		}
		bits += size * 8
		ticks += d
	}
	if ticks > 0 {
		out.AverageBandwidth = int64(math.Ceil(bits / ticks))
	}
	return out
}

func seconds(ticks uint64, timescale uint32) float64 {
	return float64(ticks) / float64(timescale)
}

// MasterPlaylist implements Service.
func (s *service) MasterPlaylist(ctx context.Context, src Source, links Links) ([]byte, error) {
	if src.Rendition != "" {
		return nil, fmt.Errorf("%w: a rendition has no master playlist", domain.ErrInvalidArgument)
	}
	c, err := s.load(ctx, src)
	if err != nil {
		return nil, err
	}
	d := c.p.describe(src)
	var b bytes.Buffer
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	// The subtitle tracks that apply to this content: a rendition group
	// every variant refers to.
	tracks, err := s.currentTracks(ctx, src.Bucket, src.Key, domain.ObjectInfo{VersionID: d.VersionID, ETag: d.Revision})
	if err != nil {
		s.deps.Logger.WarnContext(ctx, "media: subtitle manifest unreadable", "bucket", src.Bucket, "key", src.Key, "err", err)
	}
	subtitleMedia(&b, tracks, links)
	subs := len(tracks) > 0
	prot, err := s.Protection(ctx, src)
	if err != nil {
		return nil, err
	}
	// A stream marked by the forensic watermark plays only the renditions
	// with both variants: the source is not marked.
	forensic := forensicViewer(src, prot)
	if !forensic {
		variant(&b, d, subs, links.Media(d.Revision))
	}
	// The renditions of the media worker, when they derive from this
	// content: one variant each.
	st, err := s.outputs(ctx, src.Bucket, src.Key, d.VersionID, d.Revision)
	if err != nil {
		s.deps.Logger.WarnContext(ctx, "media: status of the outputs unreadable", "bucket", src.Bucket, "key", src.Key, "err", err)
	}
	marked := 0
	if st != nil && st.Current {
		for _, r := range st.Renditions {
			if forensic && !r.Forensic {
				continue
			}
			rc, err := s.load(ctx, Source{Bucket: src.Bucket, Key: renditionKey(src.Key, d.VersionID, r.Name, false)})
			if err != nil {
				s.deps.Logger.WarnContext(ctx, "media: rendition unreadable", "bucket", src.Bucket, "key", src.Key, "rendition", r.Name, "err", err)
				continue
			}
			rd := rc.p.describe(src)
			variant(&b, rd, subs, links.Rendition(r.Name, rd.Revision))
			marked++
		}
	}
	if forensic && marked == 0 {
		return nil, errForensicNotReady(src)
	}
	return b.Bytes(), nil
}

// variant writes the EXT-X-STREAM-INF of a presentation and its URI.
// subs refers the variant to the subtitle group.
func variant(b *bytes.Buffer, d *Presentation, subs bool, uri string) {
	fmt.Fprintf(b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d,CODECS=%q,RESOLUTION=%dx%d",
		max(d.PeakBandwidth, 1), max(d.AverageBandwidth, 1), strings.Join(d.Codecs, ","), d.Width, d.Height)
	if d.FrameRate > 0 {
		fmt.Fprintf(b, ",FRAME-RATE=%.3f", d.FrameRate)
	}
	if subs {
		fmt.Fprintf(b, ",SUBTITLES=%q", subtitleGroup)
	}
	b.WriteString("\n" + uri + "\n")
}

// MediaPlaylist implements Service.
func (s *service) MediaPlaylist(ctx context.Context, src Source, links Links) ([]byte, error) {
	src, err := s.resolve(ctx, src)
	if err != nil {
		return nil, err
	}
	c, err := s.load(ctx, src)
	if err != nil {
		return nil, err
	}
	d := c.p.describe(src)
	var longest time.Duration
	for _, seg := range d.Segments {
		longest = max(longest, seg)
	}
	var b bytes.Buffer
	prot, err := s.Protection(ctx, src)
	if err != nil {
		return nil, err
	}
	if forensicViewer(src, prot) && src.variantB == "" {
		return nil, errForensicNotReady(src)
	}
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", max(int64(math.Ceil(longest.Seconds())), 1))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	// The init segment holds no picture: it stays clear, before the
	// first key.
	fmt.Fprintf(&b, "#EXT-X-MAP:URI=%q\n", links.Init(d.Revision))
	for i, seg := range d.Segments {
		// A session gets its keys over its channel: no key tag.
		if prot != nil && src.Session == "" && i%KeyPeriod == 0 {
			uri := links.Key(d.Revision, i/KeyPeriod)
			if uri == "" {
				return nil, fmt.Errorf("%w: %s/%s is a protected video: it plays with a playback token", domain.ErrAccessDenied, src.Bucket, src.Key)
			}
			// No IV: the media sequence number of each segment is its IV.
			fmt.Fprintf(&b, "#EXT-X-KEY:METHOD=AES-128,URI=%q\n", uri)
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", seg.Seconds(), links.Segment(d.Revision, i))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.Bytes(), nil
}

// Init implements Service.
func (s *service) Init(ctx context.Context, src Source) ([]byte, error) {
	src, err := s.resolve(ctx, src)
	if err != nil {
		return nil, err
	}
	c, err := s.load(ctx, src)
	if err != nil {
		return nil, err
	}
	c.initOnce.Do(func() { c.init, c.initErr = c.p.initSegment() })
	return c.init, c.initErr
}

// Segment implements Service.
func (s *service) Segment(ctx context.Context, src Source, n int) ([]byte, error) {
	src, err := s.resolve(ctx, src)
	if err != nil {
		return nil, err
	}
	c, err := s.load(ctx, src)
	if err != nil {
		return nil, err
	}
	p := c.p
	if n < 0 || n >= len(p.segments) {
		return nil, fmt.Errorf("%w: segment %d of %s/%s (%d segments)", domain.ErrNotFound, n, src.Bucket, src.Key, len(p.segments))
	}
	prot, err := s.Protection(ctx, src)
	if err != nil {
		return nil, err
	}
	// The forensic watermark: the variant the code of the viewer selects
	// for this segment (the keys stay those of the A variant).
	from, fromKey := p, src.Key
	if forensicViewer(src, prot) {
		if src.variantB == "" {
			return nil, errForensicNotReady(src)
		}
		if traceBit(s.traceCode(src.Viewer), n) == 1 {
			b := s.variantB(ctx, src, c)
			if b == nil {
				return nil, errForensicNotReady(src)
			}
			from, fromKey = b.p, src.variantB
		}
	}
	seg := from.segments[n]
	if seg.hi-seg.lo > s.opts.maxSegment {
		return nil, fmt.Errorf("%w: segment %d spans %d bytes, over %d", ErrUnsupported, n, seg.hi-seg.lo, s.opts.maxSegment)
	}
	data, err := s.readRange(ctx, from.info, src.Bucket, fromKey, seg.lo, seg.hi-seg.lo)
	if err != nil {
		return nil, err
	}
	out, err := from.mediaSegment(n, data)
	if err != nil {
		return nil, err
	}
	if prot == nil {
		return out, nil
	}
	if src.Session != "" {
		return sealSegment(s.periodKey(src, p.id(), n/KeyPeriod), n, out)
	}
	return encryptSegment(s.segmentKey(src.Bucket, src.Key, p.id(), n/KeyPeriod), segmentIV(n), out)
}
