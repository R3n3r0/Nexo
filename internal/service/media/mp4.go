package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// presentation is a parsed MP4: its tracks and the plan of its segments.
type presentation struct {
	info   domain.ObjectInfo
	tracks []*track
	// timescale of the movie (mvhd).
	timescale uint32
	segments  []segment
	codecs    []string
	width     int
	height    int
	frameRate float64
	// weight is the memory the presentation holds, roughly.
	weight int
}

// track is a video or audio track of the presentation.
type track struct {
	trak      *mp4.TrakBox
	kind      string // "video" or "audio"
	timescale uint32
	samples   uint32
	chunkOff  func(chunkNr int) (uint64, error)
	// outID is the track ID in the fragmented presentation.
	outID uint32
}

// segment is one media segment: per track, the samples first..last
// (1-based, inclusive; last < first for none), and the byte span of the
// object holding them.
type segment struct {
	first, last []uint32
	// start and end are in the timescale of the video track.
	start, end uint64
	// lo and hi bound the bytes of the samples: [lo, hi).
	lo, hi int64
}

// readRange reads [off, off+n) of the object.
func (s *service) readRange(ctx context.Context, info domain.ObjectInfo, bucket, key string, off, n int64) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	r, err := s.deps.Objects.GetObject(ctx, bucket, key, object.GetObjectOptions{
		VersionID:  info.VersionID,
		Range:      &domain.Range{Start: off, End: off + n - 1},
		Conditions: domain.Conditions{IfMatch: info.ETag},
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("media: read %d bytes at %d of %s/%s: %w", n, off, bucket, key, err)
	}
	return buf, nil
}

// findMoov walks the top-level boxes and returns the moov box.
func (s *service) findMoov(ctx context.Context, info domain.ObjectInfo, bucket, key string) (*mp4.MoovBox, int, error) {
	var off int64
	for i := 0; i < maxTopLevelBoxes && off+8 <= info.Size; i++ {
		head, err := s.readRange(ctx, info, bucket, key, off, min(16, info.Size-off))
		if err != nil {
			return nil, 0, err
		}
		size := int64(binary.BigEndian.Uint32(head[0:4]))
		typ := string(head[4:8])
		hdr := int64(8)
		switch size {
		case 1:
			if len(head) < 16 {
				return nil, 0, fmt.Errorf("%w: truncated box header at %d", ErrUnsupported, off)
			}
			large := binary.BigEndian.Uint64(head[8:16])
			if large > math.MaxInt64 {
				return nil, 0, fmt.Errorf("%w: box size out of range at %d", ErrUnsupported, off)
			}
			size, hdr = int64(large), 16
		case 0:
			size = info.Size - off
		}
		if size < hdr || size > info.Size-off {
			return nil, 0, fmt.Errorf("%w: invalid %q box at %d", ErrUnsupported, typ, off)
		}
		if i == 0 && typ != "ftyp" {
			return nil, 0, fmt.Errorf("%w: not an MP4 file", ErrUnsupported)
		}
		switch typ {
		case "moov":
			if size > s.opts.maxIndex {
				return nil, 0, fmt.Errorf("%w: the moov box (%d bytes) is over %d bytes", ErrUnsupported, size, s.opts.maxIndex)
			}
			data, err := s.readRange(ctx, info, bucket, key, off, size)
			if err != nil {
				return nil, 0, err
			}
			box, err := decodeBox(data)
			if err != nil {
				return nil, 0, fmt.Errorf("%w: moov: %v", ErrUnsupported, err)
			}
			moov, ok := box.(*mp4.MoovBox)
			if !ok {
				return nil, 0, fmt.Errorf("%w: moov box expected", ErrUnsupported)
			}
			return moov, len(data), nil
		case "moof":
			return nil, 0, fmt.Errorf("%w: fragmented MP4 is not supported", ErrUnsupported)
		}
		off += size
	}
	return nil, 0, fmt.Errorf("%w: no moov box", ErrUnsupported)
}

// decodeBox decodes one box, turning the panics of malformed input into
// errors.
func decodeBox(data []byte) (box mp4.Box, err error) {
	defer func() {
		if r := recover(); r != nil {
			box, err = nil, fmt.Errorf("malformed box: %v", r)
		}
	}()
	return mp4.DecodeBox(0, bytes.NewReader(data))
}

// newPresentation parses moov and plans the segments of the presentation.
func newPresentation(info domain.ObjectInfo, moov *mp4.MoovBox, size int, target time.Duration) (p *presentation, err error) {
	defer func() {
		if r := recover(); r != nil {
			p, err = nil, fmt.Errorf("%w: malformed sample tables: %v", ErrUnsupported, r)
		}
	}()
	if moov.Mvex != nil {
		return nil, fmt.Errorf("%w: fragmented MP4 is not supported", ErrUnsupported)
	}
	if moov.Mvhd == nil {
		return nil, fmt.Errorf("%w: no mvhd box", ErrUnsupported)
	}
	p = &presentation{info: info, timescale: moov.Mvhd.Timescale, weight: size}
	var video, audio *track
	for _, trak := range moov.Traks {
		if trak.Mdia == nil || trak.Mdia.Hdlr == nil || trak.Mdia.Mdhd == nil || trak.Mdia.Minf == nil || trak.Mdia.Minf.Stbl == nil {
			continue
		}
		stbl := trak.Mdia.Minf.Stbl
		if stbl.Stsd == nil || stbl.Stsz == nil || stbl.Stsc == nil || stbl.Stts == nil || trak.Mdia.Mdhd.Timescale == 0 {
			continue
		}
		t := &track{trak: trak, timescale: trak.Mdia.Mdhd.Timescale, samples: stbl.Stsz.SampleNumber}
		switch {
		case stbl.Stco != nil:
			t.chunkOff = stbl.Stco.GetOffset
		case stbl.Co64 != nil:
			t.chunkOff = stbl.Co64.GetOffset
		default:
			continue
		}
		if t.samples == 0 || t.samples > maxSamples || !samplesFit(stbl.Stsz, info.Size) {
			continue
		}
		switch trak.Mdia.Hdlr.HandlerType {
		case "vide":
			if video == nil && (stbl.Stsd.AvcX != nil || stbl.Stsd.HvcX != nil) {
				t.kind = "video"
				video = t
			}
		case "soun":
			if audio == nil && stbl.Stsd.Mp4a != nil {
				t.kind = "audio"
				audio = t
			}
		}
	}
	if video == nil {
		return nil, fmt.Errorf("%w: no H.264 or H.265 video track", ErrUnsupported)
	}
	p.tracks = append(p.tracks, video)
	if audio != nil {
		p.tracks = append(p.tracks, audio)
	}
	for i, t := range p.tracks {
		// The track IDs AddEmptyTrack assigns in initSegment.
		t.outID = uint32(i + 1) // #nosec G115 -- at most two tracks
	}
	if err := p.codecInfo(video, audio); err != nil {
		return nil, err
	}
	if err := p.plan(video, uint64(target.Seconds()*float64(video.timescale))); err != nil {
		return nil, err
	}
	return p, nil
}

// codecInfo fills the codecs, the dimensions and the frame rate.
func (p *presentation) codecInfo(video, audio *track) error {
	stsd := video.trak.Mdia.Minf.Stbl.Stsd
	switch {
	case stsd.AvcX != nil:
		entry := stsd.AvcX
		p.width, p.height = int(entry.Width), int(entry.Height)
		codec := "avc1.640028"
		if entry.AvcC != nil && len(entry.AvcC.SPSnalus) > 0 {
			if sps, err := avc.ParseSPSNALUnit(entry.AvcC.SPSnalus[0], false); err == nil {
				codec = avc.CodecString("avc1", sps)
			}
		}
		p.codecs = append(p.codecs, codec)
	case stsd.HvcX != nil:
		entry := stsd.HvcX
		p.width, p.height = int(entry.Width), int(entry.Height)
		codec := "hvc1.1.6.L120.B0"
		if entry.HvcC != nil {
			if nalus := entry.HvcC.GetNalusForType(hevc.NALU_SPS); len(nalus) > 0 {
				if sps, err := hevc.ParseSPSNALUnit(nalus[0]); err == nil {
					codec = hevc.CodecString("hvc1", sps)
				}
			}
		}
		p.codecs = append(p.codecs, codec)
	}
	if d := video.trak.Mdia.Mdhd.Duration; d > 0 {
		p.frameRate = float64(video.samples) * float64(video.timescale) / float64(d)
	}
	if audio != nil {
		codec := "mp4a.40.2"
		if e := audio.trak.Mdia.Minf.Stbl.Stsd.Mp4a.Esds; e != nil && e.DecConfigDescriptor != nil {
			if dsi := e.DecConfigDescriptor.DecSpecificInfo; dsi != nil {
				if asc, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(dsi.DecConfig)); err == nil {
					codec = fmt.Sprintf("mp4a.40.%d", asc.ObjectType)
				}
			}
		}
		p.codecs = append(p.codecs, codec)
	}
	return nil
}

// plan cuts the segments at the first sync sample of the video whose
// presentation time is at or after each multiple of target (in the video
// timescale), and assigns the samples of the other tracks by decode time.
// Times are presentation times shifted so that the video starts at 0, as
// its edit list does: with B-frames a sync sample is decoded before it is
// shown, and cutting by decode time would skip keyframes.
func (p *presentation) plan(video *track, target uint64) error {
	stbl := video.trak.Mdia.Minf.Stbl
	type cut struct {
		nr  uint32
		pts int64
	}
	var cuts []cut
	var shift, end, next int64
	vc := newTimes(stbl.Stts, 1)
	for nr := uint32(1); nr <= video.samples; nr++ {
		dt, dur, err := vc.next()
		if err != nil {
			return fmt.Errorf("%w: video sample %d: %v", ErrUnsupported, nr, err)
		}
		if dt > math.MaxInt64/2 {
			return fmt.Errorf("%w: video sample %d: decode time out of range", ErrUnsupported, nr)
		}
		pts := int64(dt)
		if stbl.Ctts != nil {
			pts += int64(stbl.Ctts.GetCompositionTimeOffset(nr))
		}
		if nr == 1 {
			shift = pts
		}
		end = max(end, pts+int64(dur))
		if stbl.Stss != nil && !stbl.Stss.IsSyncSample(nr) {
			continue
		}
		switch {
		case len(cuts) == 0:
			cuts = append(cuts, cut{nr: nr})
			next = int64(target)
		case pts-shift >= next:
			if len(cuts) == maxSegments {
				return fmt.Errorf("%w: more than %d segments", ErrUnsupported, maxSegments)
			}
			cuts = append(cuts, cut{nr: nr, pts: pts - shift})
			next = pts - shift + int64(target)
		}
	}
	if len(cuts) == 0 {
		return fmt.Errorf("%w: no sync sample", ErrUnsupported)
	}
	end -= shift
	p.segments = make([]segment, len(cuts))
	for i := range p.segments {
		seg := &p.segments[i]
		seg.first = make([]uint32, len(p.tracks))
		seg.last = make([]uint32, len(p.tracks))
		seg.start = uint64(max(cuts[i].pts, 0))
		seg.end = uint64(max(end, cuts[i].pts, 0))
		if i+1 < len(cuts) {
			seg.end = uint64(max(cuts[i+1].pts, 0))
		}
		seg.lo, seg.hi = math.MaxInt64, -1
		// The video samples in decode order, from a sync sample to the
		// next; the ones before the first sync sample go to the first
		// segment.
		seg.first[0] = cuts[i].nr
		if i == 0 {
			seg.first[0] = 1
		}
		seg.last[0] = video.samples
		if i+1 < len(cuts) {
			seg.last[0] = cuts[i+1].nr - 1
		}
	}
	// The other tracks by decode time; their samples before the first
	// segment go to the first segment.
	for ti, t := range p.tracks {
		if ti == 0 {
			continue
		}
		tstbl := t.trak.Mdia.Minf.Stbl
		si := 0
		p.segments[0].first[ti] = 1
		tc := newTimes(tstbl.Stts, 1)
		for nr := uint32(1); nr <= t.samples; nr++ {
			dt, _, err := tc.next()
			if err != nil {
				return fmt.Errorf("%w: %s sample %d: %v", ErrUnsupported, t.kind, nr, err)
			}
			vt := rescale(dt, t.timescale, video.timescale)
			for si+1 < len(p.segments) && vt >= p.segments[si+1].start {
				p.segments[si].last[ti] = nr - 1
				si++
				p.segments[si].first[ti] = nr
			}
		}
		p.segments[si].last[ti] = t.samples
		for k := si + 1; k < len(p.segments); k++ {
			p.segments[k].first[ti], p.segments[k].last[ti] = t.samples+1, t.samples
		}
	}
	for i := range p.segments {
		seg := &p.segments[i]
		for ti, t := range p.tracks {
			if seg.last[ti] < seg.first[ti] {
				continue
			}
			ranges, err := t.ranges(seg.first[ti], seg.last[ti])
			if err != nil {
				return err
			}
			for _, r := range ranges {
				seg.lo = min(seg.lo, r.off)
				seg.hi = max(seg.hi, r.off+r.size)
			}
		}
		if seg.hi < 0 || seg.hi > p.info.Size {
			return fmt.Errorf("%w: segment %d has no data in the object", ErrUnsupported, i)
		}
	}
	p.weight += len(p.segments) * (64 + 8*len(p.tracks))
	return nil
}

// samplesFit reports sample sizes that fit in an object of size bytes:
// a few bytes of sample table cannot describe more work than the object
// holds (a uniform size declares any number of samples).
func samplesFit(stsz *mp4.StszBox, size int64) bool {
	n := uint64(stsz.SampleNumber)
	if stsz.SampleUniformSize > 0 {
		return n*uint64(stsz.SampleUniformSize) <= uint64(size)
	}
	if uint64(len(stsz.SampleSize)) < n {
		return false
	}
	var total uint64
	for _, s := range stsz.SampleSize[:n] {
		total += uint64(s)
		if total > uint64(size) {
			return false
		}
	}
	return true
}

// rescale converts t from timescale from to timescale to.
func rescale(t uint64, from, to uint32) uint64 {
	if from == to {
		return t
	}
	hi, lo := mulDiv(t, uint64(to), uint64(from))
	if hi {
		return math.MaxUint64
	}
	return lo
}

// mulDiv returns a*b/c, reporting an overflow of the result.
func mulDiv(a, b, c uint64) (overflow bool, q uint64) {
	if a == 0 || b == 0 {
		return false, 0
	}
	if a > math.MaxUint64/b {
		return true, 0
	}
	return false, a * b / c
}

// byteRange is a range of bytes of the object.
type byteRange struct {
	off, size int64
}

// ranges returns the byte ranges of samples first..last of the track, in
// sample order (adjacent chunks merged).
func (t *track) ranges(first, last uint32) ([]byteRange, error) {
	stbl := t.trak.Mdia.Minf.Stbl
	chunks, err := stbl.Stsc.GetContainingChunks(first, last)
	if err != nil {
		return nil, fmt.Errorf("%w: %s samples %d-%d: %v", ErrUnsupported, t.kind, first, last, err)
	}
	var out []byteRange
	for i, c := range chunks {
		off, err := t.chunkOff(int(c.ChunkNr))
		if err != nil {
			return nil, fmt.Errorf("%w: %s chunk %d: %v", ErrUnsupported, t.kind, c.ChunkNr, err)
		}
		from, to := c.StartSampleNr, c.StartSampleNr+c.NrSamples-1
		if i == 0 && first > from {
			skipped, err := stbl.Stsz.GetTotalSampleSize(from, first-1)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
			}
			off += skipped
			from = first
		}
		if i == len(chunks)-1 {
			to = last
		}
		size, err := stbl.Stsz.GetTotalSampleSize(from, to)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		if off > math.MaxInt64 || size > math.MaxInt64-off {
			return nil, fmt.Errorf("%w: sample data out of range", ErrUnsupported)
		}
		r := byteRange{off: int64(off), size: int64(size)}
		if n := len(out); n > 0 && out[n-1].off+out[n-1].size == r.off {
			out[n-1].size += r.size
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// init builds the initialization segment.
func (p *presentation) initSegment() ([]byte, error) {
	init := mp4.CreateEmptyInit()
	init.Moov.Mvhd.Timescale = p.timescale
	for _, t := range p.tracks {
		out := init.AddEmptyTrack(t.timescale, t.kind, t.trak.Mdia.Mdhd.GetLanguage())
		if out.Tkhd.TrackID != t.outID {
			return nil, fmt.Errorf("media: init segment: track ID %d, %d expected", out.Tkhd.TrackID, t.outID)
		}
		if t.trak.Edts != nil {
			// The edit list keeps the tracks in sync (the B-frame delay
			// of the video).
			out.AddChild(t.trak.Edts)
		}
		stsd := t.trak.Mdia.Minf.Stbl.Stsd
		switch t.kind {
		case "video":
			if stsd.AvcX != nil {
				out.Mdia.Minf.Stbl.Stsd.AddChild(stsd.AvcX)
			} else {
				out.Mdia.Minf.Stbl.Stsd.AddChild(stsd.HvcX)
			}
			out.Tkhd.Width, out.Tkhd.Height = t.trak.Tkhd.Width, t.trak.Tkhd.Height
		case "audio":
			out.Mdia.Minf.Stbl.Stsd.AddChild(stsd.Mp4a)
		}
	}
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return nil, fmt.Errorf("media: encode the init segment: %w", err)
	}
	return buf.Bytes(), nil
}

// mediaSegment builds segment n from data, the bytes [seg.lo, seg.hi) of
// the object.
func (p *presentation) mediaSegment(n int, data []byte) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("%w: malformed sample tables: %v", ErrUnsupported, r)
		}
	}()
	seg := p.segments[n]
	ids := make([]uint32, len(p.tracks))
	for i, t := range p.tracks {
		ids[i] = t.outID
	}
	// #nosec G115 -- n is a segment index, far below the uint32 range
	frag, err := mp4.CreateMultiTrackFragment(uint32(n+1), ids)
	if err != nil {
		return nil, fmt.Errorf("media: segment %d: %w", n, err)
	}
	for ti, t := range p.tracks {
		if seg.last[ti] < seg.first[ti] {
			continue
		}
		samples, err := t.fullSamples(seg.first[ti], seg.last[ti], data, seg.lo)
		if err != nil {
			return nil, err
		}
		for _, s := range samples {
			if err := frag.AddFullSampleToTrack(s, t.outID); err != nil {
				return nil, fmt.Errorf("media: segment %d: %w", n, err)
			}
		}
	}
	var buf bytes.Buffer
	if err := frag.Encode(&buf); err != nil {
		return nil, fmt.Errorf("media: encode segment %d: %w", n, err)
	}
	return buf.Bytes(), nil
}

// fullSamples returns samples first..last with their data, read from
// data, the bytes of the object from offset base.
func (t *track) fullSamples(first, last uint32, data []byte, base int64) ([]mp4.FullSample, error) {
	stbl := t.trak.Mdia.Minf.Stbl
	ranges, err := t.ranges(first, last)
	if err != nil {
		return nil, err
	}
	var payload []byte
	for _, r := range ranges {
		lo, hi := r.off-base, r.off-base+r.size
		if lo < 0 || hi > int64(len(data)) {
			return nil, fmt.Errorf("%w: %s samples outside the segment data", ErrUnsupported, t.kind)
		}
		if len(ranges) == 1 {
			payload = data[lo:hi]
			break
		}
		payload = append(payload, data[lo:hi]...)
	}
	out := make([]mp4.FullSample, 0, last-first+1)
	var pos uint64
	times := newTimes(stbl.Stts, first)
	for nr := first; nr <= last; nr++ {
		size := uint64(stbl.Stsz.GetSampleSize(int(nr)))
		if pos+size > uint64(len(payload)) {
			return nil, fmt.Errorf("%w: %s sample %d outside its chunk", ErrUnsupported, t.kind, nr)
		}
		dt, dur, err := times.next()
		if err != nil {
			return nil, fmt.Errorf("%w: %s sample %d: %v", ErrUnsupported, t.kind, nr, err)
		}
		var cto int32
		if stbl.Ctts != nil {
			cto = stbl.Ctts.GetCompositionTimeOffset(nr)
		}
		var flags mp4.SampleFlags
		if stbl.Stss != nil {
			flags.SampleIsNonSync = !stbl.Stss.IsSyncSample(nr)
		}
		if !flags.SampleIsNonSync {
			// Does not depend on other samples (an I picture, or audio).
			flags.SampleDependsOn = 2
		}
		out = append(out, mp4.FullSample{
			Sample:     mp4.Sample{Flags: flags.Encode(), Size: uint32(size), Dur: dur, CompositionTimeOffset: cto},
			DecodeTime: dt,
			Data:       payload[pos : pos+size],
		})
		pos += size
	}
	return out, nil
}

// times walks the decode times of the samples of an stts box in order:
// SttsBox.GetDecodeTime scans the entries from the start at every call,
// quadratic on variable frame rate tracks with an entry per sample.
type times struct {
	stts    *mp4.SttsBox
	entry   int    // current entry
	used    uint32 // samples of the entry already returned
	decTime uint64 // decode time of the next sample
}

// newTimes returns the walk from sample nr (1-based).
func newTimes(stts *mp4.SttsBox, nr uint32) *times {
	t := &times{stts: stts}
	skip := nr - 1
	for t.entry < len(stts.SampleCount) && t.entry < len(stts.SampleTimeDelta) && skip > 0 {
		n := min(skip, stts.SampleCount[t.entry])
		t.decTime += uint64(n) * uint64(stts.SampleTimeDelta[t.entry])
		skip -= n
		if n == stts.SampleCount[t.entry] {
			t.entry++
		} else {
			t.used = n
		}
	}
	return t
}

// next returns the decode time and the duration of the next sample.
func (t *times) next() (uint64, uint32, error) {
	for t.entry < len(t.stts.SampleCount) && t.used >= t.stts.SampleCount[t.entry] {
		t.entry++
		t.used = 0
	}
	if t.entry >= len(t.stts.SampleCount) || t.entry >= len(t.stts.SampleTimeDelta) {
		return 0, 0, fmt.Errorf("stts entries cover fewer samples than the track")
	}
	dur := t.stts.SampleTimeDelta[t.entry]
	dt := t.decTime
	t.decTime += uint64(dur)
	t.used++
	return dt, dur, nil
}
