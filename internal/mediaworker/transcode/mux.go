package transcode

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// muxSample is a sample of an output track.
type muxSample struct {
	size uint32
	dur  uint32
	cto  int32
	sync bool
	// src locates the data: an offset in the data reader of the track.
	src int64
}

// muxTrack is a track of the output file.
type muxTrack struct {
	id        uint32
	handler   string // vide or soun
	timescale uint32
	language  string
	stsd      []byte // the encoded stsd box
	width     int
	height    int
	samples   []muxSample
	data      io.ReaderAt
}

func (t *muxTrack) duration() uint64 {
	var d uint64
	for _, s := range t.samples {
		d += uint64(s.dur)
	}
	return d
}

// chunk is a run of samples of one track stored together.
type chunk struct {
	track  *muxTrack
	first  int // first sample
	count  int
	offset uint64
}

// interleave groups the samples into chunks of about window seconds,
// alternating the tracks.
func interleave(tracks []*muxTrack, window float64) []chunk {
	next := make([]int, len(tracks))
	times := make([]uint64, len(tracks))
	var chunks []chunk
	for end := window; ; end += window {
		done := true
		for i, t := range tracks {
			c := chunk{track: t, first: next[i]}
			limit := uint64(end * float64(t.timescale))
			for next[i] < len(t.samples) && times[i] < limit {
				times[i] += uint64(t.samples[next[i]].dur)
				next[i]++
			}
			if c.count = next[i] - c.first; c.count > 0 {
				chunks = append(chunks, c)
			}
			if next[i] < len(t.samples) {
				done = false
			}
		}
		if done {
			return chunks
		}
	}
}

// writeMP4 writes a progressive MP4 file (moov first) with the tracks.
func writeMP4(w io.Writer, tracks []*muxTrack) error {
	chunks := interleave(tracks, 2)
	ftyp := box("ftyp", []byte("isom"), u32(0x200), []byte("isomiso2avc1mp41"))
	var dataSize uint64
	for _, c := range chunks {
		for _, s := range c.track.samples[c.first : c.first+c.count] {
			dataSize += uint64(s.size)
		}
	}
	// The offsets are 64-bit: the size of moov does not depend on them.
	moov := moovBox(tracks, chunks)
	base := uint64(len(ftyp)+len(moov)) + 16 // mdat with a 64-bit size
	pos := base
	for i := range chunks {
		chunks[i].offset = pos
		for _, s := range chunks[i].track.samples[chunks[i].first : chunks[i].first+chunks[i].count] {
			pos += uint64(s.size)
		}
	}
	moov = moovBox(tracks, chunks)
	if _, err := w.Write(ftyp); err != nil {
		return err
	}
	if _, err := w.Write(moov); err != nil {
		return err
	}
	hdr := make([]byte, 16)
	binary.BigEndian.PutUint32(hdr, 1)
	copy(hdr[4:], "mdat")
	binary.BigEndian.PutUint64(hdr[8:], dataSize+16)
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	for _, c := range chunks {
		for _, s := range c.track.samples[c.first : c.first+c.count] {
			if _, err := io.Copy(w, io.NewSectionReader(c.track.data, s.src, int64(s.size))); err != nil {
				return fmt.Errorf("transcode: copy sample: %w", err)
			}
		}
	}
	return nil
}

const movieTimescale = 1000

func moovBox(tracks []*muxTrack, chunks []chunk) []byte {
	var movieDur uint64
	for _, t := range tracks {
		movieDur = max(movieDur, t.duration()*movieTimescale/uint64(t.timescale))
	}
	mvhd := fullBox("mvhd", 0, 0,
		u32(0), u32(0), u32(movieTimescale), u32(uint32(min(movieDur, 0xffffffff))), // #nosec G115 -- clamped
		u32(0x00010000), []byte{1, 0}, make([]byte, 10), matrix(), make([]byte, 24),
		u32(uint32(len(tracks)+1))) // #nosec G115 -- two tracks
	parts := [][]byte{mvhd}
	for _, t := range tracks {
		parts = append(parts, trakBox(t, chunks, movieDur))
	}
	return box("moov", parts...)
}

func trakBox(t *muxTrack, chunks []chunk, movieDur uint64) []byte {
	dur := t.duration()
	var volume []byte
	if t.handler == "soun" {
		volume = []byte{1, 0}
	} else {
		volume = []byte{0, 0}
	}
	tkhd := fullBox("tkhd", 0, 3,
		u32(0), u32(0), u32(t.id), u32(0), u32(uint32(min(dur*movieTimescale/uint64(t.timescale), 0xffffffff))), // #nosec G115 -- clamped
		make([]byte, 8), []byte{0, 0, 0, 0}, volume, []byte{0, 0}, matrix(),
		u32(uint32(t.width)<<16), u32(uint32(t.height)<<16)) // #nosec G115 -- picture sizes
	_ = movieDur
	mdhd := fullBox("mdhd", 0, 0, u32(0), u32(0), u32(t.timescale), u32(uint32(min(dur, 0xffffffff))), // #nosec G115 -- clamped
		language(t.language), []byte{0, 0})
	name := "VideoHandler"
	var mhd []byte
	if t.handler == "soun" {
		name = "SoundHandler"
		mhd = fullBox("smhd", 0, 0, make([]byte, 4))
	} else {
		mhd = fullBox("vmhd", 0, 1, make([]byte, 8))
	}
	hdlr := fullBox("hdlr", 0, 0, u32(0), []byte(t.handler), make([]byte, 12), []byte(name), []byte{0})
	dinf := box("dinf", fullBox("dref", 0, 0, u32(1), fullBox("url ", 0, 1)))
	stbl := box("stbl", t.stsd, stts(t), ctts(t), stss(t), stsc(t, chunks), stsz(t), co64(t, chunks))
	return box("trak", tkhd, box("mdia", mdhd, hdlr, box("minf", mhd, dinf, stbl)))
}

func stts(t *muxTrack) []byte {
	var entries [][2]uint32
	for _, s := range t.samples {
		if n := len(entries); n > 0 && entries[n-1][1] == s.dur {
			entries[n-1][0]++
			continue
		}
		entries = append(entries, [2]uint32{1, s.dur})
	}
	return fullBox("stts", 0, 0, pairs(entries))
}

func ctts(t *muxTrack) []byte {
	var entries [][2]uint32
	nonzero := false
	for _, s := range t.samples {
		nonzero = nonzero || s.cto != 0
		if n := len(entries); n > 0 && int32(entries[n-1][1]) == s.cto { // #nosec G115 -- signed offsets (version 1)
			entries[n-1][0]++
			continue
		}
		entries = append(entries, [2]uint32{1, uint32(s.cto)}) // #nosec G115 -- signed offsets (version 1)
	}
	if !nonzero {
		return nil
	}
	return fullBox("ctts", 1, 0, pairs(entries))
}

func stss(t *muxTrack) []byte {
	var nrs []byte
	n, all := 0, true
	for i, s := range t.samples {
		if s.sync {
			nrs = append(nrs, u32(uint32(i+1))...) // #nosec G115 -- sample numbers
			n++
		} else {
			all = false
		}
	}
	if all {
		return nil // every sample is a sync sample
	}
	return fullBox("stss", 0, 0, u32(uint32(n)), nrs) // #nosec G115 -- sample counts
}

func stsc(t *muxTrack, chunks []chunk) []byte {
	var entries []byte
	count, nr, last := 0, 0, -1
	for _, c := range chunks {
		if c.track != t {
			continue
		}
		nr++
		if c.count != last {
			entries = append(entries, u32(uint32(nr))...)      // #nosec G115 -- chunk numbers
			entries = append(entries, u32(uint32(c.count))...) // #nosec G115 -- sample counts
			entries = append(entries, u32(1)...)
			count++
			last = c.count
		}
	}
	return fullBox("stsc", 0, 0, u32(uint32(count)), entries) // #nosec G115 -- entry counts
}

func stsz(t *muxTrack) []byte {
	sizes := make([]byte, 0, 4*len(t.samples))
	for _, s := range t.samples {
		sizes = append(sizes, u32(s.size)...)
	}
	return fullBox("stsz", 0, 0, u32(0), u32(uint32(len(t.samples))), sizes) // #nosec G115 -- sample counts
}

func co64(t *muxTrack, chunks []chunk) []byte {
	var offs []byte
	n := 0
	for _, c := range chunks {
		if c.track == t {
			offs = binary.BigEndian.AppendUint64(offs, c.offset)
			n++
		}
	}
	return fullBox("co64", 0, 0, u32(uint32(n)), offs) // #nosec G115 -- chunk counts
}

// --- box encoding -------------------------------------------------------------

func box(typ string, parts ...[]byte) []byte {
	n := 8
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 8, n)
	binary.BigEndian.PutUint32(out, uint32(n)) // #nosec G115 -- boxes of the moov
	copy(out[4:], typ)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func fullBox(typ string, version byte, flags uint32, parts ...[]byte) []byte {
	vf := u32(flags)
	vf[0] = version
	return box(typ, append([][]byte{vf}, parts...)...)
}

func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

func pairs(entries [][2]uint32) []byte {
	out := u32(uint32(len(entries))) // #nosec G115 -- entry counts
	for _, e := range entries {
		out = binary.BigEndian.AppendUint32(out, e[0])
		out = binary.BigEndian.AppendUint32(out, e[1])
	}
	return out
}

func matrix() []byte {
	var b bytes.Buffer
	for _, v := range []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000} {
		b.Write(u32(v))
	}
	return b.Bytes()
}

// language packs an ISO 639-2 code (und when invalid).
func language(lang string) []byte {
	if len(lang) != 3 {
		lang = "und"
	}
	var v uint16
	for i := range 3 {
		c := lang[i]
		if c < 'a' || c > 'z' {
			return []byte{0x55, 0xc4} // und
		}
		v = v<<5 | uint16(c-0x60)
	}
	return []byte{byte(v >> 8), byte(v)}
}
