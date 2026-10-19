package imaging

import (
	"context"
	"fmt"
	"image"
	"math"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/r3n3r0/nexo/internal/mediaworker/resample"
)

// Size is the size of a variant.
type Size struct {
	// Width is the width asked for, 0 for the image at its own size.
	Width int
	// PixelWidth and PixelHeight are the size of the variant.
	PixelWidth, PixelHeight int
}

// Sizes returns the sizes of the variants of a w x h image, largest
// first: its own size, then every width smaller than w (an image is never
// enlarged), the height following the aspect ratio (at least 1 pixel).
func Sizes(w, h int, widths []int) []Size {
	out := []Size{{PixelWidth: w, PixelHeight: h}}
	ws := slices.Clone(widths)
	slices.Sort(ws)
	for _, tw := range slices.Backward(slices.Compact(ws)) {
		if tw <= 0 || tw >= w {
			continue
		}
		th := max(1, int(math.Round(float64(h)*float64(tw)/float64(w))))
		out = append(out, Size{Width: tw, PixelWidth: tw, PixelHeight: th})
	}
	return out
}

// Variant is an encoded variant.
type Variant struct {
	Format string
	Size
	Data []byte
}

// Options configure Variants.
type Options struct {
	// Formats of the variants (EncodeFormats names; default all).
	Formats []string
	// Widths of the variants (see Sizes).
	Widths []int
	// Workers bounds the variants encoded at once (default GOMAXPROCS).
	Workers int
	// MemoryBudget bounds the memory of the variants being encoded, as
	// estimated by EncodeMemory (default DefaultMemoryBudget): a variant
	// starts when its estimate fits. A variant whose estimate exceeds the
	// whole budget is not produced (an AVIF of a very large image): the
	// other formats of its size serve it, and the worker never runs out of
	// memory on it, which would retry the job for ever.
	MemoryBudget int64
}

// DefaultMemoryBudget is the default Options.MemoryBudget.
const DefaultMemoryBudget = 2 << 30

// encodeOrder returns the order in which the formats of a size start:
// the slowest first, so that they do not end the job alone.
func encodeOrder() []string { return []string{AVIF, WebP, PNG, JPEG} }

// EncodeMemory estimates the peak memory of the encoding of a picture of
// pixels pixels in a format, its conversions and the garbage it leaves
// included (measured at 12 MP, BenchmarkEncode and docs/bench/media.md):
// AVIF (aom in WebAssembly) needs about 100 bytes per pixel, 1.2 GB for a
// 12 MP photo at its own size.
func EncodeMemory(format string, pixels int) int64 {
	perPixel := map[string]int64{AVIF: 100, WebP: 26, PNG: 6, JPEG: 4}[format]
	return int64(pixels) * perPixel
}

// Variants encodes every variant of the image: each size of Sizes in
// each format. The sizes are produced largest first and their formats
// encoded in parallel, at most Workers at once and within the memory
// budget. emit receives each variant as soon as it is encoded, one at a
// time; an error of emit stops the job and is returned.
func (im *Image) Variants(ctx context.Context, o Options, emit func(Variant) error) error {
	formats := o.Formats
	if len(formats) == 0 {
		formats = EncodeFormats()
	}
	for _, f := range formats {
		if !slices.Contains(EncodeFormats(), f) {
			return fmt.Errorf("%w: cannot encode %q", ErrUnsupported, f)
		}
	}
	formats = slices.DeleteFunc(encodeOrder(), func(f string) bool { return !slices.Contains(formats, f) })
	workers := o.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	budget := o.MemoryBudget
	if budget <= 0 {
		budget = DefaultMemoryBudget
	}

	g, ctx := errgroup.WithContext(ctx)
	slots := semaphore.NewWeighted(int64(workers))
	mem := semaphore.NewWeighted(budget)
	var emitMu sync.Mutex
	stopped := false // an emit failed: guarded by emitMu
	produce := func() error {
		for _, s := range Sizes(im.Width, im.Height, o.Widths) {
			// The picture is held until every format of its size is
			// encoded; each encoding holds its own estimate. The caps
			// leave room for one encoding next to one picture, so that
			// the budget never blocks for good.
			px := s.PixelWidth * s.PixelHeight
			formats := slices.DeleteFunc(slices.Clone(formats), func(f string) bool { return EncodeMemory(f, px) > budget })
			if len(formats) == 0 {
				continue
			}
			picNeed := min(budget/4, int64(px)*4)
			if err := mem.Acquire(ctx, picNeed); err != nil {
				return err
			}
			pic := im.picture(s)
			var left atomic.Int32
			left.Store(int32(len(formats))) // #nosec G115 -- at most 4
			done := func() {
				if left.Add(-1) == 0 {
					mem.Release(picNeed)
				}
			}
			for i, f := range formats {
				need := min(budget-picNeed, EncodeMemory(f, px))
				if err := mem.Acquire(ctx, need); err != nil {
					if left.Add(-int32(len(formats)-i)) == 0 { // #nosec G115 -- at most 4
						mem.Release(picNeed)
					}
					return err
				}
				if err := slots.Acquire(ctx, 1); err != nil {
					mem.Release(need)
					if left.Add(-int32(len(formats)-i)) == 0 { // #nosec G115 -- at most 4
						mem.Release(picNeed)
					}
					return err
				}
				g.Go(func() error {
					defer slots.Release(1)
					defer mem.Release(need)
					defer done()
					if err := ctx.Err(); err != nil {
						return err
					}
					data, err := Encode(f, pic, im.opaque)
					if err != nil {
						return err
					}
					emitMu.Lock()
					defer emitMu.Unlock()
					if stopped {
						return nil // the error is returned by the emit that failed
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					err = emit(Variant{Format: f, Size: s, Data: data})
					stopped = err != nil
					return err
				})
			}
		}
		return nil
	}
	g.Go(produce)
	return g.Wait()
}

// picture returns the upright picture of a size: the source resized in
// its stored orientation (fewer pixels to turn), then turned.
func (im *Image) picture(s Size) *image.RGBA {
	w, h := s.PixelWidth, s.PixelHeight
	if im.orientation >= 5 {
		w, h = h, w
	}
	var m *image.RGBA
	switch {
	case im.rgba != nil && s.Width == 0:
		m = im.rgba
	case im.rgba != nil:
		src := im.rgba
		m = image.NewRGBA(image.Rect(0, 0, w, h))
		resample.New(src.Rect.Dx(), src.Rect.Dy(), w, h, 4).Resize(src.Pix, src.Stride, m.Pix, m.Stride)
	case s.Width == 0:
		m = toRGBA(im.ycc)
	default:
		m = toRGBA(resizeYCbCr(im.ycc, w, h))
	}
	return orient(m, im.orientation)
}

// resizeYCbCr resamples the planes of a YCbCr picture (at the origin) to
// w x h, keeping its subsampling.
func resizeYCbCr(src *image.YCbCr, w, h int) *image.YCbCr {
	dst := image.NewYCbCr(image.Rect(0, 0, w, h), src.SubsampleRatio)
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	resample.New(sw, sh, w, h, 1).Resize(src.Y, src.YStride, dst.Y, dst.YStride)
	scw, sch := chromaSize(src.SubsampleRatio, sw, sh)
	dcw, dch := chromaSize(src.SubsampleRatio, w, h)
	c := resample.New(scw, sch, dcw, dch, 1)
	c.Resize(src.Cb, src.CStride, dst.Cb, dst.CStride)
	c.Resize(src.Cr, src.CStride, dst.Cr, dst.CStride)
	return dst
}

// chromaSize returns the size of the chroma planes of a w x h picture.
func chromaSize(r image.YCbCrSubsampleRatio, w, h int) (int, int) {
	switch r {
	case image.YCbCrSubsampleRatio422:
		return (w + 1) / 2, h
	case image.YCbCrSubsampleRatio420:
		return (w + 1) / 2, (h + 1) / 2
	case image.YCbCrSubsampleRatio440:
		return w, (h + 1) / 2
	}
	return w, h
}
