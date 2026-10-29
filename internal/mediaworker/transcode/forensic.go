package transcode

import (
	"image"
	"math"
	"math/rand/v2"
)

// The forensic watermark (docs/proposals/video-protection.md §11): a
// rendition marked for it is encoded twice from the same pictures, the A
// variant with an invisible pattern added to the luma and the B variant
// with it subtracted. The server serves every segment of a stream from the
// variant a bit of the code of its playback token selects, so that a
// leaked copy carries that code; Trace reads it back by comparing the copy
// with the two variants.
//
// The pattern is smooth (a grid of random signs, bilinearly interpolated
// over the picture) and faint (ForensicStrength luma levels): a low
// spatial frequency survives the re-encoding of a leaked copy, its
// scaling and its recompression, and it is static within a segment, so
// the encoder carries it from the IDR picture through the predicted ones.
// Its grid is defined on the whole picture: the renditions of a video,
// whatever their size, carry the same pattern.

// ForensicStrength is the amplitude of the mark, in luma levels (of 255).
const ForensicStrength = 2.0

// Size of the grid of the pattern, in cells across and down.
const (
	markColumns = 16
	markRows    = 9
)

// forensicMark is the pattern of a video at the size of a rendition.
type forensicMark struct {
	w, h  int
	field []int8 // luma offsets, row-major
}

// newForensicMark returns the pattern of seed at w x h.
func newForensicMark(seed uint64, w, h int, strength float64) *forensicMark {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) // #nosec G404 -- a watermark pattern, not a secret
	var grid [markRows + 1][markColumns + 1]float64
	for r := range grid {
		for c := range grid[r] {
			if rng.IntN(2) == 0 {
				grid[r][c] = -1
			} else {
				grid[r][c] = 1
			}
		}
	}
	m := &forensicMark{w: w, h: h, field: make([]int8, w*h)}
	for y := range h {
		gy := float64(y) / float64(max(h-1, 1)) * markRows
		r0 := min(int(gy), markRows-1)
		fy := gy - float64(r0)
		for x := range w {
			gx := float64(x) / float64(max(w-1, 1)) * markColumns
			c0 := min(int(gx), markColumns-1)
			fx := gx - float64(c0)
			v := (1-fy)*((1-fx)*grid[r0][c0]+fx*grid[r0][c0+1]) + fy*((1-fx)*grid[r0+1][c0]+fx*grid[r0+1][c0+1])
			m.field[y*w+x] = int8(math.Round(v * strength)) // #nosec G115 -- |v * strength| is at most a few levels
		}
	}
	return m
}

// apply adds the pattern (sign +1, the A variant) or subtracts it (sign
// -1, the B variant) to the luma of pic, in place.
func (m *forensicMark) apply(pic *image.YCbCr, sign int) {
	for y := range m.h {
		row := pic.Y[y*pic.YStride : y*pic.YStride+m.w]
		f := m.field[y*m.w : (y+1)*m.w]
		for x, v := range row {
			n := int(v) + sign*int(f[x])
			row[x] = uint8(min(max(n, 0), 255)) // #nosec G115 -- clamped
		}
	}
}
