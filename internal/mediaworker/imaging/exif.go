package imaging

import (
	"bytes"
	"encoding/binary"
)

// TIFF (EXIF) tags read from IFD0.
const (
	tagOrientation = 0x0112
	tagICCProfile  = 34675 // InterColorProfile (TIFF/EP)
)

// tiffTag returns the type and the value bytes of a tag of IFD0 of a TIFF
// structure (a TIFF file or the payload of an EXIF segment or chunk), and
// its byte order; ok is false when the structure is invalid or has no such
// tag, or its value lies outside the data.
func tiffTag(t []byte, tag uint16) (typ uint16, value []byte, bo binary.ByteOrder, ok bool) {
	if len(t) < 8 {
		return 0, nil, nil, false
	}
	switch string(t[:4]) {
	case "II*\x00":
		bo = binary.LittleEndian
	case "MM\x00*":
		bo = binary.BigEndian
	default:
		return 0, nil, nil, false
	}
	ifd := int64(bo.Uint32(t[4:]))
	if ifd < 8 || ifd+2 > int64(len(t)) {
		return 0, nil, nil, false
	}
	n := int64(bo.Uint16(t[ifd:]))
	for k := range n {
		e := ifd + 2 + k*12
		if e+12 > int64(len(t)) {
			return 0, nil, nil, false
		}
		if bo.Uint16(t[e:]) != tag {
			continue
		}
		typ = bo.Uint16(t[e+2:])
		size := tiffTypeSize(typ) * int64(bo.Uint32(t[e+4:]))
		if size == 0 {
			return 0, nil, nil, false
		}
		if size <= 4 { // the value fits in the entry, left-justified
			return typ, t[e+8 : e+8+size], bo, true
		}
		off := int64(bo.Uint32(t[e+8:]))
		if off < 8 || off+size > int64(len(t)) {
			return 0, nil, nil, false
		}
		return typ, t[off : off+size], bo, true
	}
	return 0, nil, nil, false
}

// tiffOrientation returns the Orientation tag of IFD0 of a TIFF structure,
// 1 when it has none or an invalid one.
func tiffOrientation(t []byte) int {
	typ, v, bo, ok := tiffTag(t, tagOrientation)
	if !ok || typ != 3 { // a SHORT
		return 1
	}
	if o := int(bo.Uint16(v)); o >= 1 && o <= 8 {
		return o
	}
	return 1
}

// tiffICC returns the ICC profile of IFD0 of a TIFF file, nil when it has
// none.
func tiffICC(t []byte) []byte {
	typ, v, _, ok := tiffTag(t, tagICCProfile)
	if !ok || (typ != 7 && typ != 1) { // UNDEFINED or BYTE
		return nil
	}
	return v
}

// exifPayload returns the TIFF structure of an EXIF block: some writers
// put the "Exif\0\0" header of the JPEG segment before it in PNG and WebP
// files too.
func exifPayload(b []byte) []byte {
	return bytes.TrimPrefix(b, []byte("Exif\x00\x00"))
}

// tiffTypeSize returns the size in bytes of a value of a TIFF field type,
// 0 for an unknown type.
func tiffTypeSize(typ uint16) int64 {
	switch typ {
	case 1, 2, 6, 7: // BYTE, ASCII, SBYTE, UNDEFINED
		return 1
	case 3, 8: // SHORT, SSHORT
		return 2
	case 4, 9, 11: // LONG, SLONG, FLOAT
		return 4
	case 5, 10, 12: // RATIONAL, SRATIONAL, DOUBLE
		return 8
	}
	return 0
}
