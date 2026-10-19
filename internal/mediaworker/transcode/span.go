package transcode

import (
	"encoding/binary"
	"math"
)

// span is the samples [first, end) of the video, in decoding order: it
// starts at an IDR picture and none of its pictures is shown after a
// picture of the next span, so it decodes alone and its pictures follow
// those of the previous span.
type span struct {
	first, end int
}

// maxSpan is the length after which a span ends at the next IDR picture
// even when it is not at a keyInterval boundary (see spans).
const maxSpan = 4 * keyInterval

// spans splits the video into spans of at least keyInterval seconds. A
// span starts where a single pass would put an IDR picture anyway (the
// first picture of a keyInterval period), so splitting adds no IDR
// picture; a source whose IDR pictures never fall there is split at the
// first IDR picture after maxSpan. A video without such points is one
// span.
func (s *Source) spans() ([]span, error) {
	samples := s.Video.Samples
	scale := float64(s.Video.Timescale)
	base := s.firstPTS()
	at := func(pts int64) float64 { return float64(pts-base) / scale }
	// suffixMin[i] is the earliest presentation time of samples[i:].
	suffixMin := make([]int64, len(samples)+1)
	suffixMin[len(samples)] = math.MaxInt64
	for i := len(samples) - 1; i >= 0; i-- {
		suffixMin[i] = min(suffixMin[i+1], samples[i].PTS())
	}
	var out []span
	first, latest := 0, int64(math.MinInt64) // latest: of the current span
	for i, sm := range samples {
		if i > first && sm.Sync && latest < suffixMin[i] {
			start, prev := at(sm.PTS()), at(latest)
			length := start - at(samples[first].PTS())
			aligned := math.Floor(start/keyInterval) > math.Floor(prev/keyInterval)
			if (aligned && length >= keyInterval) || length >= maxSpan {
				idr, err := s.isIDR(sm)
				if err != nil {
					return nil, err
				}
				if idr {
					out = append(out, span{first, i})
					first, latest = i, math.MinInt64
				}
			}
		}
		latest = max(latest, sm.PTS())
	}
	return append(out, span{first, len(samples)}), nil
}

// isIDR reports whether a sample holds an IDR picture: a sync sample can
// also be an I picture of an open GOP, which later pictures read past.
func (s *Source) isIDR(sm Sample) (bool, error) {
	data, err := s.ReadSample(sm)
	if err != nil {
		return false, err
	}
	for len(data) >= 5 {
		n := binary.BigEndian.Uint32(data)
		if uint64(n) > uint64(len(data)-4) || n == 0 {
			return false, nil
		}
		if data[4]&0x1f == 5 {
			return true, nil
		}
		data = data[4+n:]
	}
	return false, nil
}
