// Package openh264 binds the Cisco OpenH264 library (BSD licensed) at run
// time, without cgo: the media worker decodes H.264 and encodes H.264
// (Constrained Baseline) with it, so no ffmpeg is needed.
//
// The library is a shared object loaded from a path (Open). Its C API is a
// pair of vtables (ISVCEncoder, ISVCDecoder); the structures below mirror
// the 64-bit layouts of codec_api.h, codec_app_def.h and codec_def.h of
// OpenH264 2.4.
package openh264

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"math"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Version is the OpenH264 release the structures follow.
const Version = "2.4.1"

// ErrUnsupported is returned on a platform without 64-bit pointers.
var ErrUnsupported = errors.New("openh264: only 64-bit platforms are supported")

// Library is a loaded OpenH264 shared library.
type Library struct {
	createEncoder  uintptr
	destroyEncoder uintptr
	createDecoder  uintptr
	destroyDecoder uintptr
	version        uintptr
}

// Open loads the library at path.
func Open(path string) (*Library, error) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return nil, ErrUnsupported
	}
	h, err := dlopen(path)
	if err != nil {
		return nil, fmt.Errorf("openh264: load %s: %w", path, err)
	}
	l := &Library{}
	for name, p := range map[string]*uintptr{
		"WelsCreateSVCEncoder":  &l.createEncoder,
		"WelsDestroySVCEncoder": &l.destroyEncoder,
		"WelsCreateDecoder":     &l.createDecoder,
		"WelsDestroyDecoder":    &l.destroyDecoder,
		"WelsGetCodecVersion":   &l.version,
	} {
		if *p, err = dlsym(h, name); err != nil {
			return nil, fmt.Errorf("openh264: %s: symbol %s: %w", path, name, err)
		}
	}
	return l, nil
}

// CodecVersion returns the version the library reports.
func (l *Library) CodecVersion() string {
	// OpenH264Version is a struct of four uint32 returned by value: in
	// registers on amd64 and arm64 (16 bytes).
	r1, r2, _ := purego.SyscallN(l.version)
	return fmt.Sprintf("%d.%d.%d", uint32(r1), uint32(r1>>32), uint32(r2)) // #nosec G115 -- unpacking
}

// call invokes a function of a vtable: the object pointer comes first.
func call(fn uintptr, obj uintptr, args ...uintptr) uintptr {
	r, _, _ := purego.SyscallN(fn, append([]uintptr{obj}, args...)...)
	return r
}

// cptr converts an address of C memory, which the Go garbage collector
// neither tracks nor moves, into a pointer.
func cptr(p uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&p)) // #nosec G103 -- C memory
}

// vtable returns the function at index i of the vtable of obj (a pointer
// to a pointer to the vtable).
func vtable(obj uintptr, i int) uintptr {
	vt := *(*uintptr)(cptr(obj))                                // #nosec G103 -- the C object
	return *(*uintptr)(cptr(vt + uintptr(i)*unsafe.Sizeof(vt))) // #nosec G103 -- its vtable
}

// --- decoder ------------------------------------------------------------------

// sDecodingParam mirrors SDecodingParam.
type sDecodingParam struct {
	fileNameRestructed uintptr
	cpuLoad            uint32
	targetDqLayer      uint8
	_                  [3]byte
	ecActiveIdc        int32
	parseOnly          bool
	_                  [3]byte
	videoPropertySize  uint32
	videoBsType        int32
}

// sBufferInfo mirrors SBufferInfo.
type sBufferInfo struct {
	bufferStatus    int32
	_               int32
	inBsTimeStamp   uint64
	outYuvTimeStamp uint64
	width           int32
	height          int32
	format          int32
	stride          [2]int32
	_               int32
	dst             [3]uintptr
}

// Decoder vtable indexes (ISVCDecoderVtbl).
const (
	decInitialize = iota
	decUninitialize
	decDecodeFrame
	decDecodeFrameNoDelay
	decDecodeFrame2
	decFlushFrame
	decDecodeParser
	decDecodeFrameEx
	decSetOption
)

const (
	videoBitstreamAVC = 0
	errorConDisable   = 0
	optionEndOfStream = 1
)

// Decoder decodes an H.264 Annex B stream into I420 pictures.
type Decoder struct {
	lib *Library
	obj uintptr
	mu  sync.Mutex
}

// NewDecoder creates a decoder.
func (l *Library) NewDecoder() (*Decoder, error) {
	var obj uintptr
	if r := call(l.createDecoder, uintptr(unsafe.Pointer(&obj))); int32(r) != 0 || obj == 0 { // #nosec G103 G115 -- out parameter, C int
		return nil, fmt.Errorf("openh264: create decoder: %d", int32(r)) // #nosec G115 -- C int
	}
	quiet := int32(traceQuiet)
	call(vtable(obj, decSetOption), obj, optionDecTraceLevel, uintptr(unsafe.Pointer(&quiet))) // #nosec G103 -- option value
	p := &sDecodingParam{targetDqLayer: 0xff, ecActiveIdc: errorConDisable, videoPropertySize: 8, videoBsType: videoBitstreamAVC}
	if r := call(vtable(obj, decInitialize), obj, uintptr(unsafe.Pointer(p))); r != 0 { // #nosec G103 -- the parameters
		call(l.destroyDecoder, obj)
		return nil, fmt.Errorf("openh264: initialize decoder: %d", int64(r)) // #nosec G115 -- C long
	}
	runtime.KeepAlive(p)
	return &Decoder{lib: l, obj: obj}, nil
}

// Decode decodes one access unit (Annex B, with its parameter sets when
// it starts a sequence) and returns the picture it completes, if any: the
// decoder may hold pictures back to reorder them (Flush returns the rest).
func (d *Decoder) Decode(au []byte) (*image.YCbCr, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(au) == 0 {
		return nil, nil
	}
	var info sBufferInfo
	var dst [3]uintptr
	var pin runtime.Pinner
	pin.Pin(&au[0])
	defer pin.Unpin()
	r := call(vtable(d.obj, decDecodeFrameNoDelay), d.obj,
		uintptr(unsafe.Pointer(&au[0])), uintptr(len(au)), // #nosec G103 -- input
		uintptr(unsafe.Pointer(&dst[0])), uintptr(unsafe.Pointer(&info))) // #nosec G103 -- outputs
	if int32(r) != 0 { // #nosec G115 -- DECODING_STATE
		return nil, fmt.Errorf("openh264: decode: state %#x", int32(r)) // #nosec G115 -- DECODING_STATE
	}
	return info.picture(), nil
}

// Flush returns the pictures still held by the decoder, in order.
func (d *Decoder) Flush() ([]*image.YCbCr, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	end := int32(1)
	call(vtable(d.obj, decSetOption), d.obj, optionEndOfStream, uintptr(unsafe.Pointer(&end))) // #nosec G103 -- SetOption
	var out []*image.YCbCr
	for range 64 {
		var info sBufferInfo
		var dst [3]uintptr
		r := call(vtable(d.obj, decFlushFrame), d.obj, uintptr(unsafe.Pointer(&dst[0])), uintptr(unsafe.Pointer(&info))) // #nosec G103 -- outputs
		pic := info.picture()
		if int32(r) != 0 || pic == nil { // #nosec G115 -- DECODING_STATE
			break
		}
		out = append(out, pic)
	}
	return out, nil
}

// Close releases the decoder.
func (d *Decoder) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.obj == 0 {
		return
	}
	call(vtable(d.obj, decUninitialize), d.obj)
	call(d.lib.destroyDecoder, d.obj)
	d.obj = 0
}

// picture copies the decoded picture out of the decoder's buffers.
func (b *sBufferInfo) picture() *image.YCbCr {
	if b.bufferStatus != 1 || b.width <= 0 || b.height <= 0 {
		return nil
	}
	w, h := int(b.width), int(b.height)
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	copyPlane(img.Y, img.YStride, b.dst[0], int(b.stride[0]), w, h)
	cw, ch := (w+1)/2, (h+1)/2
	copyPlane(img.Cb, img.CStride, b.dst[1], int(b.stride[1]), cw, ch)
	copyPlane(img.Cr, img.CStride, b.dst[2], int(b.stride[1]), cw, ch)
	return img
}

func copyPlane(dst []byte, dstStride int, src uintptr, srcStride, w, h int) {
	for y := range h {
		row := unsafe.Slice((*byte)(cptr(src+uintptr(y*srcStride))), w) // #nosec G103 G115 -- the decoder's plane
		copy(dst[y*dstStride:y*dstStride+w], row)
	}
}

// --- encoder ------------------------------------------------------------------

// sEncParamExt holds an SEncParamExt (920 bytes on the 64-bit platforms:
// the structure has no pointer and no long, so its layout is the same on
// Linux, macOS and Windows). Only the fields below are set, at the offsets
// computed with offsetof() on the headers of OpenH264 2.4; the rest keeps
// the defaults of GetDefaultParams.
type sEncParamExt [920 / 4]uint32

const (
	extUsageType         = 0
	extPicWidth          = 4
	extPicHeight         = 8
	extTargetBitrate     = 12
	extRCMode            = 16
	extMaxFrameRate      = 20
	extSpatialLayerNum   = 28
	extSpatialLayers     = 32 // SSpatialLayerConfig[4], 200 bytes each
	extIntraPeriod       = 836
	extSpsPpsIDStrategy  = 844
	extEnableFrameSkip   = 860
	extMultipleThreadIdc = 892

	layerVideoWidth     = 0
	layerVideoHeight    = 4
	layerFrameRate      = 8
	layerSpatialBitrate = 12
)

func (e *sEncParamExt) bytes() []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(e)), len(e)*4) // #nosec G103 -- the structure as bytes
}

func (e *sEncParamExt) put32(off int, v uint32) { binary.LittleEndian.PutUint32(e.bytes()[off:], v) }

// set fills the parameters of a single-layer stream: one SPS and one PPS
// for the whole stream, as the sample entry of an MP4 track declares
// them (the default increments their ids at every IDR picture), no frame
// skipping, IDR pictures only where the caller forces them, threads
// chosen by the library.
func (e *sEncParamExt) set(p EncoderParams) {
	w, h, br := uint32(p.Width), uint32(p.Height), uint32(p.Bitrate) // #nosec G115 -- sizes and bitrates
	fps := math.Float32bits(float32(p.FrameRate))
	e.put32(extUsageType, cameraVideoRealTime)
	e.put32(extPicWidth, w)
	e.put32(extPicHeight, h)
	e.put32(extTargetBitrate, br)
	e.put32(extRCMode, rcBitrateMode)
	e.put32(extMaxFrameRate, fps)
	e.put32(extSpatialLayerNum, 1)
	e.put32(extSpatialLayers+layerVideoWidth, w)
	e.put32(extSpatialLayers+layerVideoHeight, h)
	e.put32(extSpatialLayers+layerFrameRate, fps)
	e.put32(extSpatialLayers+layerSpatialBitrate, br)
	e.put32(extIntraPeriod, 0)
	e.put32(extSpsPpsIDStrategy, constantID)
	e.bytes()[extEnableFrameSkip] = 0
	binary.LittleEndian.PutUint16(e.bytes()[extMultipleThreadIdc:], 0)
}

// sSourcePicture mirrors SSourcePicture.
type sSourcePicture struct {
	colorFormat int32
	stride      [4]int32
	_           int32
	data        [4]uintptr
	picWidth    int32
	picHeight   int32
	timeStamp   int64
}

// sLayerBSInfo mirrors SLayerBSInfo.
type sLayerBSInfo struct {
	temporalID      uint8
	spatialID       uint8
	qualityID       uint8
	_               uint8
	frameType       int32
	layerType       uint8
	_               [3]byte
	subSeqID        int32
	nalCount        int32
	_               int32
	nalLengthInByte uintptr
	bsBuf           uintptr
}

// maxLayerNumOfFrame is MAX_LAYER_NUM_OF_FRAME.
const maxLayerNumOfFrame = 128

// sFrameBSInfo mirrors SFrameBSInfo.
type sFrameBSInfo struct {
	layerNum        int32
	_               int32
	layerInfo       [maxLayerNumOfFrame]sLayerBSInfo
	frameType       int32
	frameSizeInByte int32
	timeStamp       int64
}

// Encoder vtable indexes (ISVCEncoderVtbl).
const (
	encInitialize = iota
	encInitializeExt
	encGetDefaultParams
	encUninitialize
	encEncodeFrame
	encEncodeParameterSets
	encForceIntraFrame
	encSetOption
	encGetOption
)

const (
	cameraVideoRealTime = 0
	rcBitrateMode       = 1
	videoFormatI420     = 23
	constantID          = 0
	optionEncTraceLevel = 25
	optionDecTraceLevel = 9
	traceQuiet          = 0

	frameTypeIDR  = 1
	frameTypeSkip = 4
)

// EncoderParams configure an Encoder.
type EncoderParams struct {
	Width, Height int
	// Bitrate is the target bitrate in bits per second.
	Bitrate int
	// FrameRate is the frame rate of the input.
	FrameRate float64
}

// Encoder encodes I420 pictures into H.264 Constrained Baseline.
type Encoder struct {
	lib    *Library
	obj    uintptr
	params EncoderParams
	info   *sFrameBSInfo
	src    *sSourcePicture
}

// NewEncoder creates an encoder. Frame skipping is off: every input
// picture produces an output picture.
func (l *Library) NewEncoder(p EncoderParams) (*Encoder, error) {
	if p.Width <= 0 || p.Height <= 0 || p.Width%2 != 0 || p.Height%2 != 0 {
		return nil, fmt.Errorf("openh264: invalid encoder size %dx%d (positive and even)", p.Width, p.Height)
	}
	var obj uintptr
	if r := call(l.createEncoder, uintptr(unsafe.Pointer(&obj))); int32(r) != 0 || obj == 0 { // #nosec G103 G115 -- out parameter, C int
		return nil, fmt.Errorf("openh264: create encoder: %d", int32(r)) // #nosec G115 -- C int
	}
	quiet := int32(traceQuiet)
	call(vtable(obj, encSetOption), obj, optionEncTraceLevel, uintptr(unsafe.Pointer(&quiet))) // #nosec G103 -- option value
	ext := new(sEncParamExt)
	if r := call(vtable(obj, encGetDefaultParams), obj, uintptr(unsafe.Pointer(ext))); int32(r) != 0 { // #nosec G103 G115 -- parameters
		call(l.destroyEncoder, obj)
		return nil, fmt.Errorf("openh264: default parameters: %d", int32(r)) // #nosec G115 -- C int
	}
	ext.set(p)
	if r := call(vtable(obj, encInitializeExt), obj, uintptr(unsafe.Pointer(ext))); int32(r) != 0 { // #nosec G103 G115 -- parameters
		call(l.destroyEncoder, obj)
		return nil, fmt.Errorf("openh264: initialize encoder %dx%d: %d", p.Width, p.Height, int32(r)) // #nosec G115 -- C int
	}
	runtime.KeepAlive(ext)
	return &Encoder{lib: l, obj: obj, params: p, info: new(sFrameBSInfo), src: new(sSourcePicture)}, nil
}

// Encode encodes a picture of the size of the encoder at ts milliseconds;
// idr forces an IDR picture. It returns the NAL units of the picture
// (without start codes, parameter sets included before an IDR) and
// whether it is an IDR picture.
func (e *Encoder) Encode(img *image.YCbCr, ts int64, idr bool) (nalus [][]byte, key bool, err error) {
	if img.Rect.Dx() != e.params.Width || img.Rect.Dy() != e.params.Height || img.SubsampleRatio != image.YCbCrSubsampleRatio420 {
		return nil, false, fmt.Errorf("openh264: picture %v (%v) does not match the encoder %dx%d I420", img.Rect, img.SubsampleRatio, e.params.Width, e.params.Height)
	}
	if idr {
		call(vtable(e.obj, encForceIntraFrame), e.obj, 1)
	}
	var pin runtime.Pinner
	pin.Pin(&img.Y[0])
	pin.Pin(&img.Cb[0])
	pin.Pin(&img.Cr[0])
	pin.Pin(e.src)
	pin.Pin(e.info)
	defer pin.Unpin()
	*e.src = sSourcePicture{
		colorFormat: videoFormatI420,
		stride:      [4]int32{int32(img.YStride), int32(img.CStride), int32(img.CStride)}, // #nosec G115 -- strides
		data: [4]uintptr{
			uintptr(unsafe.Pointer(&img.Y[0])), uintptr(unsafe.Pointer(&img.Cb[0])), uintptr(unsafe.Pointer(&img.Cr[0])), // #nosec G103 -- planes, pinned
		},
		picWidth: int32(e.params.Width), picHeight: int32(e.params.Height), timeStamp: ts, // #nosec G115 -- sizes
	}
	*e.info = sFrameBSInfo{}
	if r := call(vtable(e.obj, encEncodeFrame), e.obj, uintptr(unsafe.Pointer(e.src)), uintptr(unsafe.Pointer(e.info))); int32(r) != 0 { // #nosec G103 G115 -- pinned
		return nil, false, fmt.Errorf("openh264: encode: %d", int32(r)) // #nosec G115 -- C int
	}
	if e.info.frameType == frameTypeSkip {
		return nil, false, nil
	}
	for i := range int(e.info.layerNum) {
		l := &e.info.layerInfo[i]
		lengths := unsafe.Slice((*int32)(cptr(l.nalLengthInByte)), int(l.nalCount)) // #nosec G103 -- the encoder's array
		off := uintptr(0)
		for _, n := range lengths {
			nal := unsafe.Slice((*byte)(cptr(l.bsBuf+off)), int(n)) // #nosec G103 -- the encoder's buffer
			off += uintptr(n)
			nalus = append(nalus, stripStartCode(nal))
		}
	}
	return nalus, e.info.frameType == frameTypeIDR, nil
}

// stripStartCode copies a NAL unit without its Annex B start code.
func stripStartCode(nal []byte) []byte {
	switch {
	case len(nal) >= 4 && nal[0] == 0 && nal[1] == 0 && nal[2] == 0 && nal[3] == 1:
		nal = nal[4:]
	case len(nal) >= 3 && nal[0] == 0 && nal[1] == 0 && nal[2] == 1:
		nal = nal[3:]
	}
	return append([]byte(nil), nal...)
}

// Close releases the encoder.
func (e *Encoder) Close() {
	if e.obj == 0 {
		return
	}
	call(vtable(e.obj, encUninitialize), e.obj)
	call(e.lib.destroyEncoder, e.obj)
	e.obj = 0
}
